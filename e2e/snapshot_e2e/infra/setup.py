# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Reusable setup orchestration for Snapshot e2e environments."""

from __future__ import annotations

import argparse
import json
import os
import shlex
import ssl
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
from dataclasses import asdict, dataclass
from pathlib import Path
from typing import Any

import yaml
from kubernetes import client
from kubernetes.client import ApiException
from packaging.version import Version

from snapshot_e2e.infra import preflight


DEFAULT_MODE = "direct"
DEFAULT_TEST_NAMESPACE = "snapshot-e2e"
DEFAULT_SNAPSHOT_RELEASE = "snapshot"
DEFAULT_SNAPSHOT_TAG = "v0.0.0-g71827d8e"
DEFAULT_PVC_NAME = "snapshot-pvc"
DEFAULT_PVC_SIZE = "2Gi"
DEFAULT_VCLUSTER_K8S_VERSION = "v1.32.13"
DEFAULT_VCLUSTER_LOCAL_PORT = 8443
DEFAULT_HELM_TIMEOUT = "10m"
DEFAULT_READY_TIMEOUT_SECONDS = 900

SNAPSHOT_LABEL = "app.kubernetes.io/name=snapshot"


class SetupError(RuntimeError):
    """Raised when setup cannot continue."""


@dataclass(frozen=True)
class SetupResult:
    mode: str
    host_namespace: str
    vcluster_name: str
    test_namespace: str
    target_kubeconfig: str
    vcluster_kubeconfig: str
    snapshot_release: str
    pvc_name: str


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv)
    try:
        result = setup(args)
    except SetupError as exc:
        print(f"ERROR {exc}", file=sys.stderr)
        return 1

    result_data = asdict(result)
    if args.result_file:
        result_path = Path(args.result_file)
        result_path.parent.mkdir(parents=True, exist_ok=True)
        result_path.write_text(json.dumps(result_data, indent=2) + "\n", encoding="utf-8")

    print("\nSnapshot e2e setup result:")
    for key, value in result_data.items():
        print(f"  {key}: {value}")
    return 0


