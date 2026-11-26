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
from kubernetes import client
from kubernetes.client import ApiException

from snapshot_e2e import k8s
from snapshot_e2e import lifecycle as snap
from snapshot_e2e import workloads

from test_snapshot_lifecycle import create_ready_source

ATTEMPT_FAILED = "CheckpointAttemptFailed"

# What becomes of the workload a failed checkpoint had suspended.
SOURCE_OUTCOMES = ("SourcePodResumed", "SourcePodTerminated", "SourcePodTerminationFailed")

# The ceiling every way of asking for retries shares, from
# api/v1alpha1.MaxCheckpointRetryLimit.
MAX_RETRY_LIMIT = 10

# Large enough that the dump cannot fit the margin left in staging, small
# enough to hold on a CI node alongside everything else.
BALLAST_MB = 192

# Room for CRIU's metadata, so the dump starts and then runs out rather than
# failing before it begins. A failure before the dump is a different reason.
STAGING_MARGIN_BYTES = 32 * 1024 * 1024

# Enough attempts that freeing the volume does not have to beat the first
# retry. Repeats back off 4s, then 8s, then 16s, so a later attempt still finds
# the room even if the drain lands after one or two of them.
RECOVERY_RETRY_LIMIT = 5


def attempt_failed_events(namespace: str, pod: str) -> list[str]:
    """Messages of every per-attempt failure reported against one source pod."""
    return [
        event.message
        for event in k8s.list_events(
            namespace,
            field_selector={"involvedObject.name": pod, "reason": ATTEMPT_FAILED},
        )
    ]


