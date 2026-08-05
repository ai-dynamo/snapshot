# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Preflight checks for Snapshot e2e host clusters."""

from __future__ import annotations

import argparse
import atexit
import os
import re
import subprocess
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Iterable

from kubernetes import client, config
from kubernetes.client import ApiException
from packaging.version import InvalidVersion, Version


DEFAULT_GPU_OPERATOR_NAMESPACE = "gpu-operator"
DEFAULT_RUNTIME_CLASS = "nvidia"
DEFAULT_MIN_GPU_OPERATOR_VERSION = "26.3.0"
DEFAULT_MIN_CUDA_DRIVER_MAJOR = 580
DEFAULT_NODE_SELECTOR = {
    "nvidia.com/gpu.present": "true",
    "nvidia.com/mig.config": "all-disabled",
}
MIG_CONFIG_LABEL = "nvidia.com/mig.config"
MIG_CAPABLE_LABEL = "nvidia.com/mig.capable"

BAD_CONTAINER_WAITING_REASONS = {
    "CrashLoopBackOff",
    "CreateContainerConfigError",
    "ErrImagePull",
    "ImagePullBackOff",
    "InvalidImageName",
    "RunContainerError",
}

_NORMALIZED_KUBECONFIGS: list[Path] = []


@dataclass(frozen=True)
class CheckResult:
    name: str
    ok: bool
    detail: str


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv)
    load_config(args.kubeconfig, args.context)

    checks = [
        check_runtime_class(args.runtime_class),
        check_gpu_operator(
            namespace=args.gpu_operator_namespace,
            min_version=Version(args.min_gpu_operator_version),
        ),
        check_gpu_nodes(
            min_cuda_driver_major=args.min_cuda_driver_major,
            required_labels=parse_label_selectors(args.node_selector),
        ),
    ]

    failed = False
    for result in checks:
        status = "PASS" if result.ok else "FAIL"
        print(f"{status} {result.name}: {result.detail}")
        failed = failed or not result.ok

    return 1 if failed else 0


def parse_args(argv: list[str] | None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Validate the host cluster baseline required by Snapshot e2e."
    )
    parser.add_argument(
        "--kubeconfig",
        default=os.environ.get("KUBECONFIG"),
        help="Path to kubeconfig. Defaults to KUBECONFIG or standard kubeconfig lookup.",
    )
    parser.add_argument(
        "--context",
        default=None,
        help="Optional kubeconfig context to use.",
    )
    parser.add_argument(
        "--runtime-class",
        default=DEFAULT_RUNTIME_CLASS,
        help=f"Required GPU RuntimeClass name. Default: {DEFAULT_RUNTIME_CLASS}.",
    )
    parser.add_argument(
        "--gpu-operator-namespace",
        default=DEFAULT_GPU_OPERATOR_NAMESPACE,
        help=(
            "Namespace containing GPU Operator pods. "
            f"Default: {DEFAULT_GPU_OPERATOR_NAMESPACE}."
        ),
    )
    parser.add_argument(
        "--min-gpu-operator-version",
        default=DEFAULT_MIN_GPU_OPERATOR_VERSION,
        help=(
            "Minimum GPU Operator chart version inferred from pod helm.sh/chart "
            f"labels. Default: {DEFAULT_MIN_GPU_OPERATOR_VERSION}."
        ),
    )
    parser.add_argument(
        "--min-cuda-driver-major",
        type=int,
        default=DEFAULT_MIN_CUDA_DRIVER_MAJOR,
        help=(
            "Minimum nvidia.com/cuda.driver-version.major node label. "
            f"Default: {DEFAULT_MIN_CUDA_DRIVER_MAJOR}."
        ),
    )
    parser.add_argument(
        "--node-selector",
        action="append",
        default=[f"{key}={value}" for key, value in DEFAULT_NODE_SELECTOR.items()],
        metavar="KEY=VALUE",
        help=(
            "Required label for eligible GPU nodes. Can be repeated. "
            "Defaults to full-GPU nodes with MIG disabled."
        ),
    )
    return parser.parse_args(argv)


