# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Small helpers for Snapshot functional e2e tests."""

from __future__ import annotations

import time
from typing import Any, Callable

from kubernetes import client
from kubernetes.client import ApiException

from snapshot_e2e import k8s
from snapshot_e2e.workloads import CHECKPOINT_DIR
from snapshot_e2e.workloads import SOURCE_READY
from snapshot_e2e.workloads import TestRun
from snapshot_e2e.workloads import restore_pod
from snapshot_e2e.workloads import source_pod


GROUP = "nvidia.com"
VERSION = "v1alpha1"
PODSNAPSHOTS = "podsnapshots"
PODSNAPSHOTCONTENTS = "podsnapshotcontents"
PROGRESS_INTERVAL_SECONDS = 30


def wait_for_pod_deleted(namespace: str, name: str, timeout: int = 180) -> None:
    def gone() -> bool:
        try:
            k8s.read_pod(namespace, name)
        except ApiException as exc:
            if exc.status == 404:
                return True
            raise
        return False

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
) -> dict[str, Any]:
    body = {
        "apiVersion": f"{GROUP}/{VERSION}",
        "kind": "PodSnapshot",
        "metadata": {"name": name, "namespace": namespace},
        "spec": {"source": {"podRef": {"name": pod_name, "uid": pod_uid}}},
    }
    return client.CustomObjectsApi().create_namespaced_custom_object(
        GROUP,
        VERSION,
        namespace,
        PODSNAPSHOTS,
        body,
    )


def wait_for_pod_ready(namespace: str, name: str, timeout: int = 600) -> client.V1Pod:
    def ready() -> client.V1Pod | None:
        pod = k8s.read_pod(namespace, name)
        return pod if k8s.pod_containers_ready(pod) else None

    def detail() -> str:
        try:
            pod = k8s.read_pod(namespace, name)
        except ApiException as exc:
            return f"api_error={k8s.api_error_detail(exc)}"
        return k8s.pod_readiness_detail(pod)

    return wait_for(f"pod {namespace}/{name} Ready", ready, timeout, detail=detail)


def wait_for_file(namespace: str, pod: str, path: str, timeout: int = 180) -> None:
    def exists() -> bool:
        try:
            response = k8s.exec_command(namespace, pod, f"test -f {path}")
            return response == "" or response is None
        except Exception:
            return False

    wait_for(f"{namespace}/{pod}:{path}", exists, timeout)


def last_tick(namespace: str, pod: str) -> int:
    output = k8s.exec_command(
        namespace,
        pod,
        "test -f /tmp/tick.log || { echo 0; exit 0; }; "
        "awk '/^tick / {n=$2} END {print n+0}' /tmp/tick.log",
    )
    return int(output.strip() or "0")


def wait_for_tick_at_least(
    namespace: str,
    pod: str,
    minimum: int,
    timeout: int = 180,
) -> int:
    def check() -> int | None:
        tick = last_tick(namespace, pod)
        return tick if tick >= minimum else None

    return wait_for(
        f"{namespace}/{pod} tick >= {minimum}",
        check,
        timeout,
        detail=lambda: f"last_tick={last_tick(namespace, pod)}",
    )


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


def checkpoint_artifact_manifest(namespace: str, pod: str, checkpoint_id: str) -> str:
    return k8s.exec_command(
        namespace,
        pod,
        f"cat {CHECKPOINT_DIR}/{checkpoint_id}/versions/1/manifest.yaml",
    )


def checkpoint_artifact_listing(namespace: str, pod: str, checkpoint_id: str) -> str:
    return k8s.exec_command(
        namespace,
        pod,
        f"cd {CHECKPOINT_DIR}/{checkpoint_id}/versions/1 && "
        "find . -maxdepth 1 -type f -print | sort && "
        "tar -tf rootfs-diff.tar | sort",
    )

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
    while time.monotonic() < deadline:
        result = fn()
        if result:
            return result
        now = time.monotonic()
        if last_report == 0.0 or now - last_report >= PROGRESS_INTERVAL_SECONDS:
            suffix = f": {detail()}" if detail else ""
            elapsed = now - start
            print(
                f"[{time.strftime('%H:%M:%S')}] waiting for {description} "
                f"({elapsed:.0f}s/{timeout}s){suffix}",
                flush=True,
            )
            last_report = now
        time.sleep(5)
    raise AssertionError(f"timed out waiting for {description}")
