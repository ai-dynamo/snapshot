# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Small helpers for Snapshot functional e2e tests."""

from __future__ import annotations

import shlex
import time
from contextlib import contextmanager
from typing import Any, Callable, Iterator

import yaml
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


def file_present(namespace: str, pod: str, path: str) -> bool:
    # Require a stdout marker because exec does not expose remote exit status,
    # and look for it rather than match on it: exec returns stderr too, and a
    # login shell is free to write to it.
    marker = "__snapshot_e2e_file_present__"
    command = f"[[ -f {shlex.quote(path)} ]] && printf '%s' {shlex.quote(marker)}"
    return marker in k8s.exec_command(namespace, pod, command)


def wait_for_file(namespace: str, pod: str, path: str, timeout: int = 180) -> None:
    last_error: str | None = None

    def exists() -> bool | None:
        nonlocal last_error
        try:
            present = file_present(namespace, pod, path)
            last_error = None
            return True if present else None
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


def wait_for_restored_condition(
    namespace: str,
    pod_name: str,
    status: str,
    reason: str,
    timeout: int = 600,
) -> client.V1Pod:
    def check() -> client.V1Pod | None:
        pod = k8s.read_pod(namespace, pod_name)
        restored = pod_condition(pod, "snapshot/Restored")
        if restored and restored.status == status and restored.reason == reason:
            return pod
        if reason != "RestoreFailed" and restored and restored.reason == "RestoreFailed":
            raise AssertionError(
                f"restore failed for {namespace}/{pod_name}: {restored.message}"
            )
        return None

    def detail() -> str:
        try:
            pod = k8s.read_pod(namespace, pod_name)
        except ApiException as exc:
            return f"api_error={k8s.api_error_detail(exc)}"
        restored = pod_condition(pod, "snapshot/Restored")
        return f"snapshot/Restored={restored or '<unset>'}"

    return wait_for(
        f"snapshot/Restored={status}/{reason} on {namespace}/{pod_name}",
        check,
        timeout,
        detail=detail,
    )


def pod_condition(pod: client.V1Pod, condition_type: str) -> client.V1PodCondition | None:
    for item in pod.status.conditions or []:
        if item.type == condition_type:
            return item
    return None


def wait_for_restore_past_the_gate(
    namespace: str,
    pod_name: str,
    timeout: int = 600,
) -> client.V1Pod:
    """Wait for any reason the agent only records once the gate has let the restore through.

    RestoreInProgress is transient, so waiting for it alone is a race a fast
    restore wins. A restore refused at the gate never reaches any of these.
    """
    past = ("RestoreInProgress", "RestoreSucceeded", "RestoreFailed")

    def check() -> client.V1Pod | None:
        pod = k8s.read_pod(namespace, pod_name)
        restored = pod_condition(pod, "snapshot/Restored")
        return pod if restored and restored.reason in past else None

    def detail() -> str:
        try:
            pod = k8s.read_pod(namespace, pod_name)
        except ApiException as exc:
            return f"api_error={k8s.api_error_detail(exc)}"
        restored = pod_condition(pod, "snapshot/Restored")
        return f"snapshot/Restored={restored.reason if restored else '<unset>'}"

    return wait_for(
        f"restore past the gate on {namespace}/{pod_name}",
        check,
        timeout,
        detail=detail,
    )


def checkpoint_artifact_manifest(
    config: k8s.E2EConfig, node: str, content_uid: str
) -> str:
    return k8s.exec_payload(
        config.namespace,
        checkpoint_agent_pod(config, node),
        f"cat {checkpoint_artifact_path(content_uid)}/manifest.yaml",
    )


def checkpoint_manifest(
    config: k8s.E2EConfig, node: str, content_uid: str
) -> dict[str, Any]:
    """The manifest as the agent will read it back, rather than as text."""
    return yaml.safe_load(checkpoint_artifact_manifest(config, node, content_uid))


def visible_gpus(namespace: str, pod: str) -> list[dict[str, str]]:
    """The GPUs a pod can see, as nvidia-smi inside that pod reports them.

    The same query the agent runs, so a test comparing the two is comparing what
    the machine says against what the artifact recorded, not two spellings of it.
    """
    output = k8s.exec_payload(
        namespace,
        pod,
        "nvidia-smi --query-gpu=gpu_uuid,name,driver_version --format=csv,noheader",
    )
    gpus = []
    for line in output.strip().splitlines():
        fields = [field.strip() for field in line.split(",")]
        if len(fields) != 3:
            raise AssertionError(f"unexpected nvidia-smi row {line!r}")
        gpus.append({"uuid": fields[0], "name": fields[1], "driver": fields[2]})
    return gpus


