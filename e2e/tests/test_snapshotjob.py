# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""SnapshotJob lifecycle e2e under capture-driven completion.

Unlike test_snapshot_lifecycle.py (which drives PodSnapshot directly against a
plain pod the test creates and annotates itself), these tests exercise the
SnapshotJob CRD end to end. The contract under test is a two-stage completion
gate: the controller creates the source batch/v1 Job; once the pod is ready,
the agent dumps it and the dump terminates the target container (there is no
leave-running mode) — that death is the expected success sequence. A Ready
capture sets Captured=True and WaitingForPodCompletion; the SnapshotJob then
waits for the source Job to finish so helper containers can complete their
work, requires every non-target container to exit 0, and only then flips
Completed=True/JobCompleted and deletes the Job. Failure paths preserve the
Job and stamp all four conditions.

Terminal reasons that race (the Job controller vs. the capture pipeline
observing the same dead workload) are asserted as reason *sets* via
assert_snapshotjob_failure_vector — pinning one raced reason would codify a
race as a contract.
"""

from __future__ import annotations

import hashlib
import json
import os
import re
import shlex
import time
import uuid
import xml.etree.ElementTree as ET
from pathlib import Path
from typing import Any

import pytest
import yaml
from kubernetes import client
from kubernetes import config as kube_config
from kubernetes.client import ApiException
from snapshot_e2e import gms_workload, k8s, workloads
from snapshot_e2e import lifecycle as snap
from snapshot_e2e.infra.preflight import write_normalized_kubeconfig


@pytest.fixture
def run(request: pytest.FixtureRequest, config: k8s.E2EConfig) -> snap.TestRun:
    # Overrides the conftest.py `run` fixture for this module: SnapshotJob
    # cleanup is shaped differently (delete the SnapshotJob, not a bare
    # PodSnapshot by run.snapshot_name — see cleanup_snapshotjob's docstring).
    value = snap.TestRun.new(request.node.name.replace("_", "-")[:24])
    yield value
    snap.cleanup_snapshotjob(config, value)


def assert_snapshotjob_completed(sj: dict[str, Any]) -> None:
    """Asserts the full success vector of the two-stage completion gate."""
    completed = snap.condition(sj, "Completed")
    assert completed and completed.get("status") == "True" and completed.get("reason") == "JobCompleted"
    captured = snap.condition(sj, "Captured")
    assert captured and captured.get("status") == "True"
    assert captured.get("reason") == "CaptureCompleted"
    # The source Job fails by design (the dump kills the target container); a
    # completed SnapshotJob must not advertise that as JobFailed.
    running = snap.condition(sj, "Running")
    assert running and running.get("status") == "False"
    assert running.get("reason") == "JobCompleted"
    # All four conditions are present from the first reconcile and are only
    # updated, never removed: success must carry an explicit Failed=False.
    failed = snap.condition(sj, "Failed")
    assert failed is not None, "Failed must be present (known False), not absent"
    assert failed.get("status") == "False"
    assert failed.get("reason") == "NoFailure"
    assert sj["status"]["completedAt"]
    # status.startedAt is deliberately not asserted: it is recorded only if a
    # reconcile observes job.status.ready > 0, and under kill-based capture
    # that readiness window can be shorter than the watch latency.


@pytest.fixture
def gms_fixture(config: k8s.E2EConfig, run: snap.TestRun):
    image = os.environ.get("SNAPSHOT_E2E_GMS_IMAGE")
    if not image:
        pytest.skip("SNAPSHOT_E2E_GMS_IMAGE must contain the managed GMS V1 changes")
    assert os.environ.get("SNAPSHOT_E2E_ALLOW_OPERATOR_RESTART") == "1", (
        "GMS rollback qualification restarts the selected Snapshot operator; "
        "set SNAPSHOT_E2E_ALLOW_OPERATOR_RESTART=1 only for an isolated e2e release"
    )
    api = client.CustomObjectsApi()
    claim_template = f"{run.suffix}-gpu"
    fixture_configmap = f"{run.suffix}-gms"
    # Follow Dynamo's existing DRA contract: all three source containers refer
    # to one claim, rather than independently requesting scalar GPUs.
    api.create_namespaced_custom_object(
        "resource.k8s.io", "v1", config.namespace, "resourceclaimtemplates",
        {
            "apiVersion": "resource.k8s.io/v1", "kind": "ResourceClaimTemplate",
            "metadata": {"name": claim_template, "namespace": config.namespace, "labels": run.labels},
            "spec": {"spec": {"devices": {"requests": [{
                "name": "gpus", "exactly": {
                    "deviceClassName": "gpu.nvidia.com", "allocationMode": "ExactCount", "count": 1,
                },
            }]}}},
        },
    )
    try:
        k8s.apply_configmap(config.namespace, {
            "apiVersion": "v1", "kind": "ConfigMap",
            "metadata": {"name": fixture_configmap, "namespace": config.namespace, "labels": run.labels},
            "data": {"gms_workload.py": Path(gms_workload.__file__).read_text(encoding="utf-8")},
        })
        yield image, claim_template, fixture_configmap
    finally:
        # Pods must stop before their mounted script and GPU template disappear.
        # The module's run fixture repeats this idempotent, scoped cleanup.
        snap.cleanup_snapshotjob(config, run)
        for delete in (
            lambda: client.CoreV1Api().delete_namespaced_config_map(fixture_configmap, config.namespace),
            lambda: api.delete_namespaced_custom_object(
                "resource.k8s.io", "v1", config.namespace, "resourceclaimtemplates", claim_template,
            ),
        ):
            try:
                delete()
            except ApiException as exc:
                if exc.status != 404:
                    raise


def _helper_root(snapshot_job_uid: str) -> str:
    assert str(uuid.UUID(snapshot_job_uid)) == snapshot_job_uid
    return f"{workloads.CHECKPOINT_DIR}/helper-artifacts/{snapshot_job_uid}"


def _helper_root_exists(config: k8s.E2EConfig, node: str, uid: str) -> bool:
    output = k8s.exec_payload(
        config.namespace, snap.checkpoint_agent_pod(config, node),
        f"if [ -d {shlex.quote(_helper_root(uid))} ]; then printf present; else printf absent; fi",
    )
    assert output in {"present", "absent"}, output
    return output == "present"


def _published_gms_manifest(config: k8s.E2EConfig, node: str, uid: str) -> dict[str, Any]:
    path = f"{_helper_root(uid)}/gms/device-0/manifest.json"
    output = k8s.exec_payload(
        config.namespace, snap.checkpoint_agent_pod(config, node), f"cat {shlex.quote(path)}",
    )
    manifest = json.loads(output)
    assert manifest["allocations"], "must be an actual committed CUDA allocation artifact"
    return manifest


def _release_gms_saver(config: k8s.E2EConfig, pod_name: str) -> None:
    k8s.exec_command(
        config.namespace, pod_name, f"touch {workloads.GMS_HANDSHAKE_DIR}/release", container="gms-saver",
    )


def _restart_isolated_operator(config: k8s.E2EConfig) -> None:
    assert os.environ.get("SNAPSHOT_E2E_ALLOW_OPERATOR_RESTART") == "1"
    pods = k8s.list_snapshot_pods(config.namespace, config.release, "operator")
    assert len(pods) == 1, "qualification requires exactly one isolated operator replica"
    old = pods[0]
    client.CoreV1Api().delete_namespaced_pod(
        old.metadata.name, config.namespace,
        body=client.V1DeleteOptions(preconditions=client.V1Preconditions(uid=old.metadata.uid)),
    )
    def replacement():
        pods = k8s.list_snapshot_pods(config.namespace, config.release, "operator")
        return next((pod for pod in pods if pod.metadata.uid != old.metadata.uid and k8s.pod_containers_ready(pod)), None)
    snap.wait_for("replacement isolated Snapshot operator Ready", replacement, 300, poll_interval=1)


def _deployed_ghost_limit(config: k8s.E2EConfig) -> int:
    selector = f"app.kubernetes.io/name=snapshot,app.kubernetes.io/instance={config.release}"
    maps = client.CoreV1Api().list_namespaced_config_map(config.namespace, label_selector=selector).items
    configs = [yaml.safe_load(item.data["config.yaml"]) for item in maps if "config.yaml" in (item.data or {})]
    assert len(configs) == 1, "must identify the installed agent configuration without guessing"
    limit = configs[0]["criu"]["ghostLimit"]
    # Helm's numeric JSON/YAML rendering can express an integer as an exponent.
    assert type(limit) in {int, float} and 0 < limit < 2**32
    assert int(limit) == limit, "CRIU ghost limit must be an exact integer"
    return int(limit)


def _gms_phase(logs: str, phase: str) -> dict[str, Any]:
    for line in logs.splitlines():
        try:
            value = json.loads(line)
        except json.JSONDecodeError:
            continue
        if isinstance(value, dict) and value.get("phase") == phase:
            return value
    raise AssertionError(f"missing real GMS worker phase {phase!r}")


def _gms_fixture_log_evidence(logs: str, source_token: str) -> list[dict[str, Any]]:
    """Keep validated fixture records, never arbitrary native logs/tracebacks."""
    records = []
    for line in logs.splitlines():
        try:
            value = json.loads(line)
        except json.JSONDecodeError:
            continue
        if not isinstance(value, dict):
            continue
        phase = value.get("phase")
        if not isinstance(phase, str):
            continue
        if phase == "saver-published":
            records.append({"phase": phase})
        elif (
            phase == "gpu-gated"
            and isinstance(value.get("gate"), str)
            and value["gate"] in {gms_workload.SERVER_GATE, gms_workload.WORKER_GATE}
            and re.fullmatch(r"GPU-[0-9a-fA-F-]{36}", str(value.get("uuid", "")))
        ):
            records.append({key: value[key] for key in ("phase", "gate", "uuid")})
        elif (
            phase in {"quiesced", "restored"}
            and value.get("token") == source_token
            and type(value.get("bytes")) is int
            and value["bytes"] > 0
            and re.fullmatch(r"[0-9a-f]{64}", str(value.get("sha256", "")))
        ):
            records.append(
                {key: value[key] for key in ("phase", "token", "bytes", "sha256")}
            )
    return records


def _allocated_gpu_uuid(pod: dict[str, Any], claim: dict[str, Any], slices: list[dict[str, Any]]) -> str:
    """Resolve the actual bound DRA device, never infer UUID from gpu-N."""
    bindings = [
        item
        for item in pod["status"].get("resourceClaimStatuses", [])
        if item["name"] == workloads.GMS_CLAIM
    ]
    assert (
        len(bindings) == 1
        and bindings[0]["resourceClaimName"] == claim["metadata"]["name"]
    )
    assert claim["metadata"]["namespace"] == pod["metadata"]["namespace"]
    assert any(
        owner.get("controller") is True
        and owner.get("kind") == "Pod"
        and owner.get("name") == pod["metadata"]["name"]
        and owner.get("uid") == pod["metadata"]["uid"]
        for owner in claim["metadata"].get("ownerReferences", [])
    ), "generated DRA claim belongs to a different Pod generation"
    assert any(
        item.get("uid") == pod["metadata"]["uid"]
        and item.get("name") == pod["metadata"]["name"]
        and item.get("resource") == "pods"
        for item in claim["status"].get("reservedFor", [])
    ), "DRA claim must be reserved for this exact Pod UID"
    results = claim["status"]["allocation"]["devices"]["results"]
    assert len(results) == 1 and results[0]["driver"] == "gpu.nvidia.com", results
    result = results[0]
    assert result["request"] == "gpus" and not result.get("adminAccess", False), result
    pool = [
        item["spec"]
        for item in slices
        if item["spec"]["driver"] == result["driver"]
        and item["spec"]["pool"]["name"] == result["pool"]
    ]
    assert pool and len({item["pool"]["generation"] for item in pool}) == 1, (
        "ambiguous DRA pool generation"
    )
    assert all(item.get("nodeName") == pod["spec"]["nodeName"] for item in pool), (
        "DRA allocation is on a different node"
    )
    devices = [
        device
        for item in pool
        for device in item["devices"]
        if device["name"] == result["device"]
    ]
    assert len(devices) == 1, (
        "DRA device must resolve uniquely in the current ResourceSlice pool"
    )
    value = devices[0]["attributes"]["uuid"]["string"]
    assert re.fullmatch(r"GPU-[0-9a-fA-F-]{36}", value), value
    return value


def _physical_gms_pod(virtual: dict[str, Any], pods: list[dict[str, Any]], namespace: str) -> dict[str, Any]:
    """Bind physical DRA evidence to this exact logical Pod, never a name suffix."""
    identity = virtual["metadata"]
    assert all(identity.get(key) for key in ("name", "namespace", "uid"))
    matches = [
        pod for pod in pods
        if all(
            pod["metadata"].get("annotations", {}).get(f"vcluster.loft.sh/object-{key}") == identity[key]
            for key in ("name", "namespace", "uid")
        )
    ]
    assert len(matches) == 1, "vCluster Pod mapping is absent or ambiguous"
    physical = matches[0]
    metadata = physical["metadata"]
    annotations = metadata["annotations"]
    assert metadata.get("uid") and metadata["namespace"] == namespace
    assert annotations.get("vcluster.loft.sh/object-kind") == "/v1, Kind=Pod"
    assert annotations.get("vcluster.loft.sh/object-host-name") == metadata["name"]
    assert annotations.get("vcluster.loft.sh/object-host-namespace") == namespace
    assert virtual["spec"].get("nodeName") and physical["spec"].get("nodeName") == virtual["spec"]["nodeName"]
    # Host-generated claims have host Pod UIDs. Match the real container ID
    # too, so stale/spoofed translation metadata cannot admit another writer.
    def server_id(pod):
        statuses = pod["status"].get("initContainerStatuses", [])
        ids = [s.get("containerID") for s in statuses if s["name"] == "gms-server"]
        assert len(ids) == 1 and re.fullmatch(r"containerd://[0-9a-f]{64}", ids[0] or "")
        return ids[0]

    assert server_id(physical) == server_id(virtual), "translated server container generation differs"
    return physical


def _gms_dra_view(config: k8s.E2EConfig, pod: dict[str, Any]):
    """Use the authoritative allocation API for the configured deployment mode."""
    assert pod["metadata"]["namespace"] == config.namespace
    mode = os.environ.get("SNAPSHOT_E2E_MODE", "direct")
    assert mode in {"direct", "vcluster"}
    if mode == "direct":
        return client.CustomObjectsApi(), pod, config.namespace
    # Reuse the existing documented host inputs; never fall back to whichever
    # kubeconfig happens to be current or fabricate virtual ResourceClaims.
    path = os.environ.get("SNAPSHOT_E2E_HOST_KUBECONFIG")
    namespace = os.environ.get("SNAPSHOT_E2E_HOST_NAMESPACE")
    assert path and namespace, "vCluster GPU qualification requires explicit host kubeconfig and namespace"
    host = kube_config.new_client_from_config(
        config_file=str(write_normalized_kubeconfig(path, None))
    )
    pods = host.sanitize_for_serialization(
        client.CoreV1Api(host).list_namespaced_pod(namespace)
    )["items"]
    physical = _physical_gms_pod(pod, pods, namespace)
    return client.CustomObjectsApi(host), physical, namespace


def _gms_claim_released(api, namespace: str, name: str, uid: str):
    try:
        current = api.get_namespaced_custom_object(
            "resource.k8s.io", "v1", namespace, "resourceclaims", name
        )
    except ApiException as exc:
        if exc.status == 404:
            return True
        raise
    assert current["metadata"]["uid"] == uid, "DRA claim name was reused by another generation"
    return None


def _gpu_process_ids(xml: str, gpu_uuid: str) -> list[int]:
    """NVML's XML inventory includes both compute and graphics consumers."""
    gpus = ET.fromstring(xml).findall("gpu")
    assert len(gpus) == 1 and gpus[0].findtext("uuid") == gpu_uuid, (
        "NVML inventory UUID mismatch"
    )
    processes = gpus[0].find("processes")
    assert processes is not None, "NVML process inventory is unavailable"
    assert not (processes.text or "").strip(), (
        "NVML process inventory reports unavailable data"
    )
    pids = [int(item.findtext("pid", "")) for item in processes.findall("process_info")]
    assert all(pid > 0 for pid in pids) and len(pids) == len(set(pids)), (
        "invalid NVML PID inventory"
    )
    return pids


