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
from typing import Any, Callable

import yaml
from kubernetes import client
from kubernetes.client import ApiException
from packaging.version import Version

from snapshot_e2e import k8s
from snapshot_e2e.infra import preflight


DEFAULT_MODE = "direct"
DEFAULT_TEST_NAMESPACE = "snapshot-e2e"
DEFAULT_SNAPSHOT_RELEASE = "snapshot"
DEFAULT_PVC_NAME = "snapshot-pvc"
DEFAULT_PVC_SIZE = "2Gi"
DEFAULT_VCLUSTER_K8S_VERSION = "v1.32.13"
DEFAULT_VCLUSTER_LOCAL_PORT = 8443
DEFAULT_HELM_TIMEOUT = "6m"
DEFAULT_READY_TIMEOUT_SECONDS = 900
PROGRESS_INTERVAL_SECONDS = 30

SNAPSHOT_LABEL = "app.kubernetes.io/name=snapshot"
AKS_USER_NODE_LABEL = "kubernetes.azure.com/mode"
AKS_USER_NODE_VALUE = "user"


class SetupError(RuntimeError):
    """Raised when setup cannot continue."""


@dataclass(frozen=True)
class SetupResult:
    """Setup outputs consumed by the workflow and local commands.

    target_kubeconfig is always the kubeconfig tests should use. In direct mode
    it points at the configured cluster; in vCluster mode it points at the
    generated vCluster kubeconfig.
    """

    mode: str
    host_namespace: str
    vcluster_name: str
    test_namespace: str
    target_kubeconfig: str
    snapshot_release: str
    pvc_name: str


@dataclass(frozen=True)
class SetupContext:
    workspace: Path
    host_namespace: str
    vcluster_name: str
    target_kubeconfig_value: str


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv)
    try:
        result = setup(args) if args.phase == "all" else setup_phase(args)
    except SetupError as exc:
        print(f"ERROR {exc}", file=sys.stderr)
        return 1

    if result is None:
        return 0

    write_setup_result(args, result)
    return 0


def write_setup_result(args: argparse.Namespace, result: SetupResult) -> None:
    result_data = asdict(result)
    if args.result_file:
        result_path = Path(args.result_file)
        result_path.parent.mkdir(parents=True, exist_ok=True)
        result_path.write_text(json.dumps(result_data, indent=2) + "\n", encoding="utf-8")

    log("Snapshot e2e setup result:")
    for key, value in result_data.items():
        log(f"  {key}: {value}")


