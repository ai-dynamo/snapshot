# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Cluster-free checks for the small real-CUDA GMS fixture's non-GPU contract."""

from __future__ import annotations

import copy
import hashlib
import json
import os
import subprocess
import sys
from pathlib import Path
from types import SimpleNamespace

import pytest
import test_snapshotjob as jobs
from snapshot_e2e import gms_workload


@pytest.mark.workload
@pytest.mark.parametrize("literal", ["536870912", "5.36870912e+08", "1.5", "true", ".inf", "4294967296"])
def test_ghost_limit_requires_exact_in_range_integer(monkeypatch, literal):
    configmap = SimpleNamespace(data={"config.yaml": f"criu:\n  ghostLimit: {literal}\n"})
    monkeypatch.setattr(jobs.client.CoreV1Api, "list_namespaced_config_map", lambda *_args, **_kwargs: SimpleNamespace(items=[configmap]))
    config = jobs.k8s.E2EConfig("test", "snapshot", "pvc", None)
    if literal in {"536870912", "5.36870912e+08"}:
        assert jobs._deployed_ghost_limit(config) == 536870912
    else:
        with pytest.raises(AssertionError):
            jobs._deployed_ghost_limit(config)


@pytest.mark.workload
def test_weight_pattern_covers_the_entire_slab_and_distinguishes_attempts() -> None:
    expected = hashlib.sha256(b"source-token").digest()
    data = gms_workload.weight_bytes("source-token", 65)
    assert data == expected * 2 + expected[:1]
    assert data != gms_workload.weight_bytes("another-token", 65)


@pytest.mark.workload
@pytest.mark.parametrize("annotation", [None, "another-helper", "gms-saver"])
def test_real_api_response_must_retain_helper_opt_in(annotation) -> None:
    metadata = {} if annotation is None else {
        "annotations": {jobs.workloads.GMS_HELPER_ANNOTATION: annotation}
    }
    value = {"spec": {"podTemplate": {"metadata": metadata}}}
    if annotation == "gms-saver":
        jobs._assert_gms_opt_in_persisted(value)
    else:
        with pytest.raises(AssertionError, match="API pruned or changed"):
            jobs._assert_gms_opt_in_persisted(value)


@pytest.mark.workload
@pytest.mark.parametrize("policy", [None, "Retain", "CleanupHelpers"])
def test_snapshotjob_failure_policy_is_explicit_and_must_survive_admission(
    monkeypatch, policy
) -> None:
    def create(_group, _version, _namespace, _plural, body):
        return body

    monkeypatch.setattr(
        jobs.snap.client,
        "CustomObjectsApi",
        lambda: SimpleNamespace(create_namespaced_custom_object=create),
    )
    created = jobs.snap.create_snapshotjob(
        "test", "checkpoint",
        {"metadata": {"annotations": {jobs.workloads.GMS_HELPER_ANNOTATION: "gms-saver"}}},
        on_failure_policy=policy,
    )
    if policy is None:
        assert "onFailurePolicy" not in created["spec"]
    else:
        assert created["spec"]["onFailurePolicy"] == policy
    if policy == "CleanupHelpers":
        jobs._assert_gms_opt_in_persisted(created, on_failure_policy="CleanupHelpers")
    else:
        with pytest.raises(AssertionError, match="pruned or changed the failure policy"):
            jobs._assert_gms_opt_in_persisted(created, on_failure_policy="CleanupHelpers")


@pytest.mark.workload
def test_failure_fixture_holds_real_unlinked_allocated_file(
    tmp_path, monkeypatch
) -> None:
    monkeypatch.setattr(gms_workload.tempfile, "tempdir", str(tmp_path))
    fd = gms_workload.open_ghost_file(8 * 1024 * 1024 + 1)
    try:
        stat = os.fstat(fd)
        assert stat.st_nlink == 0
        assert stat.st_size == 8 * 1024 * 1024 + 1
        assert stat.st_blocks * 512 >= stat.st_size
        assert not list(tmp_path.iterdir())
    finally:
        os.close(fd)


