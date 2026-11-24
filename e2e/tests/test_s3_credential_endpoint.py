# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Snapshot installed against the S3 store with no credentials Secret.

The chart is installed with ``storage.s3.credentialEndpoint`` pointing at the
endpoint from ``snapshot_e2e.credential_endpoint``. The test checks that the
identity variables reach PageBroker, that a checkpoint is written to the store
with credentials the endpoint issued to the agent, and that the operator's
cleanup gets its own credentials the same way. Restores are not covered: the
endpoint's certificate comes from a private CA, which the native reader cannot
trust yet.

Runs only with SNAPSHOT_E2E_S3_CREDENTIALS=endpoint.
"""

from __future__ import annotations

import os

import pytest

from snapshot_e2e import credential_endpoint, k8s
from snapshot_e2e import lifecycle as snap

pytestmark = [
    pytest.mark.snapshot_success,
    pytest.mark.cpu,
    pytest.mark.skipif(
        os.environ.get("SNAPSHOT_E2E_S3_CREDENTIALS") != "endpoint",
        reason="needs SNAPSHOT_E2E_S3_CREDENTIALS=endpoint",
    ),
]

PAGEBROKER_CONTAINER = "pagebroker"
IDENTITY_VARIABLES = {
    "AWS_CONTAINER_CREDENTIALS_FULL_URI",
    "AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE",
}


def pagebroker_environment(config: k8s.E2EConfig) -> dict[str, str]:
    pods = k8s.list_snapshot_pods(config.namespace, config.release, "snapshot-agent")
    assert pods, "no Snapshot agent pod"
    for container in pods[0].spec.containers:
        if container.name == PAGEBROKER_CONTAINER:
            return {item.name: item.value or "" for item in container.env or []}
    raise AssertionError(f"no {PAGEBROKER_CONTAINER} container in {pods[0].metadata.name}")


def issued_to(config: k8s.E2EConfig, service_account: str) -> bool:
    logs = k8s.pod_logs(
        config.namespace,
        credential_endpoint.endpoint_pod_name(config.namespace),
        tail_lines=None,
        container="endpoint",
    )
    subject = f"system:serviceaccount:{config.namespace}:{service_account}"
    return f"{credential_endpoint.ISSUED} {subject}" in logs


def test_pagebroker_gets_the_endpoint_and_no_credentials_file(config: k8s.E2EConfig) -> None:
    environment = pagebroker_environment(config)
    assert IDENTITY_VARIABLES <= set(environment)
    assert environment["AWS_CONTAINER_CREDENTIALS_FULL_URI"].startswith("https://")
    assert "AWS_ACCESS_KEY_ID" not in environment
    assert "AWS_SHARED_CREDENTIALS_FILE" not in environment


def test_checkpoint_and_cleanup_use_credentials_the_endpoint_issued(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    k8s.create_pod(snap.source_pod(config=config, run=run, gpu=False))
    source = snap.wait_for_pod_ready(config.namespace, run.source_pod)
    snap.wait_for_file(config.namespace, run.source_pod, snap.SOURCE_READY)
    source_node = source.spec.node_name

    snap.create_podsnapshot(
        config.namespace, run.snapshot_name, run.source_pod, source.metadata.uid
    )
    pod_snapshot, content = snap.wait_for_snapshot_ready(config.namespace, run.snapshot_name)
    assert pod_snapshot["status"]["boundSnapshotContentName"] == content["metadata"]["name"]
    artifacts = content["status"]["storage"]["artifacts"]
    assert [artifact["containerName"] for artifact in artifacts] == [snap.CONTAINER]
    assert not snap.artifact_root_exists(config, source_node, content["metadata"]["uid"]), (
        "the checkpoint must not also be written to the checkpoint volume"
    )
    assert issued_to(config, config.release), "the endpoint issued nothing to the agent"

    snap.delete_podsnapshot(config.namespace, run.snapshot_name)
    snap.wait_for_custom_object_deleted(None, content["metadata"]["name"], "podsnapshotcontents")
    assert issued_to(config, f"{config.release}-operator"), (
        "the endpoint issued nothing to the operator"
    )