def parse_args(argv: list[str] | None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Prepare a Snapshot e2e target cluster."
    )
    parser.add_argument(
        "--mode",
        choices=("direct", "vcluster"),
        default=os.environ.get("SNAPSHOT_E2E_MODE", DEFAULT_MODE),
        help="Use the current cluster directly or create a vCluster first.",
    )
    parser.add_argument(
        "--kubeconfig",
        default=os.environ.get("KUBECONFIG"),
        help="Host kubeconfig. Defaults to KUBECONFIG or standard kubeconfig lookup.",
    )
    parser.add_argument("--context", default=None, help="Optional host kube context.")
    parser.add_argument(
        "--host-namespace",
        default=os.environ.get("SNAPSHOT_E2E_HOST_NAMESPACE"),
        help="Host namespace for vCluster mode.",
    )
    parser.add_argument(
        "--vcluster-name",
        default=os.environ.get("SNAPSHOT_E2E_VCLUSTER_NAME"),
        help="vCluster release name. Defaults to the host namespace.",
    )
    parser.add_argument(
        "--test-namespace",
        default=os.environ.get("SNAPSHOT_E2E_TEST_NAMESPACE", DEFAULT_TEST_NAMESPACE),
        help=f"Namespace where Snapshot is installed. Default: {DEFAULT_TEST_NAMESPACE}.",
    )
    parser.add_argument(
        "--snapshot-release",
        default=os.environ.get("SNAPSHOT_E2E_SNAPSHOT_RELEASE", DEFAULT_SNAPSHOT_RELEASE),
        help=f"Snapshot Helm release name. Default: {DEFAULT_SNAPSHOT_RELEASE}.",
    )
    parser.add_argument(
        "--snapshot-tag",
        default=os.environ.get("SNAPSHOT_E2E_SNAPSHOT_TAG", DEFAULT_SNAPSHOT_TAG),
        help="Operator and agent image tag for the Snapshot chart.",
    )
    parser.add_argument(
        "--pvc-name",
        default=os.environ.get("SNAPSHOT_E2E_PVC_NAME", DEFAULT_PVC_NAME),
        help=f"Checkpoint PVC name. Default: {DEFAULT_PVC_NAME}.",
    )
    parser.add_argument(
        "--pvc-size",
        default=os.environ.get("SNAPSHOT_E2E_PVC_SIZE", DEFAULT_PVC_SIZE),
        help=f"Checkpoint PVC size. Default: {DEFAULT_PVC_SIZE}.",
    )
    parser.add_argument(
        "--vcluster-k8s-version",
        default=os.environ.get(
            "SNAPSHOT_E2E_VCLUSTER_K8S_VERSION", DEFAULT_VCLUSTER_K8S_VERSION
        ),
        help=f"Kubernetes version for vCluster mode. Default: {DEFAULT_VCLUSTER_K8S_VERSION}.",
    )
    parser.add_argument(
        "--vcluster-local-port",
        type=int,
        default=int(
            os.environ.get(
                "SNAPSHOT_E2E_VCLUSTER_LOCAL_PORT", DEFAULT_VCLUSTER_LOCAL_PORT
            )
        ),
        help=f"Local port for the vCluster API port-forward. Default: {DEFAULT_VCLUSTER_LOCAL_PORT}.",
    )
    parser.add_argument(
        "--target-kubeconfig",
        default=os.environ.get("SNAPSHOT_E2E_TARGET_KUBECONFIG"),
        help="Where to write the vCluster kubeconfig. In direct mode, defaults to --kubeconfig.",
    )
    parser.add_argument(
        "--helm-timeout",
        default=os.environ.get("SNAPSHOT_E2E_HELM_TIMEOUT", DEFAULT_HELM_TIMEOUT),
        help=f"Helm install timeout. Default: {DEFAULT_HELM_TIMEOUT}.",
    )
    parser.add_argument(
        "--ready-timeout-seconds",
        type=int,
        default=int(
            os.environ.get(
                "SNAPSHOT_E2E_READY_TIMEOUT_SECONDS", DEFAULT_READY_TIMEOUT_SECONDS
            )
        ),
        help=f"Readiness timeout. Default: {DEFAULT_READY_TIMEOUT_SECONDS}.",
    )
    parser.add_argument(
        "--workspace",
        default=os.environ.get("GITHUB_WORKSPACE", os.getcwd()),
        help="Workspace used for generated files. Defaults to GITHUB_WORKSPACE or cwd.",
    )
    parser.add_argument(
        "--result-file",
        default=os.environ.get("SNAPSHOT_E2E_RESULT_FILE"),
        help="Optional JSON file to write setup outputs.",
    )
    parser.add_argument(
        "--skip-host-preflight",
        action="store_true",
        help="Skip GPU Operator and GPU node preflight checks.",
    )
    return parser.parse_args(argv)


def setup(args: argparse.Namespace) -> SetupResult:
    workspace = Path(args.workspace).resolve()
    run_id = os.environ.get("GITHUB_RUN_ID", "manual")
    run_attempt = os.environ.get("GITHUB_RUN_ATTEMPT", "1")
    default_run_name = f"snapshot-e2e-{run_id}-{run_attempt}"

    host_namespace = args.host_namespace or default_run_name
    vcluster_name = args.vcluster_name or host_namespace

    print("Loading host kubeconfig")
    preflight.load_config(args.kubeconfig, args.context)
    if not args.skip_host_preflight:
        run_host_preflight()

    if args.mode == "vcluster":
        target_kubeconfig = Path(
            args.target_kubeconfig or workspace / ".kubeconfig-snapshot-e2e"
        ).resolve()
        create_host_namespace(host_namespace)
        ensure_vcluster_unused(host_namespace, vcluster_name)
        create_vcluster(host_namespace, vcluster_name, args.vcluster_k8s_version)
        install_hostpath_mapper(host_namespace, vcluster_name, args.helm_timeout)
        connect_vcluster(
            host_namespace=host_namespace,
            vcluster_name=vcluster_name,
            target_kubeconfig=target_kubeconfig,
            local_port=args.vcluster_local_port,
            workspace=workspace,
        )
        target_kubeconfig_value = str(target_kubeconfig)
        vcluster_kubeconfig_value = str(target_kubeconfig)
    else:
        target_kubeconfig_value = args.target_kubeconfig or args.kubeconfig or ""
        vcluster_kubeconfig_value = ""

    print("Loading target kubeconfig")
    preflight.load_config(target_kubeconfig_value or None, None)

    ensure_target_namespace(args.test_namespace)
    ensure_checkpoint_pvc(
        namespace=args.test_namespace,
        name=args.pvc_name,
        size=args.pvc_size,
    )
    install_snapshot_chart(
        kubeconfig=target_kubeconfig_value or None,
        namespace=args.test_namespace,
        release=args.snapshot_release,
        image_tag=args.snapshot_tag,
        timeout=args.helm_timeout,
    )
    wait_for_snapshot_readiness(
        namespace=args.test_namespace,
        release=args.snapshot_release,
        timeout_seconds=args.ready_timeout_seconds,
    )

    return SetupResult(
        mode=args.mode,
        host_namespace=host_namespace if args.mode == "vcluster" else "",
        vcluster_name=vcluster_name if args.mode == "vcluster" else "",
        test_namespace=args.test_namespace,
        target_kubeconfig=target_kubeconfig_value,
        vcluster_kubeconfig=vcluster_kubeconfig_value,
        snapshot_release=args.snapshot_release,
        pvc_name=args.pvc_name,
    )