@pytest.mark.workload
def test_script_argument_validation_does_not_import_gpu_dependencies() -> None:
    script = Path(gms_workload.__file__)
    for args in (["--help"], ["source"], ["source", "--failure-ghost-size", "-1"]):
        result = subprocess.run(
            [sys.executable, "-S", str(script), *args],
            capture_output=True,
            text=True,
            timeout=10,
        )
        assert result.returncode == (0 if args == ["--help"] else 2)
        assert "gpu_memory_service" not in result.stderr


GPU_UUID = "GPU-01234567-89ab-cdef-0123-456789abcdef"
CONTAINER_ID = "a" * 64


@pytest.mark.workload
def test_gpu_gate_waits_and_binds_approval_to_its_visible_uuid(
    tmp_path, monkeypatch
) -> None:
    monkeypatch.setattr(
        gms_workload.subprocess, "check_output", lambda *args, **kwargs: GPU_UUID + "\n"
    )

    def approve(_):
        assert (
            tmp_path / f"{gms_workload.WORKER_GATE}.waiting"
        ).read_text() == GPU_UUID
        (tmp_path / gms_workload.WORKER_GATE).write_text(GPU_UUID)

    monkeypatch.setattr(gms_workload.time, "sleep", approve)
    gms_workload.wait_for_gpu_approval(tmp_path, gms_workload.WORKER_GATE)
    (tmp_path / gms_workload.WORKER_GATE).write_text(
        "GPU-ffffffff-ffff-ffff-ffff-ffffffffffff"
    )
    with pytest.raises(AssertionError, match="admission UUID"):
        gms_workload.wait_for_gpu_approval(tmp_path, gms_workload.WORKER_GATE)


@pytest.mark.workload
@pytest.mark.parametrize(
    "visibility",
    ["", GPU_UUID + "\n" + GPU_UUID, "N/A", "MIG-01234567-89ab-cdef-0123-456789abcdef"],
)
def test_gpu_gate_rejects_missing_multiple_or_nonphysical_visibility(
    tmp_path, monkeypatch, visibility
) -> None:
    monkeypatch.setattr(
        gms_workload.subprocess, "check_output", lambda *args, **kwargs: visibility
    )
    with pytest.raises(AssertionError, match="exactly one physical GPU"):
        gms_workload.wait_for_gpu_approval(tmp_path, gms_workload.SERVER_GATE)
    assert not list(tmp_path.iterdir())


@pytest.mark.workload
def test_source_gate_precedes_all_gpu_imports(tmp_path, monkeypatch) -> None:
    class NotAdmitted(Exception):
        pass

    def gated(path, gate):
        assert path == tmp_path and gate == gms_workload.WORKER_GATE
        raise NotAdmitted

    monkeypatch.setattr(gms_workload, "wait_for_gpu_approval", gated)
    with pytest.raises(NotAdmitted):
        gms_workload.run_source("test-token", tmp_path, 0)
    assert "gpu_memory_service.common.vmm" not in sys.modules


@pytest.mark.workload
@pytest.mark.parametrize("uid", [None, "completed-attempt-uid"])
def test_server_gate_precedes_real_server_or_loader_exec(
    tmp_path, monkeypatch, uid
) -> None:
    calls = []
    monkeypatch.setattr(
        gms_workload,
        "wait_for_gpu_approval",
        lambda path, gate: calls.append((path, gate)),
    )
    monkeypatch.setattr(
        gms_workload.os, "execv", lambda executable, command: calls.append(command)
    )
    gms_workload.run_server(tmp_path, uid)
    assert calls[0] == (tmp_path, gms_workload.SERVER_GATE)
    assert calls[1][:3] == [sys.executable, "-m", "gpu_memory_service.cli.server"]
    if uid is None:
        assert len(calls[1]) == 3
    else:
        assert calls[1][3:] == [
            "--enable-loader",
            "--snapshot-storage-dir",
            "/checkpoints",
            "--snapshot-job-uid",
            uid,
            "--device",
            "0",
            "--max-workers",
            "1",
        ]


