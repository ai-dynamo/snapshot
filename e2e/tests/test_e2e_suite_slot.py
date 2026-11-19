# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Cluster-free checks of the GPU E2E suite slots in hack/e2e-suite-slot.py."""

from __future__ import annotations

import copy
import importlib.util
import json
import subprocess
import sys
from datetime import datetime, timezone
from decimal import Decimal
from pathlib import Path
from types import ModuleType, SimpleNamespace

import pytest
import yaml


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "hack" / "e2e-suite-slot.py"
GIB = 2**30
ME = "snapshot-e2e-fw-vllm-100-1"
OTHER = "snapshot-e2e-200-1"


@pytest.fixture
def slot(monkeypatch: pytest.MonkeyPatch) -> ModuleType:
    spec = importlib.util.spec_from_file_location("e2e_suite_slot", SCRIPT)
    assert spec and spec.loader
    module = importlib.util.module_from_spec(spec)
    monkeypatch.setitem(sys.modules, spec.name, module)
    spec.loader.exec_module(module)
    for name in ("GITHUB_RUN_ID", "GITHUB_RUN_ATTEMPT", "GITHUB_SERVER_URL", "GITHUB_REPOSITORY", "HOST_NAMESPACE"):
        monkeypatch.delenv(name, raising=False)
    monkeypatch.setattr(module, "random", SimpleNamespace(shuffle=lambda items: None, uniform=lambda low, high: 0.0))
    return module


class Clock:
    def __init__(self) -> None:
        self.now = 1_800_000_000.0
        self.hooks: list = []

    def time(self) -> float:
        return self.now

    def monotonic(self) -> float:
        return self.now

    def sleep(self, seconds: float) -> None:
        self.now += seconds
        for hook in list(self.hooks):
            hook()

    def at(self, seconds_ago: float = 0) -> str:
        moment = datetime.fromtimestamp(self.now - seconds_ago, tz=timezone.utc)
        return moment.strftime("%Y-%m-%dT%H:%M:%S.%fZ")


@pytest.fixture
def clock(slot: ModuleType, monkeypatch: pytest.MonkeyPatch) -> Clock:
    fake = Clock()
    monkeypatch.setattr(slot, "time", fake)
    return fake


def gpu_node(name: str, cpu: str = "96", memory: str = "880Gi", gpus: str = "16", taints=None, labels=None) -> dict:
    node_labels = {"nvidia.com/gpu.present": "true"} if labels is None else labels
    return {
        "metadata": {"name": name, "labels": node_labels},
        "spec": {"taints": taints or []},
        "status": {"allocatable": {"cpu": cpu, "memory": memory, "nvidia.com/gpu": gpus}},
    }


def pod(namespace: str, node: str, cpu: str = "0", memory: str = "0", gpus: str | None = None, **extra) -> dict:
    requests = {"cpu": cpu, "memory": memory}
    if gpus:
        requests["nvidia.com/gpu"] = gpus
    spec = {"nodeName": node, "containers": [{"name": "main", "resources": {"requests": requests}}]}
    spec.update(extra.get("spec", {}))
    return {
        "metadata": {"name": f"{namespace}-pod", "namespace": namespace},
        "spec": spec,
        "status": {"phase": extra.get("phase", "Running")},
    }