def load_config(kubeconfig: str | None, context: str | None) -> None:
    config_error: Exception | None = None
    try:
        normalized_kubeconfig = write_normalized_kubeconfig(kubeconfig, context)
        config.load_kube_config(config_file=str(normalized_kubeconfig), context=None)
        return
    except Exception as kube_config_error:
        config_error = kube_config_error
        try:
            config.load_kube_config(config_file=kubeconfig, context=context)
            return
        except Exception as direct_kube_config_error:
            config_error = direct_kube_config_error

        if kubeconfig:
            raise RuntimeError(
                f"failed to load kubeconfig {kubeconfig}: {type(config_error).__name__}"
            ) from None
        try:
            config.load_incluster_config()
        except Exception as incluster_error:
            raise RuntimeError(
                "failed to load Kubernetes config from kubeconfig or in-cluster config"
            ) from incluster_error


def write_normalized_kubeconfig(kubeconfig: str | None, context: str | None) -> Path:
    """Render kubeconfig through kubectl before the Python client reads it.

    kubectl tolerates and normalizes kubeconfig shapes the Python client rejects,
    for example a cert/key user entry that also contains `token: null`.
    """
    command = ["kubectl"]
    if kubeconfig:
        command.extend(["--kubeconfig", kubeconfig])
    if context:
        command.extend(["--context", context])
    command.extend(["config", "view", "--raw", "--flatten", "--minify"])

    with tempfile.NamedTemporaryFile(
        "w",
        encoding="utf-8",
        prefix="snapshot-e2e-kubeconfig-",
        delete=False,
    ) as output:
        path = Path(output.name)
        subprocess.run(command, stdout=output, text=True, check=True)

    _NORMALIZED_KUBECONFIGS.append(path)
    return path


def cleanup_normalized_kubeconfigs() -> None:
    for path in _NORMALIZED_KUBECONFIGS:
        path.unlink(missing_ok=True)


atexit.register(cleanup_normalized_kubeconfigs)


def check_runtime_class(runtime_class: str) -> CheckResult:
    """Verify the host cluster exposes the GPU RuntimeClass used by test pods."""
    try:
        client.NodeV1Api().read_runtime_class(runtime_class)
    except ApiException as exc:
        if exc.status == 404:
            return CheckResult(
                "runtime class",
                False,
                f"RuntimeClass/{runtime_class} was not found",
            )
        return CheckResult("runtime class", False, f"Kubernetes API error: {exc}")
    except Exception as exc:
        return CheckResult("runtime class", False, str(exc))
    return CheckResult("runtime class", True, f"RuntimeClass/{runtime_class} exists")


def check_gpu_operator(namespace: str, min_version: Version) -> CheckResult:
    """Verify GPU Operator pods exist, are not obviously failing, and report a new enough Helm chart version."""
    try:
        pods = client.CoreV1Api().list_namespaced_pod(namespace=namespace).items
    except ApiException as exc:
        if exc.status == 404:
            return CheckResult(
                "gpu operator",
                False,
                f"namespace {namespace!r} was not found",
            )
        return CheckResult("gpu operator", False, f"Kubernetes API error: {exc}")
    except Exception as exc:
        return CheckResult("gpu operator", False, str(exc))

    if not pods:
        return CheckResult("gpu operator", False, f"no pods found in {namespace!r}")

    failing = list(failing_gpu_operator_pods(pods))
    if failing:
        return CheckResult(
            "gpu operator",
            False,
            "failing pods: " + ", ".join(failing),
        )

    versions = sorted(
        {
            version
            for pod in pods
            if (version := gpu_operator_chart_version(pod.metadata.labels or {}))
        }
    )
    if not versions:
        return CheckResult(
            "gpu operator",
            False,
            "no parseable helm.sh/chart label found on GPU Operator pods",
        )

    latest = versions[-1]
    if latest < min_version:
        return CheckResult(
            "gpu operator",
            False,
            f"chart version {latest} is older than required {min_version}",
        )

    non_ready = sum(1 for pod in pods if pod.status.phase not in {"Running", "Succeeded"})
    detail = f"{len(pods)} pod(s), chart version {latest}"
    if non_ready:
        detail += f", {non_ready} pod(s) not yet Running/Succeeded"
    return CheckResult("gpu operator", True, detail)