def dra_identity():
    pod = {
        "metadata": {"uid": "exact-pod-uid", "name": "run-owned-pod", "namespace": "run-owned"},
        "spec": {"nodeName": "node-a"},
        "status": {
            "resourceClaimStatuses": [
                {"name": jobs.workloads.GMS_CLAIM, "resourceClaimName": "claim-a"}
            ]
        },
    }
    claim = {
        "metadata": {
            "name": "claim-a", "namespace": "run-owned",
            "ownerReferences": [{"kind": "Pod", "name": "run-owned-pod", "uid": "exact-pod-uid", "controller": True}],
        },
        "status": {
            "reservedFor": [{"resource": "pods", "name": "run-owned-pod", "uid": "exact-pod-uid"}],
            "allocation": {
                "devices": {
                    "results": [
                        {
                            "request": "gpus",
                            "driver": "gpu.nvidia.com",
                            "pool": "pool-a",
                            "device": "gpu-7",
                        }
                    ]
                }
            },
        },
    }
    slices = [
        {
            "spec": {
                "driver": "gpu.nvidia.com",
                "pool": {"name": "pool-a", "generation": 1},
                "nodeName": "node-a",
                "devices": [
                    {"name": "gpu-7", "attributes": {"uuid": {"string": GPU_UUID}}}
                ],
            }
        }
    ]
    return pod, claim, slices


@pytest.mark.workload
def test_dra_resolution_uses_exact_bound_claim_and_slice_uuid() -> None:
    pod, claim, slices = dra_identity()
    assert jobs._allocated_gpu_uuid(pod, claim, slices) == GPU_UUID
    # A same-name device on another pool must not influence the allocation.
    unrelated = copy.deepcopy(slices[0])
    unrelated["spec"]["pool"]["name"] = "other-pool"
    unrelated["spec"]["devices"][0]["attributes"]["uuid"]["string"] = (
        "GPU-ffffffff-ffff-ffff-ffff-ffffffffffff"
    )
    assert jobs._allocated_gpu_uuid(pod, claim, slices + [unrelated]) == GPU_UUID


@pytest.mark.workload
@pytest.mark.parametrize(
    "fault",
    [
        "binding",
        "reservation",
        "reservation-name",
        "owner",
        "namespace",
        "driver",
        "multi-device",
        "node",
        "duplicate",
        "generation",
        "uuid",
    ],
)
def test_dra_resolution_fails_closed_on_ambiguous_or_wrong_generation_identity(
    fault,
) -> None:
    pod, claim, slices = dra_identity()
    if fault == "binding":
        pod["status"]["resourceClaimStatuses"] = []
    elif fault == "reservation":
        claim["status"]["reservedFor"][0]["uid"] = "different-pod-generation"
    elif fault == "reservation-name":
        claim["status"]["reservedFor"][0]["name"] = "different-pod"
    elif fault == "owner":
        claim["metadata"]["ownerReferences"][0]["uid"] = "different-pod-generation"
    elif fault == "namespace":
        claim["metadata"]["namespace"] = "different-namespace"
    elif fault == "driver":
        claim["status"]["allocation"]["devices"]["results"][0]["driver"] = (
            "other-driver"
        )
    elif fault == "multi-device":
        claim["status"]["allocation"]["devices"]["results"] *= 2
    elif fault == "node":
        slices[0]["spec"]["nodeName"] = "different-node"
    elif fault == "duplicate":
        slices *= 2
    elif fault == "generation":
        slices.append(copy.deepcopy(slices[0]))
        slices[-1]["spec"]["pool"]["generation"] = 2
    else:
        slices[0]["spec"]["devices"][0]["attributes"]["uuid"]["string"] = "gpu-7"
    with pytest.raises(AssertionError):
        jobs._allocated_gpu_uuid(pod, claim, slices)


