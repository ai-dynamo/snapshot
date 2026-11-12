# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Checkpoint retry: what an operator sees when a capture is retried, or is not.

SNEP-394 asks that each attempt and the final outcome be attributable through
the Kubernetes API. A capture that succeeds, one that fails for a reason no retry could clear, and
one whose dump runs the node's staging volume out of room part-written — the
last being the case that actually spends a retry. Plus what the agent reports
about the CRIU it runs, since a dump failure is attributed to a build by it.
"""

from __future__ import annotations

import re

import pytest

from snapshot_e2e import k8s
from snapshot_e2e import lifecycle as snap

from test_snapshot_lifecycle import create_ready_source

ATTEMPT_FAILED = "CheckpointAttemptFailed"

# Large enough that the dump cannot fit the margin left in staging, small
# enough to hold on a CI node alongside everything else.
BALLAST_MB = 192

# Room for CRIU's metadata, so the dump starts and then runs out rather than
# failing before it begins. A failure before the dump is a different reason.
STAGING_MARGIN_BYTES = 32 * 1024 * 1024


def attempt_failed_events(namespace: str, pod: str) -> list[str]:
    """Messages of every per-attempt failure reported against one source pod."""
    return [
        event.message
        for event in k8s.list_events(
            namespace,
            field_selector={"involvedObject.name": pod, "reason": ATTEMPT_FAILED},
        )
    ]


@pytest.mark.snapshot_success
@pytest.mark.cpu
def test_a_capture_that_succeeds_spends_no_retries(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    # Retry is on for this install, so a clean capture is the case that proves
    # the machinery stays out of the way: no attempt is reported, and the
    # checkpoint does not read as though one was spent reaching it.
    try:
        source, _ = create_ready_source(config, run, gpu=False)
        snap.create_podsnapshot(
            config.namespace,
            run.snapshot_name,
            run.source_pod,
            source.metadata.uid,
        )
        _, content = snap.wait_for_snapshot_ready(config.namespace, run.snapshot_name)

        assert attempt_failed_events(config.namespace, run.source_pod) == [], (
            "a capture that succeeded first time must report no failed attempt"
        )
        ready = snap.condition(content, "Ready")
        assert ready and "retries" not in ready.get("message", ""), ready
    except Exception:
        snap.debug_dump(config, run)
        raise


@pytest.mark.snapshot_failure
@pytest.mark.cpu
def test_a_settled_failure_keeps_its_retries_and_its_reason(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    # A probe that can never run is settled: retrying cannot change it. The
    # checkpoint must therefore end on its own reason rather than reading as
    # though the retries ran out, and must not spend any reaching that.
    try:
        source, _ = create_ready_source(config, run, gpu=False)
        snap.create_podsnapshot(
            config.namespace,
            run.snapshot_name,
            run.source_pod,
            source.metadata.uid,
            containers=[
                {
                    "name": snap.CONTAINER,
                    "checkpointReadinessProbe": {
                        "httpGet": {"path": "/ready", "port": "no-such-port"}
                    },
                }
            ],
        )
        _, content = snap.wait_for_snapshot_failed(
            config.namespace, run.snapshot_name, timeout=120
        )
        assert content is not None

        failed = snap.condition(content, "Failed")
        assert failed, content
        assert failed.get("reason") == "InvalidCheckpointReadinessProbe", (
            "a settled failure keeps its own reason rather than RetriesExhausted"
        )
        assert "retries" not in failed.get("message", ""), (
            f"no retry was spent, so the message must not count any: {failed.get('message')!r}"
        )
        assert attempt_failed_events(config.namespace, run.source_pod) == [], (
            "a failure no retry could clear must not report an attempt"
        )
        assert k8s.read_pod(config.namespace, run.source_pod).status.phase == "Running", (
            "a refused probe must not touch the source"
        )
    except Exception:
        snap.debug_dump(config, run)
        raise


@pytest.mark.snapshot_failure
@pytest.mark.cpu
@pytest.mark.serial
def test_a_dump_that_runs_out_of_staging_spends_its_retries(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    # The case retry exists for: a dump that fails on a shortage that could
    # clear. Staging is memory-backed and shared by every capture on the node,
    # so this test runs alone — hence the serial marker.
    #
    # The source holds anonymous memory so its image cannot fit the margin
    # left free, and the margin leaves room for CRIU's metadata so the dump
    # starts before it runs out. Filling staging entirely would fail earlier,
    # under a different reason.
    node = None
    try:
        source, node = create_ready_source(
            config, run, gpu=False, anon_ballast_mb=BALLAST_MB
        )
        snap.fill_staging(config, node, leave_bytes=STAGING_MARGIN_BYTES)

        snap.create_podsnapshot(
            config.namespace,
            run.snapshot_name,
            run.source_pod,
            source.metadata.uid,
        )
        _, content = snap.wait_for_snapshot_failed(
            config.namespace, run.snapshot_name, timeout=300
        )
        assert content is not None

        failed = snap.condition(content, "Failed")
        assert failed, content
        assert failed.get("reason") == "RetriesExhausted", (
            f"a shortage that could clear is retried until the budget is gone: {failed}"
        )
        assert "retries" in failed.get("message", ""), (
            f"the message names what was spent reaching it: {failed.get('message')!r}"
        )

        attempts = attempt_failed_events(config.namespace, run.source_pod)
        assert attempts, "every attempt a retry follows is reported on the source pod"
        assert any("StagingUnavailable" in message for message in attempts), (
            f"staging running out is reported as its own shortage: {attempts}"
        )
    except Exception:
        snap.debug_dump(config, run)
        raise
    finally:
        if node:
            snap.drain_staging(config, node)


@pytest.mark.environment
@pytest.mark.cpu
def test_the_agent_reports_the_criu_build_it_runs(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    # CRIU is pinned by the agent image rather than by this repository, and the
    # default reference is a development branch where every commit reports the
    # same version number. A bug report quoting the number alone cannot say
    # which build produced a dump, so the agent names the commit too.
    source, node = create_ready_source(config, run, gpu=False)
    assert source is not None

    startup = [
        line
        for line in snap.agent_startup_log(config, node).splitlines()
        if "Starting snapshot agent" in line
    ]
    assert startup, "the agent logs what it starts with"

    reported = re.search(r'"criu_version":\s*"([^"]+)"', startup[-1])
    assert reported, f"the startup line names the CRIU it runs: {startup[-1]}"

    version = reported.group(1)
    assert re.fullmatch(r"\d+\.\d+\.\d+ \([0-9a-f]{7,40}\)", version), (
        "a development build is identified by the commit the image was built "
        f"from, not by its version alone: {version!r}"
    )
