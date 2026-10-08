# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

from pathlib import Path
from types import SimpleNamespace

import pytest
import yaml
from kubernetes import client

from snapshot_e2e.upgrade import checks
from snapshot_e2e.upgrade import configs
from snapshot_e2e.upgrade import scenarios
from snapshot_e2e import k8s
from snapshot_e2e import lifecycle
from snapshot_e2e import workloads
from snapshot_e2e.upgrade.context import ScenarioState, Timings, UpgradeContext, UpgradeSettings


WORKFLOW = Path(__file__).resolve().parents[2] / ".github" / "workflows" / "e2e-upgrade.yaml"
ENV = (
    "SNAPSHOT_E2E_UPGRADE_FROM_TAG",
    "SNAPSHOT_E2E_SNAPSHOT_TAG",
    "SNAPSHOT_E2E_UPGRADE_CONFIG",
    "SNAPSHOT_E2E_UPGRADE_PROFILE",
    "SNAPSHOT_E2E_UPGRADE_SCENARIOS",
)


@pytest.fixture
def env(monkeypatch: pytest.MonkeyPatch) -> pytest.MonkeyPatch:
    for name in ENV:
        monkeypatch.delenv(name, raising=False)
    monkeypatch.setenv("SNAPSHOT_E2E_UPGRADE_FROM_TAG", "v0.1.0")
    monkeypatch.setenv("SNAPSHOT_E2E_SNAPSHOT_TAG", "v0.0.0-g1a2b3c4d")
    return monkeypatch


def test_settings_default_to_the_basic_full_upgrade(env: pytest.MonkeyPatch) -> None:
    settings = UpgradeSettings.from_env()

    assert (settings.from_tag, settings.to_tag) == ("v0.1.0", "v0.0.0-g1a2b3c4d")
    assert (settings.config, settings.profile, settings.scenarios) == ("full", "basic", ())


@pytest.mark.parametrize(
    ("name", "value", "message"),
    [
        ("SNAPSHOT_E2E_UPGRADE_FROM_TAG", "", "UPGRADE_FROM_TAG is required"),
        ("SNAPSHOT_E2E_SNAPSHOT_TAG", "", "SNAPSHOT_TAG is required"),
        ("SNAPSHOT_E2E_SNAPSHOT_TAG", "v0.1.0", "same version"),
        ("SNAPSHOT_E2E_UPGRADE_PROFILE", "nightly", "UPGRADE_PROFILE"),
    ],
)
def test_settings_reject_invalid_environment(
    env: pytest.MonkeyPatch, name: str, value: str, message: str
) -> None:
    env.setenv(name, value)

    with pytest.raises(ValueError, match=message):
        UpgradeSettings.from_env()


def test_basic_profile_selects_basic_scenarios(env: pytest.MonkeyPatch) -> None:
    selected = scenarios.selected(UpgradeSettings.from_env())

    assert [scenario.name for scenario in selected] == ["restore-pre-upgrade-snapshot-gpu", "delete-pre-upgrade-snapshot"]


def test_all_profile_includes_every_scenario(env: pytest.MonkeyPatch) -> None:
    env.setenv("SNAPSHOT_E2E_UPGRADE_PROFILE", "all")

    assert [scenario.name for scenario in scenarios.selected(UpgradeSettings.from_env())] == [
        "restore-pre-upgrade-snapshot-gpu",
        "delete-pre-upgrade-snapshot",
        "restore-pre-upgrade-snapshot-cpu",
        "restored-pod-survives-upgrade",
        "snapshotjob-in-flight",
        "snapshotjob-completed",
        "failed-restore-stays-failed",
        "compat-check-still-enforced",
    ]


def test_explicit_scenarios_override_the_profile(env: pytest.MonkeyPatch) -> None:
    env.setenv("SNAPSHOT_E2E_UPGRADE_SCENARIOS", "delete-pre-upgrade-snapshot")

    assert [s.name for s in scenarios.selected(UpgradeSettings.from_env())] == ["delete-pre-upgrade-snapshot"]


def test_unknown_scenario_is_rejected(env: pytest.MonkeyPatch) -> None:
    env.setenv("SNAPSHOT_E2E_UPGRADE_SCENARIOS", "restore-everything")

    with pytest.raises(ValueError, match="unknown upgrade scenario"):
        scenarios.selected(UpgradeSettings.from_env())