def vcluster_dra_identity():
    physical, claim, slices = dra_identity()
    physical["metadata"].update(name="physical-pod", namespace="host-run")
    physical["status"]["initContainerStatuses"] = [
        {"name": "gms-server", "containerID": "containerd://" + CONTAINER_ID}
    ]
    virtual = copy.deepcopy(physical)
    virtual["metadata"].update(name="logical-pod", namespace="run-owned", uid="logical-generation")
    physical["metadata"]["annotations"] = {
        "vcluster.loft.sh/object-name": "logical-pod",
        "vcluster.loft.sh/object-namespace": "run-owned",
        "vcluster.loft.sh/object-uid": "logical-generation",
        "vcluster.loft.sh/object-kind": "/v1, Kind=Pod",
        "vcluster.loft.sh/object-host-name": "physical-pod",
        "vcluster.loft.sh/object-host-namespace": "host-run",
        "vcluster.loft.sh/token-placeholder": "never-export-this-credential",
    }
    claim["metadata"]["namespace"] = "host-run"
    claim["metadata"]["ownerReferences"][0]["name"] = "physical-pod"
    claim["status"]["reservedFor"][0]["name"] = "physical-pod"
    return virtual, physical, claim, slices


@pytest.mark.workload
def test_vcluster_mapping_uses_physical_pod_uid_for_authoritative_dra_proof():
    virtual, physical, claim, slices = vcluster_dra_identity()
    assert jobs._physical_gms_pod(virtual, [physical], "host-run") is physical
    assert jobs._allocated_gpu_uuid(physical, claim, slices) == GPU_UUID
    with pytest.raises(AssertionError):
        jobs._allocated_gpu_uuid(virtual, claim, slices)


@pytest.mark.workload
@pytest.mark.parametrize("fault", ["uid", "name", "namespace", "duplicate", "node", "scope", "kind", "self-name", "self-namespace", "container", "missing-container", "malformed-container"])
def test_vcluster_mapping_fails_closed_on_ambiguous_or_stale_identity(fault):
    virtual, physical, _, _ = vcluster_dra_identity()
    pods = [physical]
    annotations = physical["metadata"]["annotations"]
    if fault in {"uid", "name", "namespace"}:
        annotations[f"vcluster.loft.sh/object-{fault}"] = "different-generation"
    elif fault == "duplicate":
        pods.append(copy.deepcopy(physical))
    elif fault == "node":
        physical["spec"]["nodeName"] = "another-node"
    elif fault == "scope":
        physical["metadata"]["namespace"] = "another-host-scope"
    elif fault == "kind":
        annotations["vcluster.loft.sh/object-kind"] = "/v1, Kind=Secret"
    elif fault == "self-name":
        annotations["vcluster.loft.sh/object-host-name"] = "another-host-pod"
    elif fault == "self-namespace":
        annotations["vcluster.loft.sh/object-host-namespace"] = "another-host-scope"
    elif fault == "missing-container":
        physical["status"]["initContainerStatuses"] = []
    else:
        physical["status"]["initContainerStatuses"][0]["containerID"] = (
            "containerd://" + "b" * 64 if fault == "container" else "unknown-container"
        )
    with pytest.raises(AssertionError):
        jobs._physical_gms_pod(virtual, pods, "host-run")


@pytest.mark.workload
def test_direct_dra_view_never_opens_a_host_client(monkeypatch):
    pod, _, _ = dra_identity()
    monkeypatch.setenv("SNAPSHOT_E2E_MODE", "direct")
    marker = object()
    monkeypatch.setattr(jobs.client, "CustomObjectsApi", lambda: marker)
    monkeypatch.setattr(jobs.kube_config, "new_client_from_config", lambda **kwargs: pytest.fail("direct mode opened host credentials"))
    config = jobs.k8s.E2EConfig("run-owned", "release", "pvc", None)
    assert jobs._gms_dra_view(config, pod) == (marker, pod, "run-owned")


@pytest.mark.workload
@pytest.mark.parametrize("missing", ["SNAPSHOT_E2E_HOST_KUBECONFIG", "SNAPSHOT_E2E_HOST_NAMESPACE"])
def test_vcluster_dra_view_requires_explicit_host_inputs(monkeypatch, missing):
    virtual, _, _, _ = vcluster_dra_identity()
    monkeypatch.setenv("SNAPSHOT_E2E_MODE", "vcluster")
    monkeypatch.setenv("SNAPSHOT_E2E_HOST_KUBECONFIG", "/placeholder-host-config")
    monkeypatch.setenv("SNAPSHOT_E2E_HOST_NAMESPACE", "host-run")
    monkeypatch.delenv(missing)
    config = jobs.k8s.E2EConfig("run-owned", "release", "pvc", None)
    with pytest.raises(AssertionError, match="explicit host"):
        jobs._gms_dra_view(config, virtual)