def run_host_preflight() -> None:
    print("Running host preflight")
    checks = [
        preflight.check_runtime_class(preflight.DEFAULT_RUNTIME_CLASS),
        preflight.check_gpu_operator(
            namespace=preflight.DEFAULT_GPU_OPERATOR_NAMESPACE,
            min_version=Version(preflight.DEFAULT_MIN_GPU_OPERATOR_VERSION),
        ),
        preflight.check_gpu_nodes(
            min_cuda_driver_major=preflight.DEFAULT_MIN_CUDA_DRIVER_MAJOR,
            required_labels=preflight.DEFAULT_NODE_SELECTOR,
        ),
    ]

    failed = False
    for result in checks:
        status = "PASS" if result.ok else "FAIL"
        print(f"{status} {result.name}: {result.detail}")
        failed = failed or not result.ok
    if failed:
        raise SetupError("host preflight failed")


def create_host_namespace(namespace: str) -> None:
    labels = {
        "snapshot-e2e": "true",
        "snapshot.github/run-id": os.environ.get("GITHUB_RUN_ID", "manual"),
        "snapshot.github/run-attempt": os.environ.get("GITHUB_RUN_ATTEMPT", "1"),
        "nscleanup/enabled": "true",
        "nscleanup/ttl": "7200",
    }
    ensure_namespace(namespace, labels=labels)


def ensure_namespace(namespace: str, labels: dict[str, str] | None = None) -> None:
    api = client.CoreV1Api()
    body = client.V1Namespace(
        metadata=client.V1ObjectMeta(name=namespace, labels=labels or {})
    )
    try:
        api.create_namespace(body)
        print(f"Created namespace {namespace}")
    except ApiException as exc:
        if exc.status != 409:
            raise SetupError(f"failed to create namespace {namespace}: {exc}") from exc
        print(f"Namespace {namespace} already exists")

    if labels:
        try:
            api.patch_namespace(namespace, {"metadata": {"labels": labels}})
        except ApiException as exc:
            raise SetupError(f"failed to label namespace {namespace}: {exc}") from exc


def ensure_vcluster_unused(namespace: str, name: str) -> None:
    apps = client.AppsV1Api()
    try:
        apps.read_namespaced_stateful_set(name=name, namespace=namespace)
    except ApiException as exc:
        if exc.status != 404:
            raise SetupError(
                f"failed to check vCluster StatefulSet {namespace}/{name}: {exc}"
            ) from exc
    else:
        raise SetupError(f"vCluster StatefulSet {namespace}/{name} already exists")

    completed = subprocess.run(
        ["helm", "status", name, "-n", namespace],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        check=False,
    )
    if completed.returncode == 0:
        raise SetupError(f"vCluster Helm release {namespace}/{name} already exists")
    print(f"vCluster name {namespace}/{name} is available")


