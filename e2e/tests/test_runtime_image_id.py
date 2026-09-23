# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Cluster-free regressions for optional CRI image IDs in environment checks."""

import json
import shlex
from types import SimpleNamespace

import pytest
import test_snapshot_lifecycle as environment_tests

from snapshot_e2e import k8s
from snapshot_e2e import lifecycle


@pytest.mark.parametrize(
    "image_fields,want",
    [
        ({"imageId": " sha256:config "}, "sha256:config"),
        ({"imageId": "", "imageRef": "repo@sha256:index"}, ""),
        ({"imageRef": "repo@sha256:index"}, ""),
        ({"imageId": " "}, ""),
        ({"imageId": None}, ""),
    ],
)
def test_runtime_image_id_reads_optional_field_once(monkeypatch, image_fields, want):
    config = k8s.E2EConfig("test-ns", "snapshot", "pvc", None)
    commands = []

    def exec_payload(namespace, pod, command):
        assert (namespace, pod) == ("test-ns", "agent-pod")
        commands.append(shlex.split(command))
        return json.dumps({"status": {"id": "container-id", **image_fields}})

    monkeypatch.setattr(lifecycle, "checkpoint_agent_pod", lambda *_: "agent-pod")
    monkeypatch.setattr(k8s, "exec_payload", exec_payload)

    assert lifecycle.runtime_image_id(config, "node", "containerd://container-id") == want
    # No polling and no imageRef/image-service fallback, even on old CRI.
    assert commands == [
        ["nsenter", "-t", "1", "-m", "--", "crictl", "inspect", "container-id"]
    ]


@pytest.mark.parametrize("response", [{}, {"status": None}, {"status": {}}, {"status": []}])
def test_runtime_image_id_rejects_missing_status(monkeypatch, response):
    config = k8s.E2EConfig("test-ns", "snapshot", "pvc", None)
    monkeypatch.setattr(lifecycle, "checkpoint_agent_pod", lambda *_: "agent-pod")
    monkeypatch.setattr(k8s, "exec_payload", lambda *_: json.dumps(response))
    with pytest.raises(AssertionError, match="no container status"):
        lifecycle.runtime_image_id(config, "node", "containerd://container-id")


def test_runtime_image_id_does_not_hide_inspection_failure(monkeypatch):
    config = k8s.E2EConfig("test-ns", "snapshot", "pvc", None)

    def failed_exec(*_):
        raise RuntimeError("container status unavailable")

    monkeypatch.setattr(lifecycle, "checkpoint_agent_pod", lambda *_: "agent-pod")
    monkeypatch.setattr(k8s, "exec_payload", failed_exec)
    with pytest.raises(RuntimeError, match="container status unavailable"):
        lifecycle.runtime_image_id(config, "node", "containerd://container-id")


@pytest.mark.parametrize("image_id", ["", "sha256:" + "a" * 64])
@pytest.mark.parametrize(
    "defect",
    [
        None,
        "manifest_identity",
        "published_identity",
        "wrong_manifest_identity",
        "wrong_published_identity",
        "kernel",
        "memory",
        "gpu",
    ],
)
def test_environment_check_preserves_assertions_with_optional_image_id(
    monkeypatch, image_id, defect
):
    """Execute the real E2E assertions against fake cluster observations.

    Absent IDs must be omitted and present IDs must match. Neither case may
    skip checkpoint creation or the remaining environment comparisons.
    """
    config = k8s.E2EConfig("test-ns", "snapshot", "pvc", None)
    run = SimpleNamespace(source_pod="source", source_token="token", snapshot_name="snap")
    source = SimpleNamespace(
        metadata=SimpleNamespace(uid="pod-uid"),
        status=SimpleNamespace(
            container_statuses=[
                SimpleNamespace(
                    name=lifecycle.CONTAINER, container_id="containerd://container-id"
                )
            ]
        ),
    )
    gpu = {"uuid": "gpu-uuid", "name": "GPU model", "driver": "580.0"}
    manifest_pod = {"image": "repo:tag", "memoryLimit": "4Gi"}
    published_pod = {"image": "repo:tag", "memory": "4Gi"}
    if image_id:
        manifest_pod["imageId"] = image_id
        published_pod["imageDigest"] = image_id
    manifest = {
        "host": {"kernelVersion": "test-kernel", "cpuArch": "amd64"},
        "k8s": manifest_pod,
        "cudaRestore": {
            "sourceGpus": [{"uuid": gpu["uuid"], "productName": gpu["name"]}],
            "sourceDriverVersion": gpu["driver"],
        },
    }
    content = {
        "metadata": {"uid": "content-uid"},
        "status": {
            "source": {
                "node": {
                    "name": "node",
                    "architecture": "amd64",
                    "kernelVersion": "test-kernel",
                },
                "pod": published_pod,
                "devices": {
                    "nvidia": {
                        "driverVersion": gpu["driver"],
                        "instances": [{"productName": gpu["name"]}],
                    }
                },
            }
        },
    }
    if defect == "manifest_identity":
        if image_id:
            del manifest_pod["imageId"]
        else:
            manifest_pod["imageId"] = "sha256:invented"
    elif defect == "published_identity":
        if image_id:
            del published_pod["imageDigest"]
        else:
            published_pod["imageDigest"] = "sha256:invented"
    elif defect == "wrong_manifest_identity":
        manifest_pod["imageId"] = "sha256:" + "b" * 64
    elif defect == "wrong_published_identity":
        published_pod["imageDigest"] = "sha256:" + "b" * 64
    elif defect == "kernel":
        manifest["host"]["kernelVersion"] = "wrong-kernel"
    elif defect == "memory":
        manifest_pod["memoryLimit"] = "wrong-memory"
    elif defect == "gpu":
        manifest["cudaRestore"]["sourceDriverVersion"] = "wrong-driver"

    captures = []
    monkeypatch.setattr(
        environment_tests, "create_ready_source", lambda *_a, **_k: (source, "node")
    )
    monkeypatch.setattr(lifecycle, "wait_for_state_observations", lambda *_a, **_k: None)
    monkeypatch.setattr(lifecycle, "visible_gpus", lambda *_: [gpu])
    monkeypatch.setattr(lifecycle, "runtime_image_id", lambda *_: image_id)
    monkeypatch.setattr(lifecycle, "create_podsnapshot", lambda *args: captures.append(args))
    monkeypatch.setattr(lifecycle, "wait_for_snapshot_ready", lambda *_: (None, content))
    monkeypatch.setattr(lifecycle, "checkpoint_manifest", lambda *_: manifest)
    monkeypatch.setattr(lifecycle, "debug_dump", lambda *_: None)
    node = SimpleNamespace(
        status=SimpleNamespace(
            node_info=SimpleNamespace(kernel_version="test-kernel", architecture="amd64")
        )
    )
    pod = SimpleNamespace(
        spec=SimpleNamespace(
            containers=[
                SimpleNamespace(
                    name=lifecycle.CONTAINER,
                    image="repo:tag",
                    resources=SimpleNamespace(limits={"memory": "4Gi"}),
                )
            ]
        )
    )
    monkeypatch.setattr(k8s, "read_node", lambda *_: node)
    monkeypatch.setattr(k8s, "read_pod", lambda *_: pod)

    check = environment_tests.test_snapshot_records_the_environment_a_restore_is_checked_against
    if defect:
        with pytest.raises((AssertionError, KeyError)):
            check(config, run)
    else:
        check(config, run)
    assert len(captures) == 1