@pytest.mark.workload
def test_vcluster_dra_view_reads_host_without_changing_virtual_client(monkeypatch):
    virtual, physical, _, _ = vcluster_dra_identity()
    monkeypatch.setenv("SNAPSHOT_E2E_MODE", "vcluster")
    monkeypatch.setenv("SNAPSHOT_E2E_HOST_KUBECONFIG", "/placeholder-host-config")
    monkeypatch.setenv("SNAPSHOT_E2E_HOST_NAMESPACE", "host-run")
    calls = []
    host = SimpleNamespace(sanitize_for_serialization=lambda value: value)
    marker = object()
    monkeypatch.setattr(jobs, "write_normalized_kubeconfig", lambda path, context: calls.append((path, context)) or Path("normalized-host-placeholder"))
    monkeypatch.setattr(jobs.kube_config, "new_client_from_config", lambda **kwargs: calls.append(kwargs) or host)
    monkeypatch.setattr(jobs.client, "CoreV1Api", lambda api: SimpleNamespace(list_namespaced_pod=lambda ns: calls.append(ns) or {"items": [physical]}))
    monkeypatch.setattr(jobs.client, "CustomObjectsApi", lambda api: marker if api is host else pytest.fail("allocation used virtual API"))
    config = jobs.k8s.E2EConfig("run-owned", "release", "pvc", None)
    assert jobs._gms_dra_view(config, virtual) == (marker, physical, "host-run")
    assert calls == [("/placeholder-host-config", None), {"config_file": "normalized-host-placeholder"}, "host-run"]


@pytest.mark.workload
@pytest.mark.parametrize("state", ["active", "gone", "new-generation", "forbidden"])
def test_claim_release_checks_authoritative_scope_and_generation(state):
    calls = []

    def read(*args):
        calls.append(args)
        if state in {"gone", "forbidden"}:
            raise jobs.ApiException(status=404 if state == "gone" else 403)
        return {"metadata": {"uid": "old-uid" if state == "active" else "new-uid"}}

    api = SimpleNamespace(get_namespaced_custom_object=read)
    if state == "new-generation":
        with pytest.raises(AssertionError, match="another generation"):
            jobs._gms_claim_released(api, "host-run", "owned-claim", "old-uid")
    elif state == "forbidden":
        with pytest.raises(jobs.ApiException):
            jobs._gms_claim_released(api, "host-run", "owned-claim", "old-uid")
    else:
        assert jobs._gms_claim_released(api, "host-run", "owned-claim", "old-uid") is (True if state == "gone" else None)
    assert calls == [("resource.k8s.io", "v1", "host-run", "resourceclaims", "owned-claim")]


@pytest.mark.workload
def test_nvml_inventory_includes_compute_and_graphics_and_requires_matching_uuid() -> (
    None
):
    xml = (
        f"<nvidia_smi_log><gpu><uuid>{GPU_UUID}</uuid><processes>"
        "<process_info><pid>123</pid><type>C</type></process_info>"
        "<process_info><pid>456</pid><type>G</type></process_info>"
        "</processes></gpu></nvidia_smi_log>"
    )
    assert jobs._gpu_process_ids(xml, GPU_UUID) == [123, 456]
    with pytest.raises(AssertionError, match="UUID mismatch"):
        jobs._gpu_process_ids(xml, "GPU-ffffffff-ffff-ffff-ffff-ffffffffffff")
    for processes in ("", "<processes>N/A</processes>"):
        with pytest.raises(AssertionError, match="unavailable"):
            jobs._gpu_process_ids(
                f"<nvidia_smi_log><gpu><uuid>{GPU_UUID}</uuid>{processes}</gpu></nvidia_smi_log>",
                GPU_UUID,
            )