def _assert_owned_gpu_process(cgroup: str, container_ids: set[str]) -> None:
    ids = set(re.findall(r"(?<![0-9a-f])[0-9a-f]{64}(?![0-9a-f])", cgroup))
    assert len(ids) == 1 and ids <= container_ids, (
        "physical GPU has an unrelated or unattributable NVML consumer; refusing allocation/capture"
    )


def _gpu_guard(config: k8s.E2EConfig, pod: client.V1Pod, gpu_uuid: str):
    statuses = (pod.status.container_statuses or []) + (
        pod.status.init_container_statuses or []
    )
    container_ids = {
        status.container_id.split("://", 1)[-1]
        for status in statuses
        if status.container_id
    }
    assert container_ids and all(
        re.fullmatch(r"[0-9a-f]{64}", value) for value in container_ids
    )
    agent = snap.checkpoint_agent_pod(config, pod.spec.node_name)

    def check():
        xml = k8s.exec_payload(
            config.namespace,
            agent,
            f"nvidia-smi --id={shlex.quote(gpu_uuid)} --query --xml-format",
        )
        pids = _gpu_process_ids(xml, gpu_uuid)
        for pid in pids:
            # /host/proc is the existing agent's read-only host proc mount. A
            # disappeared/unreadable PID fails closed, rather than hiding it.
            cgroup = k8s.exec_payload(
                config.namespace, agent, f"cat /host/proc/{pid}/cgroup"
            )
            _assert_owned_gpu_process(cgroup, container_ids)
        return pids

    return check