class FakeCluster:
    def __init__(self, slot: ModuleType) -> None:
        self.slot = slot
        self.leases: dict[str, dict] = {}
        self.version = 0
        self.nodes = [gpu_node("node-a"), gpu_node("node-b")]
        self.pods: list[dict] = []
        self.namespaces: dict[str, dict[str, str]] = {}
        self.deleted: list[str] = []
        self.failures: dict[tuple[str, str], str] = {}
        self.failures_once: dict[tuple[str, str], str] = {}
        self.before_write = None
        self.writes = 0

    def error(self, args: tuple, reason: str) -> Exception:
        return self.slot.KubectlError(list(args), f"Error from server ({reason}): {reason.lower()}")

    def seed(self, index: int, holder: str, renewed: str, acquired: str | None = None) -> dict:
        self.version += 1
        lease = {
            "metadata": {
                "name": f"suite-slot-{index}",
                "namespace": "snapshot-e2e-slots",
                "resourceVersion": str(self.version),
            },
            "spec": {
                "holderIdentity": holder,
                "leaseDurationSeconds": 300,
                "acquireTime": acquired or renewed,
                "renewTime": renewed,
            },
        }
        self.leases[lease["metadata"]["name"]] = lease
        return lease

    def holder(self, index: int) -> str:
        return self.leases[f"suite-slot-{index}"]["spec"].get("holderIdentity", "")

    def __call__(self, *args: str, stdin: str | None = None) -> str:
        verb = args[0]
        kind = "lease" if args[1] == "-f" else args[1]
        for failures in (self.failures_once, self.failures):
            reason = failures.get((verb, kind))
            if reason:
                if failures is self.failures_once:
                    del failures[(verb, kind)]
                raise self.error(args, reason)

        if (verb, kind) == ("create", "namespace"):
            if args[2] in self.namespaces:
                raise self.error(args, "AlreadyExists")
            self.namespaces[args[2]] = {}
            return ""
        if verb == "get":
            items = {
                "leases": lambda: list(self.leases.values()),
                "nodes": lambda: self.nodes,
                "pods": lambda: self.pods,
                "namespaces": lambda: [
                    {"metadata": {"name": name, "labels": labels}}
                    for name, labels in self.namespaces.items()
                    if labels.get("snapshot-e2e") == "true"
                ],
            }
            if kind == "lease":
                if args[2] not in self.leases:
                    raise self.error(args, "NotFound")
                return json.dumps(self.leases[args[2]])
            return json.dumps({"items": copy.deepcopy(items[kind]())})
        if verb in ("create", "replace"):
            if self.before_write:
                hook, self.before_write = self.before_write, None
                hook()
            body = json.loads(stdin)
            name = body["metadata"]["name"]
            current = self.leases.get(name)
            if verb == "create" and current:
                raise self.error(args, "AlreadyExists")
            if verb == "replace":
                if not current:
                    raise self.error(args, "NotFound")
                if body["metadata"].get("resourceVersion") != current["metadata"]["resourceVersion"]:
                    raise self.error(args, "Conflict")
            self.version += 1
            self.writes += 1
            body["metadata"]["resourceVersion"] = str(self.version)
            self.leases[name] = body
            return ""
        if (verb, kind) == ("delete", "namespace"):
            self.deleted.append(args[2])
            self.namespaces.pop(args[2], None)
            return ""
        raise AssertionError(f"unexpected kubectl {args}")


@pytest.fixture
def cluster(slot: ModuleType, clock: Clock, monkeypatch: pytest.MonkeyPatch) -> FakeCluster:
    fake = FakeCluster(slot)
    fake.renewers = []
    fake.best_effort = []
    monkeypatch.setattr(slot, "kubectl", fake)
    monkeypatch.setattr(
        slot, "start_renewer", lambda name, identity, log_path: fake.renewers.append((name, identity)) or 4242
    )
    monkeypatch.setattr(slot, "run_best_effort", lambda command: fake.best_effort.append(command))
    return fake


def take(slot: ModuleType, tmp_path: Path, *extra: str, identity: str = ME) -> int:
    args = slot.parse_args(
        ["take", "--identity", identity, "--state-file", str(tmp_path / f"{identity}.json"), *extra]
    )
    return slot.take(args)


def limit(slot: ModuleType, cluster: FakeCluster, holders=(), e2e=()):
    return slot.compute_limit(cluster.nodes, cluster.pods, set(holders), set(e2e))


@pytest.mark.parametrize(
    ("raw", "value"),
    [
        ("500m", Decimal("0.5")),
        ("2", Decimal(2)),
        ("1.5", Decimal("1.5")),
        ("1Gi", Decimal(GIB)),
        ("64Mi", Decimal(64 * 2**20)),
        ("1056731148Ki", Decimal(1056731148 * 1024)),
        ("1k", Decimal(1000)),
        ("2G", Decimal(2 * 10**9)),
        ("1e3", Decimal(1000)),
    ],
)
def test_quantities_are_read_like_kubernetes_reads_them(slot, raw, value):
    assert slot.quantity(raw) == value