@pytest.mark.workload
@pytest.mark.parametrize(
    "cgroup",
    [
        f"0::/kubepods/pod-owned/cri-containerd-{CONTAINER_ID}.scope\n",
        f"11:memory:/kubepods/pod-owned/{CONTAINER_ID}\n",
    ],
)
def test_nvml_consumer_attribution_accepts_only_exact_container_identity(
    cgroup,
) -> None:
    jobs._assert_owned_gpu_process(cgroup, {CONTAINER_ID})
    for unrelated in ("b" * 64, "host-process", CONTAINER_ID + "/" + "b" * 64):
        with pytest.raises(AssertionError, match="unrelated or unattributable"):
            jobs._assert_owned_gpu_process(
                f"0::/kubepods/{unrelated}\n", {CONTAINER_ID}
            )


@pytest.mark.workload
def test_admission_checks_server_before_startup_and_all_containers_before_worker(
    tmp_path, monkeypatch
) -> None:
    raw, claim, slices = dra_identity()
    claim["metadata"]["uid"] = "exact-claim-uid"
    actions = []
    approvals = set()
    visible = set()
    running = SimpleNamespace(running=True)
    pending = SimpleNamespace(running=False)

    def pod():
        return SimpleNamespace(
            metadata=SimpleNamespace(uid=raw["metadata"]["uid"]),
            status=SimpleNamespace(
                init_container_statuses=[
                    SimpleNamespace(name="gms-server", state=running)
                ],
                container_statuses=[
                    SimpleNamespace(
                        name=name,
                        state=running if "gms-server" in approvals else pending,
                    )
                    for name in (jobs.workloads.CONTAINER, "gms-saver")
                ],
            ),
        )

    monkeypatch.setattr(jobs.k8s, "read_pod", lambda *args: pod())
    monkeypatch.setattr(
        jobs.client,
        "ApiClient",
        lambda: SimpleNamespace(sanitize_for_serialization=lambda value: raw),
    )
    monkeypatch.setattr(
        jobs.client,
        "CustomObjectsApi",
        lambda: SimpleNamespace(
            get_namespaced_custom_object=lambda *args: claim,
            list_cluster_custom_object=lambda *args: {"items": slices},
        ),
    )

    def guard(config, current, uuid):
        assert uuid == GPU_UUID
        stage = "all" if "gms-server" in approvals else "server"

        def check():
            actions.append(f"census-{stage}")

        return check

    monkeypatch.setattr(jobs, "_gpu_guard", guard)

    def exec_payload(namespace, pod_name, command, *, container):
        if command.startswith("if "):
            return GPU_UUID
        if "--query-gpu=uuid" in command:
            visible.add(container)
            assert jobs.workloads.CONTAINER not in approvals
            return GPU_UUID
        if command.startswith("printf "):
            if container == "gms-server":
                assert "census-server" in actions and not visible
            else:
                assert container == jobs.workloads.CONTAINER
                assert visible == {jobs.workloads.CONTAINER, "gms-server", "gms-saver"}
                assert "census-all" in actions
            actions.append(f"approve-{container}")
            approvals.add(container)
            return GPU_UUID
        raise AssertionError(command)

    monkeypatch.setattr(jobs.k8s, "exec_payload", exec_payload)
    config = jobs.k8s.E2EConfig("run-owned", "isolated", "pvc", None)
    uuid, _, released = jobs._admit_gms_pod(config, "run-owned-pod", source=True)
    assert uuid == GPU_UUID
    assert actions.index("census-server") < actions.index("approve-gms-server")
    assert (
        actions.index("approve-gms-server")
        < actions.index("census-all")
        < actions.index(f"approve-{jobs.workloads.CONTAINER}")
    )
    # Terminal reconciliation can clear allocation fields; the release check
    # retains the generation proven at admission instead of re-proving it.
    claim["status"] = {}
    assert released() is None
    claim["metadata"]["uid"] = "reused-claim-name"
    with pytest.raises(AssertionError, match="another generation"):
        released()


