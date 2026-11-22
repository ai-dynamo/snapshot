# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Wait for one of a limited number of GPU E2E suite slots on the host cluster.

Every GPU, framework and upgrade suite creates its own vCluster on the same GPU
nodes and installs Snapshot in it, which places an agent Pod on every GPU node.
Once a node has no room for another agent, the next suite's agents stay Pending
and its setup times out. So a suite first waits for a slot.

A suite's host namespace is its place in line. This creates it, labelled
snapshot-e2e=true like every suite's host namespace, and the suite starts once
its namespace is among the oldest of those namespaces, up to the limit. The
suite's vCluster cleanup deletes the namespace, which frees the slot, so there
is nothing to release. A namespace that is being deleted holds no slot.

The limit is recomputed while a suite waits: how many suite budgets fit into
what the tightest GPU node has left after every Pod outside the slot-holding
namespaces, capped by free GPUs, keeping one suite of headroom. If the limit
cannot be computed, the last limit stays, or FALLBACK_LIMIT if there is none.

Namespaces created in the same second are ordered by name, so the first check
waits SETTLE_SECONDS for every namespace that could sort ahead to exist.
"""

from __future__ import annotations

import argparse
import os
import time
from collections import Counter, defaultdict
from collections.abc import Sequence
from dataclasses import dataclass
from decimal import Decimal

from kubernetes import client
from kubernetes.client import ApiException
from kubernetes.utils import parse_quantity
from urllib3.exceptions import HTTPError

from snapshot_e2e.infra import preflight
from snapshot_e2e.infra.setup import create_host_namespace, log, node_sync_selector


E2E_NAMESPACE_SELECTOR = "snapshot-e2e=true"
GPU_NODE_LABEL = "nvidia.com/gpu.present"
GPU_RESOURCE = "nvidia.com/gpu"
AGENT_ARCHITECTURES = ("amd64", "arm64")
FALLBACK_LIMIT = 6
SETTLE_SECONDS = 5
POLL_SECONDS = 20
LIMIT_REFRESH_SECONDS = 60
REPORT_SECONDS = 60
API_ERROR_POLLS = 3
REQUEST_TIMEOUT_SECONDS = 60
DEFAULT_TIMEOUT_MINUTES = 175
# The chart's default agent and PageBroker requests (10 CPUs, 33 GiB) plus the
# hostpath mapper and vCluster's own Pods.
SUITE_CPU = Decimal("10.5")
SUITE_MEMORY = Decimal(34 * 2**30)
SUITE_GPUS = Decimal(2)
API_ERRORS = (ApiException, HTTPError)


class CapacityError(RuntimeError):
    pass


@dataclass(frozen=True)
class Limit:
    value: int
    detail: str
    computed: bool = True


def describe(exc: Exception) -> str:
    if isinstance(exc, ApiException):
        return f"{exc.status} {exc.reason}"
    return str(exc)


def annotate(level: str, title: str, message: str) -> None:
    escaped = message.replace("%", "%25").replace("\r", "%0D").replace("\n", "%0A")
    print(f"::{level} title={title}::{escaped}", flush=True)


def run_without_slot(exc: Exception) -> int:
    annotate(
        "warning",
        "Running without a suite slot",
        f"the suite slots could not be checked: {describe(exc)}",
    )
    return 0


def quantities(values: dict[str, str] | None) -> Counter[str]:
    return Counter({name: parse_quantity(value) for name, value in (values or {}).items()})


def container_requests(container: client.V1Container) -> Counter[str]:
    return quantities(container.resources.requests if container.resources else None)


def pod_requests(pod: client.V1Pod) -> Counter[str]:
    requests: Counter[str] = Counter()
    for container in pod.spec.containers or []:
        requests.update(container_requests(container))
    sidecars: Counter[str] = Counter()
    init_peak: Counter[str] = Counter()
    for container in pod.spec.init_containers or []:
        if container.restart_policy == "Always":
            sidecars.update(container_requests(container))
        else:
            init_peak |= container_requests(container) + sidecars
    requests.update(sidecars)
    requests |= init_peak
    requests.update(quantities(pod.spec.overhead))
    return requests


def agent_nodes(nodes: Sequence[client.V1Node]) -> list[client.V1Node]:
    synced = node_sync_selector(nodes)
    eligible = []
    for node in nodes:
        labels = node.metadata.labels or {}
        if (
            all(labels.get(key) == value for key, value in synced.items())
            and labels.get(GPU_NODE_LABEL) == "true"
            and labels.get("kubernetes.io/arch") in AGENT_ARCHITECTURES
        ):
            eligible.append(node)
    return eligible


def compute_limit(
    nodes: Sequence[client.V1Node], pods: Sequence[client.V1Pod], holders: set[str]
) -> Limit:
    eligible = agent_nodes(nodes)
    if not eligible:
        raise CapacityError(f"no node the vCluster syncs is labelled {GPU_NODE_LABEL}=true")

    used: defaultdict[str, Counter[str]] = defaultdict(Counter)
    for pod in pods:
        if pod.spec.node_name and pod.metadata.namespace not in holders:
            used[pod.spec.node_name].update(pod_requests(pod))

    room = []
    free_gpus = Decimal(0)
    for node in eligible:
        allocatable = quantities(node.status.allocatable if node.status else None)
        taken = used[node.metadata.name]
        cpu = allocatable["cpu"] - taken["cpu"]
        memory = allocatable["memory"] - taken["memory"]
        fit = max(int(min(cpu / SUITE_CPU, memory / SUITE_MEMORY)), 0)
        room.append((fit, node.metadata.name, cpu, memory))
        gpus = allocatable[GPU_RESOURCE] - taken[GPU_RESOURCE]
        free_gpus += max(gpus, Decimal(0))

    node_fit, name, cpu, memory = min(room)
    gpu_fit = int(free_gpus / SUITE_GPUS)
    fit = min(node_fit, gpu_fit)
    limit = max(fit - 1, min(fit, 1))
    return Limit(
        limit,
        f"suite limit {limit}: the tightest of {len(eligible)} GPU nodes, {name}, has "
        f"{cpu:.1f} CPUs and {memory / 2**30:.0f} GiB free outside the slot holders, room for "
        f"{node_fit} suites of {SUITE_CPU} CPUs and {SUITE_MEMORY / 2**30:.0f} GiB; "
        f"{free_gpus:.0f} free GPUs fit {gpu_fit} suites of {SUITE_GPUS}",
    )


def refresh_limit(previous: Limit | None, holders: set[str]) -> Limit:
    core = client.CoreV1Api()
    try:
        nodes = core.list_node(_request_timeout=REQUEST_TIMEOUT_SECONDS).items
        pods = core.list_pod_for_all_namespaces(
            field_selector="status.phase!=Succeeded,status.phase!=Failed",
            _request_timeout=REQUEST_TIMEOUT_SECONDS,
        ).items
        fresh = compute_limit(nodes, pods, holders)
    except (*API_ERRORS, CapacityError, ValueError, ArithmeticError) as exc:
        value = previous.value if previous else FALLBACK_LIMIT
        source = "kept from the last check" if previous else "the fallback"
        fresh = Limit(
            value,
            f"suite limit {value}, {source}: the GPU nodes could not be read: {describe(exc)}",
            computed=False,
        )
    if previous is None or not (previous.computed and fresh.computed) or fresh.value != previous.value:
        log(fresh.detail)
    return fresh


def queue() -> list[str]:
    items = client.CoreV1Api().list_namespace(
        label_selector=E2E_NAMESPACE_SELECTOR, _request_timeout=REQUEST_TIMEOUT_SECONDS
    ).items
    live = [item for item in items if item.metadata.deletion_timestamp is None]
    live.sort(key=lambda item: (item.metadata.creation_timestamp, item.metadata.name))
    return [item.metadata.name for item in live]


def wait(identity: str, timeout_minutes: int) -> int:
    try:
        preflight.load_config(None, None)
        create_host_namespace(identity)
    except (RuntimeError, *API_ERRORS) as exc:
        return run_without_slot(exc)
    time.sleep(SETTLE_SECONDS)

    deadline = time.monotonic() + timeout_minutes * 60
    limit: Limit | None = None
    refreshed_at = reported_at = float("-inf")
    errors = 0
    held_by: list[str] = []
    while True:
        try:
            names = queue()
        except API_ERRORS as exc:
            errors += 1
            forbidden = isinstance(exc, ApiException) and exc.status == 403
            if forbidden or errors >= API_ERROR_POLLS:
                return run_without_slot(exc)
            log(f"Could not list the suite slots, retrying: {describe(exc)}")
        else:
            errors = 0
            if limit is None or time.monotonic() - refreshed_at >= LIMIT_REFRESH_SECONDS:
                limit, refreshed_at = refresh_limit(limit, set(names)), time.monotonic()
            held_by = names[: limit.value]
            if identity in held_by:
                log(f"Took suite slot {held_by.index(identity) + 1} of {limit.value} as {identity}")
                return 0
            if time.monotonic() - reported_at >= REPORT_SECONDS:
                log(f"Waiting for one of {limit.value} suite slots, held by: {', '.join(held_by)}")
                reported_at = time.monotonic()

        remaining = deadline - time.monotonic()
        if remaining <= 0:
            allowed = limit.value if limit else FALLBACK_LIMIT
            annotate(
                "error",
                "No suite slot",
                f"waited {timeout_minutes} minutes for one of {allowed} suite slots; "
                f"held by: {', '.join(held_by) or 'unknown'}",
            )
            return 1
        time.sleep(min(POLL_SECONDS, remaining))


def parse_args(argv: Sequence[str] | None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Wait for a GPU E2E suite slot on the host cluster.")
    parser.add_argument(
        "--identity",
        default=os.environ.get("HOST_NAMESPACE"),
        help="the suite's host namespace; defaults to HOST_NAMESPACE",
    )
    parser.add_argument(
        "--timeout-minutes",
        type=int,
        default=DEFAULT_TIMEOUT_MINUTES,
        help="fail when no slot frees up within this many minutes",
    )
    args = parser.parse_args(argv)
    if not args.identity:
        parser.error("--identity or HOST_NAMESPACE is required")
    return args


def main(argv: Sequence[str] | None = None) -> int:
    args = parse_args(argv)
    return wait(args.identity, args.timeout_minutes)


if __name__ == "__main__":
    raise SystemExit(main())
