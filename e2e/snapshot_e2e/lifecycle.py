# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Small helpers for Snapshot functional e2e tests."""

from __future__ import annotations

import shlex
import time
from typing import Any, Callable

from kubernetes import client
from kubernetes.client import ApiException

from snapshot_e2e import k8s
from snapshot_e2e.workloads import CONTAINER
from snapshot_e2e.workloads import FILE_TOKEN
from snapshot_e2e.workloads import OBSERVATIONS
from snapshot_e2e.workloads import RESTORE_DONE
from snapshot_e2e.workloads import RESTORE_INITIAL_TOKEN
from snapshot_e2e.workloads import SOURCE_READY
from snapshot_e2e.workloads import TestRun
from snapshot_e2e.workloads import restore_pod
from snapshot_e2e.workloads import source_pod


GROUP = "nvidia.com"
VERSION = "v1alpha1"
PODSNAPSHOTS = "podsnapshots"
PODSNAPSHOTCONTENTS = "podsnapshotcontents"
SNAPSHOTJOBS = "snapshotjobs"
PROGRESS_INTERVAL_SECONDS = 30
TERMINAL_POD_PHASES = {"Failed", "Succeeded"}
AGENT_CHECKPOINT_DIR = "/checkpoints"


def wait_for_pod_deleted(namespace: str, name: str, timeout: int = 180) -> None:
    def gone() -> bool | None:
        try:
            k8s.read_pod(namespace, name)
        except ApiException as exc:
            if exc.status == 404:
                return True
            raise
        return None

    def detail() -> str:
        try:
            pod = k8s.read_pod(namespace, name)
        except ApiException as exc:
            return f"api_error={k8s.api_error_detail(exc)}"
        return f"phase={pod.status.phase} node={pod.spec.node_name or '<none>'}"

    wait_for(f"pod {namespace}/{name} deleted", gone, timeout, detail=detail)


def create_podsnapshot(
    namespace: str,
    name: str,
    pod_name: str,
    pod_uid: str,
    container: str = CONTAINER,
) -> dict[str, Any]:
    body = {
        "apiVersion": f"{GROUP}/{VERSION}",
        "kind": "PodSnapshot",
        "metadata": {"name": name, "namespace": namespace},
        "spec": {
            "source": {
                "podRef": {
                    "name": pod_name,
                    "uid": pod_uid,
                    "containers": [container],
                }
            }
        },
    }
    return client.CustomObjectsApi().create_namespaced_custom_object(
        GROUP,
        VERSION,
        namespace,
        PODSNAPSHOTS,
        body,
    )


def create_snapshotjob(
    namespace: str,
    name: str,
    pod_template: dict[str, Any],
    *,
    target_containers: list[str] | None = None,
    active_deadline_seconds: int | None = None,
) -> dict[str, Any]:
    spec: dict[str, Any] = {
        "podTemplate": pod_template,
        "podSnapshotTemplate": {"targetContainers": target_containers or [CONTAINER]},
    }
    if active_deadline_seconds is not None:
        spec["activeDeadlineSeconds"] = active_deadline_seconds
    body = {
        "apiVersion": f"{GROUP}/{VERSION}",
        "kind": "SnapshotJob",
        "metadata": {"name": name, "namespace": namespace},
        "spec": spec,
    }
    return client.CustomObjectsApi().create_namespaced_custom_object(
        GROUP,
        VERSION,
        namespace,
        SNAPSHOTJOBS,
        body,
    )


def wait_for_job_source_pod(namespace: str, job_name: str, timeout: int = 300) -> client.V1Pod:
    """Waits for the batch/v1 Job's pod to exist and returns it, by name unknown
    in advance (the Job controller appends a random suffix to job_name).
    """

    def found() -> client.V1Pod | None:
        pods = k8s.list_job_pods(namespace, job_name)
        return pods[0] if pods else None

    def detail() -> str:
        return f"pods={[p.metadata.name for p in k8s.list_job_pods(namespace, job_name)]}"

    return wait_for(f"pod for Job {namespace}/{job_name}", found, timeout, detail=detail)


def wait_for_pod_ready(namespace: str, name: str, timeout: int = 600) -> client.V1Pod:
    def ready() -> client.V1Pod | None:
        pod = k8s.read_pod(namespace, name)
        if k8s.pod_containers_ready(pod):
            return pod
        if pod.status.phase in TERMINAL_POD_PHASES:
            raise AssertionError(
                f"pod {namespace}/{name} reached phase {pod.status.phase} before Ready"
            )
        return None

    def detail() -> str:
        try:
            pod = k8s.read_pod(namespace, name)
        except ApiException as exc:
            return f"api_error={k8s.api_error_detail(exc)}"
        return k8s.pod_readiness_detail(pod)

    return wait_for(f"pod {namespace}/{name} Ready", ready, timeout, detail=detail)