@pytest.mark.workload
def test_failed_evidence_whitelist_excludes_admission_credentials(monkeypatch) -> None:
    poisoned = "never-export-this-credential"
    metadata = {
        "name": "failed",
        "uid": "failed-uid",
        "namespace": "isolated",
        "annotations": {"vcluster-token": poisoned},
        "labels": {"untrusted": poisoned},
        "ownerReferences": [{"kind": "Job", "name": "owner", "uid": "owner-uid"}],
    }
    status = {
        "conditions": [
            {
                "type": "Failed",
                "status": "True",
                "reason": "CaptureFailed",
                "message": poisoned,
            }
        ],
        "containerStatuses": [
            {
                "name": "worker",
                "containerID": "containerd://" + CONTAINER_ID,
                "state": {
                    "terminated": {
                        "exitCode": 42,
                        "reason": "Error",
                        "message": poisoned,
                    }
                },
            }
        ],
    }
    value = {
        "metadata": metadata,
        "status": status,
        "spec": {"containers": [{"env": [{"value": poisoned}]}]},
    }
    monkeypatch.setattr(
        jobs.client,
        "ApiClient",
        lambda: SimpleNamespace(sanitize_for_serialization=lambda item: value),
    )
    pod = SimpleNamespace(
        spec=SimpleNamespace(node_name="node-a"), status=SimpleNamespace(phase="Failed")
    )
    job = SimpleNamespace(spec=SimpleNamespace(backoff_limit=0))
    result = jobs._failed_gms_evidence(
        value, pod, job, {"snapshot-e2e/test": "known-test"}
    )
    assert poisoned not in json.dumps(result)
    assert result["pod"]["containers"][0]["state"]["terminated"]["exitCode"] == 42
    assert result["job"]["backoffLimit"] == 0
    assert result["snapshotJob"]["conditions"][0]["reason"] == "CaptureFailed"


@pytest.mark.workload
def test_invoked_gms_diagnostic_never_exports_admission_or_freeform_tokens(
    monkeypatch, capsys
) -> None:
    monkeypatch.setenv("SNAPSHOT_E2E_WORKLOAD_IMAGE", "safe-fixture:test")
    poison = "never-export-this-admission-token"
    run = jobs.snap.TestRun.new("safe-gms-debug")
    meta = jobs.client.V1ObjectMeta(
        name="source",
        namespace="isolated",
        uid="pod-uid",
        annotations={"vcluster-token": poison},
        labels={"unknown": poison},
    )
    pod = jobs.client.V1Pod(
        metadata=meta,
        spec=jobs.client.V1PodSpec(
            node_name="node-a",
            containers=[
                jobs.client.V1Container(
                    name=name, env=[jobs.client.V1EnvVar(name="POISON", value=poison)]
                )
                for name in (jobs.workloads.CONTAINER, "gms-saver")
            ],
        ),
        status=jobs.client.V1PodStatus(
            phase="Failed",
            container_statuses=[
                jobs.client.V1ContainerStatus(
                    name=jobs.workloads.CONTAINER,
                    image="test",
                    image_id="test",
                    restart_count=0,
                    ready=False,
                    state=jobs.client.V1ContainerState(
                        terminated=jobs.client.V1ContainerStateTerminated(
                            exit_code=42, message=poison
                        )
                    ),
                )
            ],
        ),
    )
    job = jobs.client.V1Job(
        metadata=meta,
        spec=jobs.client.V1JobSpec(
            backoff_limit=0, template=jobs.client.V1PodTemplateSpec(spec=pod.spec)
        ),
        status=jobs.client.V1JobStatus(
            conditions=[
                jobs.client.V1JobCondition(
                    type="Failed",
                    status="True",
                    reason="BackoffLimitExceeded",
                    message=poison,
                )
            ]
        ),
    )
    sj = {
        "metadata": {
            "name": run.snapshotjob_name,
            "uid": "sj-uid",
            "annotations": {"token": poison},
        },
        "status": {
            "conditions": [
                {
                    "type": "Failed",
                    "status": "True",
                    "reason": "CaptureFailed",
                    "message": poison,
                }
            ]
        },
    }
    monkeypatch.setattr(jobs.client, "CustomObjectsApi", lambda: SimpleNamespace())
    monkeypatch.setattr(
        jobs.client,
        "CoreV1Api",
        lambda: SimpleNamespace(
            list_namespaced_pod=lambda *args, **kwargs: SimpleNamespace(items=[pod])
        ),
    )
    monkeypatch.setattr(jobs.snap, "get_custom_object", lambda *args: sj)
    monkeypatch.setattr(jobs.k8s, "read_job", lambda *args: job)
    monkeypatch.setattr(jobs.k8s, "list_job_pods", lambda *args: [pod])

    def missing_restore(*args):
        raise jobs.ApiException(status=404, reason=poison, http_resp=None)

    monkeypatch.setattr(jobs.k8s, "read_pod", missing_restore)
    safe_worker = {
        "phase": "quiesced",
        "token": run.source_token,
        "bytes": 4096,
        "sha256": "a" * 64,
        "annotations": {"token": poison},
    }
    logs = (
        poison
        + "\n"
        + json.dumps(safe_worker)
        + "\n"
        + json.dumps({"phase": "saver-published", "token": poison})
    )
    monkeypatch.setattr(jobs.k8s, "pod_logs", lambda *args, **kwargs: logs)
    monkeypatch.setattr(
        jobs.snap,
        "debug_dump_snapshotjob",
        lambda *args: pytest.fail("unsafe generic diagnostic invoked"),
    )
    config = jobs.k8s.E2EConfig("isolated", "snapshot", "pvc", None)
    jobs._debug_gms_snapshotjob(config, run)
    output = capsys.readouterr().out
    assert poison not in output
    assert '"uid": "sj-uid"' in output and '"exitCode": 42' in output
    assert '"reason": "CaptureFailed"' in output
    assert '"phase": "quiesced"' in output and '"phase": "saver-published"' in output
    assert "http_status=404" in output