def test_an_unreadable_quantity_is_a_capacity_error(slot):
    with pytest.raises(slot.CapacityError, match="lots"):
        slot.quantity("lots")


def test_the_tightest_node_sets_the_limit(slot, cluster):
    cluster.pods = [pod("training", "node-b", cpu="60")]
    result = limit(slot, cluster)
    assert result.value == 2
    assert "the tightest GPU node node-b has 96.0 CPUs and 880 GiB allocatable, 60.0 CPUs" in result.detail
    assert "room for 3 suites" in result.detail


def test_memory_can_be_the_tight_resource(slot, cluster):
    cluster.nodes = [gpu_node("node-a", memory="200Gi")]
    result = limit(slot, cluster)
    assert result.value == 4
    assert "room for 5 suites" in result.detail


def test_free_gpus_cap_the_limit(slot, cluster):
    cluster.nodes = [gpu_node("node-a", gpus="2"), gpu_node("node-b", gpus="2")]
    cluster.pods = [pod("inference", "node-b", gpus="1")]
    result = limit(slot, cluster)
    assert result.value == 1
    assert "3 free GPUs fit 1 suites of 2" in result.detail


@pytest.mark.parametrize(("suites_that_fit", "expected"), [(0, 0), (1, 1), (2, 1), (3, 2), (9, 8)])
def test_one_suite_of_headroom_is_kept_once_two_fit(slot, cluster, suites_that_fit, expected):
    cpu = slot.SUITE_CPU * suites_that_fit + Decimal("0.4")
    cluster.nodes = [gpu_node("node-a", cpu=str(cpu), gpus="32")]
    assert limit(slot, cluster).value == expected


def test_slot_holders_use_their_budget_and_unslotted_suites_use_room(slot, cluster):
    cluster.nodes = [gpu_node("node-a", gpus="32")]
    cluster.pods = [pod(OTHER, "node-a", cpu="10", memory="33Gi")]
    held = limit(slot, cluster, holders={OTHER}, e2e={OTHER})
    assert held.value == 8
    assert f"10.0 CPUs and 33 GiB ({OTHER} on node-a)" in held.detail
    assert "0 E2E namespaces hold no slot" in held.detail

    unslotted = limit(slot, cluster, e2e={OTHER})
    assert unslotted.value == 7
    assert "1 E2E namespaces hold no slot" in unslotted.detail


def test_init_containers_and_overhead_count_like_the_scheduler_counts_them(slot):
    workload = pod(
        "ns",
        "node-a",
        cpu="2",
        memory="1Gi",
        spec={
            "initContainers": [{"name": "init", "resources": {"requests": {"cpu": "6", "memory": "512Mi"}}}],
            "overhead": {"cpu": "250m"},
        },
    )
    assert slot.pod_requests(workload) == {"cpu": Decimal("6.25"), "memory": Decimal(GIB)}


def test_finished_pods_and_nodes_closed_to_the_agent_are_skipped(slot, cluster):
    cluster.nodes = [
        gpu_node("node-a"),
        gpu_node("tainted", cpu="1", taints=[{"key": "maintenance", "effect": "NoSchedule"}]),
        gpu_node("cpu-only", cpu="1", labels={}),
        gpu_node(
            "gpu-tainted",
            taints=[
                {"key": "nvidia.com/gpu", "effect": "NoSchedule"},
                {"key": "node.kubernetes.io/unschedulable", "effect": "NoSchedule"},
                {"key": "prefer", "effect": "PreferNoSchedule"},
            ],
        ),
    ]
    cluster.pods = [
        pod("done", "node-a", cpu="90", phase="Succeeded"),
        pod("unscheduled", "", cpu="90"),
    ]
    result = limit(slot, cluster)
    assert result.value == 8
    assert "2 GPU nodes" in result.detail