def _admit_gms_pod(config: k8s.E2EConfig, pod_name: str, *, source: bool):
    def server_waiting():
        pod = k8s.read_pod(config.namespace, pod_name)
        statuses = pod.status.init_container_statuses or []
        return (
            pod
            if any(s.name == "gms-server" and s.state.running for s in statuses)
            else None
        )

    pod = snap.wait_for(
        "GMS server waiting before CUDA initialization",
        server_waiting,
        300,
        poll_interval=1,
    )
    raw = client.ApiClient().sanitize_for_serialization(pod)
    api, allocation_pod, allocation_namespace = _gms_dra_view(config, raw)
    bindings = [
        item
        for item in allocation_pod["status"].get("resourceClaimStatuses", [])
        if item["name"] == workloads.GMS_CLAIM
    ]
    assert len(bindings) == 1, "Pod must expose its exact generated DRA claim binding"
    claim = api.get_namespaced_custom_object(
        "resource.k8s.io",
        "v1",
        allocation_namespace,
        "resourceclaims",
        bindings[0]["resourceClaimName"],
    )
    slices = api.list_cluster_custom_object("resource.k8s.io", "v1", "resourceslices")[
        "items"
    ]
    gpu_uuid = _allocated_gpu_uuid(allocation_pod, claim, slices)

    def approve(container: str, gate: str) -> None:
        current = k8s.read_pod(config.namespace, pod_name)
        assert current.metadata.uid == raw["metadata"]["uid"], "GPU admission Pod generation changed"
        path = f"{workloads.GMS_HANDSHAKE_DIR}/{gate}"
        waiting = snap.wait_for(
            f"{container} reached its pre-CUDA GPU gate",
            lambda: (
                k8s.exec_payload(
                    config.namespace,
                    pod_name,
                    f"if [ -f {path}.waiting ]; then cat {path}.waiting; fi",
                    container=container,
                )
                or None
            ),
            60,
            poll_interval=1,
        )
        assert waiting.strip() == gpu_uuid, (
            "container visibility differs from its exact DRA allocation"
        )
        output = k8s.exec_payload(
            config.namespace,
            pod_name,
            f"printf '%s' {shlex.quote(gpu_uuid)} > {path}; cat {path}",
            container=container,
        )
        assert output.strip() == gpu_uuid

    guard = _gpu_guard(config, pod, gpu_uuid)
    guard()
    approve("gms-server", gms_workload.SERVER_GATE)

    def all_running():
        guard()
        current = k8s.read_pod(config.namespace, pod_name)
        assert current.metadata.uid == raw["metadata"]["uid"], "GPU admission Pod generation changed"
        statuses = (current.status.container_statuses or []) + (
            current.status.init_container_statuses or []
        )
        running = {s.name for s in statuses if s.state.running}
        return (
            current
            if {workloads.CONTAINER, "gms-saver", "gms-server"} <= running
            else None
        )

    if source:
        pod = snap.wait_for(
            "worker, server and saver running on one physical GPU",
            all_running,
            600,
            poll_interval=1,
        )
        for container in (workloads.CONTAINER, "gms-server", "gms-saver"):
            visible = k8s.exec_payload(
                config.namespace,
                pod_name,
                "nvidia-smi --query-gpu=uuid --format=csv,noheader",
                container=container,
            )
            assert visible.strip() == gpu_uuid, (
                f"{container} is not on the DRA-assigned GPU"
            )
        guard = _gpu_guard(config, pod, gpu_uuid)
        guard()
        approve(workloads.CONTAINER, gms_workload.WORKER_GATE)
    print(
        f"physical GPU admission: {json.dumps({'pod_uid': pod.metadata.uid, 'allocation_pod_uid': allocation_pod['metadata']['uid'], 'claim_uid': claim['metadata']['uid'], 'uuid': gpu_uuid})}",
        flush=True,
    )
    # Freeze the proven claim generation before terminal-Pod reconciliation
    # can release its reservation/allocation or remove it altogether.
    claim_name, claim_uid = claim["metadata"]["name"], claim["metadata"]["uid"]
    return gpu_uuid, guard, lambda: _gms_claim_released(
        api, allocation_namespace, claim_name, claim_uid
    )


def _wait_gms_condition(
    config: k8s.E2EConfig, run: snap.TestRun, condition_type: str, guard
):
    def check():
        guard()
        value = snap.get_custom_object(
            client.CustomObjectsApi(),
            config.namespace,
            run.snapshotjob_name,
            snap.SNAPSHOTJOBS,
        )
        found = snap.condition(value, condition_type)
        return value if found and found.get("status") == "True" else None

    return snap.wait_for(
        f"GMS SnapshotJob {condition_type} with isolated NVML consumers",
        check,
        600,
        poll_interval=1,
    )


def _gms_artifact_hashes(config: k8s.E2EConfig, node: str, uid: str) -> str:
    root = shlex.quote(_helper_root(uid))
    output = k8s.exec_payload(
        config.namespace,
        snap.checkpoint_agent_pod(config, node),
        f"find {root} -type f -exec sha256sum {{}} + | LC_ALL=C sort",
    )
    assert output.strip() and all(
        re.match(r"^[0-9a-f]{64}  /", line) for line in output.splitlines()
    ), output
    return output