def wait_for_file(namespace: str, pod: str, path: str, timeout: int = 180) -> None:
    last_error: str | None = None
    marker = "__snapshot_e2e_file_present__"

    def exists() -> bool | None:
        nonlocal last_error
        try:
            # Require a stdout marker because exec does not expose remote exit status.
            command = (
                f"[[ -f {shlex.quote(path)} ]] && "
                f"printf '%s' {shlex.quote(marker)}"
            )
            response = k8s.exec_command(namespace, pod, command)
            last_error = None
            return True if response == marker else None
        except Exception as exc:
            last_error = f"{type(exc).__name__}: {exc}"
            return None

    def detail() -> str:
        return f"last_error={last_error}" if last_error else "file not observed yet"

    wait_for(f"{namespace}/{pod}:{path}", exists, timeout, detail=detail)


def matching_observation_count(
    namespace: str,
    pod: str,
    token: str,
    *,
    gpu: bool,
) -> int:
    expected_gpu = token if gpu else "disabled"
    command = (
        f"test -f {OBSERVATIONS} || {{ echo 0; exit 0; }}; "
        f"grep -F {shlex.quote('cpu=' + token)} {OBSERVATIONS} | "
        f"grep -F {shlex.quote('file=' + token)} | "
        f"grep -F {shlex.quote('gpu=' + expected_gpu)} | "
        "wc -l"
    )
    output = k8s.exec_command(namespace, pod, command)
    return int(output.strip() or "0")


def wait_for_state_observations(
    namespace: str,
    pod: str,
    token: str,
    *,
    gpu: bool,
    minimum: int,
    timeout: int = 180,
) -> int:
    def check() -> int | None:
        count = matching_observation_count(namespace, pod, token, gpu=gpu)
        return count if count >= minimum else None

    return wait_for(
        f"{namespace}/{pod} observations for source token >= {minimum}",
        check,
        timeout,
        detail=lambda: observations_tail(namespace, pod),
    )


def observations_tail(namespace: str, pod: str) -> str:
    return k8s.exec_command(
        namespace,
        pod,
        f"test -f {OBSERVATIONS} && tail -5 {OBSERVATIONS} || echo '<no observations>'",
    ).strip()


def wait_for_snapshot_ready(
    namespace: str,
    name: str,
    timeout: int = 600,
) -> tuple[dict[str, Any], dict[str, Any]]:
    snap = wait_for_condition(
        namespace,
        name,
        plural=PODSNAPSHOTS,
        condition_type="Ready",
        timeout=timeout,
    )
    content_name = snap.get("status", {}).get("boundSnapshotContentName")
    if not content_name:
        raise AssertionError(f"PodSnapshot {namespace}/{name} is Ready without bound content")
    content = wait_for_condition(
        None,
        content_name,
        plural=PODSNAPSHOTCONTENTS,
        condition_type="Ready",
        timeout=timeout,
    )
    return snap, content


def wait_for_snapshot_failed(
    namespace: str,
    name: str,
    timeout: int = 300,
) -> tuple[dict[str, Any], dict[str, Any] | None]:
    snap = wait_for_condition(
        namespace,
        name,
        plural=PODSNAPSHOTS,
        condition_type="Failed",
        timeout=timeout,
    )
    content_name = snap.get("status", {}).get("boundSnapshotContentName")
    content = None
    if content_name:
        content = wait_for_condition(
            None,
            content_name,
            plural=PODSNAPSHOTCONTENTS,
            condition_type="Failed",
            timeout=timeout,
        )
    return snap, content


def wait_for_condition(
    namespace: str | None,
    name: str,
    *,
    plural: str,
    condition_type: str,
    timeout: int,
) -> dict[str, Any]:
    api = client.CustomObjectsApi()

    def check() -> dict[str, Any] | None:
        obj = get_custom_object(api, namespace, name, plural)
        cond = condition(obj, condition_type)
        if cond and cond.get("status") == "True":
            return obj
        failed = condition(obj, "Failed")
        if condition_type != "Failed" and failed and failed.get("status") == "True":
            raise AssertionError(f"{plural}/{name} failed: {failed}")
        return None

    def detail() -> str:
        try:
            obj = get_custom_object(api, namespace, name, plural)
        except ApiException as exc:
            return f"api_error={k8s.api_error_detail(exc)}"
        return f"conditions={obj.get('status', {}).get('conditions', [])}"

    return wait_for(
        f"{plural}/{name} {condition_type}=True",
        check,
        timeout,
        detail=detail,
    )