def source_outcome_events(namespace: str, pod: str) -> list[tuple[str, str]]:
    """Reason and message of every source-outcome event for one source pod."""
    return [
        (event.reason, event.message)
        for event in k8s.list_events(namespace, field_selector={"involvedObject.name": pod})
        if event.reason in SOURCE_OUTCOMES
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


@pytest.mark.snapshot_success
@pytest.mark.cpu
@pytest.mark.serial
def test_a_checkpoint_succeeds_on_a_retry_once_staging_frees(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    # The outcome retry exists for. Every other failure here proves it gives up
    # correctly, which is worth nothing on its own: this is the case that says
    # spending the attempts ever pays, and the only one where the checkpoint
    # the user asked for is the thing they get.
    #
    # Serial for the same reason as the test below: staging is memory-backed
    # and shared by every checkpoint on the node, so filling it cannot overlap.
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
            retry_limit=RECOVERY_RETRY_LIMIT,
        )

        # Freed once an attempt has actually failed on it, rather than on a
        # timer: what is under test is a retry finding room the attempt before
        # it did not, so the shortage has to have bitten first.
        snap.wait_for(
            "an attempt to fail on the full staging volume",
            lambda: attempt_failed_events(config.namespace, run.source_pod) or None,
            timeout=300,
            poll_interval=0.5,
        )
        snap.drain_staging(config, node)

        _, content = snap.wait_for_snapshot_ready(
            config.namespace, run.snapshot_name, timeout=600
        )

        attempts = attempt_failed_events(config.namespace, run.source_pod)
        assert any("StagingUnavailable" in message for message in attempts), (
            f"the checkpoint has to have failed on staging before it won: {attempts}"
        )
        assert snap.condition(content, "Failed") is None, (
            f"a checkpoint that recovered carries no failure: {content.get('status')}"
        )
        ready = snap.condition(content, "Ready")
        assert ready and ready.get("status") == "True", content
    except Exception:
        snap.debug_dump(config, run)
        raise
    finally:
        if node:
            snap.drain_staging(config, node)


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

        # The dump suspends the workload before it runs out, so the owner is
        # owed an answer about it. Which outcome depends on whether the agent
        # repeated the dump or took a fresh attempt, so only the ending is
        # asserted: nothing may be left suspended once the checkpoint is over.
        outcomes = source_outcome_events(config.namespace, run.source_pod)
        assert outcomes, "a checkpoint that suspended the workload reports what became of it"
        assert any(reason == "SourcePodTerminated" for reason, _ in outcomes), (
            f"the workload is ended once no further attempt will run: {outcomes}"
        )
    except Exception:
        snap.debug_dump(config, run)
        raise
    finally:
        if node:
            snap.drain_staging(config, node)


@pytest.mark.snapshot_success
@pytest.mark.cpu
def test_a_checkpoint_carries_its_own_retry_limit(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    # The request's own limit, the entry point a user reaches for first. The
    # agent never reads the PodSnapshot, so the value only reaches a capture if
    # the operator copies it to the content; reading it back there is what says
    # the whole path works, not just the field's validation.
    asked = 3
    try:
        source, _ = create_ready_source(config, run, gpu=False)
        snap.create_podsnapshot(
            config.namespace,
            run.snapshot_name,
            run.source_pod,
            source.metadata.uid,
            retry_limit=asked,
        )
        _, content = snap.wait_for_snapshot_ready(config.namespace, run.snapshot_name)

        assert content["spec"].get("checkpointRetryLimit") == asked, (
            f"the content carries the request's limit, unchanged: {content['spec']}"
        )
    except Exception:
        snap.debug_dump(config, run)
        raise


@pytest.mark.snapshot_success
@pytest.mark.cpu
def test_a_snapshotjob_carries_its_retry_limit_to_the_checkpoint(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    # Most checkpoints are asked for through a SnapshotJob, so its template is
    # an entry point in its own right: the value has to survive two copies,
    # into the generated PodSnapshot and on into the content.
    asked = 2
    try:
        snap.create_snapshotjob(
            config.namespace,
            run.snapshotjob_name,
            workloads.snapshotjob_pod_template(config=config, run=run, gpu=False),
            retry_limit=asked,
        )
        sj = snap.wait_for_condition(
            config.namespace,
            run.snapshotjob_name,
            plural=snap.SNAPSHOTJOBS,
            condition_type="Completed",
            timeout=600,
        )
        snapshot_name = sj["status"]["podSnapshotName"]

        generated = snap.get_custom_object(
            client.CustomObjectsApi(),
            config.namespace,
            snapshot_name,
            snap.PODSNAPSHOTS,
        )
        assert generated["spec"].get("checkpointRetryLimit") == asked, (
            f"the template's limit reaches the generated PodSnapshot: {generated['spec']}"
        )

        _, content = snap.wait_for_snapshot_ready(
            config.namespace, snapshot_name, timeout=60
        )
        assert content["spec"].get("checkpointRetryLimit") == asked, (
            f"and on to the content the agent reads: {content['spec']}"
        )
    except Exception:
        snap.debug_dump(config, run)
        raise
    finally:
        # The run fixture cleans up a plain PodSnapshot flow. A SnapshotJob
        # leaves its own Job, the pod that Job made, and the PodSnapshot it
        # produced, none of which carry an ownerReference back to it.
        snap.cleanup_snapshotjob(config, run)


@pytest.mark.cpu
def test_a_retry_limit_past_the_maximum_is_refused(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    # The ceiling is a validation marker, so only the API server can say
    # whether it holds. Checked on both request kinds: each carries its own
    # copy of the marker, and a typo is the case it exists for.
    #
    # Submitted dry-run throughout. What is under test is whether the schema
    # accepts the value, and a real SnapshotJob would start a Job and a pod to
    # answer a question the API server settles on its own.
    pod_template = workloads.snapshotjob_pod_template(config=config, run=run, gpu=False)

    def create_snapshot(limit: int) -> None:
        snap.create_podsnapshot(
            config.namespace,
            run.snapshot_name,
            run.source_pod,
            "00000000-0000-0000-0000-000000000000",
            retry_limit=limit,
            dry_run=True,
        )

    def create_job(limit: int) -> None:
        snap.create_snapshotjob(
            config.namespace,
            run.snapshotjob_name,
            pod_template,
            retry_limit=limit,
            dry_run=True,
        )

    try:
        for kind, create in (("PodSnapshot", create_snapshot), ("SnapshotJob", create_job)):
            with pytest.raises(ApiException) as refused:
                create(MAX_RETRY_LIMIT + 1)
            assert refused.value.status == 422, (
                f"{kind} rejected {MAX_RETRY_LIMIT + 1} for the wrong reason: {refused.value}"
            )
            # The boundary itself is allowed, so the test is about the ceiling
            # rather than about the field refusing large numbers in general.
            create(MAX_RETRY_LIMIT)
    except Exception:
        snap.debug_dump(config, run)
        raise


@pytest.mark.snapshot_failure
@pytest.mark.cpu
@pytest.mark.serial
def test_a_checkpoint_that_asks_for_no_retry_spends_none(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    # An explicit 0 is not the same as asking for nothing: this install's
    # cluster default allows a retry, and the request refuses it. The same
    # shortage that spends a retry above must end the checkpoint here on the
    # first attempt, under the shortage's own reason.
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
            retry_limit=0,
        )
        _, content = snap.wait_for_snapshot_failed(
            config.namespace, run.snapshot_name, timeout=300
        )
        assert content is not None

        failed = snap.condition(content, "Failed")
        assert failed, content
        assert failed.get("reason") == "StagingUnavailable", (
            f"a refused retry leaves the shortage to end the checkpoint: {failed}"
        )
        assert "retries" not in failed.get("message", ""), (
            f"no retry was spent, so the message counts none: {failed.get('message')!r}"
        )
        assert attempt_failed_events(config.namespace, run.source_pod) == [], (
            "an attempt is reported only when another follows it"
        )
    except Exception:
        snap.debug_dump(config, run)
        raise
    finally:
        if node:
            snap.drain_staging(config, node)


@pytest.mark.cpu
def test_the_agent_reports_the_criu_build_it_runs(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    # CRIU is pinned by the agent image rather than by this repository, and the
    # default reference is a development branch where every commit reports the
    # same version. A bug report quoting the version alone cannot say which
    # build produced a dump, so the agent names the commit beside it.
    source, node = create_ready_source(config, run, gpu=False)
    assert source is not None

    # Quoted on failure because an unreadable log comes back as a sentinel
    # string rather than an error, and an empty match alone names neither.
    log = snap.agent_startup_log(config, node)
    startup = [line for line in log.splitlines() if "Starting snapshot agent" in line]
    assert startup, f"the agent logs what it starts with; read instead: {log[-2000:]!r}"

    reported = re.search(r'"criu_version":\s*"([^"]+)"', startup[-1])
    assert reported, f"the startup line names the CRIU it runs: {startup[-1]}"

    # What `criu --version` states: a version, and for a build from git the
    # GitID beside it. The agent builds from criu-dev by default, so the GitID
    # is what tells two builds reporting the same version apart.
    version = reported.group(1)
    assert re.fullmatch(r"\d+(\.\d+)+ \(\S+\)", version), (
        "a development build is named by the commit it was built at, not by "
        f"its version alone: {version!r}"
    )