def _failed_gms_evidence(
    sj: dict[str, Any] | None,
    pod: client.V1Pod | None,
    job: client.V1Job | None,
    labels: dict[str, str],
) -> dict[str, Any]:
    """Export lifecycle evidence without admission annotations or credentials."""
    serialize = client.ApiClient().sanitize_for_serialization
    pod_value, job_value = serialize(pod) or {}, serialize(job) or {}

    def identity(value):
        meta = value.get("metadata") or {}
        return {
            **{key: meta.get(key) for key in ("name", "namespace", "uid")},
            "ownerReferences": [
                {
                    key: owner.get(key)
                    for key in ("apiVersion", "kind", "name", "uid", "controller")
                }
                for owner in meta.get("ownerReferences", [])
            ],
        }

    def conditions(value):
        return [
            {
                key: item.get(key)
                for key in ("type", "status", "reason", "lastTransitionTime")
            }
            for item in (value.get("status") or {}).get("conditions", []) or []
        ]

    containers = []
    for field in (
        "containerStatuses",
        "initContainerStatuses",
        "ephemeralContainerStatuses",
    ):
        for status in (pod_value.get("status") or {}).get(field, []) or []:
            state = status.get("state") or {}
            containers.append(
                {
                    **{
                        key: status.get(key)
                        for key in ("name", "containerID", "restartCount")
                    },
                    "state": {
                        kind: {
                            key: detail.get(key)
                            for key in (
                                "exitCode",
                                "signal",
                                "reason",
                                "startedAt",
                                "finishedAt",
                            )
                            if key in detail
                        }
                        for kind, detail in state.items()
                        if detail is not None
                    },
                }
            )
    return {
        "knownTestLabels": labels,
        "snapshotJob": {**identity(sj or {}), "conditions": conditions(sj or {})},
        "job": {
            **identity(job_value),
            "backoffLimit": getattr(getattr(job, "spec", None), "backoff_limit", None),
            "conditions": conditions(job_value),
            "status": {
                key: (job_value.get("status") or {}).get(key)
                for key in ("active", "failed", "succeeded")
            },
        },
        "pod": {
            **identity(pod_value),
            "nodeName": getattr(getattr(pod, "spec", None), "node_name", None),
            "phase": getattr(getattr(pod, "status", None), "phase", None),
            "conditions": conditions(pod_value),
            "containers": containers,
        },
    }


def _debug_gms_snapshotjob(config: k8s.E2EConfig, run: snap.TestRun) -> None:
    """GMS failures must not use the generic annotation/free-form debug dump."""

    def read(description, callback):
        try:
            return callback()
        except Exception as exc:  # noqa: BLE001 - diagnostics must preserve the original failure
            # API error bodies, headers and exception messages are untrusted.
            http_status = (
                exc.status
                if isinstance(exc, ApiException) and type(exc.status) is int
                else None
            )
            print(
                f"GMS diagnostic unavailable: {description}; error_type={type(exc).__name__}; http_status={http_status}",
                flush=True,
            )
            return None

    api = client.CustomObjectsApi()
    core = client.CoreV1Api()
    sj = read(
        "SnapshotJob",
        lambda: snap.get_custom_object(
            api, config.namespace, run.snapshotjob_name, snap.SNAPSHOTJOBS
        ),
    )
    job = read(
        "source Job", lambda: k8s.read_job(config.namespace, run.snapshotjob_name)
    )
    labeled = (
        read(
            "run-labeled Pods",
            lambda: (
                core.list_namespaced_pod(
                    config.namespace, label_selector=f"snapshot-e2e-test={run.suffix}"
                ).items
            ),
        )
        or []
    )
    source = (
        read(
            "source Pods",
            lambda: k8s.list_job_pods(config.namespace, run.snapshotjob_name),
        )
        or []
    )
    restored = read(
        "restore Pod", lambda: k8s.read_pod(config.namespace, run.restore_pod)
    )
    pods = {
        pod.metadata.uid: pod
        for pod in [*labeled, *source, *([restored] if restored else [])]
    }
    for pod in list(pods.values()) or [None]:
        value = read(
            "safe lifecycle evidence",
            lambda: _failed_gms_evidence(sj, pod, job, run.labels),
        )
        if value is not None:
            if pod is not None:
                names = {
                    container.name
                    for container in (pod.spec.containers or [])
                    + (pod.spec.init_containers or [])
                }
                value["fixtureLogs"] = {}
                for container in (workloads.CONTAINER, "gms-saver", "gms-server"):
                    if container in names:
                        logs = read(
                            f"{container} fixture logs",
                            lambda: k8s.pod_logs(
                                config.namespace, pod.metadata.name, container=container
                            ),
                        )
                        if logs is not None:
                            value["fixtureLogs"][container] = _gms_fixture_log_evidence(
                                logs, run.source_token
                            )
            print("GMS lifecycle diagnostic: " + json.dumps(value), flush=True)


def _create_completed_gms(config, run, image, claim_template, fixture_configmap):
    created = snap.create_snapshotjob(
        config.namespace,
        run.snapshotjob_name,
        workloads.gms_snapshotjob_pod_template(
            config=config,
            run=run,
            image=image,
            claim_template=claim_template,
            fixture_configmap=fixture_configmap,
        ),
        active_deadline_seconds=1800,
    )
    _assert_gms_opt_in_persisted(created)
    uid = created["metadata"]["uid"]
    source = snap.wait_for_job_source_pod(config.namespace, run.snapshotjob_name)
    _, guard, _ = _admit_gms_pod(config, source.metadata.name, source=True)
    captured = _wait_gms_condition(config, run, "Captured", guard)
    source = k8s.read_pod(config.namespace, source.metadata.name)
    node = source.spec.node_name
    state = _gms_phase(
        k8s.pod_logs(
            config.namespace, source.metadata.name, container=workloads.CONTAINER
        ),
        "quiesced",
    )
    manifest = _published_gms_manifest(config, node, uid)
    assert manifest["allocations"][0]["allocation_id"] == state["mappings"][0][0]
    _release_gms_saver(config, source.metadata.name)
    completed = _wait_gms_condition(config, run, "Completed", guard)
    assert_snapshotjob_completed(completed)
    ps, _ = snap.wait_for_snapshot_ready(
        config.namespace, captured["status"]["podSnapshotName"], timeout=60
    )
    assert ps["metadata"]["labels"]["nvidia.com/snapshot-job-uid"] == uid
    snap.wait_for_pod_deleted(config.namespace, source.metadata.name)
    return uid, node, ps, state, _gms_artifact_hashes(config, node, uid)


def _assert_gms_opt_in_persisted(
    snapshot_job: dict[str, Any], *, on_failure_policy: str | None = None
) -> None:
    metadata = snapshot_job["spec"]["podTemplate"].get("metadata", {})
    assert metadata.get("annotations", {}).get(workloads.GMS_HELPER_ANNOTATION) == "gms-saver", (
        "SnapshotJob API pruned or changed the helper opt-in; install the matching generated CRD"
    )
    if on_failure_policy is not None:
        assert snapshot_job["spec"].get("onFailurePolicy") == on_failure_policy, (
            "SnapshotJob API pruned or changed the failure policy; install the matching generated CRD"
        )