def test_only_one_scenario_holds_a_gpu_at_a_time() -> None:
    gpu = [scenario.name for scenario in scenarios.SCENARIOS if getattr(scenario, "gpu", False)]

    assert gpu == ["restore-pre-upgrade-snapshot-gpu", "failed-restore-stays-failed"], (
        "a new GPU scenario must not hold a GPU pod across the upgrade while another scenario "
        "needs its node's GPU afterwards (restores are pinned to the checkpoint node); "
        "check that, then update this list"
    )


def test_scenario_names_and_run_prefixes_are_unique() -> None:
    names = [scenario.name for scenario in scenarios.SCENARIOS]
    prefixes = [scenario.run_prefix for scenario in scenarios.SCENARIOS]

    assert len(set(names)) == len(names)
    assert len(set(prefixes)) == len(prefixes)
    assert all(len(prefix) <= 24 for prefix in prefixes)


@pytest.mark.parametrize(
    ("name", "operator", "agent", "reset_then_reuse"),
    [
        ("full", "v0.0.0-g1a2b3c4d", "v0.0.0-g1a2b3c4d", False),
        ("reset-then-reuse-values", "v0.0.0-g1a2b3c4d", "v0.0.0-g1a2b3c4d", True),
        ("operator-first", "v0.0.0-g1a2b3c4d", "v0.1.0", False),
        ("agent-first", "v0.1.0", "v0.0.0-g1a2b3c4d", False),
    ],
)
def test_configs_say_which_version_each_component_runs_after_the_upgrade(
    env: pytest.MonkeyPatch, name: str, operator: str, agent: str, reset_then_reuse: bool
) -> None:
    settings = UpgradeSettings.from_env()
    config = configs.get(name)

    assert (config.operator_tag(settings), config.agent_tag(settings), config.reset_then_reuse_values) == (
        operator,
        agent,
        reset_then_reuse,
    )


def test_manual_workflow_offers_every_config() -> None:
    workflow = yaml.safe_load(WORKFLOW.read_text(encoding="utf-8"))
    inputs = workflow[True]["workflow_dispatch"]["inputs"]

    options = inputs["upgrade_config"]["options"]

    assert options[0] == inputs["upgrade_config"]["default"] == "default"
    assert sorted(options[1:]) == sorted(configs.CONFIGS)


def test_upgrade_installs_the_checked_out_chart() -> None:
    assert configs.CHART_DIR.is_absolute()
    assert (configs.CHART_DIR / "Chart.yaml").is_file()


def test_unknown_config_is_rejected() -> None:
    with pytest.raises(ValueError, match="unknown upgrade config"):
        configs.get("sideways")


@pytest.mark.parametrize(
    ("image", "tag"),
    [
        ("ghcr.io/ai-dynamo/snapshot/operator:v0.1.0", "v0.1.0"),
        ("localhost:5000/snapshot/agent:v0.0.0-g1a2b3c4d", "v0.0.0-g1a2b3c4d"),
        ("ghcr.io/ai-dynamo/snapshot/agent:v0.1.0@sha256:abc", "v0.1.0"),
    ],
)
def test_image_tag(image: str, tag: str) -> None:
    assert checks.image_tag(image) == tag


def crd(properties: dict) -> dict:
    return {
        "spec": {
            "versions": [
                {"name": "v1alpha1", "schema": {"openAPIV3Schema": {"properties": properties}}}
            ]
        }
    }


def test_missing_crd_fields_reports_fields_the_served_crd_lacks() -> None:
    expected = crd(
        {
            "status": {
                "properties": {
                    "source": {"properties": {"node": {"type": "object"}}},
                    "conditions": {"items": {"properties": {"type": {"type": "string"}}}},
                }
            }
        }
    )
    served = crd({"status": {"properties": {"conditions": {"items": {"properties": {"type": {}}}}}}})

    assert checks.missing_crd_fields(expected, served) == [
        "v1alpha1.status.source",
        "v1alpha1.status.source.node",
    ]
    assert checks.missing_crd_fields(served, expected) == []