def create_vcluster(namespace: str, name: str, k8s_version: str) -> None:
    print(f"Creating vCluster {namespace}/{name}")
    values = {
        "controlPlane": {
            "hostPathMapper": {
                "enabled": True,
            },
        },
        "sync": {
            "fromHost": {
                "nodes": {
                    "enabled": True,
                    "clearImageStatus": True,
                    "selector": {
                        "labels": {
                            "kubernetes.azure.com/mode": "user",
                        },
                    },
                },
                "runtimeClasses": {
                    "enabled": True,
                },
                "storageClasses": {
                    "enabled": True,
                },
            },
        },
    }
    values_file = write_temp_yaml("snapshot-vcluster-values-", values)
    try:
        run(
            [
                "vcluster",
                "create",
                name,
                "--namespace",
                namespace,
                "--connect=false",
                "--set",
                "controlPlane.distro.k8s.enabled=true",
                "--set",
                f"controlPlane.distro.k8s.version={k8s_version}",
                "--values",
                str(values_file),
            ]
        )
    finally:
        values_file.unlink(missing_ok=True)

    wait_for_pods_ready(
        namespace=namespace,
        label_selector=f"app=vcluster,release={name}",
        timeout_seconds=900,
        description=f"vCluster pod {namespace}/{name}",
    )


def install_hostpath_mapper(namespace: str, vcluster_name: str, helm_timeout: str) -> None:
    print(f"Installing vCluster HostPath Mapper in {namespace}")
    values = {
        "nodeSelector": {
            "nvidia.com/gpu.present": "true",
        },
        "tolerations": [
            {
                "operator": "Exists",
            },
        ],
    }
    values_file = write_temp_yaml("snapshot-vcluster-hpm-values-", values)
    try:
        run(
            [
                "helm",
                "upgrade",
                "--install",
                "vcluster-hpm",
                "vcluster-hpm",
                "--repo",
                "https://charts.loft.sh",
                "--namespace",
                namespace,
                "--wait",
                "--timeout",
                helm_timeout,
                "--set",
                f"VclusterReleaseName={vcluster_name}",
                "--values",
                str(values_file),
            ]
        )
    finally:
        values_file.unlink(missing_ok=True)

    wait_for_daemonset_rollout(
        namespace=namespace,
        name="vcluster-hpm-hostpath-mapper",
        timeout_seconds=600,
    )


def connect_vcluster(
    *,
    host_namespace: str,
    vcluster_name: str,
    target_kubeconfig: Path,
    local_port: int,
    workspace: Path,
) -> None:
    print(f"Connecting to vCluster {host_namespace}/{vcluster_name}")
    target_kubeconfig.parent.mkdir(parents=True, exist_ok=True)

    log_path = workspace / ".snapshot-e2e-vcluster-port-forward.log"
    pid_path = workspace / ".snapshot-e2e-vcluster-port-forward.pid"
    log_path.parent.mkdir(parents=True, exist_ok=True)
    log_file = log_path.open("w", encoding="utf-8")

    process = subprocess.Popen(
        [
            "kubectl",
            "port-forward",
            "-n",
            host_namespace,
            f"svc/{vcluster_name}",
            f"{local_port}:443",
        ],
        stdout=log_file,
        stderr=subprocess.STDOUT,
        start_new_session=True,
    )
    pid_path.write_text(f"{process.pid}\n", encoding="utf-8")

    try:
        wait_for_https(f"https://127.0.0.1:{local_port}/healthz", process, log_path)
        with target_kubeconfig.open("w", encoding="utf-8") as output:
            run(
                [
                    "vcluster",
                    "connect",
                    vcluster_name,
                    "--namespace",
                    host_namespace,
                    "--server",
                    f"https://127.0.0.1:{local_port}",
                    "--print",
                ],
                stdout=output,
            )
        target_kubeconfig.chmod(0o600)
    finally:
        log_file.close()


def wait_for_https(url: str, process: subprocess.Popen[Any], log_path: Path) -> None:
    context = ssl._create_unverified_context()
    for _ in range(60):
        if process.poll() is not None:
            raise SetupError(
                "vCluster port-forward exited early; tail:\n" + tail_file(log_path)
            )
        try:
            with urllib.request.urlopen(url, timeout=2, context=context):
                return
        except (urllib.error.URLError, TimeoutError):
            time.sleep(2)
    raise SetupError(
        "vCluster API was not reachable through the local port-forward; tail:\n"
        + tail_file(log_path)
    )