def _restore_completed_gms(
    config, run, image, claim_template, fixture_configmap, uid, node, ps, source_state
):
    k8s.create_pod(
        workloads.gms_restore_pod(
            config=config,
            run=run,
            image=image,
            claim_template=claim_template,
            fixture_configmap=fixture_configmap,
            snapshot_name=ps["metadata"]["name"],
            snapshot_job_uid=uid,
            source_node=node,
        )
    )
    gpu_uuid, _, _ = _admit_gms_pod(config, run.restore_pod, source=False)

    # The loader's server was admitted before allocating any saved weights.
    # Refresh IDs once CRIU has replaced the inert target container.
    def restore_ready():
        current = k8s.read_pod(config.namespace, run.restore_pod)
        _gpu_guard(config, current, gpu_uuid)()
        condition = snap.pod_condition(current, "nvidia.com/Restored")
        return (
            current
            if condition
            and condition.status == "True"
            and condition.reason == "RestoreSucceeded"
            else None
        )

    snap.wait_for(
        "canary restored with only owned physical GPU consumers",
        restore_ready,
        600,
        poll_interval=1,
    )
    pod = k8s.read_pod(config.namespace, run.restore_pod)
    guard = _gpu_guard(config, pod, gpu_uuid)
    for container in (workloads.CONTAINER, "gms-server"):
        visible = k8s.exec_payload(
            config.namespace,
            run.restore_pod,
            "nvidia-smi --query-gpu=uuid --format=csv,noheader",
            container=container,
        )
        assert visible.strip() == gpu_uuid, (
            f"restored {container} is not on its DRA GPU"
        )

    def restored_result():
        guard()
        output = k8s.exec_payload(
            config.namespace,
            run.restore_pod,
            f"if [ -f {gms_workload.RESTORED_RESULT} ]; then cat {gms_workload.RESTORED_RESULT}; fi",
            container=workloads.CONTAINER,
        )
        return json.loads(output) if output.strip() else None

    result = snap.wait_for(
        "restored GMS worker's exact allocation/VA/CUDA byte verification",
        restored_result,
        300,
        poll_interval=1,
    )
    assert result["phase"] == "restored"
    for field in ("token", "mappings", "bytes", "sha256"):
        assert result[field] == source_state[field], field
    assert result["token"] == run.source_token
    assert (
        result["sha256"]
        == hashlib.sha256(
            gms_workload.weight_bytes(run.source_token, result["bytes"])
        ).hexdigest()
    )
    assert _helper_root_exists(config, node, uid)
    print(f"real CUDA GMS restore verified: {json.dumps(result)}", flush=True)


@pytest.mark.snapshot_failure
@pytest.mark.gpu
def test_snapshotjob_gms_capture_failure_reclaims_after_operator_restart(
    config: k8s.E2EConfig,
    run: snap.TestRun,
    gms_fixture,
) -> None:
    image, claim_template, fixture_configmap = gms_fixture
    canary = snap.TestRun.new("gms-completed-canary")
    try:
        # Keep a genuinely Ready/Completed restore point alongside the failure,
        # not only in another pytest case that cannot see this cleanup sweep.
        canary_uid, canary_node, canary_ps, canary_state, canary_hashes = (
            _create_completed_gms(
                config,
                canary,
                image,
                claim_template,
                fixture_configmap,
            )
        )

        def assert_canary_retained():
            completed = snap.get_custom_object(
                client.CustomObjectsApi(),
                config.namespace,
                canary.snapshotjob_name,
                snap.SNAPSHOTJOBS,
            )
            assert completed["metadata"]["uid"] == canary_uid
            assert_snapshotjob_completed(completed)
            api = client.CustomObjectsApi()
            ps = api.get_namespaced_custom_object(
                snap.GROUP,
                snap.VERSION,
                config.namespace,
                snap.PODSNAPSHOTS,
                canary_ps["metadata"]["name"],
            )
            assert ps["metadata"]["uid"] == canary_ps["metadata"]["uid"]
            content = api.get_cluster_custom_object(
                snap.GROUP,
                snap.VERSION,
                snap.PODSNAPSHOTCONTENTS,
                ps["status"]["boundSnapshotContentName"],
            )
            assert snap.condition(ps, "Ready")["status"] == "True"
            assert snap.condition(content, "Ready")["status"] == "True"
            assert (
                _gms_artifact_hashes(config, canary_node, canary_uid) == canary_hashes
            )

        created = snap.create_snapshotjob(
            config.namespace,
            run.snapshotjob_name,
            workloads.gms_snapshotjob_pod_template(
                config=config,
                run=run,
                image=image,
                claim_template=claim_template,
                fixture_configmap=fixture_configmap,
                failure_ghost_size=_deployed_ghost_limit(config) + 1,
            ),
            active_deadline_seconds=1800,
            on_failure_policy="CleanupHelpers",
        )
        _assert_gms_opt_in_persisted(created, on_failure_policy="CleanupHelpers")
        uid = created["metadata"]["uid"]
        assert uid != canary_uid
        source = snap.wait_for_job_source_pod(config.namespace, run.snapshotjob_name)
        _, guard, claim_released = _admit_gms_pod(config, source.metadata.name, source=True)
        sj = _wait_gms_condition(config, run, "Failed", guard)
        snap.assert_snapshotjob_failure_vector(sj, allowed_reasons={"CaptureFailed"})
        source = k8s.read_pod(config.namespace, source.metadata.name)
        node = source.spec.node_name
        manifest = _published_gms_manifest(config, node, uid)
        source_state = _gms_phase(
            k8s.pod_logs(
                config.namespace, source.metadata.name, container=workloads.CONTAINER
            ),
            "quiesced",
        )
        assert (
            manifest["allocations"][0]["allocation_id"]
            == source_state["mappings"][0][0]
        )
        statuses = {status.name: status for status in source.status.container_statuses}
        assert statuses["gms-saver"].state.running is not None
        assert _helper_root_exists(config, node, uid)
        failed_hashes = _gms_artifact_hashes(config, node, uid)

        ps = client.CustomObjectsApi().get_namespaced_custom_object(
            snap.GROUP,
            snap.VERSION,
            config.namespace,
            snap.PODSNAPSHOTS,
            sj["status"]["podSnapshotName"],
        )
        content = client.CustomObjectsApi().get_cluster_custom_object(
            snap.GROUP,
            snap.VERSION,
            snap.PODSNAPSHOTCONTENTS,
            ps["status"]["boundSnapshotContentName"],
        )
        content_uid = content["metadata"]["uid"]
        logs = k8s.pod_logs(
            config.namespace, snap.checkpoint_agent_pod(config, node), tail_lines=1000
        )
        evidence = [
            line
            for line in logs.splitlines()
            if content_uid in line
            and "CRIU dump failed" in line
            and "ghost" in line.lower()
        ]
        assert evidence and any("limit" in line.lower() for line in evidence), (
            "must prove the real CRIU ghost-limit failure for this capture, not only Failed status"
        )
        print(f"real GMS publication: {json.dumps(manifest)}", flush=True)
        print("real CRIU failure: " + "\n".join(evidence), flush=True)

        _restart_isolated_operator(config)
        # Startup and periodic sweeps must retain even a fully published root
        # while its declared saver process is still active.
        for _ in range(3):
            guard()
            assert _gms_artifact_hashes(config, node, uid) == failed_hashes
            active = k8s.read_pod(config.namespace, source.metadata.name)
            assert next(
                s for s in active.status.container_statuses if s.name == "gms-saver"
            ).state.running
            assert_canary_retained()
            time.sleep(5)
        guard()
        assert _gms_artifact_hashes(config, node, uid) == failed_hashes
        _release_gms_saver(config, source.metadata.name)
        snap.wait_for(
            "failed source Pod and native GMS sidecar terminated",
            lambda: (
                True
                if k8s.read_pod(config.namespace, source.metadata.name).status.phase
                in snap.TERMINAL_POD_PHASES
                else None
            ),
            120,
            poll_interval=1,
        )

        def reclaimed():
            guard()
            assert_canary_retained()
            return True if not _helper_root_exists(config, node, uid) else None

        snap.wait_for(
            "failed attempt's real GMS artifact reclaimed",
            reclaimed,
            120,
            poll_interval=1,
        )
        assert k8s.read_job(config.namespace, run.snapshotjob_name) is not None
        current = snap.get_custom_object(
            client.CustomObjectsApi(),
            config.namespace,
            run.snapshotjob_name,
            snap.SNAPSHOTJOBS,
        )
        assert snap.condition(current, "Failed") == snap.condition(sj, "Failed")
        snap.wait_for(
            "UID-scoped helper reclamation event",
            lambda: next(
                (
                    event
                    for event in k8s.list_events(
                        config.namespace, field_selector={"involvedObject.uid": uid}
                    )
                    if event.reason == "HelperArtifactsReclaimed"
                ),
                None,
            ),
            60,
            poll_interval=1,
        )
        assert_canary_retained()
        # Terminal Pods can keep their generated DRA claim reserved. Export
        # the retained evidence after every rollback assertion, then release
        # only this failed Pod so the canary restore needs no second GPU.
        failed_pod = k8s.read_pod(config.namespace, source.metadata.name)

        def terminal_job():
            value = k8s.read_job(config.namespace, run.snapshotjob_name)
            return (
                value
                if value
                and any(
                    condition.type == "Failed" and condition.status == "True"
                    for condition in value.status.conditions or []
                )
                else None
            )

        failed_job = snap.wait_for(
            "failed source Job terminal before GPU release",
            terminal_job,
            120,
            poll_interval=1,
        )
        assert failed_job is not None and failed_job.spec.backoff_limit == 0
        assert current["metadata"]["uid"] == uid
        assert failed_pod.metadata.uid == source.metadata.uid
        assert any(owner.controller and owner.kind == "Job" and owner.uid == failed_job.metadata.uid
            for owner in failed_pod.metadata.owner_references or []), "failed Pod belongs to a different Job generation"
        assert any(owner.controller and owner.kind == "SnapshotJob" and owner.uid == uid
            for owner in failed_job.metadata.owner_references or []), "failed Job belongs to a different checkpoint generation"
        assert failed_pod.status.phase in snap.TERMINAL_POD_PHASES
        statuses = (
            (failed_pod.status.container_statuses or [])
            + (failed_pod.status.init_container_statuses or [])
            + (failed_pod.status.ephemeral_container_statuses or [])
        )
        assert statuses and all(status.state.terminated for status in statuses)
        assert any(
            condition.type == "Failed" and condition.status == "True"
            for condition in failed_job.status.conditions or []
        )
        print(
            "retained failed-checkpoint evidence before scoped GPU release: "
            + json.dumps(
                {
                    **_failed_gms_evidence(current, failed_pod, failed_job, run.labels),
                    "workerLogs": _gms_fixture_log_evidence(k8s.pod_logs(
                        config.namespace,
                        source.metadata.name,
                        container=workloads.CONTAINER,
                    ), run.source_token),
                    "saverLogs": _gms_fixture_log_evidence(k8s.pod_logs(
                        config.namespace, source.metadata.name, container="gms-saver"
                    ), run.source_token),
                    "captureFailure": evidence,
                }
            ),
            flush=True,
        )
        client.CoreV1Api().delete_namespaced_pod(
            source.metadata.name,
            config.namespace,
            body=client.V1DeleteOptions(
                preconditions=client.V1Preconditions(uid=failed_pod.metadata.uid)
            ),
        )
        snap.wait_for_pod_deleted(config.namespace, source.metadata.name)

        snap.wait_for(
            "failed source's generated DRA claim released",
            claim_released,
            180,
            poll_interval=1,
        )
        assert not k8s.list_job_pods(config.namespace, run.snapshotjob_name), (
            "terminal backoff-zero Job must not create a replacement writer"
        )
        _restore_completed_gms(
            config,
            canary,
            image,
            claim_template,
            fixture_configmap,
            canary_uid,
            canary_node,
            canary_ps,
            canary_state,
        )
        assert_canary_retained()
    except Exception:
        _debug_gms_snapshotjob(config, run)
        _debug_gms_snapshotjob(config, canary)
        raise
    finally:
        snap.cleanup_snapshotjob(config, canary)


