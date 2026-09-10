# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Collects the environment metadata every benchmark run reports.

Nothing here is authoritative unless it comes straight from the cluster: GPU
product/driver via `nvidia-smi` exec'd into the running pod, storage class and
its provisioner via the PVC/StorageClass objects, and node placement via the
pods' own `spec.node_name`. There is no size field on `PodSnapshot` or
`PodSnapshotContent` status (checked in api/v1alpha1/podsnapshotcontent_types.go),
so checkpoint size is measured separately, on the node, via `du -sb` — see
`run.py`.
"""

from __future__ import annotations

from kubernetes import client
from kubernetes.client import ApiException

from snapshot_e2e import k8s

from snapshot_benchmarks.schema import BenchmarkEnvironment

# The label GPU Operator's Node Feature Discovery writes when it manages the
# node. Absent on manually-driver-installed nodes (e.g. a plain `dnf install
# nvidia-driver` box) -- that is expected and reported as None, not an error.
CUDA_DRIVER_MAJOR_LABEL = "nvidia.com/cuda.driver-version.major"


def gpu_identity(namespace: str, pod: str, container: str) -> tuple[str | None, str | None]:
    """Returns (gpu_product, driver_version) queried live from inside the pod,
    which already has GPU access via `runtimeClassName: nvidia`."""
    output = k8s.exec_command(
        namespace,
        pod,
        "nvidia-smi --query-gpu=name,driver_version --format=csv,noheader",
        container=container,
    )
    line = output.strip().splitlines()[0] if output.strip() else ""
    parts = [part.strip() for part in line.split(",")]
    if len(parts) != 2:
        return None, None
    return parts[0] or None, parts[1] or None


def engine_version(namespace: str, pod: str, container: str, probe_command: str) -> str | None:
    """Runs `probe_command` (e.g. `python3 -c "import vllm; print(vllm.__version__)"`)
    inside the pod and returns its stripped stdout, or None on failure. Never
    hardcoded: the guides and the published benchmark doc have already drifted
    on the pinned vLLM version, so a live query is the only way a result stays
    self-describing instead of silently reproducing that drift."""
    output = k8s.exec_command(namespace, pod, probe_command, container=container)
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


def collect_environment(
    *,
    namespace: str,
    pvc_name: str,
    gpu_pod: str | None = None,
    gpu_container: str | None = None,
    capture_node: str | None = None,
    restore_node: str | None = None,
) -> BenchmarkEnvironment:
    """Collects the full `BenchmarkEnvironment` bundle. `gpu_pod` is any
    currently-running GPU pod in `namespace` -- typically the just-created
    source or restore pod -- used only to exec `nvidia-smi`. Omit it (e.g. for
    a standalone pre-flight check with no workload deployed yet) to collect
    everything except `gpu_product`/`gpu_driver_version`."""
    gpu_product = driver_version = None
    if gpu_pod:
        gpu_product, driver_version = gpu_identity(namespace, gpu_pod, gpu_container)
    storage_class, provisioner = storage_backend(namespace, pvc_name)
    reference_node = capture_node or restore_node
    return BenchmarkEnvironment(
        gpu_product=gpu_product,
        gpu_driver_version=driver_version,
        cuda_driver_major_label=node_cuda_driver_major_label(reference_node),
        storage_class=storage_class,
        storage_provisioner=provisioner,
        k8s_version=k8s_server_version(),
        capture_node=capture_node,
        restore_node=restore_node,
        placement=placement(capture_node, restore_node),
    )