def failing_gpu_operator_pods(pods: Iterable[client.V1Pod]) -> Iterable[str]:
    for pod in pods:
        phase = pod.status.phase
        if phase in {"Failed", "Unknown"}:
            yield f"{pod.metadata.name} phase={phase}"
            continue
        statuses = list(pod.status.init_container_statuses or [])
        statuses.extend(pod.status.container_statuses or [])
        for status in statuses:
            waiting = status.state.waiting if status.state else None
            if waiting and waiting.reason in BAD_CONTAINER_WAITING_REASONS:
                yield f"{pod.metadata.name}/{status.name} waiting={waiting.reason}"


def gpu_operator_chart_version(labels: dict[str, str]) -> Version | None:
    chart = labels.get("helm.sh/chart", "")
    match = re.match(r"^gpu-operator-v?(.+)$", chart)
    if not match:
        return None
    raw_version = match.group(1)
    try:
        return Version(raw_version)
    except InvalidVersion:
        return None


def check_gpu_nodes(
    min_cuda_driver_major: int,
    required_labels: dict[str, str],
) -> CheckResult:
    """Find usable GPU nodes by node labels, schedulability, Ready state, and CUDA driver major version."""
    try:
        nodes = client.CoreV1Api().list_node().items
    except ApiException as exc:
        return CheckResult("gpu nodes", False, f"Kubernetes API error: {exc}")
    except Exception as exc:
        return CheckResult("gpu nodes", False, str(exc))

    eligible: list[str] = []
    rejected_gpu_nodes: list[str] = []

    for node in nodes:
        labels = node.metadata.labels or {}
        if labels.get("nvidia.com/gpu.present") != "true":
            continue
        rejection = gpu_node_rejection(
            node,
            required_labels=required_labels,
            min_cuda_driver_major=min_cuda_driver_major,
        )
        if rejection:
            rejected_gpu_nodes.append(f"{node.metadata.name}: {rejection}")
            continue
        eligible.append(node.metadata.name)

    if not eligible:
        detail = "no eligible GPU nodes found"
        if rejected_gpu_nodes:
            detail += "; rejected " + "; ".join(rejected_gpu_nodes[:5])
        return CheckResult("gpu nodes", False, detail)

    return CheckResult(
        "gpu nodes",
        True,
        f"{len(eligible)} eligible node(s): {', '.join(eligible[:5])}",
    )


def gpu_node_rejection(
    node: client.V1Node,
    *,
    required_labels: dict[str, str],
    min_cuda_driver_major: int,
) -> str | None:
    labels = node.metadata.labels or {}
    for key, expected in required_labels.items():
        actual = labels.get(key)
        if key == MIG_CONFIG_LABEL and actual is None and not node_is_mig_capable(labels):
            continue
        if actual != expected:
            return f"label {key}={actual!r}, want {expected!r}"

    if node.spec.unschedulable:
        return "node is unschedulable"
    if not node_is_ready(node):
        return "node is not Ready"

    raw_major = labels.get("nvidia.com/cuda.driver-version.major")
    if raw_major is None:
        return "missing nvidia.com/cuda.driver-version.major"
    try:
        major = int(raw_major)
    except ValueError:
        return f"invalid nvidia.com/cuda.driver-version.major={raw_major!r}"
    if major < min_cuda_driver_major:
        return (
            f"nvidia.com/cuda.driver-version.major={major}, "
            f"want >= {min_cuda_driver_major}"
        )
    return None


def node_is_mig_capable(labels: dict[str, str]) -> bool:
    return labels.get(MIG_CAPABLE_LABEL) == "true"


def node_is_ready(node: client.V1Node) -> bool:
    for condition in node.status.conditions or []:
        if condition.type == "Ready":
            return condition.status == "True"
    return False


def parse_label_selectors(selectors: list[str]) -> dict[str, str]:
    parsed: dict[str, str] = {}
    for selector in selectors:
        if "=" not in selector:
            raise SystemExit(f"--node-selector must be KEY=VALUE, got {selector!r}")
        key, value = selector.split("=", 1)
        if not key or not value:
            raise SystemExit(f"--node-selector must be KEY=VALUE, got {selector!r}")
        parsed[key] = value
    return parsed


if __name__ == "__main__":
    raise SystemExit(main())