def checkpoint_artifact_listing(
    config: k8s.E2EConfig, node: str, content_uid: str
) -> str:
    return k8s.exec_command(
        config.namespace,
        checkpoint_agent_pod(config, node),
        f"cd {checkpoint_artifact_path(content_uid)} && "
        "find . -maxdepth 1 -type f -print | sort && "
        "tar -tf rootfs-diff.tar | sort",
    )


def checkpoint_rootfs_file(
    config: k8s.E2EConfig,
    node: str,
    content_uid: str,
    path: str,
) -> str:
    return k8s.exec_command(
        config.namespace,
        checkpoint_agent_pod(config, node),
        f"cd {checkpoint_artifact_path(content_uid)} && "
        f"tar -xOf rootfs-diff.tar {path}",
    )


def checkpoint_artifact_path(content_uid: str) -> str:
    return shlex.quote(
        f"{AGENT_CHECKPOINT_DIR}/artifacts/{content_uid}/containers/{CONTAINER}"
    )


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


AGENT_CONFIG_VOLUME = "config"
AGENT_CONFIG_KEY = "config.yaml"


def agent_config_source(config: k8s.E2EConfig) -> tuple[str, str]:
    """The ConfigMap the agent reads from and the path it reads it at.

    Read off the DaemonSet so a test edits the file the agent is actually
    mounting rather than one the chart happens to name the same way.
    """
    daemonsets = k8s.list_snapshot_daemonsets(
        config.namespace, config.release, "snapshot-agent"
    )
    if len(daemonsets) != 1:
        raise AssertionError(f"expected one snapshot agent DaemonSet, found {len(daemonsets)}")

    spec = daemonsets[0].spec.template.spec
    volume = next(v for v in spec.volumes if v.name == AGENT_CONFIG_VOLUME)
    mount = next(
        m
        for container in spec.containers
        for m in container.volume_mounts or []
        if m.name == AGENT_CONFIG_VOLUME
    )
    return volume.config_map.name, f"{mount.mount_path}/{AGENT_CONFIG_KEY}"


def wait_for_agent_config(config: k8s.E2EConfig, expected: str, timeout: int = 180) -> None:
    """Wait for a ConfigMap edit to reach every agent in the release.

    The ConfigMap belongs to the release, so an edit reaches the whole
    DaemonSet; waiting on one node would leave the others holding the old value.
    The kubelet refreshes projected ConfigMaps on its own schedule, so the file
    inside each container is the only honest signal that an edit has landed.
    """
    _, path = agent_config_source(config)
    command = f"cat {shlex.quote(path)}"
    agents = [
        pod.metadata.name
        for pod in k8s.list_snapshot_pods(config.namespace, config.release, "snapshot-agent")
    ]
    if not agents:
        raise AssertionError("no snapshot agent pods to wait on")

    for agent in agents:
        def projected(agent: str = agent) -> bool | None:
            content = k8s.exec_command(config.namespace, agent, command)
            return True if expected in content else None

        wait_for(
            f"{expected!r} in {agent}:{path}",
            projected,
            timeout,
            detail=lambda agent=agent: k8s.exec_command(config.namespace, agent, command),
        )


@contextmanager
def node_skip_compat_check(config: k8s.E2EConfig) -> Iterator[None]:
    """Turn the node switch on for the body, and put it back afterwards.

    Waits for the projection on both edges: leaving the switch on would let a
    later test's restore through the very gate it means to exercise.
    """
    name, _ = agent_config_source(config)
    original = k8s.read_config_map(config.namespace, name).data[AGENT_CONFIG_KEY]
    off, on = "skipCompatCheck: false", "skipCompatCheck: true"
    if off not in original:
        raise AssertionError(f"{name}:{AGENT_CONFIG_KEY} does not carry {off!r}")

    k8s.patch_config_map(config.namespace, name, {AGENT_CONFIG_KEY: original.replace(off, on)})
    try:
        wait_for_agent_config(config, on)
        yield
    finally:
        k8s.patch_config_map(config.namespace, name, {AGENT_CONFIG_KEY: original})
        wait_for_agent_config(config, off)


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
        print(
            "conditions="
            + str(
                [
                    (c.type, c.status, c.reason, c.message)
                    for c in pod.status.conditions or []
                ]
            )
        )
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