def test_no_gpu_node_is_a_capacity_error(slot, cluster):
    cluster.nodes = [gpu_node("cpu-only", labels={})]
    with pytest.raises(slot.CapacityError, match="no node is labelled"):
        limit(slot, cluster)


@pytest.mark.parametrize("kind", ["nodes", "pods"])
def test_unreadable_capacity_falls_back_to_the_fixed_limit(slot, cluster, kind):
    cluster.failures[("get", kind)] = "Forbidden"
    result = slot.current_limit(set(), set(), None)
    assert result.value == slot.FALLBACK_LIMIT == 8
    assert "the fallback: the GPU nodes could not be read" in result.detail


def test_max_running_caps_the_computed_limit(slot, cluster):
    result = slot.current_limit(set(), set(), 2)
    assert result.value == 2
    assert result.detail.startswith("suite limit 8:")
    assert result.detail.endswith("; capped at 2 by --max-running")


def test_the_budget_covers_the_charts_default_agent_requests(slot):
    values = yaml.safe_load((ROOT / "charts" / "snapshot" / "values.yaml").read_text())
    containers = [values["daemonset"]["resources"]["requests"], values["pageBroker"]["resources"]["requests"]]
    assert sum(slot.quantity(requests["cpu"]) for requests in containers) <= slot.SUITE_CPU
    assert sum(slot.quantity(requests["memory"]) for requests in containers) <= slot.SUITE_MEMORY


def test_take_creates_a_missing_slot_and_records_it(slot, cluster, tmp_path, monkeypatch):
    monkeypatch.setenv("GITHUB_RUN_ID", "100")
    monkeypatch.setenv("GITHUB_RUN_ATTEMPT", "1")
    monkeypatch.setenv("GITHUB_SERVER_URL", "https://github.com")
    monkeypatch.setenv("GITHUB_REPOSITORY", "ai-dynamo/snapshot")

    assert take(slot, tmp_path) == 0

    lease = cluster.leases["suite-slot-0"]
    assert lease["spec"]["holderIdentity"] == ME
    assert lease["metadata"]["labels"] == {"snapshot.github/run-id": "100", "snapshot.github/run-attempt": "1"}
    assert lease["metadata"]["annotations"] == {
        "snapshot.github/run-url": "https://github.com/ai-dynamo/snapshot/actions/runs/100"
    }
    assert "snapshot-e2e-slots" in cluster.namespaces
    assert cluster.renewers == [("suite-slot-0", ME)]
    state = json.loads((tmp_path / f"{ME}.json").read_text())
    assert state["slot"] == "suite-slot-0"
    assert state["identity"] == ME
    assert state["renewer_pid"] == 4242


def test_take_reuses_a_released_slot(slot, cluster, clock, tmp_path):
    cluster.seed(0, "", clock.at(30))
    assert take(slot, tmp_path) == 0
    assert cluster.holder(0) == ME


def test_take_waits_while_every_slot_is_held_then_takes_the_first_freed(slot, cluster, clock, tmp_path, capsys):
    cluster.seed(0, OTHER, clock.at())
    start = clock.now

    def release_after_two_minutes():
        if clock.now - start >= 120:
            cluster.seed(0, "", clock.at())

    clock.hooks.append(release_after_two_minutes)
    assert take(slot, tmp_path, "--max-running", "1") == 0
    assert cluster.holder(0) == ME
    assert 120 <= clock.now - start < 160
    assert f"held: suite-slot-0 {OTHER}, 0 min" in capsys.readouterr().out


def test_take_times_out_and_names_the_holders(slot, cluster, clock, tmp_path, capsys):
    acquired = clock.at()
    cluster.seed(0, OTHER, acquired)
    clock.hooks.append(lambda: cluster.seed(0, OTHER, clock.at(), acquired=acquired))
    assert take(slot, tmp_path, "--max-running", "1", "--timeout-minutes", "1") == 1
    out = capsys.readouterr().out
    assert (
        "::error title=No suite slot::waited 1 minutes for one of 1 suite slots; "
        f"held: suite-slot-0 {OTHER}, 1 min"
    ) in out
    assert cluster.holder(0) == OTHER
    assert not (tmp_path / f"{ME}.json").exists()