def ensure_target_namespace(namespace: str) -> None:
    ensure_namespace(namespace)


def ensure_checkpoint_pvc(namespace: str, name: str, size: str) -> None:
    api = client.CoreV1Api()
    body = client.V1PersistentVolumeClaim(
        metadata=client.V1ObjectMeta(name=name, namespace=namespace),
        spec=client.V1PersistentVolumeClaimSpec(
            access_modes=["ReadWriteOnce"],
            resources=client.V1ResourceRequirements(requests={"storage": size}),
        ),
    )
    try:
        pvc = api.create_namespaced_persistent_volume_claim(namespace, body)
        print(f"Created PVC {namespace}/{name}: phase={pvc.status.phase}")
    except ApiException as exc:
        if exc.status != 409:
            raise SetupError(f"failed to create PVC {namespace}/{name}: {exc}") from exc
        pvc = api.read_namespaced_persistent_volume_claim(name, namespace)
        requested = (pvc.spec.resources.requests or {}).get("storage")
        detail = f"phase={pvc.status.phase}"
        if requested != size:
            detail += f", requested storage={requested}, configured size={size}"
        print(f"PVC {namespace}/{name} already exists: {detail}")


def install_snapshot_chart(
    *,
    kubeconfig: str | None,
    namespace: str,
    release: str,
    image_tag: str,
    timeout: str,
) -> None:
    print(f"Installing Snapshot chart release {namespace}/{release}")
    command = [
        "helm",
        "upgrade",
        "--install",
        release,
        "./charts/snapshot",
        "--namespace",
        namespace,
        "--create-namespace",
        "--timeout",
        timeout,
        "--set",
        f"image.operator.tag={image_tag}",
        "--set",
        f"image.agent.tag={image_tag}",
        "--set",
        "storage.accessMode=podMount",
        "--set-json",
        "daemonset.imagePullSecrets=[]",
    ]
    env = os.environ.copy()
    if kubeconfig:
        env["KUBECONFIG"] = kubeconfig
    run(command, env=env)


def wait_for_snapshot_readiness(
    *,
    namespace: str,
    release: str,
    timeout_seconds: int,
) -> None:
    operator_selector = ",".join(
        [
            SNAPSHOT_LABEL,
            f"app.kubernetes.io/instance={release}",
            "app.kubernetes.io/component=operator",
        ]
    )
    agent_selector = ",".join(
        [
            SNAPSHOT_LABEL,
            f"app.kubernetes.io/instance={release}",
            "app.kubernetes.io/component=snapshot-agent",
        ]
    )
    wait_for_deployment_rollout_by_selector(
        namespace=namespace,
        label_selector=operator_selector,
        timeout_seconds=timeout_seconds,
        description="Snapshot operator",
    )
    wait_for_daemonset_rollout_by_selector(
        namespace=namespace,
        label_selector=agent_selector,
        timeout_seconds=timeout_seconds,
        description="Snapshot agent",
    )
    print_pods(namespace, SNAPSHOT_LABEL)


def wait_for_pods_ready(
    *,
    namespace: str,
    label_selector: str,
    timeout_seconds: int,
    description: str,
) -> None:
    api = client.CoreV1Api()
    deadline = time.monotonic() + timeout_seconds
    while time.monotonic() < deadline:
        pods = api.list_namespaced_pod(
            namespace=namespace, label_selector=label_selector
        ).items
        if pods and all(pod_ready(pod) for pod in pods):
            print(f"{description} ready")
            return
        time.sleep(5)
    print_pods(namespace, label_selector)
    raise SetupError(f"timed out waiting for {description}")


def wait_for_deployment_rollout_by_selector(
    *,
    namespace: str,
    label_selector: str,
    timeout_seconds: int,
    description: str,
) -> None:
    api = client.AppsV1Api()
    deadline = time.monotonic() + timeout_seconds
    while time.monotonic() < deadline:
        deployments = api.list_namespaced_deployment(
            namespace=namespace, label_selector=label_selector
        ).items
        if deployments and all(deployment_available(item) for item in deployments):
            print(f"{description} deployment ready")
            return
        time.sleep(5)
    raise SetupError(f"timed out waiting for {description} deployment")


