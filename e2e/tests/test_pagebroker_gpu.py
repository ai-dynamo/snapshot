# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Run the PageBroker GPU binaries without installing Snapshot."""

import os
import subprocess
import uuid
from pathlib import Path

import pytest

from snapshot_e2e import k8s
from snapshot_e2e.lifecycle import wait_for
from snapshot_e2e.workloads import workload_scheduling


@pytest.mark.gpu
def test_pagebroker_gpu(config: k8s.E2EConfig) -> None:
    revision = subprocess.check_output(
        ["git", "rev-parse", "HEAD"],
        cwd=Path(__file__).resolve().parents[2],
        text=True,
    ).strip()
    image = os.environ.get(
        "SNAPSHOT_E2E_PAGEBROKER_GPU_IMAGE",
        f"ghcr.io/ai-dynamo/snapshot/pagebroker:gpu-tests-{revision}",
    )
    name = f"pagebroker-gpu-{uuid.uuid4().hex[:8]}"
    scheduling = workload_scheduling()
    scheduling["nodeSelector"]["kubernetes.io/arch"] = "amd64"
    k8s.create_job({
        "apiVersion": "batch/v1",
        "kind": "Job",
        "metadata": {"name": name, "namespace": config.namespace},
        "spec": {
            "backoffLimit": 0,
            "activeDeadlineSeconds": 600,
            "template": {"spec": {
                **scheduling,
                "restartPolicy": "Never",
                "runtimeClassName": "nvidia",
                "terminationGracePeriodSeconds": 5,
                "containers": [{
                    "name": "tests",
                    "image": image,
                    "imagePullPolicy": "Always",
                    "env": [{"name": "PAGEBROKER_EXPECTED_REVISION", "value": revision}],
                    "resources": {
                        "requests": {"cpu": "1", "memory": "1Gi"},
                        "limits": {"nvidia.com/gpu": "1", "memory": "4Gi"},
                    },
                    "securityContext": {
                        "capabilities": {"add": ["SYS_PTRACE"]},
                        "seccompProfile": {"type": "Unconfined"},
                    },
                    "volumeMounts": [{"name": "scratch", "mountPath": "/tmp"}],
                }],
                "volumes": [{"name": "scratch", "emptyDir": {"sizeLimit": "4Gi"}}],
            }},
        },
    })

    def completed():
        job = k8s.read_job(config.namespace, name)
        assert job is not None, f"GPU test Job {name} disappeared"
        assert not job.status.failed, f"GPU test Job failed: {job.status.conditions}"
        return job if job.status.succeeded else None

    try:
        wait_for(f"GPU test Job {name}", completed, timeout=630)
    finally:
        try:
            for pod in k8s.list_job_pods(config.namespace, name):
                print(f"{pod.metadata.name}: {pod.status.phase}, node={pod.spec.node_name}")
                for container in pod.status.container_statuses or []:
                    print(f"{container.name}: imageID={container.image_id}")
                print(k8s.pod_logs(config.namespace, pod.metadata.name, tail_lines=2000))
                for event in k8s.list_events(
                    config.namespace, field_selector={"involvedObject.uid": pod.metadata.uid}
                ):
                    print(f"{event.reason}: {event.message}")
        finally:
            k8s.delete_job(config.namespace, name)