@pytest.mark.parametrize("released", [False, True])
def test_a_taker_that_loses_a_race_moves_on_to_the_next_slot(slot, cluster, clock, tmp_path, released):
    if released:
        cluster.seed(0, "", clock.at())
    start = clock.now

    def other_taker_wins():
        lease = cluster.leases.get("suite-slot-0")
        slot.claim("suite-slot-0", copy.deepcopy(lease), OTHER, slot.utc_now())

    cluster.before_write = other_taker_wins
    assert take(slot, tmp_path, "--max-running", "2") == 0
    assert cluster.holder(0) == OTHER
    assert cluster.holder(1) == ME
    assert clock.now == start


def test_takers_never_exceed_the_limit(slot, cluster, tmp_path):
    results = [
        take(slot, tmp_path, "--max-running", "2", "--timeout-minutes", "1", identity=f"suite-{n}")
        for n in range(3)
    ]
    assert results == [0, 0, 1]
    assert {cluster.holder(0), cluster.holder(1)} == {"suite-0", "suite-1"}
    assert "suite-slot-2" not in cluster.leases


def test_a_slot_above_a_shrunken_limit_is_left_to_its_holder(slot, cluster, clock, tmp_path):
    cluster.seed(5, OTHER, clock.at())
    assert take(slot, tmp_path, "--max-running", "1") == 0
    assert cluster.holder(0) == ME
    assert cluster.holder(5) == OTHER


def test_take_reuses_a_slot_it_already_holds(slot, cluster, clock, tmp_path):
    cluster.seed(3, ME, clock.at())
    writes = cluster.writes
    assert take(slot, tmp_path, "--max-running", "1") == 0
    assert cluster.writes == writes
    assert json.loads((tmp_path / f"{ME}.json").read_text())["slot"] == "suite-slot-3"


def test_an_unrenewed_slot_is_free_only_after_its_duration_and_grace(slot, cluster, clock, tmp_path):
    renewed = clock.now - 300
    cluster.seed(0, OTHER, clock.at(300))
    assert take(slot, tmp_path, "--max-running", "1") == 0
    assert cluster.holder(0) == ME
    assert 360 <= clock.now - renewed < 385


@pytest.mark.parametrize(
    ("expired_seconds", "labelled", "reaped"),
    [(599, True, False), (600, True, True), (900, False, False)],
)
def test_a_dead_runs_namespace_is_reaped_only_once_it_is_clearly_dead(
    slot, cluster, clock, tmp_path, expired_seconds, labelled, reaped
):
    cluster.seed(0, OTHER, clock.at(360 + expired_seconds))
    cluster.namespaces[OTHER] = {"snapshot-e2e": "true"} if labelled else {}
    assert take(slot, tmp_path, "--max-running", "1") == 0
    assert cluster.holder(0) == ME
    assert cluster.deleted == ([OTHER] if reaped else [])
    if reaped:
        assert ["helm", "uninstall", "vcluster-hpm", "-n", OTHER] in cluster.best_effort


def test_take_fails_open_when_the_slots_are_forbidden(slot, cluster, tmp_path, capsys):
    cluster.failures[("get", "leases")] = "Forbidden"
    assert take(slot, tmp_path) == 0
    assert "::warning title=Running without a suite slot::" in capsys.readouterr().out
    assert not (tmp_path / f"{ME}.json").exists()


def test_take_retries_a_transient_slot_error(slot, cluster, tmp_path):
    cluster.failures_once[("get", "leases")] = "InternalError"
    assert take(slot, tmp_path) == 0
    assert cluster.holder(0) == ME


def test_take_fails_open_after_repeated_slot_errors(slot, cluster, tmp_path, capsys):
    cluster.failures[("get", "leases")] = "InternalError"
    assert take(slot, tmp_path) == 0
    assert "::warning title=Running without a suite slot::" in capsys.readouterr().out