def get_custom_object(
    api: client.CustomObjectsApi,
    namespace: str | None,
    name: str,
    plural: str,
) -> dict[str, Any]:
    if namespace:
        return api.get_namespaced_custom_object(GROUP, VERSION, namespace, plural, name)
    return api.get_cluster_custom_object(GROUP, VERSION, plural, name)


def condition(obj: dict[str, Any], condition_type: str) -> dict[str, Any] | None:
    for item in obj.get("status", {}).get("conditions", []) or []:
        if item.get("type") == condition_type:
            return item
    return None


def wait_for_restore_status(
    namespace: str,
    pod_name: str,
    status: str,
    timeout: int = 600,
) -> client.V1Pod:
    key = "nvidia.com/snapshot-restore-status.main"

    def check() -> client.V1Pod | None:
        pod = k8s.read_pod(namespace, pod_name)
        actual = (pod.metadata.annotations or {}).get(key)
        if actual == status:
            return pod
        if status != "failed" and actual == "failed":
            raise AssertionError(f"restore failed for {namespace}/{pod_name}")
        return None

    def detail() -> str:
        try:
            pod = k8s.read_pod(namespace, pod_name)
        except ApiException as exc:
            return f"api_error={k8s.api_error_detail(exc)}"
        actual = (pod.metadata.annotations or {}).get(key, "<unset>")
        return f"{key}={actual}"

    return wait_for(
        f"restore status {status} on {namespace}/{pod_name}",
        check,
        timeout,
        detail=detail,
    )


def checkpoint_artifact_manifest(
    config: k8s.E2EConfig, node: str, checkpoint_id: str
) -> str:
    return k8s.exec_command(
        config.namespace,
        checkpoint_agent_pod(config, node),
        f"cat {checkpoint_artifact_path(checkpoint_id)}/manifest.yaml",
    )


def checkpoint_artifact_listing(
    config: k8s.E2EConfig, node: str, checkpoint_id: str
) -> str:
    return k8s.exec_command(
        config.namespace,
        checkpoint_agent_pod(config, node),
        f"cd {checkpoint_artifact_path(checkpoint_id)} && "
        "find . -maxdepth 1 -type f -print | sort && "
        "tar -tf rootfs-diff.tar | sort",
    )


def checkpoint_rootfs_file(
    config: k8s.E2EConfig,
    node: str,
    checkpoint_id: str,
    path: str,
) -> str:
    return k8s.exec_command(
        config.namespace,
        checkpoint_agent_pod(config, node),
        f"cd {checkpoint_artifact_path(checkpoint_id)} && "
        f"tar -xOf rootfs-diff.tar {path}",
    )


def checkpoint_artifact_path(checkpoint_id: str) -> str:
    return shlex.quote(f"{AGENT_CHECKPOINT_DIR}/{checkpoint_id}/versions/1")


def checkpoint_agent_pod(config: k8s.E2EConfig, node: str) -> str:
    agents = [
        pod
        for pod in k8s.list_snapshot_pods(
            config.namespace, config.release, "snapshot-agent"
        )
        if pod.spec.node_name == node
    ]
    if len(agents) != 1:
        names = [pod.metadata.name for pod in agents]
        raise AssertionError(
            f"expected one snapshot agent on node {node!r}, found {names}"
        )
    return agents[0].metadata.name


def assert_restored_state(
    namespace: str,
    pod: str,
    *,
    source_token: str,
    restore_token: str,
    checkpoint_observations: int,
    gpu: bool,
) -> str:
    expected_gpu = source_token if gpu else "disabled"
    command = f"""
    set -euo pipefail
    source_token={shlex.quote(source_token)}
    restore_token={shlex.quote(restore_token)}
    expected_gpu={shlex.quote(expected_gpu)}
    test -f {RESTORE_DONE}
    test "$(cat {RESTORE_INITIAL_TOKEN})" = "$restore_token"
    test "$(cat {FILE_TOKEN})" = "$source_token"
    grep -F "cpu=$source_token" {OBSERVATIONS}
    grep -F "file=$source_token" {OBSERVATIONS}
    grep -F "gpu=$expected_gpu" {OBSERVATIONS}
    if grep -F "$restore_token" {OBSERVATIONS}; then
      echo "restore token appeared in restored observations"
      exit 1
    fi
    before=$(awk '/^observation / {{count++}} END {{print count+0}}' {OBSERVATIONS})
    sleep 12
    after=$(awk '/^observation / {{count++}} END {{print count+0}}' {OBSERVATIONS})
    echo "source_token=$source_token restore_token=$restore_token checkpoint_observations={checkpoint_observations} before=$before after=$after"
    test "$before" -ge "{checkpoint_observations}"
    test "$after" -gt "$before"
    """
    return k8s.exec_command(namespace, pod, command)