@pytest.mark.workload
@pytest.mark.parametrize(
    "case,expected_diagnostics",
    [
        (jobs.test_snapshotjob_gms_capture_failure_reclaims_after_operator_restart, 2),
        (
            jobs.test_snapshotjob_gms_completed_artifact_survives_job_deletion_and_restore,
            1,
        ),
    ],
)
def test_gms_exception_handlers_route_only_to_safe_diagnostic(
    monkeypatch, case, expected_diagnostics
) -> None:
    monkeypatch.setenv("SNAPSHOT_E2E_WORKLOAD_IMAGE", "safe-fixture:test")

    class FixtureFailure(Exception):
        pass

    def failed_fixture(*args):
        raise FixtureFailure

    calls = []
    monkeypatch.setattr(jobs, "_create_completed_gms", failed_fixture)
    monkeypatch.setattr(
        jobs, "_debug_gms_snapshotjob", lambda config, run: calls.append(run)
    )
    monkeypatch.setattr(jobs.snap, "cleanup_snapshotjob", lambda *args: None)
    monkeypatch.setattr(
        jobs.snap,
        "debug_dump_snapshotjob",
        lambda *args: pytest.fail("unsafe generic diagnostic invoked"),
    )
    config = jobs.k8s.E2EConfig("isolated", "snapshot", "pvc", None)
    run = jobs.snap.TestRun.new("safe-handler")
    with pytest.raises(FixtureFailure):
        case(config, run, ("image", "claim-template", "fixture-script"))
    assert len(calls) == expected_diagnostics
    assert calls[0] is run


@pytest.mark.workload
def test_missing_gms_phase_does_not_echo_untrusted_logs() -> None:
    with pytest.raises(AssertionError, match="missing real GMS worker phase") as caught:
        jobs._gms_phase("never-export-this-admission-token", "quiesced")
    assert "never-export-this-admission-token" not in str(caught.value)


@pytest.mark.workload
def test_safe_evidence_handles_missing_and_pending_lifecycle_objects() -> None:
    pending = jobs.client.V1Pod(
        metadata=jobs.client.V1ObjectMeta(name="pending", uid="pending-uid"),
        spec=jobs.client.V1PodSpec(containers=[]),
    )
    value = jobs._failed_gms_evidence(None, pending, None, {})
    assert value["pod"]["uid"] == "pending-uid" and value["pod"]["containers"] == []
    assert value["job"]["backoffLimit"] is None
