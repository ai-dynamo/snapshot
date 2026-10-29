# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Collects the environment metadata every benchmark run reports.

Nothing here is authoritative unless it comes straight from the cluster: GPU
product/driver via `nvidia-smi` exec'd into the running pod, storage class and
its provisioner via the PVC/StorageClass objects, and node placement via the
pods' own `spec.node_name`. There is no size field on `PodSnapshot` or
`PodSnapshotContent` status (checked in api/v1alpha1/podsnapshotcontent_types.go),
so PVC checkpoint size is measured by a regular-file inventory on the node,
and S3 checkpoint size by PageBroker's bundled rclone. See `measurement.py`.
"""

from __future__ import annotations

import json

from kubernetes import client
from kubernetes.client import ApiException

from snapshot_e2e import k8s

from snapshot_benchmarks.schema import BenchmarkEnvironment, GpuIdentity

# The label GPU Operator's Node Feature Discovery writes when it manages the
# node. Absent on manually-driver-installed nodes (e.g. a plain `dnf install
# nvidia-driver` box) -- that is expected and reported as None, not an error.
CUDA_DRIVER_MAJOR_LABEL = "nvidia.com/cuda.driver-version.major"


def gpu_identity(namespace: str, pod: str, container: str) -> tuple[str | None, str | None]:
    """Returns (gpu_product, driver_version) queried live from inside the pod,
    which already has GPU access via `runtimeClassName: nvidia`. Returns
    (None, None) if the exec handshake or transport fails -- diagnostic
    metadata, never worth failing the whole run over."""
    try:
        output = k8s.exec_command(
            namespace,
            pod,
            "nvidia-smi --query-gpu=name,driver_version --format=csv,noheader",
            container=container,
        )
    except Exception:  # noqa: BLE001 - exec-over-websocket fails in many untyped ways; best-effort
        return None, None
    line = output.strip().splitlines()[0] if output.strip() else ""
    parts = [part.strip() for part in line.split(",")]
    if len(parts) != 2:
        return None, None
    return parts[0] or None, parts[1] or None


def engine_version(namespace: str, pod: str, container: str, probe_command: str) -> str | None:
    """Runs `probe_command` (e.g. `python3 -c "import vllm; print(vllm.__version__)"`)
    inside the pod and returns its stripped stdout, or None on failure (including
    an exec handshake/transport failure). Never hardcoded: the guides and the
    published benchmark doc have already drifted on the pinned vLLM version, so
    a live query is the only way a result stays self-describing instead of
    silently reproducing that drift."""
    try:
        output = k8s.exec_command(namespace, pod, probe_command, container=container)
    except Exception:  # noqa: BLE001 - exec-over-websocket fails in many untyped ways; best-effort
        return None
    version = output.strip().splitlines()[-1].strip() if output.strip() else ""
    return version or None


def storage_backend(namespace: str, pvc_name: str) -> tuple[str | None, str | None]:
    """Returns (storage_class_name, provisioner) for the checkpoint PVC."""
    try:
        pvc = k8s.read_pvc(namespace, pvc_name)
    except ApiException:
        return None, None
    storage_class_name = pvc.spec.storage_class_name
    if not storage_class_name:
        return None, None
    try:
        storage_class = client.StorageV1Api().read_storage_class(storage_class_name)
    except ApiException:
        return storage_class_name, None
    return storage_class_name, storage_class.provisioner


def k8s_server_version() -> str | None:
    try:
        version_info = client.VersionApi().get_code()
    except ApiException:
        return None
    return f"{version_info.major}.{version_info.minor} ({version_info.git_version})"


def node_cuda_driver_major_label(node_name: str | None) -> str | None:
    if not node_name:
        return None
    try:
        node = client.CoreV1Api().read_node(node_name)
    except ApiException:
        return None
    return (node.metadata.labels or {}).get(CUDA_DRIVER_MAJOR_LABEL)


def placement(capture_node: str | None, restore_node: str | None) -> str | None:
    if not capture_node or not restore_node:
        return None
    return "same_node" if capture_node == restore_node else "different_node"


def collect_gpu_identity(
    namespace: str, pod: str, container: str, node_name: str | None
) -> GpuIdentity:
    """Collects the GPU/driver identity of one pod+node. Called once for the
    capture pod/node and, for `mode="both"`, once more for the restore
    pod/node -- see `run.py`. Never reused across the two: on a heterogeneous
    cluster the restore node's GPU product/driver can genuinely differ from
    the capture node's."""
    gpu_product, driver_version = gpu_identity(namespace, pod, container)
    return GpuIdentity(
        gpu_product=gpu_product,
        gpu_driver_version=driver_version,
        cuda_driver_major_label=node_cuda_driver_major_label(node_name),
    )


def s3_settings(namespace: str, release: str) -> dict[str, str]:
    maps = client.CoreV1Api().list_namespaced_config_map(
        namespace, label_selector=f"app.kubernetes.io/instance={release}"
    )
    configs = [
        json.loads(cm.data["pagebroker-s3-config.json"])
        for cm in maps.items if "pagebroker-s3-config.json" in (cm.data or {})
    ]
    if len(configs) != 1:
        raise RuntimeError("expected one release-scoped PageBroker S3 configuration")
    config = configs[0]
    keys = ("storeId", "bucket", "region", "endpoint", "prefix")
    if not isinstance(config, dict) or any(
        not isinstance(config.get(key), str) or not config[key].strip()
        for key in keys[:-1]
    ) or not isinstance(config.get("prefix", ""), str):
        raise RuntimeError("incomplete PageBroker S3 configuration")
    # Record connection metadata only, never credential or TLS configuration.
    return {key: config.get(key, "") for key in keys}


def collect_environment(
    *,
    pvc_namespace: str,
    pvc_name: str,
    storage_type: str = "pvc",
    release: str = "snapshot",
    capture_node: str | None = None,
    restore_node: str | None = None,
) -> BenchmarkEnvironment:
    """Collects the `BenchmarkEnvironment` bundle minus GPU identity, which is
    per-pod and collected separately via `collect_gpu_identity` (once for the
    capture pod, once more for the restore pod when `mode="both"`) -- see
    `run.py`.

    `pvc_namespace` is deliberately not the workload namespace: the checkpoint
    PVC (`storage.pvc.name`, mounted by every snapshot-agent) lives in the
    Snapshot release's own namespace, which a hand-installed cluster commonly
    keeps separate from the workload namespace (e.g. `snapshot` vs.
    `default`) -- see `BenchmarkConfig`'s docstring in `run.py`."""
    if storage_type not in ("pvc", "s3"):
        raise ValueError(f"unsupported checkpoint storage type: {storage_type!r}")
    s3 = s3_settings(pvc_namespace, release) if storage_type == "s3" else None
    storage_class, provisioner = (None, None) if s3 is not None else storage_backend(pvc_namespace, pvc_name)
    return BenchmarkEnvironment(
        storage_type=storage_type,
        s3=s3,
        storage_class=storage_class,
        storage_provisioner=provisioner,
        k8s_version=k8s_server_version(),
        capture_node=capture_node,
        restore_node=restore_node,
        placement=placement(capture_node, restore_node),
    )