def debug_dump(config: k8s.E2EConfig, run: TestRun) -> None:
    print("\n--- snapshot e2e debug ---")
    print(f"namespace={config.namespace} test={run.suffix}")
    core = client.CoreV1Api()
    pods = core.list_namespaced_pod(
        config.namespace, label_selector=f"snapshot-e2e-test={run.suffix}"
    ).items
    for pod in pods:
        print(f"pod {pod.metadata.name} phase={pod.status.phase} node={pod.spec.node_name}")
        print(f"annotations={pod.metadata.annotations or {}}")
        print(k8s.pod_logs(config.namespace, pod.metadata.name, tail_lines=80))
    print_custom_objects(config, run)
    print_snapshot_controller_logs(config)
    events = core.list_namespaced_event(config.namespace).items
    for event in events[-30:]:
        involved = event.involved_object
        if involved and involved.name in {run.source_pod, run.restore_pod, run.snapshot_name}:
            print(f"event {event.reason}: {event.message}")
    print("--- end debug ---\n")


def print_custom_objects(config: k8s.E2EConfig, run: TestRun) -> None:
    api = client.CustomObjectsApi()
    try:
        snap = api.get_namespaced_custom_object(
            GROUP, VERSION, config.namespace, PODSNAPSHOTS, run.snapshot_name
        )
        print(f"PodSnapshot conditions={snap.get('status', {}).get('conditions', [])}")
        content_name = snap.get("status", {}).get("boundSnapshotContentName")
        if content_name:
            content = api.get_cluster_custom_object(
                GROUP, VERSION, PODSNAPSHOTCONTENTS, content_name
            )
            print(
                "PodSnapshotContent "
                f"{content_name} conditions={content.get('status', {}).get('conditions', [])}"
            )
    except ApiException as exc:
        print(f"Snapshot CR debug unavailable: {k8s.api_error_detail(exc)}")


def print_snapshot_controller_logs(config: k8s.E2EConfig) -> None:
    core = client.CoreV1Api()
    try:
        pods = core.list_namespaced_pod(
            config.namespace, label_selector="app.kubernetes.io/name=snapshot"
        ).items
    except ApiException as exc:
        print(f"Snapshot controller logs unavailable: {k8s.api_error_detail(exc)}")
        return
    for pod in pods[:8]:
        print(f"snapshot pod {pod.metadata.name} phase={pod.status.phase}")
        print(k8s.pod_logs(config.namespace, pod.metadata.name, tail_lines=50))


def cleanup(config: k8s.E2EConfig, run: TestRun) -> None:
    api = client.CustomObjectsApi()
    for pod_name in (run.restore_pod, run.source_pod):
        if k8s.delete_pod(config.namespace, pod_name):
            try:
                wait_for_pod_deleted(config.namespace, pod_name)
            except AssertionError as exc:
                print(f"cleanup warning: {exc}")
    try:
        api.delete_namespaced_custom_object(
            GROUP,
            VERSION,
            config.namespace,
            PODSNAPSHOTS,
            run.snapshot_name,
        )
    except ApiException as exc:
        if exc.status != 404:
            raise

    contents = api.list_cluster_custom_object(GROUP, VERSION, PODSNAPSHOTCONTENTS)
    for item in contents.get("items", []):
        ref = item.get("spec", {}).get("snapshotRef", {})
        if ref.get("namespace") == config.namespace and ref.get("name") == run.snapshot_name:
            try:
                api.delete_cluster_custom_object(
                    GROUP,
                    VERSION,
                    PODSNAPSHOTCONTENTS,
                    item["metadata"]["name"],
                )
            except ApiException as exc:
                if exc.status != 404:
                    raise