@pytest.mark.snapshot_success
@pytest.mark.gpu
def test_snapshotjob_gms_completed_artifact_survives_job_deletion_and_restore(
    config: k8s.E2EConfig,
    run: snap.TestRun,
    gms_fixture,
) -> None:
    image, claim_template, fixture_configmap = gms_fixture
    try:
        uid, node, ps, source_state, hashes = _create_completed_gms(
            config, run, image, claim_template, fixture_configmap
        )
        client.CustomObjectsApi().delete_namespaced_custom_object(
            snap.GROUP,
            snap.VERSION,
            config.namespace,
            snap.SNAPSHOTJOBS,
            run.snapshotjob_name,
        )
        snap.wait_for_custom_object_deleted(
            config.namespace, run.snapshotjob_name, snap.SNAPSHOTJOBS
        )
        _restart_isolated_operator(config)
        for _ in range(3):
            assert _helper_root_exists(config, node, uid), (
                "completed helper artifact outlives SnapshotJob deletion"
            )
            assert _gms_artifact_hashes(config, node, uid) == hashes
            time.sleep(5)
        _restore_completed_gms(
            config,
            run,
            image,
            claim_template,
            fixture_configmap,
            uid,
            node,
            ps,
            source_state,
        )
        assert _gms_artifact_hashes(config, node, uid) == hashes
    except Exception:
        _debug_gms_snapshotjob(config, run)
        raise