def parse_args(argv: list[str] | None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Prepare a Snapshot e2e target cluster."
    )
    parser.add_argument(
        "--phase",
        choices=(
            "all",
            "host-preflight",
            "vcluster",
            "snapshot-install",
            "snapshot-ready",
            "snapshot-uninstall",
        ),
        default=os.environ.get("SNAPSHOT_E2E_SETUP_PHASE", "all"),
        help="Run one setup phase. Default: all.",
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
        default=os.environ.get("SNAPSHOT_E2E_SNAPSHOT_TAG"),
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


def setup_context(args: argparse.Namespace) -> SetupContext:
    """Resolve run-scoped names and kubeconfig paths before any cluster mutation."""
    workspace = Path(args.workspace).resolve()
    run_id = os.environ.get("GITHUB_RUN_ID", "manual")
    run_attempt = os.environ.get("GITHUB_RUN_ATTEMPT", "1")
    default_run_name = f"snapshot-e2e-{run_id}-{run_attempt}"

    host_namespace = args.host_namespace or default_run_name
    vcluster_name = args.vcluster_name or host_namespace

    if args.mode == "vcluster":
        target_kubeconfig = Path(
            args.target_kubeconfig or workspace / ".kubeconfig-snapshot-e2e"
        ).resolve()
        target_kubeconfig_value = str(target_kubeconfig)
    else:
        # SNAPSHOT_E2E_TARGET_KUBECONFIG is vCluster-only. Ignore it in direct
        # mode so a stale localhost vCluster kubeconfig cannot shadow KUBECONFIG.
        target_kubeconfig_value = args.kubeconfig or ""

    return SetupContext(
        workspace=workspace,
        host_namespace=host_namespace,
        vcluster_name=vcluster_name,
        target_kubeconfig_value=target_kubeconfig_value,
    )


def setup(args: argparse.Namespace) -> SetupResult:
    context = setup_context(args)

    run_timed("host preflight", lambda: setup_host_preflight(args))
    if args.mode == "vcluster":
        run_timed("vCluster", lambda: setup_vcluster(args, context))
    run_timed("Snapshot install", lambda: setup_snapshot_install(args, context))
    run_timed("Snapshot readiness", lambda: setup_snapshot_readiness(args, context))
    return setup_result(args, context)


def setup_phase(args: argparse.Namespace) -> SetupResult | None:
    context = setup_context(args)
    if args.phase == "host-preflight":
        run_timed("host preflight", lambda: setup_host_preflight(args))
        return None
    if args.phase == "vcluster":
        if args.mode != "vcluster":
            raise SetupError("--phase vcluster requires --mode vcluster")
        run_timed("vCluster", lambda: setup_vcluster(args, context))
        return None
    if args.phase == "snapshot-install":
        run_timed("Snapshot install", lambda: setup_snapshot_install(args, context))
        return None
    if args.phase == "snapshot-ready":
        run_timed("Snapshot readiness", lambda: setup_snapshot_readiness(args, context))
        return setup_result(args, context)
    if args.phase == "snapshot-uninstall":
        run_timed("Snapshot uninstall", lambda: setup_snapshot_uninstall(args, context))
        return None
    raise SetupError(f"unsupported setup phase: {args.phase}")


def setup_host_preflight(args: argparse.Namespace) -> None:
    log("Loading host kubeconfig")
    preflight.load_config(args.kubeconfig, args.context)
    if args.skip_host_preflight:
        log("Skipping host preflight")
        return
    run_host_preflight()


def setup_vcluster(args: argparse.Namespace, context: SetupContext) -> None:
    """Create the virtual test cluster and write the kubeconfig used by later phases."""
    log("Loading host kubeconfig")
    preflight.load_config(args.kubeconfig, args.context)
    target_kubeconfig = Path(context.target_kubeconfig_value)
    create_host_namespace(context.host_namespace)
    ensure_vcluster_unused(context.host_namespace, context.vcluster_name)
    create_vcluster(
        context.host_namespace,
        context.vcluster_name,
        args.vcluster_k8s_version,
    )
    install_hostpath_mapper(
        context.host_namespace,
        context.vcluster_name,
        args.helm_timeout,
    )
    connect_vcluster(
        host_namespace=context.host_namespace,
        vcluster_name=context.vcluster_name,
        target_kubeconfig=target_kubeconfig,
        local_port=args.vcluster_local_port,
        workspace=context.workspace,
    )


def setup_snapshot_install(args: argparse.Namespace, context: SetupContext) -> None:
    log("Loading target kubeconfig")
    if not args.snapshot_tag:
        raise SetupError("--snapshot-tag or SNAPSHOT_E2E_SNAPSHOT_TAG is required")
    preflight.load_config(context.target_kubeconfig_value or None, None)
    ensure_snapshot_release_can_own_cluster_resources(
        args.test_namespace,
        args.snapshot_release,
    )
    ensure_target_namespace(args.test_namespace)
    ensure_checkpoint_pvc(
        namespace=args.test_namespace,
        name=args.pvc_name,
        size=args.pvc_size,
    )
    install_snapshot_chart(
        kubeconfig=context.target_kubeconfig_value or None,
        namespace=args.test_namespace,
        release=args.snapshot_release,
        image_tag=args.snapshot_tag,
        timeout=args.helm_timeout,
    )


def setup_snapshot_readiness(args: argparse.Namespace, context: SetupContext) -> None:
    log("Loading target kubeconfig")
    preflight.load_config(context.target_kubeconfig_value or None, None)
    wait_for_snapshot_readiness(
        namespace=args.test_namespace,
        release=args.snapshot_release,
        timeout_seconds=args.ready_timeout_seconds,
    )


def setup_snapshot_uninstall(args: argparse.Namespace, context: SetupContext) -> None:
    log("Loading target kubeconfig")
    preflight.load_config(context.target_kubeconfig_value or None, None)
    failures = []
    try:
        uninstall_snapshot_chart(
            kubeconfig=context.target_kubeconfig_value or None,
            namespace=args.test_namespace,
            release=args.snapshot_release,
            timeout=args.helm_timeout,
        )
    except SetupError as exc:
        failures.append(f"chart uninstall: {exc}")

    try:
        delete_checkpoint_pvc(namespace=args.test_namespace, name=args.pvc_name)
    except SetupError as exc:
        failures.append(f"PVC deletion: {exc}")

    if failures:
        raise SetupError("snapshot uninstall incomplete: " + "; ".join(failures))


def setup_result(args: argparse.Namespace, context: SetupContext) -> SetupResult:
    return SetupResult(
        mode=args.mode,
        host_namespace=context.host_namespace if args.mode == "vcluster" else "",
        vcluster_name=context.vcluster_name if args.mode == "vcluster" else "",
        test_namespace=args.test_namespace,
        target_kubeconfig=context.target_kubeconfig_value,
        snapshot_release=args.snapshot_release,
        pvc_name=args.pvc_name,
    )


def log(message: str) -> None:
    print(f"[{time.strftime('%H:%M:%S')}] {message}", flush=True)


def run_timed(label: str, fn: Callable[[], Any]) -> Any:
    start = time.monotonic()
    log(f"Starting {label}")
    try:
        result = fn()
    except Exception:
        log(f"Failed {label} after {elapsed(start)}")
        raise
    log(f"Finished {label} in {elapsed(start)}")
    return result


def elapsed(start: float) -> str:
    return f"{time.monotonic() - start:.1f}s"


def run_host_preflight() -> None:
    log("Running host preflight")
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
        log(f"{status} {result.name}: {result.detail}")
        failed = failed or not result.ok
    if failed:
        raise SetupError("host preflight failed")


def create_host_namespace(namespace: str) -> None:
    labels = {
        "snapshot-e2e": "true",
        "snapshot.github/run-id": os.environ.get("GITHUB_RUN_ID", "manual"),
        "snapshot.github/run-attempt": os.environ.get("GITHUB_RUN_ATTEMPT", "1"),
    }
    ensure_namespace(namespace, labels=labels)


def ensure_namespace(namespace: str, labels: dict[str, str] | None = None) -> None:
    api = client.CoreV1Api()
    body = client.V1Namespace(
        metadata=client.V1ObjectMeta(name=namespace, labels=labels or {})
    )
    try:
        api.create_namespace(body)
        log(f"Created namespace {namespace}")
    except ApiException as exc:
        if exc.status != 409:
            raise SetupError(f"failed to create namespace {namespace}: {exc}") from exc
        log(f"Namespace {namespace} already exists")

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
    log(f"vCluster name {namespace}/{name} is available")


def create_vcluster(namespace: str, name: str, k8s_version: str) -> None:
    log(f"Creating vCluster {namespace}/{name}")
    synced_nodes = {
        "enabled": True,
        "clearImageStatus": True,
        "selector": {
            "all": True,
        },
    }
    node_selector = vcluster_node_sync_selector_labels()
    if node_selector:
        synced_nodes["selector"]["labels"] = node_selector

    values = {
        "controlPlane": {
            "hostPathMapper": {
                "enabled": True,
            },
        },
        "sync": {
            "fromHost": {
                "nodes": synced_nodes,
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


def vcluster_node_sync_selector_labels() -> dict[str, str]:
    """Keep the AKS CI node filter when available; local clusters usually lack it."""
    try:
        nodes = client.CoreV1Api().list_node().items
    except ApiException as exc:
        raise SetupError(f"failed to list nodes for vCluster node sync: {exc}") from exc

    for node in nodes:
        labels = node.metadata.labels or {}
        if labels.get(AKS_USER_NODE_LABEL) == AKS_USER_NODE_VALUE:
            # This matches the current Dynamo AKS runner layout. If that cluster
            # layout changes, revisit which host nodes the vCluster should sync.
            log(
                "Using AKS user-node selector for vCluster node sync: "
                f"{AKS_USER_NODE_LABEL}={AKS_USER_NODE_VALUE}"
            )
            return {AKS_USER_NODE_LABEL: AKS_USER_NODE_VALUE}

    log("No AKS user-node label found; vCluster will sync all host nodes")
    return {}


def install_hostpath_mapper(namespace: str, vcluster_name: str, helm_timeout: str) -> None:
    """Install HostPath Mapper so vCluster hostPath volumes map to real host paths.

    Snapshot agents mount kubelet/containerd host paths; without this mapper,
    vCluster rewrites those paths under /tmp/vcluster and the DaemonSet cannot start.
    """
    log(f"Installing vCluster HostPath Mapper in {namespace}")
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
    """Create a local API tunnel and render a kubeconfig that points at the vCluster."""
    log(f"Connecting to vCluster {host_namespace}/{vcluster_name}")
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
        temp_kubeconfig = None
        try:
            with tempfile.NamedTemporaryFile(
                "w",
                encoding="utf-8",
                dir=target_kubeconfig.parent,
                prefix=f".{target_kubeconfig.name}.",
                suffix=".tmp",
                delete=False,
            ) as output:
                temp_kubeconfig = Path(output.name)
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
            if temp_kubeconfig.stat().st_size == 0:
                raise SetupError("vcluster connect produced an empty kubeconfig")
            temp_kubeconfig.replace(target_kubeconfig)
        except Exception:
            if temp_kubeconfig:
                temp_kubeconfig.unlink(missing_ok=True)
            raise
        target_kubeconfig.chmod(0o600)
    except Exception:
        terminate_process(process)
        raise
    finally:
        log_file.close()


def terminate_process(process: subprocess.Popen[Any]) -> None:
    if process.poll() is not None:
        return
    process.terminate()
    try:
        process.wait(timeout=10)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait(timeout=10)


def wait_for_https(url: str, process: subprocess.Popen[Any], log_path: Path) -> None:
    context = ssl.create_default_context()
    context.check_hostname = False
    context.verify_mode = ssl.CERT_NONE
    last_report = 0.0
    for _ in range(60):
        if process.poll() is not None:
            raise SetupError(
                "vCluster port-forward exited early; tail:\n" + tail_file(log_path)
            )
        try:
            with urllib.request.urlopen(url, timeout=2, context=context):
                log(f"vCluster API reachable at {url}")
                return
        except (urllib.error.URLError, TimeoutError):
            last_report = progress(
                "vCluster API",
                f"waiting for {url}; port-forward pid={process.pid}",
                last_report,
            )
            time.sleep(2)
    raise SetupError(
        "vCluster API was not reachable through the local port-forward; tail:\n"
        + tail_file(log_path)
    )


def ensure_target_namespace(namespace: str) -> None:
    ensure_namespace(namespace)


def ensure_snapshot_release_can_own_cluster_resources(
    namespace: str,
    release: str,
) -> None:
    """Fail clearly when another Snapshot Helm release owns cluster-scoped RBAC.

    Direct-cluster mode can share a real cluster with older manual installs.
    Snapshot creates ClusterRoles/ClusterRoleBindings, so Helm cannot install a
    second release with the same names in a different namespace.
    """
    api = client.RbacAuthorizationV1Api()
    fullname = snapshot_chart_fullname(release)
    resources = [
        ("ClusterRole", f"{fullname}-operator", api.read_cluster_role),
        ("ClusterRole", f"{fullname}-agent-podsnapshotcontents", api.read_cluster_role),
        ("ClusterRole", f"{fullname}-agent-resourceslices", api.read_cluster_role),
        ("ClusterRole", f"{fullname}-agent", api.read_cluster_role),
        ("ClusterRoleBinding", f"{fullname}-operator", api.read_cluster_role_binding),
        (
            "ClusterRoleBinding",
            f"{fullname}-agent-podsnapshotcontents",
            api.read_cluster_role_binding,
        ),
        (
            "ClusterRoleBinding",
            f"{fullname}-agent-resourceslices",
            api.read_cluster_role_binding,
        ),
        ("ClusterRoleBinding", f"{fullname}-agent", api.read_cluster_role_binding),
    ]

    conflicts = []
    for kind, name, read in resources:
        try:
            resource = read(name)
        except ApiException as exc:
            if exc.status == 404:
                continue
            raise SetupError(f"failed to check {kind} {name}: {exc}") from exc

        annotations = resource.metadata.annotations or {}
        owner_name = annotations.get("meta.helm.sh/release-name")
        owner_namespace = annotations.get("meta.helm.sh/release-namespace")
        if owner_name == release and owner_namespace == namespace:
            continue

        owner = (
            f"Helm release {owner_namespace}/{owner_name}"
            if owner_name or owner_namespace
            else "no Helm owner annotations"
        )
        conflicts.append(f"{kind}/{name} owned by {owner}")

    if conflicts:
        raise SetupError(
            "Snapshot cluster-scoped resources already exist for this release name. "
            "Uninstall the old Snapshot Helm release, or run direct mode with the "
            "same SNAPSHOT_E2E_TEST_NAMESPACE/SNAPSHOT_E2E_SNAPSHOT_RELEASE. "
            "Conflicts: "
            + "; ".join(conflicts[:5])
        )


def snapshot_chart_fullname(release: str) -> str:
    name = release if "snapshot" in release else f"{release}-snapshot"
    return name[:63].rstrip("-")


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
        log(f"Created PVC {namespace}/{name}: phase={pvc.status.phase}")
    except ApiException as exc:
        if exc.status != 409:
            raise SetupError(f"failed to create PVC {namespace}/{name}: {exc}") from exc
        pvc = api.read_namespaced_persistent_volume_claim(name, namespace)
        requested = (pvc.spec.resources.requests or {}).get("storage")
        access_modes = list(pvc.spec.access_modes or [])
        detail = f"phase={pvc.status.phase}"
        mismatches = []
        if requested != size:
            mismatches.append(f"requested storage={requested}, configured size={size}")
        if access_modes != ["ReadWriteOnce"]:
            mismatches.append(f"accessModes={access_modes}, expected ['ReadWriteOnce']")
        if mismatches:
            raise SetupError(
                f"PVC {namespace}/{name} already exists with incompatible spec: "
                + ", ".join(mismatches)
            )
        log(f"PVC {namespace}/{name} already exists: {detail}")


def install_snapshot_chart(
    *,
    kubeconfig: str | None,
    namespace: str,
    release: str,
    image_tag: str,
    timeout: str,
) -> None:
    log(f"Installing Snapshot chart release {namespace}/{release}")
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


def uninstall_snapshot_chart(
    *,
    kubeconfig: str | None,
    namespace: str,
    release: str,
    timeout: str,
) -> None:
    log(f"Uninstalling Snapshot chart release {namespace}/{release}")
    command = [
        "helm",
        "uninstall",
        release,
        "--namespace",
        namespace,
        "--ignore-not-found",
        "--wait",
        "--timeout",
        timeout,
    ]
    env = os.environ.copy()
    if kubeconfig:
        env["KUBECONFIG"] = kubeconfig
    run(command, env=env)


def delete_checkpoint_pvc(namespace: str, name: str) -> None:
    api = client.CoreV1Api()
    try:
        api.delete_namespaced_persistent_volume_claim(name=name, namespace=namespace)
        log(f"Deleted PVC {namespace}/{name}")
    except ApiException as exc:
        if exc.status == 404:
            log(f"PVC {namespace}/{name} does not exist")
            return
        raise SetupError(f"failed to delete PVC {namespace}/{name}: {exc}") from exc


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
    wait_for_pods_ready(
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
    last_report = 0.0
    last_detail = "not checked"
    while time.monotonic() < deadline:
        pods = api.list_namespaced_pod(
            namespace=namespace, label_selector=label_selector
        ).items
        last_detail = pod_progress(pods)
        if pods and all(pod_ready(pod) for pod in pods):
            log(f"{description} ready: {last_detail}")
            return
        last_report = progress(description, last_detail, last_report)
        time.sleep(5)
    print_pods(namespace, label_selector)
    raise SetupError(f"timed out waiting for {description}; last state: {last_detail}")


def wait_for_daemonset_rollout_by_selector(
    *,
    namespace: str,
    label_selector: str,
    timeout_seconds: int,
    description: str,
) -> None:
    apps_api = client.AppsV1Api()
    core_api = client.CoreV1Api()
    deadline = time.monotonic() + timeout_seconds
    last_report = 0.0
    last_detail = "not checked"
    while time.monotonic() < deadline:
        daemonsets = apps_api.list_namespaced_daemon_set(
            namespace=namespace, label_selector=label_selector
        ).items
        pods = core_api.list_namespaced_pod(
            namespace=namespace, label_selector=label_selector
        ).items
        last_detail = f"{daemonset_progress(daemonsets)}; pods: {pod_progress(pods)}"
        if (
            daemonsets
            and all(k8s.daemonset_scheduled(item) for item in daemonsets)
            and pods
            and all(pod_ready(pod) for pod in pods)
        ):
            log(f"{description} daemonset ready: {last_detail}")
            return
        last_report = progress(f"{description} daemonset", last_detail, last_report)
        time.sleep(5)
    raise SetupError(
        f"timed out waiting for {description} daemonset; last state: {last_detail}"
    )


def wait_for_daemonset_rollout(
    *,
    namespace: str,
    name: str,
    timeout_seconds: int,
) -> None:
    api = client.AppsV1Api()
    deadline = time.monotonic() + timeout_seconds
    last_report = 0.0
    last_detail = "not checked"
    while time.monotonic() < deadline:
        try:
            daemonset = api.read_namespaced_daemon_set(name=name, namespace=namespace)
        except ApiException as exc:
            if exc.status != 404:
                raise SetupError(
                    f"failed to read daemonset {namespace}/{name}: {exc}"
                ) from exc
            last_detail = "not found"
            last_report = progress(
                f"daemonset {namespace}/{name}",
                last_detail,
                last_report,
            )
            time.sleep(5)
            continue
        last_detail = daemonset_progress([daemonset])
        if k8s.daemonset_ready(daemonset):
            log(f"DaemonSet {namespace}/{name} ready: {last_detail}")
            return
        last_report = progress(
            f"daemonset {namespace}/{name}",
            last_detail,
            last_report,
        )
        time.sleep(5)
    raise SetupError(
        f"timed out waiting for daemonset {namespace}/{name}; last state: {last_detail}"
    )


def progress(description: str, detail: str, last_report: float) -> float:
    now = time.monotonic()
    if last_report == 0.0 or now - last_report >= PROGRESS_INTERVAL_SECONDS:
        log(f"Waiting for {description}: {detail}")
        return now
    return last_report


def pod_progress(pods: list[client.V1Pod]) -> str:
    if not pods:
        return "no pods found"
    details = []
    for pod in pods[:10]:
        statuses = []
        for status in pod.status.container_statuses or []:
            statuses.append(f"{status.name}:{status.ready}")
        if not statuses:
            statuses.append("no container statuses")
        details.append(
            f"{pod.metadata.name} phase={pod.status.phase} "
            f"ready={pod_ready(pod)} node={pod.spec.node_name or '<none>'} "
            f"containers={','.join(statuses)}"
        )
    remaining = len(pods) - len(details)
    if remaining > 0:
        details.append(f"... {remaining} more pod(s)")
    return "; ".join(details)


def daemonset_progress(daemonsets: list[client.V1DaemonSet]) -> str:
    if not daemonsets:
        return "no daemonsets found"
    return "; ".join(daemonset_detail(item) for item in daemonsets)


def daemonset_detail(daemonset: client.V1DaemonSet) -> str:
    status = daemonset.status
    desired = status.desired_number_scheduled or 0
    ready = status.number_ready or 0
    updated = status.updated_number_scheduled or 0
    unavailable = status.number_unavailable or 0
    return (
        f"{daemonset.metadata.name} desired={desired} ready={ready} "
        f"updated={updated} unavailable={unavailable} "
        f"ready_status={k8s.daemonset_ready(daemonset)}"
    )


def pod_ready(pod: client.V1Pod) -> bool:
    statuses = list(pod.status.container_statuses or [])
    if statuses:
        return all(status.ready for status in statuses)

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
    log(f"Pods in {namespace} matching {label_selector}:")
    for pod in pods:
        ready = "True" if pod_ready(pod) else "False"
        log(
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
    log("+ " + shlex.join(command))
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