def wait_for_daemonset_rollout_by_selector(
    *,
    namespace: str,
    label_selector: str,
    timeout_seconds: int,
    description: str,
) -> None:
    api = client.AppsV1Api()
    deadline = time.monotonic() + timeout_seconds
    while time.monotonic() < deadline:
        daemonsets = api.list_namespaced_daemon_set(
            namespace=namespace, label_selector=label_selector
        ).items
        if daemonsets and all(daemonset_ready(item) for item in daemonsets):
            print(f"{description} daemonset ready")
            return
        time.sleep(5)
    raise SetupError(f"timed out waiting for {description} daemonset")


def wait_for_daemonset_rollout(
    *,
    namespace: str,
    name: str,
    timeout_seconds: int,
) -> None:
    api = client.AppsV1Api()
    deadline = time.monotonic() + timeout_seconds
    while time.monotonic() < deadline:
        try:
            daemonset = api.read_namespaced_daemon_set(name=name, namespace=namespace)
        except ApiException as exc:
            if exc.status != 404:
                raise SetupError(
                    f"failed to read daemonset {namespace}/{name}: {exc}"
                ) from exc
            time.sleep(5)
            continue
        if daemonset_ready(daemonset):
            print(f"DaemonSet {namespace}/{name} ready")
            return
        time.sleep(5)
    raise SetupError(f"timed out waiting for daemonset {namespace}/{name}")


def deployment_available(deployment: client.V1Deployment) -> bool:
    desired = deployment.spec.replicas or 1
    status = deployment.status
    observed = status.observed_generation or 0
    generation = deployment.metadata.generation or 0
    available = status.available_replicas or 0
    updated = status.updated_replicas or 0
    return observed >= generation and available >= desired and updated >= desired


def daemonset_ready(daemonset: client.V1DaemonSet) -> bool:
    status = daemonset.status
    desired = status.desired_number_scheduled or 0
    ready = status.number_ready or 0
    updated = status.updated_number_scheduled or 0
    return desired > 0 and ready >= desired and updated >= desired


def pod_ready(pod: client.V1Pod) -> bool:
    for condition in pod.status.conditions or []:
        if condition.type == "Ready":
            return condition.status == "True"
    return False


def print_pods(namespace: str, label_selector: str) -> None:
    api = client.CoreV1Api()
    try:
        pods = api.list_namespaced_pod(
            namespace=namespace, label_selector=label_selector
        ).items
    except ApiException as exc:
        print(f"Could not list pods in {namespace}: {exc}")
        return
    print(f"Pods in {namespace} matching {label_selector}:")
    for pod in pods:
        ready = "True" if pod_ready(pod) else "False"
        print(
            f"  {pod.metadata.name}\tphase={pod.status.phase}\t"
            f"ready={ready}\tnode={pod.spec.node_name or ''}"
        )


def write_temp_yaml(prefix: str, data: dict[str, Any]) -> Path:
    fd, path = tempfile.mkstemp(prefix=prefix, suffix=".yaml")
    with os.fdopen(fd, "w", encoding="utf-8") as output:
        yaml.safe_dump(data, output, sort_keys=False)
    return Path(path)


def run(
    command: list[str],
    *,
    env: dict[str, str] | None = None,
    stdout: Any | None = None,
) -> subprocess.CompletedProcess[str]:
    print("+ " + shlex.join(command))
    try:
        return subprocess.run(
            command,
            env=env,
            stdout=stdout,
            text=True,
            check=True,
        )
    except FileNotFoundError as exc:
        raise SetupError(f"command not found: {command[0]}") from exc
    except subprocess.CalledProcessError as exc:
        raise SetupError(
            f"command failed with exit code {exc.returncode}: {shlex.join(command)}"
        ) from exc


def tail_file(path: Path, lines: int = 100) -> str:
    try:
        content = path.read_text(encoding="utf-8", errors="replace").splitlines()
    except FileNotFoundError:
        return f"{path} does not exist"
    return "\n".join(content[-lines:])


if __name__ == "__main__":
    raise SystemExit(main())