@pytest.mark.snapshot_success
@pytest.mark.gpu
def test_snapshotjob_captures_and_restore_recovers_state(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    try:
        snapshotjob_name = run.snapshotjob_name
        snap.create_snapshotjob(
            config.namespace,
            snapshotjob_name,
            workloads.snapshotjob_pod_template(config=config, run=run, gpu=True),
        )

        # Record the source pod's name for artifact assertions, but never poll
        # its readiness or exec into it: the dump starts on readiness and kills
        # the process, so both are races the test cannot win. The workload
        # guarantees observation seq=0 exists before it signals ready.
        source_pod = snap.wait_for_job_source_pod(config.namespace, snapshotjob_name)
        source_pod_name = source_pod.metadata.name

        sj = snap.wait_for_condition(
            config.namespace,
            snapshotjob_name,
            plural=snap.SNAPSHOTJOBS,
            condition_type="Completed",
            timeout=600,
        )
        assert_snapshotjob_completed(sj)

        pod_snapshot_name = sj["status"]["podSnapshotName"]
        assert pod_snapshot_name == snapshotjob_name

        pod_snapshot, content = snap.wait_for_snapshot_ready(
            config.namespace,
            pod_snapshot_name,
            timeout=60,
        )
        assert pod_snapshot["status"]["boundSnapshotContentName"] == content["metadata"]["name"]
        assert content["spec"]["source"]["podRef"]["name"] == source_pod_name
        assert content["spec"]["source"]["podRef"]["containers"] == [workloads.CONTAINER]
        source_node = content["spec"]["source"]["nodeName"]

        # Success is the only path that deletes the source Job; the killed pod
        # goes with it. This is the intrinsic "capture success wins over the
        # source Job's failure" check — by the time Completed=True is
        # observable, the failed Job must already be on its way out.
        snap.wait_for_pod_deleted(config.namespace, source_pod_name, timeout=120)
        assert k8s.read_job(config.namespace, snapshotjob_name) is None

        k8s.create_pod(
            workloads.restore_pod(
                config=config,
                run=run,
                gpu=True,
                source_node=source_node,
                snapshot_name=pod_snapshot_name,
            )
        )
        snap.wait_for_restored_condition(
            config.namespace, run.restore_pod, "True", "RestoreSucceeded"
        )
        snap.wait_for_pod_ready(config.namespace, run.restore_pod, timeout=300)

        # Inspect the shared artifact through the snapshot agent on the source
        # node. The source pod is already gone, but the agent and PVC remain.
        # Artifacts are keyed by the PodSnapshotContent UID, not the
        # SnapshotJob name.
        content_uid = content["metadata"]["uid"]
        manifest = snap.checkpoint_artifact_manifest(
            config,
            source_node,
            content_uid,
        )
        assert "criuDump:" in manifest
        assert f"podName: {source_pod_name}" in manifest

        artifact_listing = snap.checkpoint_artifact_listing(
            config,
            source_node,
            content_uid,
        )
        assert "./inventory.img" in artifact_listing
        assert "./manifest.yaml" in artifact_listing

        # checkpoint_observations=1: the workload writes observation seq=0
        # before signalling ready, so at least one observation is guaranteed
        # in the captured state without any pre-capture polling.
        output = snap.assert_restored_state(
            config.namespace,
            run.restore_pod,
            source_token=run.source_token,
            restore_token=run.restore_token,
            checkpoint_observations=1,
            gpu=True,
        )
        assert f"source_token={run.source_token}" in output
        assert f"restore_token={run.restore_token}" in output

    except Exception:
        snap.debug_dump_snapshotjob(config, run)
        raise


@pytest.mark.snapshot_success
def test_snapshotjob_cpu_only_captures(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    # First non-GPU SnapshotJob coverage: capture and artifact only, no
    # restore round trip (that is the GPU test's job).
    try:
        snapshotjob_name = run.snapshotjob_name
        snap.create_snapshotjob(
            config.namespace,
            snapshotjob_name,
            workloads.snapshotjob_pod_template(config=config, run=run, gpu=False),
        )

        source_pod = snap.wait_for_job_source_pod(config.namespace, snapshotjob_name)
        source_pod_name = source_pod.metadata.name

        sj = snap.wait_for_condition(
            config.namespace,
            snapshotjob_name,
            plural=snap.SNAPSHOTJOBS,
            condition_type="Completed",
            timeout=600,
        )
        assert_snapshotjob_completed(sj)

        _, content = snap.wait_for_snapshot_ready(
            config.namespace,
            sj["status"]["podSnapshotName"],
            timeout=60,
        )
        source_node = content["spec"]["source"]["nodeName"]
        manifest = snap.checkpoint_artifact_manifest(
            config, source_node, content["metadata"]["uid"]
        )
        assert f"podName: {source_pod_name}" in manifest

        snap.wait_for_pod_deleted(config.namespace, source_pod_name, timeout=120)
        assert k8s.read_job(config.namespace, snapshotjob_name) is None
    except Exception:
        snap.debug_dump_snapshotjob(config, run)
        raise


@pytest.mark.snapshot_success
def test_snapshotjob_waits_for_helper_then_completes(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    # The design's GMS-saver pattern: a helper works past the capture and must
    # be allowed to finish (exit 0) before the SnapshotJob completes and the
    # Job is deleted. 20s keeps the helper alive well past the dump.
    try:
        snapshotjob_name = run.snapshotjob_name
        snap.create_snapshotjob(
            config.namespace,
            snapshotjob_name,
            workloads.snapshotjob_helper_pod_template(
                config=config,
                run=run,
                helper_command="sleep 20; echo '[helper] done'; exit 0",
            ),
        )

        sj = snap.wait_for_condition(
            config.namespace,
            snapshotjob_name,
            plural=snap.SNAPSHOTJOBS,
            condition_type="Completed",
            timeout=600,
        )
        assert_snapshotjob_completed(sj)
        assert k8s.read_job(config.namespace, snapshotjob_name) is None
    except Exception:
        snap.debug_dump_snapshotjob(config, run)
        raise


@pytest.mark.snapshot_failure
def test_snapshotjob_fails_when_helper_fails_after_capture(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    # A helper that exits non-zero after the capture is an incomplete
    # deliverable: the SnapshotJob must fail with JobFailed while Captured
    # stays True and the artifact remains usable.
    try:
        snapshotjob_name = run.snapshotjob_name
        snap.create_snapshotjob(
            config.namespace,
            snapshotjob_name,
            workloads.snapshotjob_helper_pod_template(
                config=config,
                run=run,
                helper_command="sleep 20; echo '[helper] failing'; exit 3",
            ),
        )

        sj = snap.wait_for_condition(
            config.namespace,
            snapshotjob_name,
            plural=snap.SNAPSHOTJOBS,
            condition_type="Failed",
            timeout=600,
        )
        failed = snap.condition(sj, "Failed")
        assert failed and failed.get("reason") == "JobFailed"
        assert "helper" in failed.get("message", "")
        captured = snap.condition(sj, "Captured")
        assert captured and captured.get("status") == "True", (
            "capture success must remain independently visible when a helper fails"
        )
        completed = snap.condition(sj, "Completed")
        assert completed is not None and completed.get("status") != "True"
        assert sj["status"]["completedAt"]
        assert k8s.read_job(config.namespace, snapshotjob_name) is not None

        # The artifact survives the failure: the capture itself succeeded.
        snap.wait_for_snapshot_ready(config.namespace, sj["status"]["podSnapshotName"], timeout=60)
    except Exception:
        snap.debug_dump_snapshotjob(config, run)
        raise


@pytest.mark.snapshot_failure
def test_snapshotjob_deadline_exceeded_when_helper_overruns(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    # The deadline bounds the whole source lifecycle, helpers included: a
    # helper that never exits keeps the SnapshotJob in WaitingForPodCompletion
    # until activeDeadlineSeconds fires, even though the capture succeeded.
    try:
        snapshotjob_name = run.snapshotjob_name
        snap.create_snapshotjob(
            config.namespace,
            snapshotjob_name,
            workloads.snapshotjob_helper_pod_template(
                config=config,
                run=run,
                helper_command="sleep infinity",
            ),
            active_deadline_seconds=60,
        )

        sj = snap.wait_for_condition(
            config.namespace,
            snapshotjob_name,
            plural=snap.SNAPSHOTJOBS,
            condition_type="Failed",
            timeout=300,
        )
        failed = snap.condition(sj, "Failed")
        assert failed and failed.get("reason") == "DeadlineExceeded"
        captured = snap.condition(sj, "Captured")
        assert captured and captured.get("status") == "True", (
            "the capture finished long before the deadline; only the helper overran"
        )
        assert k8s.read_job(config.namespace, snapshotjob_name) is not None
    except Exception:
        snap.debug_dump_snapshotjob(config, run)
        raise


@pytest.mark.snapshot_failure
def test_snapshotjob_deadline_exceeded_when_never_ready(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    try:
        snapshotjob_name = run.snapshotjob_name
        snap.create_snapshotjob(
            config.namespace,
            snapshotjob_name,
            workloads.snapshotjob_hang_pod_template(config=config, run=run),
            active_deadline_seconds=30,
        )

        sj = snap.wait_for_condition(
            config.namespace,
            snapshotjob_name,
            plural=snap.SNAPSHOTJOBS,
            condition_type="Failed",
            timeout=180,
        )
        # The deadline kills the never-ready pod; the raced capture failure
        # that follows is collateral, and the explicit deadline must win.
        snap.assert_snapshotjob_failure_vector(sj, allowed_reasons={"DeadlineExceeded"})

        # Failed=True preserves the source Job for debugging. The batch Job
        # controller may delete its pod when activeDeadlineSeconds expires, so
        # pod retention is not part of the SnapshotJob contract.
        assert k8s.read_job(config.namespace, snapshotjob_name) is not None
    except Exception:
        snap.debug_dump_snapshotjob(config, run)
        raise


@pytest.mark.snapshot_failure
def test_snapshotjob_deadline_exceeded_when_pod_unschedulable(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    try:
        snapshotjob_name = run.snapshotjob_name
        # Unlike the never-ready test (whose pod schedules and runs), this pod
        # never leaves Pending: the PodSnapshot reconciler backs off on the
        # unscheduled source, no PodSnapshotContent work order exists, and no
        # agent ever touches the run. The Job's activeDeadlineSeconds is the
        # only resolver, and the explicit deadline must win over the
        # collateral capture failure it causes (the deadline deletes the
        # Pending pod, failing the still-pending capture).
        snap.create_snapshotjob(
            config.namespace,
            snapshotjob_name,
            workloads.snapshotjob_unschedulable_pod_template(config=config, run=run),
            active_deadline_seconds=30,
        )

        sj = snap.wait_for_condition(
            config.namespace,
            snapshotjob_name,
            plural=snap.SNAPSHOTJOBS,
            condition_type="Failed",
            timeout=180,
        )
        snap.assert_snapshotjob_failure_vector(sj, allowed_reasons={"DeadlineExceeded"})
        running = snap.condition(sj, "Running")
        assert running and running.get("status") == "False", "the pod never ran"
        assert k8s.read_job(config.namespace, snapshotjob_name) is not None
    except Exception:
        snap.debug_dump_snapshotjob(config, run)
        raise


@pytest.mark.snapshot_failure
def test_snapshotjob_fails_on_job_name_conflict(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    snapshotjob_name = run.snapshotjob_name
    foreign_job = {
        "apiVersion": "batch/v1",
        "kind": "Job",
        "metadata": {
            "name": snapshotjob_name,
            "namespace": config.namespace,
            "labels": {**run.labels},
        },
        "spec": {
            "backoffLimit": 0,
            "template": {
                "metadata": {"labels": {**run.labels}},
                "spec": {
                    "restartPolicy": "Never",
                    "containers": [
                        {
                            "name": "sleeper",
                            "image": run.image,
                            "command": ["/bin/bash", "-lc", "sleep 300"],
                        }
                    ],
                    **workloads.workload_scheduling(),
                },
            },
        },
    }
    try:
        k8s.create_job(foreign_job)
        snap.create_snapshotjob(
            config.namespace,
            snapshotjob_name,
            workloads.snapshotjob_hang_pod_template(config=config, run=run),
        )

        sj = snap.wait_for_condition(
            config.namespace,
            snapshotjob_name,
            plural=snap.SNAPSHOTJOBS,
            condition_type="Failed",
            timeout=180,
        )
        snap.assert_snapshotjob_failure_vector(sj, allowed_reasons={"JobNameConflict"})

        # The foreign Job must be left untouched: never adopted, never deleted.
        job = k8s.read_job(config.namespace, snapshotjob_name)
        assert job is not None
        assert not (job.metadata.owner_references or [])
    except Exception:
        snap.debug_dump_snapshotjob(config, run)
        raise
    finally:
        k8s.delete_job(config.namespace, snapshotjob_name)


@pytest.mark.snapshot_failure
def test_snapshotjob_fails_on_podsnapshot_name_conflict(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    snapshotjob_name = run.snapshotjob_name
    try:
        # A pre-existing PodSnapshot at the SnapshotJob's deterministic name,
        # owned by nobody. Its own fate (it fails on a missing source pod) is
        # irrelevant — holding the name is what makes it a terminal conflict.
        snap.create_podsnapshot(
            config.namespace,
            snapshotjob_name,
            pod_name="no-such-pod",
            pod_uid="00000000-0000-0000-0000-000000000000",
        )
        snap.create_snapshotjob(
            config.namespace,
            snapshotjob_name,
            workloads.snapshotjob_hang_pod_template(config=config, run=run),
        )

        sj = snap.wait_for_condition(
            config.namespace,
            snapshotjob_name,
            plural=snap.SNAPSHOTJOBS,
            condition_type="Failed",
            timeout=180,
        )
        snap.assert_snapshotjob_failure_vector(sj, allowed_reasons={"PodSnapshotNameConflict"})
        assert k8s.read_job(config.namespace, snapshotjob_name) is not None
    except Exception:
        snap.debug_dump_snapshotjob(config, run)
        raise


@pytest.mark.snapshot_failure
def test_snapshotjob_fails_when_job_deleted(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    try:
        snapshotjob_name = run.snapshotjob_name
        # The hang workload never becomes ready, so the capture stays pending
        # forever — deleting the Job mid-capture is deterministic, not a race
        # against a dump that could complete first.
        snap.create_snapshotjob(
            config.namespace,
            snapshotjob_name,
            workloads.snapshotjob_hang_pod_template(config=config, run=run),
        )

        snap.wait_for_status_field(
            config.namespace,
            snapshotjob_name,
            plural=snap.SNAPSHOTJOBS,
            field="podSnapshotName",
        )
        assert k8s.delete_job(config.namespace, snapshotjob_name)

        sj = snap.wait_for_condition(
            config.namespace,
            snapshotjob_name,
            plural=snap.SNAPSHOTJOBS,
            condition_type="Failed",
            timeout=180,
        )
        # One-shot: a deleted Job with an unresolved capture can never
        # complete. (A deleted Job with a *Ready* capture completes instead —
        # unit-covered; not deterministically reachable in e2e.)
        snap.assert_snapshotjob_failure_vector(sj, allowed_reasons={"JobDeleted"})
    except Exception:
        snap.debug_dump_snapshotjob(config, run)
        raise


@pytest.mark.snapshot_failure
def test_snapshotjob_fails_when_workload_exits_nonzero_before_capture(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    try:
        snapshotjob_name = run.snapshotjob_name
        snap.create_snapshotjob(
            config.namespace,
            snapshotjob_name,
            workloads.snapshotjob_exit_pod_template(config=config, run=run, exit_code=1),
        )

        sj = snap.wait_for_condition(
            config.namespace,
            snapshotjob_name,
            plural=snap.SNAPSHOTJOBS,
            condition_type="Failed",
            timeout=300,
        )
        # The Job controller (JobFailed) races the capture pipeline observing
        # the same dead workload (SourcePodGone → CaptureFailed); the class is
        # deterministic, the reporting component is not.
        snap.assert_snapshotjob_failure_vector(
            sj, allowed_reasons={"JobFailed", "CaptureFailed"}
        )
        assert k8s.read_job(config.namespace, snapshotjob_name) is not None
    except Exception:
        snap.debug_dump_snapshotjob(config, run)
        raise


@pytest.mark.snapshot_failure
def test_snapshotjob_fails_when_workload_exits_zero_before_capture(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    try:
        snapshotjob_name = run.snapshotjob_name
        snap.create_snapshotjob(
            config.namespace,
            snapshotjob_name,
            workloads.snapshotjob_exit_pod_template(config=config, run=run, exit_code=0),
        )

        sj = snap.wait_for_condition(
            config.namespace,
            snapshotjob_name,
            plural=snap.SNAPSHOTJOBS,
            condition_type="Failed",
            timeout=300,
        )
        # A zero exit before capture cannot be a success: nothing was
        # captured. The operator's SourceCompletedWithoutCapture races the
        # agent's SourcePodGone (→ CaptureFailed) on the same succeeded pod.
        snap.assert_snapshotjob_failure_vector(
            sj, allowed_reasons={"SourceCompletedWithoutCapture", "CaptureFailed"}
        )
        assert k8s.read_job(config.namespace, snapshotjob_name) is not None
    except Exception:
        snap.debug_dump_snapshotjob(config, run)
        raise


@pytest.mark.snapshot_failure
def test_snapshotjob_spec_admission(
    config: k8s.E2EConfig,
    run: snap.TestRun,
) -> None:
    snapshotjob_name = run.snapshotjob_name

    # targetContainers must name containers present in podTemplate (CEL).
    with pytest.raises(ApiException) as excinfo:
        snap.create_snapshotjob(
            config.namespace,
            snapshotjob_name,
            workloads.snapshotjob_hang_pod_template(config=config, run=run),
            target_containers=["not-in-pod-template"],
        )
    assert excinfo.value.status in (400, 422)

    # spec is immutable (self == oldSelf).
    snap.create_snapshotjob(
        config.namespace,
        snapshotjob_name,
        workloads.snapshotjob_hang_pod_template(config=config, run=run),
    )
    with pytest.raises(ApiException) as excinfo:
        client.CustomObjectsApi().patch_namespaced_custom_object(
            snap.GROUP,
            snap.VERSION,
            config.namespace,
            snap.SNAPSHOTJOBS,
            snapshotjob_name,
            {"spec": {"activeDeadlineSeconds": 60}},
        )
    assert excinfo.value.status in (400, 422)
