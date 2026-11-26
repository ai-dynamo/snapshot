# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Wait for one of a limited number of GPU E2E suite slots on the host cluster.

Every GPU, framework and upgrade suite creates its own vCluster on the same GPU
nodes and installs Snapshot in it, which places an agent Pod on every GPU node.
Once a node has no room for another agent, the next suite's agents stay Pending
and its setup times out. So a suite first waits for a slot.

A suite's host namespace is its place in line. This creates it, labelled
snapshot-e2e=true like every suite's host namespace, and the suite starts once
its namespace is among the SUITE_LIMIT oldest of those namespaces. The suite's
vCluster cleanup deletes the namespace, which frees the slot, so there is
nothing to release. A namespace that is being deleted holds no slot.

Namespaces created in the same second are ordered by name, so the first check
waits SETTLE_SECONDS for every namespace that could sort ahead to exist.
"""

from __future__ import annotations

import argparse
import os
import time
from collections.abc import Sequence

from kubernetes import client
from kubernetes.client import ApiException
from urllib3.exceptions import HTTPError

from snapshot_e2e.infra import preflight
from snapshot_e2e.infra.setup import create_host_namespace, log


E2E_NAMESPACE_SELECTOR = "snapshot-e2e=true"
SUITE_LIMIT = 6
SETTLE_SECONDS = 5
POLL_SECONDS = 20
REPORT_SECONDS = 60
API_ERROR_POLLS = 3
REQUEST_TIMEOUT_SECONDS = 60
DEFAULT_TIMEOUT_MINUTES = 175
API_ERRORS = (ApiException, HTTPError)


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
    reported_at = float("-inf")
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
            held_by = names[:SUITE_LIMIT]
            if identity in held_by:
                log(f"Took suite slot {held_by.index(identity) + 1} of {SUITE_LIMIT} as {identity}")
                return 0
            if time.monotonic() - reported_at >= REPORT_SECONDS:
                log(f"Waiting for one of {SUITE_LIMIT} suite slots, held by: {', '.join(held_by)}")
                reported_at = time.monotonic()

        remaining = deadline - time.monotonic()
        if remaining <= 0:
            annotate(
                "error",
                "No suite slot",
                f"waited {timeout_minutes} minutes for one of {SUITE_LIMIT} suite slots; "
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