def test_every_generated_crd_has_a_schema_to_compare() -> None:
    files = sorted(checks.CRD_DIR.glob("*.yaml"))

    assert [path.name for path in files] == [
        "nvidia.com_podsnapshotcontents.yaml",
        "nvidia.com_podsnapshots.yaml",
        "nvidia.com_snapshotjobs.yaml",
    ]
    for path in files:
        schemas = checks.crd_version_schemas(yaml.safe_load(Path(path).read_text(encoding="utf-8")))
        assert all(checks.schema_paths(schema) for schema in schemas.values()), path.name


def test_timings_summary_lists_each_phase(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    summary = tmp_path / "summary.md"
    monkeypatch.setenv("GITHUB_STEP_SUMMARY", str(summary))
    timings = Timings()
    with timings.phase("PreUpgrade restore-pre-upgrade-snapshot-gpu"):
        pass
    with pytest.raises(RuntimeError), timings.phase("Upgrade (full)"):
        raise RuntimeError("helm failed")
    with pytest.raises(pytest.skip.Exception), timings.phase("PostUpgrade compat-check-still-enforced"):
        pytest.skip("v0.1.0 does not record memory limits")

    timings.publish("Upgrade v0.1.0 → main")

    text = summary.read_text(encoding="utf-8")
    assert "### Upgrade v0.1.0 → main" in text
    assert "| PreUpgrade restore-pre-upgrade-snapshot-gpu | 0s | ok |" in text
    assert "| Upgrade (full) | 0s | failed |" in text
    assert "| PostUpgrade compat-check-still-enforced | 0s | skipped |" in text
    assert "| **Total** |" in text


def test_upgrade_reapplies_the_install_helm_overrides(env: pytest.MonkeyPatch) -> None:
    env.setenv("SNAPSHOT_E2E_HELM_SET", "image.agent.pullPolicy=IfNotPresent\nruntime.storageDir=/var/lib/k3s")
    calls = []
    env.setattr(configs.setup, "install_snapshot_chart", lambda **kwargs: calls.append(kwargs))
    config = k8s.E2EConfig(namespace="snapshot-e2e", release="snapshot", pvc_name="snapshot-pvc", kubeconfig=None)

    configs.get("full").apply(UpgradeContext(config=config, settings=UpgradeSettings.from_env()))

    assert calls[0]["helm_overrides"] == ["image.agent.pullPolicy=IfNotPresent", "runtime.storageDir=/var/lib/k3s"]


TO = "v0.0.0-g1a2b3c4d"


def agent_pod(agent: str, pagebroker: str | None) -> client.V1Pod:
    containers = [client.V1Container(name="agent", image=f"ghcr.io/ai-dynamo/snapshot/agent:{agent}")]
    if pagebroker:
        containers.append(
            client.V1Container(name="pagebroker", image=f"ghcr.io/ai-dynamo/snapshot/pagebroker:{pagebroker}")
        )
    return client.V1Pod(metadata=client.V1ObjectMeta(name="agent-x"), spec=client.V1PodSpec(containers=containers))


@pytest.mark.parametrize(
    ("deploys", "agent", "pagebroker", "problem"),
    [
        (True, TO, TO, None),
        (True, TO, None, "without pagebroker"),
        (True, TO, "v0.1.0", "runs pagebroker v0.1.0"),
        (False, TO, None, None),
        (False, TO, TO, "does not deploy"),
        (True, "v0.1.0", None, None),
        (False, "v0.1.0", "v0.1.0", None),
    ],
)
def test_pagebroker_matches_what_the_upgraded_chart_deploys(
    deploys: bool, agent: str, pagebroker: str | None, problem: str | None
) -> None:
    found = checks.pagebroker_problem(agent_pod(agent, pagebroker), TO, deploys)

    assert (found is None) if problem is None else (problem in found)


@pytest.mark.parametrize(("name", "kind"), [("agent-first", "deployments"), ("operator-first", "daemonsets")])
def test_held_rollouts_patch_with_strategic_merge(env: pytest.MonkeyPatch, name: str, kind: str) -> None:
    requests = []

    def call_api(self, resource_path, method, path_params=None, query_params=None, header_params=None, *args, **kwargs):
        requests.append((resource_path, method, (header_params or {}).get("Content-Type")))

    env.setattr(client.ApiClient, "call_api", call_api)
    env.setattr(checks, "operator_deployment", lambda ctx: client.V1Deployment(metadata=client.V1ObjectMeta(name="op")))
    env.setattr(checks, "agent_daemonset", lambda ctx: client.V1DaemonSet(metadata=client.V1ObjectMeta(name="agent")))
    env.setattr(configs.setup, "install_snapshot_chart", lambda **kwargs: None)
    config = k8s.E2EConfig(namespace="snapshot-e2e", release="snapshot", pvc_name="snapshot-pvc", kubeconfig=None)

    configs.get(name).apply(UpgradeContext(config=config, settings=UpgradeSettings.from_env()))

    assert [(method, content_type) for path, method, content_type in requests if kind in path] == [
        ("PATCH", "application/strategic-merge-patch+json")
    ]


def test_in_flight_snapshotjob_waits_for_the_release_file_before_its_workload(env: pytest.MonkeyPatch) -> None:
    config = k8s.E2EConfig(namespace="snapshot-e2e", release="snapshot", pvc_name="snapshot-pvc", kubeconfig=None)
    ctx = UpgradeContext(config=config, settings=None)
    state = ScenarioState(run=workloads.TestRun.new("upg-sj-inflight"))

    command = scenarios.gated_snapshotjob_template(ctx, state)["spec"]["containers"][0]["command"]

    assert command[:2] == ["/bin/bash", "-lc"]
    gate, workload = command[2].split("\n", 1)
    assert gate == f"while [ ! -f {scenarios.SNAPSHOTJOB_GATE} ]; do sleep 1; done"
    assert workload == workloads.snapshotjob_source_command(state.run.image, False)


@pytest.mark.parametrize(("config", "applies"), [("full", True), ("agent-first", True), ("operator-first", False)])
def test_failed_restore_needs_a_restarted_agent(env: pytest.MonkeyPatch, config: str, applies: bool) -> None:
    env.setenv("SNAPSHOT_E2E_UPGRADE_CONFIG", config)
    ctx = UpgradeContext(
        config=k8s.E2EConfig(namespace="n", release="r", pvc_name="p", kubeconfig=None),
        settings=UpgradeSettings.from_env(),
    )

    assert scenarios.by_name("failed-restore-stays-failed").applies_to(ctx) is applies


@pytest.mark.parametrize("makes_progress", [False, True])
def test_surviving_pod_requires_progress_after_the_upgrade(
    env: pytest.MonkeyPatch, makes_progress: bool
) -> None:
    ctx = UpgradeContext(
        config=k8s.E2EConfig(namespace="n", release="r", pvc_name="p", kubeconfig=None),
        settings=UpgradeSettings.from_env(),
    )
    state = ScenarioState(
        run=workloads.TestRun.new("upg-survivor"), observations=10, restored_pod_uid="restored-uid"
    )
    pod = client.V1Pod(
        metadata=client.V1ObjectMeta(uid=state.restored_pod_uid),
        status=client.V1PodStatus(
            phase="Running",
            container_statuses=[
                client.V1ContainerStatus(name="main", image="test", image_id="test", ready=True, restart_count=0)
            ],
        ),
    )
    env.setattr(k8s, "read_pod", lambda namespace, name: pod)
    # The file grew from 10 to 30 while the other pre-upgrade scenarios ran.
    # Only the second case writes anything after the upgrade.
    counts = iter([30, 30, 31, 32] if makes_progress else [30])
    seen = []

    def observations(*args, **kwargs) -> int:
        value = next(counts, seen[-1] if seen else 30)
        seen.append(value)
        return value

    env.setattr(lifecycle, "matching_observation_count", observations)
    env.setattr(lifecycle, "observations_tail", lambda *args: "no new observations")
    elapsed = 0.0

    def sleep(seconds: float) -> None:
        nonlocal elapsed
        elapsed += 60

    env.setattr(
        lifecycle, "time", SimpleNamespace(monotonic=lambda: elapsed, sleep=sleep, strftime=lambda fmt: "test")
    )
    scenario = scenarios.by_name("restored-pod-survives-upgrade")
    if makes_progress:
        scenario.post_upgrade(ctx, state)
        assert seen[-1] == 32
    else:
        with pytest.raises(lifecycle.LifecycleTimeoutError, match="observations for source token"):
            scenario.post_upgrade(ctx, state)