def cleanup_snapshotjob(config: k8s.E2EConfig, run: TestRun) -> None:
    """Cleanup for a SnapshotJob-driven test run.

    Deleting the SnapshotJob cascades to its source Job (controller
    ownerReference) and that Job's pod. The PodSnapshot it produces
    deliberately carries no ownerReference (artifacts must outlive the
    SnapshotJob), so it — and its bound PodSnapshotContent — need the same
    explicit cleanup as the plain PodSnapshot flow. The SnapshotJob's own name
    and its produced PodSnapshot's name are both run.checkpoint_id: buildSourceJob
    passes CheckpointID: sj.Name, and buildPodSnapshot names the PodSnapshot
    after the SnapshotJob.
    """
    api = client.CustomObjectsApi()
    if k8s.delete_pod(config.namespace, run.restore_pod):
        try:
            wait_for_pod_deleted(config.namespace, run.restore_pod)
        except AssertionError as exc:
            print(f"cleanup warning: {exc}")

    try:
        api.delete_namespaced_custom_object(
            GROUP,
            VERSION,
            config.namespace,
            SNAPSHOTJOBS,
            run.checkpoint_id,
        )
    except ApiException as exc:
        if exc.status != 404:
            raise

    try:
        api.delete_namespaced_custom_object(
            GROUP,
            VERSION,
            config.namespace,
            PODSNAPSHOTS,
            run.checkpoint_id,
        )
    except ApiException as exc:
        if exc.status != 404:
            raise

    contents = api.list_cluster_custom_object(GROUP, VERSION, PODSNAPSHOTCONTENTS)
    for item in contents.get("items", []):
        ref = item.get("spec", {}).get("snapshotRef", {})
        if ref.get("namespace") == config.namespace and ref.get("name") == run.checkpoint_id:
            try:
                api.delete_cluster_custom_object(
                    GROUP,
                    VERSION,
                    PODSNAPSHOTCONTENTS,
                    item["metadata"]["name"],
                )
            except ApiException as exc:
                if exc.status != 404:
                    raise


def debug_dump_snapshotjob(config: k8s.E2EConfig, run: TestRun) -> None:
    print("\n--- snapshotjob e2e debug ---")
    print(f"namespace={config.namespace} test={run.suffix}")
    core = client.CoreV1Api()
    pods = core.list_namespaced_pod(
        config.namespace, label_selector=f"snapshot-e2e-test={run.suffix}"
    ).items
    for pod in pods:
        print(f"pod {pod.metadata.name} phase={pod.status.phase} node={pod.spec.node_name}")
        print(f"annotations={pod.metadata.annotations or {}}")
        print(k8s.pod_logs(config.namespace, pod.metadata.name, tail_lines=80))
    api = client.CustomObjectsApi()
    try:
        sj = api.get_namespaced_custom_object(
            GROUP, VERSION, config.namespace, SNAPSHOTJOBS, run.checkpoint_id
        )
        print(f"SnapshotJob status={sj.get('status', {})}")
    except ApiException as exc:
        if exc.status != 404:
            print(f"SnapshotJob debug unavailable: {k8s.api_error_detail(exc)}")
    print_custom_objects_named(config, run.checkpoint_id)
    print_snapshot_controller_logs(config)
    events = core.list_namespaced_event(config.namespace).items
    for event in events[-30:]:
        involved = event.involved_object
        if involved and involved.name in {run.source_pod, run.restore_pod, run.checkpoint_id}:
            print(f"event {event.reason}: {event.message}")
    print("--- end debug ---\n")


def print_custom_objects_named(config: k8s.E2EConfig, snapshot_name: str) -> None:
    api = client.CustomObjectsApi()
    try:
        snap = api.get_namespaced_custom_object(
            GROUP, VERSION, config.namespace, PODSNAPSHOTS, snapshot_name
        )
        print(f"PodSnapshot conditions={snap.get('status', {}).get('conditions', [])}")
        content_name = snap.get("status", {}).get("boundSnapshotContentName")
        if content_name:
            content = api.get_cluster_custom_object(
                GROUP, VERSION, PODSNAPSHOTCONTENTS, content_name
            )
            print(
                "PodSnapshotContent "
                f"{content_name} conditions={content.get('status', {}).get('conditions', [])}"
            )
    except ApiException as exc:
        if exc.status != 404:
            print(f"Snapshot CR debug unavailable: {k8s.api_error_detail(exc)}")


def wait_for(
    description: str,
    fn: Any,
    timeout: int,
    *,
    detail: Callable[[], str] | None = None,
) -> Any:
    start = time.monotonic()
    deadline = time.monotonic() + timeout
    last_report = 0.0
    last_detail = ""
    while time.monotonic() < deadline:
        result = fn()
        if result is not None:
            return result
        now = time.monotonic()
        if last_report == 0.0 or now - last_report >= PROGRESS_INTERVAL_SECONDS:
            last_detail = detail() if detail else ""
            suffix = f": {last_detail}" if last_detail else ""
            elapsed = now - start
            print(
                f"[{time.strftime('%H:%M:%S')}] waiting for {description} "
                f"({elapsed:.0f}s/{timeout}s){suffix}",
                flush=True,
            )
            last_report = now
        time.sleep(5)
    suffix = f": {last_detail}" if last_detail else ""
    raise AssertionError(f"timed out waiting for {description}{suffix}")