def renew(slot: ModuleType, monkeypatch: pytest.MonkeyPatch, minutes: int) -> int:
    monkeypatch.setattr(slot, "RENEW_MAX_SECONDS", minutes * 60)
    return slot.renew(slot.parse_args(["renew", "--slot", "suite-slot-0", "--identity", ME]))


def test_renewal_refreshes_the_lease(slot, cluster, clock, monkeypatch):
    acquired = clock.at(600)
    cluster.seed(0, ME, clock.at(50), acquired=acquired)
    assert renew(slot, monkeypatch, 2) == 0
    lease = cluster.leases["suite-slot-0"]
    assert lease["spec"]["renewTime"] == clock.at()
    assert lease["spec"]["acquireTime"] == acquired
    assert cluster.writes == 2


def test_renewal_stops_when_the_lease_is_lost(slot, cluster, clock, monkeypatch, capsys):
    cluster.seed(0, OTHER, clock.at())
    assert renew(slot, monkeypatch, 10) == 0
    assert cluster.writes == 0
    assert f"suite slot suite-slot-0 is held by {OTHER}; stopped renewing" in capsys.readouterr().out


def test_renewal_survives_a_transient_error(slot, cluster, clock, monkeypatch, capsys):
    cluster.seed(0, ME, clock.at())
    cluster.failures_once[("get", "lease")] = "InternalError"
    assert renew(slot, monkeypatch, 2) == 0
    assert cluster.writes == 1
    assert "Could not renew suite slot suite-slot-0" in capsys.readouterr().out


def release(slot: ModuleType, tmp_path: Path) -> int:
    return slot.release(slot.parse_args(["release", "--state-file", str(tmp_path / "state.json")]))


def record(tmp_path: Path, identity: str = ME) -> None:
    (tmp_path / "state.json").write_text(json.dumps({"slot": "suite-slot-0", "identity": identity, "renewer_pid": 4242}))


def test_release_frees_its_own_slot(slot, cluster, clock, tmp_path, monkeypatch):
    stopped = []
    monkeypatch.setattr(slot, "stop_renewer", stopped.append)
    cluster.seed(0, ME, clock.at())
    record(tmp_path)
    assert release(slot, tmp_path) == 0
    lease = cluster.leases["suite-slot-0"]
    assert "holderIdentity" not in lease["spec"]
    assert lease["metadata"]["labels"] == {}
    assert stopped == [4242]
    assert not (tmp_path / "state.json").exists()


def test_release_leaves_another_holders_slot_alone(slot, cluster, clock, tmp_path, monkeypatch, capsys):
    monkeypatch.setattr(slot, "stop_renewer", lambda pid: None)
    cluster.seed(0, OTHER, clock.at())
    record(tmp_path)
    assert release(slot, tmp_path) == 0
    assert cluster.holder(0) == OTHER
    assert f"held by {OTHER}, not {ME}; left alone" in capsys.readouterr().out


def test_release_without_a_recorded_slot_does_nothing(slot, cluster, tmp_path, capsys):
    assert release(slot, tmp_path) == 0
    assert cluster.writes == 0
    assert "No suite slot held" in capsys.readouterr().out


def test_release_warns_instead_of_failing(slot, cluster, tmp_path, monkeypatch, capsys):
    monkeypatch.setattr(slot, "stop_renewer", lambda pid: None)
    cluster.failures[("get", "lease")] = "Forbidden"
    record(tmp_path)
    assert release(slot, tmp_path) == 0
    assert "::warning title=Suite slot not released::" in capsys.readouterr().out


def test_annotations_stay_on_one_line(slot, capsys):
    slot.annotate("warning", "Title", "first\nsecond 100%")
    assert capsys.readouterr().out == "::warning title=Title::first%0Asecond 100%25\n"


def test_the_script_needs_only_the_standard_library():
    result = subprocess.run(
        [sys.executable, "-I", "-S", str(SCRIPT), "take", "--help"],
        capture_output=True,
        text=True,
        check=False,
    )
    assert result.returncode == 0, result.stderr
    assert "--max-running" in result.stdout
