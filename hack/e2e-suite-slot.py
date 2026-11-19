# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Hold one of a limited number of GPU E2E suite slots on the host cluster.

Every GPU, framework and upgrade suite creates its own vCluster on the same GPU
nodes and installs Snapshot in it, which places an agent Pod on every GPU node.
Once a node has no room for another agent, the next suite's agents stay Pending
and its setup times out. So a suite first takes a slot: one of the Leases
suite-slot-0 to suite-slot-<N-1> in the snapshot-e2e-slots namespace.

N is computed from the GPU nodes while a suite waits: how many suite budgets
fit into what the tightest node has left after every Pod outside the
slot-holding namespaces, capped by free GPUs, keeping one suite of headroom.
If the nodes or Pods cannot be read, N is FALLBACK_LIMIT.

take waits for a free slot, claims it and starts a detached renewer. release
stops the renewer and frees the slot. A Lease nobody renews expires, so a run
that lost its runner frees its slot too.
"""

from __future__ import annotations

import argparse
import json
import os
import random
import re
import shutil
import signal
import subprocess
import sys
import time
from dataclasses import dataclass
from datetime import datetime, timezone
from decimal import Decimal, InvalidOperation
from pathlib import Path


NAMESPACE = "snapshot-e2e-slots"
SLOT_PREFIX = "suite-slot-"
E2E_NAMESPACE_SELECTOR = "snapshot-e2e=true"
GPU_NODE_LABEL = "nvidia.com/gpu.present"
GPU_RESOURCE = "nvidia.com/gpu"
AGENT_ARCHITECTURES = ("amd64", "arm64")
AKS_USER_NODE_LABEL = "kubernetes.azure.com/mode"
AKS_USER_NODE_VALUE = "user"
LEASE_SECONDS = 300
GRACE_SECONDS = 60
RENEW_SECONDS = 60
RENEW_MAX_SECONDS = 6 * 3600
POLL_SECONDS = 20
LIMIT_REFRESH_SECONDS = 60
REPORT_SECONDS = 60
REAP_AFTER_SECONDS = 600
LEASE_ERROR_POLLS = 3
FALLBACK_LIMIT = 8
# The chart's default agent and PageBroker requests (10 CPUs, 33 GiB) plus the
# hostpath mapper and vCluster's own Pods.
SUITE_CPU = Decimal("10.5")
SUITE_MEMORY = Decimal(34 * 2**30)
SUITE_GPUS = Decimal(2)
DEFAULT_STATE_FILE = ".snapshot-e2e-slot.json"
DEFAULT_TIMEOUT_MINUTES = 175

QUANTITY_SUFFIXES = {
    "Ki": Decimal(2**10),
    "Mi": Decimal(2**20),
    "Gi": Decimal(2**30),
    "Ti": Decimal(2**40),
    "Pi": Decimal(2**50),
    "Ei": Decimal(2**60),
    "n": Decimal("1e-9"),
    "u": Decimal("1e-6"),
    "m": Decimal("1e-3"),
    "k": Decimal(10**3),
    "M": Decimal(10**6),
    "G": Decimal(10**9),
    "T": Decimal(10**12),
    "P": Decimal(10**15),
    "E": Decimal(10**18),
}


class KubectlError(RuntimeError):
    def __init__(self, args: list[str], stderr: str) -> None:
        self.stderr = stderr.strip()
        super().__init__(f"kubectl {' '.join(args)}: {self.stderr}")

    @property
    def reason(self) -> str:
        match = re.search(r"Error from server \((\w+)\)", self.stderr)
        return match.group(1) if match else ""


class CapacityError(RuntimeError):
    pass


@dataclass(frozen=True)
class Limit:
    value: int
    detail: str


def kubectl(*args: str, stdin: str | None = None) -> str:
    try:
        result = subprocess.run(
            ["kubectl", *args],
            input=stdin,
            capture_output=True,
            text=True,
            timeout=60,
        )
    except (OSError, subprocess.TimeoutExpired) as exc:
        raise KubectlError(list(args), str(exc)) from exc
    if result.returncode != 0:
        raise KubectlError(list(args), result.stderr)
    return result.stdout


def kubectl_json(*args: str) -> dict:
    return json.loads(kubectl(*args, "-o", "json"))


def utc_now() -> datetime:
    return datetime.fromtimestamp(time.time(), tz=timezone.utc)


def format_time(moment: datetime) -> str:
    return moment.strftime("%Y-%m-%dT%H:%M:%S.%fZ")


def parse_time(value: str | None) -> datetime | None:
    if not value:
        return None
    return datetime.fromisoformat(value.replace("Z", "+00:00"))


def log(message: str) -> None:
    print(f"[{utc_now().strftime('%H:%M:%S')}] {message}", flush=True)


def annotate(level: str, title: str, message: str) -> None:
    escaped = message.replace("%", "%25").replace("\r", "%0D").replace("\n", "%0A")
    print(f"::{level} title={title}::{escaped}", flush=True)


def quantity(value: object) -> Decimal:
    text = str(value).strip()
    try:
        for suffix, factor in QUANTITY_SUFFIXES.items():
            if text.endswith(suffix) and len(text) > len(suffix):
                return Decimal(text[: -len(suffix)]) * factor
        return Decimal(text)
    except InvalidOperation as exc:
        raise CapacityError(f"cannot read the quantity {text!r}") from exc


def amount(resources: dict[str, Decimal], name: str) -> Decimal:
    return resources.get(name, Decimal(0))


def add_requests(total: dict[str, Decimal], extra: dict[str, Decimal]) -> None:
    for name, value in extra.items():
        total[name] = total.get(name, Decimal(0)) + value


def container_requests(container: dict) -> dict[str, Decimal]:
    requests = (container.get("resources") or {}).get("requests") or {}
    return {name: quantity(value) for name, value in requests.items()}


def pod_requests(pod: dict) -> dict[str, Decimal]:
    spec = pod.get("spec") or {}
    requests: dict[str, Decimal] = {}
    for container in spec.get("containers") or []:
        add_requests(requests, container_requests(container))
    for container in spec.get("initContainers") or []:
        for name, value in container_requests(container).items():
            requests[name] = max(requests.get(name, Decimal(0)), value)
    overhead = spec.get("overhead") or {}
    add_requests(requests, {name: quantity(value) for name, value in overhead.items()})
    return requests


def node_labels(node: dict) -> dict[str, str]:
    return node["metadata"].get("labels") or {}


def agent_nodes(nodes: list[dict]) -> list[dict]:
    # Must match the nodes vcluster_node_sync_selector_labels in e2e/snapshot_e2e/infra/setup.py syncs.
    if any(node_labels(node).get(AKS_USER_NODE_LABEL) == AKS_USER_NODE_VALUE for node in nodes):
        nodes = [node for node in nodes if node_labels(node).get(AKS_USER_NODE_LABEL) == AKS_USER_NODE_VALUE]
    return [
        node
        for node in nodes
        if node_labels(node).get(GPU_NODE_LABEL) == "true"
        and node_labels(node).get("kubernetes.io/arch") in AGENT_ARCHITECTURES
    ]


def cpus(value: Decimal) -> str:
    return f"{value:.1f}"


def gib(value: Decimal) -> str:
    return f"{value / 2**30:.0f}"


def compute_limit(
    nodes: list[dict],
    pods: list[dict],
    holders: set[str],
    e2e_namespaces: set[str],
) -> Limit:
    eligible = agent_nodes(nodes)
    if not eligible:
        raise CapacityError(f"no node the vCluster syncs is labelled {GPU_NODE_LABEL}=true")

    other: dict[str, dict[str, Decimal]] = {}
    held: dict[tuple[str, str], dict[str, Decimal]] = {}
    for pod in pods:
        node = (pod.get("spec") or {}).get("nodeName")
        if not node or (pod.get("status") or {}).get("phase") in ("Succeeded", "Failed"):
            continue
        namespace = pod["metadata"].get("namespace", "")
        if namespace in holders:
            add_requests(held.setdefault((namespace, node), {}), pod_requests(pod))
        else:
            add_requests(other.setdefault(node, {}), pod_requests(pod))

    tightest = None
    free_gpus = Decimal(0)
    for node in eligible:
        name = node["metadata"]["name"]
        status = node.get("status") or {}
        allocatable = {key: quantity(value) for key, value in (status.get("allocatable") or {}).items()}
        used = other.get(name, {})
        cpu, memory = amount(allocatable, "cpu"), amount(allocatable, "memory")
        used_cpu, used_memory = amount(used, "cpu"), amount(used, "memory")
        fit = max(int(min((cpu - used_cpu) / SUITE_CPU, (memory - used_memory) / SUITE_MEMORY)), 0)
        free_gpus += max(amount(allocatable, GPU_RESOURCE) - amount(used, GPU_RESOURCE), Decimal(0))
        if tightest is None or fit < tightest[0]:
            tightest = (fit, name, cpu, memory, used_cpu, used_memory)

    node_fit, name, cpu, memory, used_cpu, used_memory = tightest
    gpu_fit = int(free_gpus / SUITE_GPUS)
    fit = min(node_fit, gpu_fit)
    limit = max(fit - 1, min(fit, 1))

    detail = (
        f"suite limit {limit}: the tightest GPU node {name} has {cpus(cpu)} CPUs and "
        f"{gib(memory)} GiB allocatable, {cpus(used_cpu)} CPUs and {gib(used_memory)} GiB "
        f"used outside slot holders, room for {node_fit} suites of {cpus(SUITE_CPU)} CPUs "
        f"and {gib(SUITE_MEMORY)} GiB; {free_gpus:.0f} free GPUs fit {gpu_fit} suites "
        f"of {SUITE_GPUS:.0f}; {len(eligible)} GPU nodes; "
        f"{len(e2e_namespaces - holders)} E2E namespaces hold no slot"
    )
    if held:
        (namespace, node), largest = max(
            held.items(), key=lambda item: (amount(item[1], "cpu"), amount(item[1], "memory"))
        )
        detail += (
            f"; the largest slot holder footprint on one node is {cpus(amount(largest, 'cpu'))} "
            f"CPUs and {gib(amount(largest, 'memory'))} GiB ({namespace} on {node})"
        )
    return Limit(limit, detail)


def list_e2e_namespaces() -> set[str] | None:
    try:
        items = kubectl_json("get", "namespaces", "-l", E2E_NAMESPACE_SELECTOR)["items"]
    except (KubectlError, ValueError, KeyError) as exc:
        log(f"Could not list E2E namespaces: {exc}")
        return None
    return {item["metadata"]["name"] for item in items}


def current_limit(holders: set[str], e2e_namespaces: set[str] | None, max_running: int | None) -> Limit:
    try:
        nodes = kubectl_json("get", "nodes")["items"]
        pods = kubectl_json(
            "get",
            "pods",
            "--all-namespaces",
            "--field-selector=status.phase!=Succeeded,status.phase!=Failed",
        )["items"]
        limit = compute_limit(nodes, pods, holders, e2e_namespaces or set())
    except (KubectlError, CapacityError, ValueError, KeyError, ArithmeticError) as exc:
        limit = Limit(
            FALLBACK_LIMIT,
            f"suite limit {FALLBACK_LIMIT}, the fallback: the GPU nodes could not be read: {exc}",
        )
    if max_running is not None and max_running < limit.value:
        limit = Limit(max_running, f"{limit.detail}; capped at {max_running} by --max-running")
    return limit


def slot_name(index: int) -> str:
    return f"{SLOT_PREFIX}{index}"


def is_slot(name: str) -> bool:
    return name.startswith(SLOT_PREFIX) and name[len(SLOT_PREFIX) :].isdigit()


def holder(lease: dict) -> str:
    return (lease.get("spec") or {}).get("holderIdentity") or ""


def seconds_expired(lease: dict, now: datetime) -> float:
    spec = lease.get("spec") or {}
    renewed = parse_time(spec.get("renewTime") or spec.get("acquireTime"))
    if renewed is None:
        return float("inf")
    duration = spec.get("leaseDurationSeconds") or LEASE_SECONDS
    return (now - renewed).total_seconds() - duration - GRACE_SECONDS


def is_free(lease: dict | None, now: datetime) -> bool:
    return lease is None or not holder(lease) or seconds_expired(lease, now) >= 0


def run_metadata() -> tuple[dict[str, str], dict[str, str]]:
    run_id = os.environ.get("GITHUB_RUN_ID", "")
    attempt = os.environ.get("GITHUB_RUN_ATTEMPT", "")
    labels = {}
    annotations = {}
    if run_id.isdigit():
        labels["snapshot.github/run-id"] = run_id
    if attempt.isdigit():
        labels["snapshot.github/run-attempt"] = attempt
    server = os.environ.get("GITHUB_SERVER_URL", "")
    repository = os.environ.get("GITHUB_REPOSITORY", "")
    if server and repository and run_id:
        annotations["snapshot.github/run-url"] = f"{server}/{repository}/actions/runs/{run_id}"
    return labels, annotations


def lease_body(name: str, resource_version: str | None, spec: dict, held: bool) -> dict:
    labels, annotations = run_metadata() if held else ({}, {})
    metadata: dict = {"name": name, "namespace": NAMESPACE, "labels": labels, "annotations": annotations}
    if resource_version:
        metadata["resourceVersion"] = resource_version
    return {"apiVersion": "coordination.k8s.io/v1", "kind": "Lease", "metadata": metadata, "spec": spec}


def held_spec(identity: str, acquired: datetime, renewed: datetime) -> dict:
    return {
        "holderIdentity": identity,
        "leaseDurationSeconds": LEASE_SECONDS,
        "acquireTime": format_time(acquired),
        "renewTime": format_time(renewed),
    }


def list_slots() -> dict[str, dict]:
    items = kubectl_json("get", "leases", "-n", NAMESPACE)["items"]
    return {item["metadata"]["name"]: item for item in items if is_slot(item["metadata"]["name"])}


def read_slot(name: str) -> dict | None:
    try:
        return kubectl_json("get", "lease", name, "-n", NAMESPACE)
    except KubectlError as exc:
        if exc.reason == "NotFound":
            return None
        raise


def claim(name: str, lease: dict | None, identity: str, now: datetime) -> bool:
    spec = held_spec(identity, now, now)
    try:
        if lease is None:
            kubectl("create", "-f", "-", stdin=json.dumps(lease_body(name, None, spec, True)))
        else:
            version = lease["metadata"]["resourceVersion"]
            kubectl("replace", "-f", "-", stdin=json.dumps(lease_body(name, version, spec, True)))
    except KubectlError as exc:
        if exc.reason in ("AlreadyExists", "Conflict", "NotFound"):
            return False
        raise
    return True


def ensure_namespace() -> None:
    try:
        kubectl("create", "namespace", NAMESPACE)
    except KubectlError as exc:
        if exc.reason != "AlreadyExists":
            raise


def run_best_effort(command: list[str]) -> None:
    if shutil.which(command[0]) is None:
        return
    try:
        subprocess.run(command, capture_output=True, text=True, timeout=180)
    except (OSError, subprocess.TimeoutExpired) as exc:
        log(f"{' '.join(command)} failed: {exc}")


def reap(name: str, lease: dict, e2e_namespaces: set[str] | None, now: datetime) -> None:
    namespace = holder(lease)
    expired = seconds_expired(lease, now)
    if not e2e_namespaces or namespace not in e2e_namespaces or expired < REAP_AFTER_SECONDS:
        return
    log(f"Deleting {namespace}: its slot {name} expired {expired:.0f} s ago, so the run that held it is gone")
    run_best_effort(["helm", "uninstall", "vcluster-hpm", "-n", namespace])
    run_best_effort(["vcluster", "delete", namespace, "--namespace", namespace])
    try:
        kubectl("delete", "namespace", namespace, "--ignore-not-found", "--wait=false")
    except KubectlError as exc:
        log(f"Could not delete {namespace}: {exc}")


def describe_holders(slots: dict[str, dict], now: datetime) -> str:
    held = []
    for name in sorted(slots, key=lambda item: int(item[len(SLOT_PREFIX) :])):
        lease = slots[name]
        if is_free(lease, now):
            continue
        acquired = parse_time((lease.get("spec") or {}).get("acquireTime"))
        minutes = f", {(now - acquired).total_seconds() / 60:.0f} min" if acquired else ""
        held.append(f"{name} {holder(lease)}{minutes}")
    return "; ".join(held) or "none"


def start_renewer(slot: str, identity: str, log_path: Path) -> int:
    script = str(Path(__file__).resolve())
    with log_path.open("a", encoding="utf-8") as output:
        process = subprocess.Popen(
            [sys.executable, "-I", "-S", script, "renew", "--slot", slot, "--identity", identity],
            stdin=subprocess.DEVNULL,
            stdout=output,
            stderr=subprocess.STDOUT,
            start_new_session=True,
        )
    return process.pid


def hold(slot: str, identity: str, state_path: Path) -> int:
    state = {"slot": slot, "identity": identity}
    state_path.write_text(json.dumps(state), encoding="utf-8")
    log_path = state_path.with_name(state_path.name + ".renew.log")
    state["renewer_pid"] = start_renewer(slot, identity, log_path)
    state["renewer_log"] = str(log_path)
    state_path.write_text(json.dumps(state), encoding="utf-8")
    log(f"Holding suite slot {slot} as {identity}")
    return 0


def fail_open(exc: Exception) -> int:
    message = f"the suite slots could not be read or written: {exc}"
    annotate("warning", "Running without a suite slot", message)
    return 0


def take(args: argparse.Namespace) -> int:
    identity = args.identity
    state_path = Path(args.state_file)
    deadline = time.monotonic() + args.timeout_minutes * 60
    try:
        ensure_namespace()
    except KubectlError as exc:
        return fail_open(exc)

    limit: Limit | None = None
    slots: dict[str, dict] = {}
    refreshed_at = reported_at = float("-inf")
    lease_errors = 0
    e2e_namespaces: set[str] | None = None
    while True:
        now = utc_now()
        try:
            slots = list_slots()
            held = {name: lease for name, lease in slots.items() if not is_free(lease, now)}
            mine = [name for name, lease in held.items() if holder(lease) == identity]
            if mine:
                return hold(mine[0], identity, state_path)

            if time.monotonic() - refreshed_at >= LIMIT_REFRESH_SECONDS:
                e2e_namespaces = list_e2e_namespaces()
                holders = {holder(lease) for lease in held.values()}
                fresh = current_limit(holders, e2e_namespaces, args.max_running)
                if limit is None or fresh.value != limit.value:
                    log(fresh.detail)
                limit = fresh
                refreshed_at = time.monotonic()

            candidates = [slot_name(index) for index in range(limit.value)]
            random.shuffle(candidates)
            for name in candidates:
                lease = slots.get(name)
                if not is_free(lease, now):
                    continue
                if lease is not None and holder(lease):
                    reap(name, lease, e2e_namespaces, now)
                if claim(name, lease, identity, now):
                    log(f"Took suite slot {name} of {limit.value}")
                    return hold(name, identity, state_path)
            lease_errors = 0
        except KubectlError as exc:
            lease_errors += 1
            if exc.reason == "Forbidden" or lease_errors >= LEASE_ERROR_POLLS:
                return fail_open(exc)
            log(f"Suite slot API error, retrying: {exc}")

        allowed = limit.value if limit else 0
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            annotate(
                "error",
                "No suite slot",
                f"waited {args.timeout_minutes} minutes for one of {allowed} suite slots; "
                f"held: {describe_holders(slots, now)}",
            )
            return 1
        if time.monotonic() - reported_at >= REPORT_SECONDS:
            log(f"Waiting for a suite slot, {allowed} allowed; held: {describe_holders(slots, now)}")
            reported_at = time.monotonic()
        time.sleep(min(POLL_SECONDS + random.uniform(0, 5), remaining))


def renew(args: argparse.Namespace) -> int:
    started = time.monotonic()
    while time.monotonic() - started < RENEW_MAX_SECONDS:
        time.sleep(RENEW_SECONDS)
        try:
            lease = read_slot(args.slot)
            if lease is None:
                log(f"warning: suite slot {args.slot} no longer exists; stopped renewing")
                return 0
            if holder(lease) != args.identity:
                log(f"warning: suite slot {args.slot} is held by {holder(lease) or 'nobody'}; stopped renewing")
                return 0
            acquired = parse_time((lease.get("spec") or {}).get("acquireTime")) or utc_now()
            spec = held_spec(args.identity, acquired, utc_now())
            body = lease_body(args.slot, lease["metadata"]["resourceVersion"], spec, True)
            kubectl("replace", "-f", "-", stdin=json.dumps(body))
        except KubectlError as exc:
            log(f"Could not renew suite slot {args.slot}: {exc}")
    log(f"warning: stopped renewing suite slot {args.slot} after {RENEW_MAX_SECONDS // 3600} hours")
    return 0


def stop_renewer(pid: object) -> None:
    if not isinstance(pid, int) or pid <= 0:
        return
    cmdline = Path(f"/proc/{pid}/cmdline")
    if cmdline.exists() and b"e2e-suite-slot" not in cmdline.read_bytes():
        return
    try:
        os.kill(pid, signal.SIGTERM)
    except ProcessLookupError:
        pass


def free(slot: str, identity: str) -> None:
    for _ in range(3):
        lease = read_slot(slot)
        if lease is None:
            log(f"Suite slot {slot} no longer exists")
            return
        if holder(lease) != identity:
            log(f"Suite slot {slot} is held by {holder(lease) or 'nobody'}, not {identity}; left alone")
            return
        spec = {"leaseDurationSeconds": LEASE_SECONDS}
        body = lease_body(slot, lease["metadata"]["resourceVersion"], spec, False)
        try:
            kubectl("replace", "-f", "-", stdin=json.dumps(body))
        except KubectlError as exc:
            if exc.reason == "Conflict":
                continue
            raise
        log(f"Released suite slot {slot}")
        return
    raise RuntimeError(f"suite slot {slot} kept changing while it was being released")


def release(args: argparse.Namespace) -> int:
    state_path = Path(args.state_file)
    if not state_path.exists():
        log("No suite slot held")
        return 0
    try:
        state = json.loads(state_path.read_text(encoding="utf-8"))
        stop_renewer(state.get("renewer_pid"))
        renewer_log = state.get("renewer_log")
        if renewer_log and Path(renewer_log).exists():
            text = Path(renewer_log).read_text(encoding="utf-8", errors="replace").rstrip()
            log(f"Renewer log for {state['slot']}:\n{text}")
        free(state["slot"], state["identity"])
        state_path.unlink()
    except Exception as exc:  # noqa: BLE001 - releasing must never fail the job
        annotate(
            "warning",
            "Suite slot not released",
            f"{exc}; it expires within {(LEASE_SECONDS + GRACE_SECONDS) // 60} minutes",
        )
    return 0


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    commands = parser.add_subparsers(dest="command", required=True)

    take_parser = commands.add_parser("take", help="wait for a free suite slot and hold it")
    take_parser.add_argument(
        "--identity",
        default=os.environ.get("HOST_NAMESPACE", ""),
        help="holder identity, unique per run, attempt and suite; defaults to HOST_NAMESPACE",
    )
    take_parser.add_argument("--state-file", default=DEFAULT_STATE_FILE, help="where the held slot is recorded")
    take_parser.add_argument(
        "--timeout-minutes",
        type=int,
        default=DEFAULT_TIMEOUT_MINUTES,
        help="fail when no slot frees up within this many minutes",
    )
    take_parser.add_argument(
        "--max-running",
        type=int,
        help="cap the computed limit; meant for validating the slots, not for normal runs",
    )

    renew_parser = commands.add_parser("renew", help="keep a held slot from expiring; started by take")
    renew_parser.add_argument("--slot", required=True)
    renew_parser.add_argument("--identity", required=True)

    release_parser = commands.add_parser("release", help="stop renewing and free the held slot")
    release_parser.add_argument("--state-file", default=DEFAULT_STATE_FILE, help="where take recorded the slot")

    args = parser.parse_args(argv)
    if args.command == "take":
        if not args.identity:
            parser.error("take needs --identity or HOST_NAMESPACE")
        if args.timeout_minutes < 1:
            parser.error("--timeout-minutes must be at least 1")
        if args.max_running is not None and args.max_running < 1:
            parser.error("--max-running must be at least 1")
    return args


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv)
    if args.command == "take":
        return take(args)
    if args.command == "renew":
        return renew(args)
    return release(args)


if __name__ == "__main__":
    sys.exit(main())
