# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Drives one benchmark run: deploy a snapshot-ready source pod, time cold
start, checkpoint it, restore it, time the restore -- and record everything
in a `RunResult`.

Built entirely on `snapshot_e2e.k8s` / `snapshot_e2e.lifecycle` (the e2e
package's own Kubernetes plumbing) rather than reimplementing pod/condition
polling; see benchmarks/pyproject.toml for the path dependency and
docs/development/benchmarks-guide.md for why `e2e/` isn't extended in place
instead (it's deliberately CI/synthetic; this needs real multi-GB engine
images and is a human-driven, ad-hoc tool).
"""

from __future__ import annotations

import dataclasses
import datetime
import uuid
from typing import Callable

from kubernetes import client
from kubernetes.client import ApiException

from snapshot_e2e import k8s
from snapshot_e2e import lifecycle

from snapshot_benchmarks import logs, metadata
from snapshot_benchmarks.engines import Engine, ModelSpec
from snapshot_benchmarks.schema import (
    SCHEMA_VERSION,
    AgentLogPhases,
    BenchmarkEngine,
    CheckpointTiming,
    ColdStartTiming,
    ModelInfo,
    RestoreTiming,
    RunResult,
)

DEFAULT_POD_READY_TIMEOUT = 1800  # models up to 145GB can take a while to load
DEFAULT_SNAPSHOT_READY_TIMEOUT = 1800
DEFAULT_RESTORE_TIMEOUT = 1800


@dataclasses.dataclass(frozen=True)
class BenchmarkConfig:
    """Cluster-targeting config for a run. Deliberately separate from
    `snapshot_e2e.k8s.E2EConfig`: a benchmark's workload namespace (where the
    source/restore pods and PodSnapshot live) and the Snapshot release's own
    namespace (where the operator/agent run) are not always the same
    namespace -- e2e's own convention installs both into one namespace, but a
    hand-installed cluster (e.g. this repo's own manual setup) commonly keeps
    them apart (`snapshot` vs. `default`)."""

    workload_namespace: str
    snapshot_namespace: str
    release: str
    pvc_name: str
    kubeconfig: str | None

    def workload_e2e_config(self) -> k8s.E2EConfig:
        return k8s.E2EConfig(
            namespace=self.workload_namespace,
            release=self.release,
            pvc_name=self.pvc_name,
            kubeconfig=self.kubeconfig,
        )

    def snapshot_e2e_config(self) -> k8s.E2EConfig:
        return k8s.E2EConfig(
            namespace=self.snapshot_namespace,
            release=self.release,
            pvc_name=self.pvc_name,
            kubeconfig=self.kubeconfig,
        )


def _wait_for_pod_condition(
    namespace: str,
    name: str,
    condition_type: str,
    *,
    timeout: int,
) -> client.V1Pod:
    """Waits for a named pod condition (e.g. "Ready") to become True and
    returns the pod, using `lifecycle.wait_for`'s generic poller and
    `lifecycle.pod_condition` -- both already exist for exactly this purpose,
    just not pre-composed for an arbitrary condition type the way
    `wait_for_restored_condition` is pre-composed for `nvidia.com/Restored`."""

    def check() -> client.V1Pod | None:
        pod = k8s.read_pod(namespace, name)
        cond = lifecycle.pod_condition(pod, condition_type)
        if cond and cond.status == "True":
            return pod
        if pod.status.phase in lifecycle.TERMINAL_POD_PHASES:
            raise AssertionError(
                f"pod {namespace}/{name} reached phase {pod.status.phase} "
                f"before condition {condition_type}=True"
            )
        return None

    def detail() -> str:
        try:
            pod = k8s.read_pod(namespace, name)
        except ApiException as exc:
            return f"api_error={k8s.api_error_detail(exc)}"
        return k8s.pod_readiness_detail(pod)

    return lifecycle.wait_for(
        f"pod {namespace}/{name} condition {condition_type}=True",
        check,
        timeout,
        detail=detail,
    )


def _container_started_at(pod, container_name: str) -> datetime.datetime | None:
    for status in pod.status.container_statuses or []:
        if status.name == container_name and status.state and status.state.running:
            return status.state.running.started_at
    return None


def _du_bytes(namespace: str, agent_pod: str, path: str) -> int | None:
    output = k8s.exec_command(namespace, agent_pod, f"du -sb {path} | cut -f1")
    stripped = output.strip().splitlines()[-1].strip() if output.strip() else ""
    return int(stripped) if stripped.isdigit() else None


def run_benchmark(
    cfg: BenchmarkConfig,
    engine: Engine,
    model: ModelSpec,
    *,
    image: str,
    image_pull_policy: str | None = None,
    tolerations: list[dict[str, str]] | None = None,
    mode: str = "both",
    keep: bool = False,
    pod_ready_timeout: int = DEFAULT_POD_READY_TIMEOUT,
    snapshot_ready_timeout: int = DEFAULT_SNAPSHOT_READY_TIMEOUT,
    restore_timeout: int = DEFAULT_RESTORE_TIMEOUT,
    run_id: str | None = None,
    progress: Callable[[str], None] = print,
) -> RunResult:
    """Runs one model against one engine and returns the raw `RunResult`.

    `mode`:
      - "cold_start": deploy the source pod, time cold start, then clean up
        (or leave it running with `keep=True`).
      - "both" (default): cold start, checkpoint it, delete the source pod to
        free the GPU, restore it, time the restore. This is the path that
        produces the "Snapshot restore" vs. "vLLM wake and copy-to-GPU" split.

    `image_pull_policy` overrides the guide's own default (`Always`, which
    assumes `image` is pushed to a registry the cluster can reach) -- pass
    `"IfNotPresent"` when `image` was built and imported directly into the
    node's container runtime with no registry involved.
    """
    if mode not in ("cold_start", "both"):
        raise ValueError(f"unsupported mode: {mode!r}")

    run_id = run_id or uuid.uuid4().hex[:12]
    workload = cfg.workload_e2e_config()
    k8s.configure(workload)

    source_name = f"bench-source-{run_id}"

    result = RunResult(
        schema_version=SCHEMA_VERSION,
        run_id=run_id,
        timestamp_utc=datetime.datetime.now(datetime.timezone.utc).isoformat(),
        mode=mode,
        environment=metadata.BenchmarkEnvironment(),
        engine=BenchmarkEngine(name=engine.name),
        model=ModelInfo(label=model.label, hf_id_or_path=model.hf_id_or_path),
    )

    # Deliberately no try/except here: on failure this raises straight
    # through to the caller rather than returning a partially-populated
    # `RunResult`. `cli.py`'s `sweep` command catches per-model, logs the
    # error, and moves to the next model without writing a result file for
    # it -- a half-filled JSON masquerading as a complete run would be worse
    # than no file at all. Pods from a failed run are left in place (unless
    # `keep=False` reached a cleanup step before the failure) for debugging.
    result.git_sha = _git_sha()

    progress(f"[{run_id}] deploying source pod {source_name}")
    source_manifest = engine.build_source_pod(
        name=source_name,
        namespace=cfg.workload_namespace,
        image=image,
        model=model,
        image_pull_policy=image_pull_policy,
        tolerations=tolerations,
    )
    k8s.create_pod(source_manifest)

    progress(f"[{run_id}] waiting for source pod Ready (cold start)")
    source_pod = _wait_for_pod_condition(
        cfg.workload_namespace, source_name, "Ready", timeout=pod_ready_timeout
    )
    result.cold_start = ColdStartTiming(
        pod_created_at=source_pod.metadata.creation_timestamp,
        container_started_at=_container_started_at(source_pod, engine.container_name),
        ready_at=_pod_condition_time(source_pod, "Ready"),
    )

    capture_node = source_pod.spec.node_name
    result.environment = metadata.collect_environment(
        namespace=cfg.workload_namespace,
        pvc_name=cfg.pvc_name,
        gpu_pod=source_name,
        gpu_container=engine.container_name,
        capture_node=capture_node,
    )
    result.engine.version = metadata.engine_version(
        cfg.workload_namespace, source_name, engine.container_name, engine.version_probe_command
    )

    if mode == "both":
        snapshot_name = f"bench-snapshot-{run_id}"
        progress(f"[{run_id}] checkpointing {source_name} as {snapshot_name}")
        created_snapshot = lifecycle.create_podsnapshot(
            cfg.workload_namespace,
            snapshot_name,
            source_name,
            source_pod.metadata.uid,
            container=engine.container_name,
        )
        snap, content = lifecycle.wait_for_snapshot_ready(
            cfg.workload_namespace, snapshot_name, timeout=snapshot_ready_timeout
        )
        result.checkpoint = CheckpointTiming(
            podsnapshot_created_at=_parse_iso(created_snapshot["metadata"]["creationTimestamp"]),
            ready_at=_custom_object_condition_time(snap, "Ready"),
        )
        content_uid = content["metadata"]["uid"]
        result.model.checkpoint_artifact_bytes = _checkpoint_size(cfg, capture_node, content_uid)

        progress(f"[{run_id}] deleting source pod {source_name} to free the GPU")
        k8s.delete_pod(cfg.workload_namespace, source_name)
        lifecycle.wait_for_pod_deleted(cfg.workload_namespace, source_name)

        restore_name = f"bench-restore-{run_id}"
        progress(f"[{run_id}] deploying restore pod {restore_name}")
        restore_manifest = engine.build_restore_pod(
            name=restore_name,
            namespace=cfg.workload_namespace,
            image=image,
            model=model,
            snapshot_name=snapshot_name,
            image_pull_policy=image_pull_policy,
            tolerations=tolerations,
        )
        restore_created_pod = k8s.create_pod(restore_manifest)

        progress(f"[{run_id}] waiting for nvidia.com/Restored=True (Snapshot restore)")
        restored_pod = lifecycle.wait_for_restored_condition(
            cfg.workload_namespace,
            restore_name,
            "True",
            "RestoreSucceeded",
            timeout=restore_timeout,
        )
        restore_container_started_at = _container_started_at(
            restored_pod, engine.container_name
        ) or _container_started_at(restore_created_pod, engine.container_name)

        progress(f"[{run_id}] waiting for restore pod Ready (vLLM wake and copy-to-GPU)")
        restore_ready_pod = _wait_for_pod_condition(
            cfg.workload_namespace, restore_name, "Ready", timeout=restore_timeout
        )

        result.restore = RestoreTiming(
            restore_pod_created_at=restore_created_pod.metadata.creation_timestamp,
            restore_container_started_at=restore_container_started_at,
            restored_condition_at=_pod_condition_time(restored_pod, "nvidia.com/Restored"),
            restore_ready_at=_pod_condition_time(restore_ready_pod, "Ready"),
        )

        restore_node = restore_ready_pod.spec.node_name
        result.environment.restore_node = restore_node
        result.environment.placement = metadata.placement(capture_node, restore_node)

        result.agent_log_phases = _collect_agent_log_phases(cfg, restore_node)

        if not keep:
            progress(f"[{run_id}] cleaning up restore pod and snapshot")
            k8s.delete_pod(cfg.workload_namespace, restore_name)
            lifecycle.delete_podsnapshot(cfg.workload_namespace, snapshot_name)
            lifecycle.delete_podsnapshotcontent(content["metadata"]["name"])
    elif not keep:
        progress(f"[{run_id}] cleaning up source pod")
        k8s.delete_pod(cfg.workload_namespace, source_name)

    return result


def _pod_condition_time(pod, condition_type: str) -> datetime.datetime | None:
    cond = lifecycle.pod_condition(pod, condition_type)
    return cond.last_transition_time if cond else None


def _parse_iso(value: str | None) -> datetime.datetime | None:
    """Parses a timestamp off a raw `PodSnapshot`/`PodSnapshotContent` dict
    (fetched via `CustomObjectsApi`, so timestamps are plain RFC3339 strings,
    unlike the typed `V1Pod` objects `_pod_condition_time` reads)."""
    if not value:
        return None
    return datetime.datetime.fromisoformat(value.replace("Z", "+00:00"))


def _custom_object_condition_time(obj: dict, condition_type: str) -> datetime.datetime | None:
    cond = lifecycle.condition(obj, condition_type)
    return _parse_iso(cond.get("lastTransitionTime")) if cond else None


def _checkpoint_size(cfg: BenchmarkConfig, capture_node: str | None, content_uid: str) -> int | None:
    if not capture_node:
        return None
    snapshot_cfg = cfg.snapshot_e2e_config()
    agent_pod = lifecycle.checkpoint_agent_pod(snapshot_cfg, capture_node)
    return _du_bytes(
        cfg.snapshot_namespace, agent_pod, lifecycle.checkpoint_artifact_root(content_uid)
    )


def _collect_agent_log_phases(cfg: BenchmarkConfig, restore_node: str | None) -> AgentLogPhases:
    if not restore_node:
        return AgentLogPhases(parse_warnings=["restore pod had no node_name; cannot locate agent"])
    snapshot_cfg = cfg.snapshot_e2e_config()
    try:
        agent_pod = lifecycle.checkpoint_agent_pod(snapshot_cfg, restore_node)
    except AssertionError as exc:
        return AgentLogPhases(parse_warnings=[str(exc)])
    log_text = k8s.pod_logs(cfg.snapshot_namespace, agent_pod, tail_lines=2000)
    return logs.parse_agent_log_phases(log_text, log_source_pod=agent_pod)


def _git_sha() -> str | None:
    import subprocess

    try:
        return (
            subprocess.run(
                ["git", "rev-parse", "--short", "HEAD"],
                capture_output=True,
                text=True,
                check=True,
                timeout=5,
            )
            .stdout.strip()
            or None
        )
    except Exception:  # noqa: BLE001 - best-effort provenance, never fatal
        return None
