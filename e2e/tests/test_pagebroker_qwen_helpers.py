# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Cluster-free checks for Qwen manifests, configuration gates and repeated restore."""

from dataclasses import replace
from datetime import datetime, timezone
import json
from pathlib import Path
from types import SimpleNamespace as NS

import pytest

from snapshot_e2e import benchmark
from snapshot_e2e import framework_workloads as fw
from snapshot_e2e import frameworks
from snapshot_e2e import k8s
from snapshot_e2e import pagebroker_qwen as qwen
from snapshot_e2e.workloads import TestRun as WorkloadRun


CONFIG = k8s.E2EConfig(namespace="snapshot", release="snapshot", pvc_name="snapshot-pvc", kubeconfig=None)
STARTED = datetime(2026, 10, 13, 10, 0, tzinfo=timezone.utc)
CONFIG_TEXT = """2026-10-13 09:59:59.123456789 +00:00
storage:
  type: pvc
pageBroker:
  restoreMode: staged
  transferEngine: model-streamer
"""


@pytest.fixture
def identity(monkeypatch):
    monkeypatch.setenv("SNAPSHOT_E2E_WORKLOAD_IMAGE", "local/agent:fixture")
    for name in ("SNAPSHOT_E2E_FRAMEWORK_IMAGE", "SNAPSHOT_E2E_MODEL_CACHE_SERVER",
                 "SNAPSHOT_E2E_MODEL_CACHE_PATH", "SNAPSHOT_E2E_QWEN_GPU_MEMORY_UTILIZATION"):
        monkeypatch.delenv(name, raising=False)
    return WorkloadRun.new("qwen-fixture")


@pytest.mark.parametrize("case", qwen.CASES, ids=lambda case: case.name)
def test_both_pods_use_the_requested_model_and_private_app(case, identity):
    spec = replace(frameworks.FRAMEWORKS["vllm"], model=case.model)
    source = qwen.prepare_pod(fw.source_pod(config=CONFIG, run=identity, spec=spec),
                             case, identity, case.gpu_memory_utilization)
    restore = qwen.prepare_pod(fw.restore_pod(config=CONFIG, run=identity, spec=spec, source_node="gpu-node"),
                              case, identity, case.gpu_memory_utilization)
    for pod in (source, restore):
        main = fw.main_container(pod)
        assert fw.env_value(main, "SNAPSHOT_MODEL") == case.model
        assert fw.env_value(main, "VLLM_GPU_MEMORY_UTILIZATION") == str(case.gpu_memory_utilization)
        assert main["resources"]["limits"]["nvidia.com/gpu"] == "1"
        assert "@sha256:" in main["image"]
        assert pod["spec"]["restartPolicy"] == "Never"
        assert any(v.get("configMap", {}).get("name") == qwen.app_name(identity) for v in pod["spec"]["volumes"])
    assert fw.main_container(restore)["command"] == ["/bin/sh", "-c", "exec sleep infinity"]
    assert restore["metadata"]["annotations"]["nvidia.com/restore-from"] == identity.snapshot_name
    assert fw.env_value(fw.main_container(fw.source_pod(config=CONFIG, run=identity, spec=spec)),
                        "SNAPSHOT_MODEL") == "Qwen/Qwen3-0.6B"


@pytest.mark.parametrize("old,new", [("type: pvc", "type: s3"), ("restoreMode: staged", "restoreMode: direct"),
                                     ("transferEngine: model-streamer", "transferEngine: posix-copy"),
                                     ("09:59:59.123456789", "10:00:00.123456789")])
def test_wrong_or_stale_agent_configuration_fails(old, new):
    with pytest.raises(AssertionError):
        qwen.require_staged_config(CONFIG_TEXT.replace(old, new), STARTED)


def test_matching_loaded_configuration_is_accepted():
    assert qwen.require_staged_config(CONFIG_TEXT, STARTED) == {
        "storageType": "pvc", "restoreMode": "staged", "transferEngine": "model-streamer",
    }


@pytest.mark.parametrize("value", ["0", "-1", "1.1", "nan", "inf", "invalid"])
def test_invalid_gpu_budget_is_rejected(value, monkeypatch):
    monkeypatch.setenv("SNAPSHOT_E2E_QWEN_GPU_MEMORY_UTILIZATION", value)
    with pytest.raises(ValueError):
        qwen.gpu_memory_utilization(qwen.CASES[1])


def pod(uid, names=("main",)):
    return NS(metadata=NS(uid=uid), spec=NS(node_name="gpu-node", containers=[NS(name=n, args=[]) for n in names]),
              status=NS(container_statuses=[NS(name=n, ready=True, restart_count=0, container_id=f"{uid}-{n}",
                        image_id=f"sha256:{n}-image", state=NS(running=NS(started_at=STARTED))) for n in names]))


@pytest.fixture
def cluster(monkeypatch, identity):
    state = NS(trace=[], manifests=[], restores=0, queries=0, mutation=None, current=None,
               source=pod("source-uid"), agent=pod("agent-uid", ("agent", "pagebroker")),
               record=benchmark.BenchmarkRecorder(suite="fixture", case="qwen", test="fixture"))

    def create(manifest):
        state.manifests.append(manifest)
        if manifest["metadata"]["name"] == identity.source_pod:
            state.trace.append("create-source")
            return state.source
        state.restores += 1
        state.trace.append(f"create-restore-{state.restores}")
        uid = f"restore-{state.restores}" if state.mutation != "reused-uid" else "restore-1"
        state.current = pod(uid)
        return state.current

    def read(namespace, name):
        return state.agent if name == "agent" else state.current

    def traffic(*args, **kwargs):
        if state.mutation == "broker-restart":
            state.agent.status.container_statuses[1].restart_count += 1
        if state.mutation == "workload-restart":
            state.current.status.container_statuses[0].restart_count = 1
        if state.mutation == "wrong-image":
            state.current.status.container_statuses[0].image_id = "sha256:other"
        return state.current, "Restored inference result"

    def generate(*args):
        state.queries += 1
        if state.mutation == "second-restore-fails" and state.restores == 2:
            raise AssertionError("second restore inference failed")
        return "" if state.mutation == "empty-inference" else "Ready."

    def logs(namespace, name, **kwargs):
        if name == identity.source_pod or state.mutation == "cold-start":
            return "vLLM pre-checkpoint output='ready'"
        return "<logs unavailable: fixture>" if state.mutation == "missing-logs" else ""

    def remove(namespace, name):
        state.trace.append("delete-source" if name == identity.source_pod else f"delete-restore-{state.restores}")
        return True

    def deleted(namespace, name, **kwargs):
        state.trace.append("source-gone" if name == identity.source_pod else f"restore-{state.restores}-gone")

    monkeypatch.setattr(k8s, "create_pod", create)
    monkeypatch.setattr(k8s, "read_pod", read)
    monkeypatch.setattr(k8s, "delete_pod", remove)
    monkeypatch.setattr(k8s, "pod_logs", logs)
    monkeypatch.setattr(k8s, "apply_configmap", lambda *args: state.trace.append("create-app"))
    monkeypatch.setattr(k8s, "exec_command", lambda *args, **kwargs: CONFIG_TEXT)
    monkeypatch.setattr(qwen.snap, "checkpoint_agent_pod", lambda *args: "agent")
    monkeypatch.setattr(qwen.snap, "wait_for_pod_ready", lambda namespace, name, **kwargs:
                        state.source if name == identity.source_pod else state.current)
    monkeypatch.setattr(qwen.snap, "wait_for_pod_deleted", deleted)
    monkeypatch.setattr(qwen.snap, "create_podsnapshot", lambda *args: state.trace.append("checkpoint"))
    monkeypatch.setattr(qwen.snap, "wait_for_snapshot_ready", lambda *args, **kwargs: (
        {"status": {"boundSnapshotContentName": "content"}},
        {"metadata": {"name": "content"}, "spec": {"source": {"nodeName": "gpu-node"}}},
    ))
    monkeypatch.setattr(qwen.snap, "wait_for_restore_traffic_ready", traffic)
    monkeypatch.setattr(qwen.snap, "debug_dump_framework", lambda *args, **kwargs: state.trace.append("diagnostics"))
    monkeypatch.setattr(qwen.inference, "request_generate", generate)
    return state


@pytest.mark.parametrize("case", qwen.CASES, ids=lambda case: case.name)
def test_one_checkpoint_two_fresh_restores_and_four_inferences(case, cluster, identity, tmp_path):
    cluster.record.result_dir = tmp_path
    qwen.exercise(CONFIG, identity, case, cluster.record)
    assert cluster.trace == ["create-app", "create-source", "checkpoint", "delete-source", "source-gone",
                             "create-restore-1", "delete-restore-1", "restore-1-gone",
                             "create-restore-2", "delete-restore-2", "restore-2-gone"]
    assert cluster.queries == 4
    assert all(fw.env_value(fw.main_container(p), "SNAPSHOT_MODEL") == case.model for p in cluster.manifests)
    report = json.loads(cluster.record.finalize("passed").read_text())
    assert report["outcome"] == "passed"
    assert report["environment"]["completedRestores"] == 2
    assert report["environment"]["transferEngine"] == "model-streamer"
    assert {event["name"] for event in report["events"]} >= {
        "checkpoint.ready", "restore.1.inference_verified", "restore.2.inference_verified",
    }


@pytest.mark.parametrize("mutation", ["reused-uid", "broker-restart", "workload-restart", "wrong-image",
                                      "empty-inference", "cold-start", "missing-logs", "second-restore-fails"])
def test_invalid_restore_cannot_pass(mutation, cluster, identity):
    cluster.mutation = mutation
    with pytest.raises(AssertionError):
        qwen.exercise(CONFIG, identity, qwen.CASES[0], cluster.record)
    assert cluster.record.environment["completedRestores"] < 2
    if mutation == "second-restore-fails":
        assert cluster.record.environment["completedRestores"] == 1
    assert cluster.trace[-1] == "diagnostics"


def test_wrong_storage_settings_fail_before_checkpoint(cluster, identity, monkeypatch):
    monkeypatch.setattr(k8s, "exec_command", lambda *args, **kwargs: CONFIG_TEXT.replace("staged", "direct"))
    with pytest.raises(AssertionError, match="restoreMode=staged"):
        qwen.exercise(CONFIG, identity, qwen.CASES[0], cluster.record)
    assert "checkpoint" not in cluster.trace


def test_s3_daemon_cannot_pass_the_filesystem_case(cluster, identity):
    cluster.agent.spec.containers[1].args = ["--storage-config=/config/s3.json"]
    with pytest.raises(AssertionError, match="filesystem PageBroker"):
        qwen.exercise(CONFIG, identity, qwen.CASES[0], cluster.record)
    assert "checkpoint" not in cluster.trace


def test_required_qwen_cases_cannot_pass_as_skipped(pytester, monkeypatch):
    monkeypatch.delenv("SNAPSHOT_E2E_PAGEBROKER_QWEN", raising=False)
    monkeypatch.setenv("SNAPSHOT_E2E_REQUIRED_TESTS", "test_pagebroker_qwen_repeated_restore")
    directory = Path(__file__).parent
    pytester.makeconftest((directory / "conftest.py").read_text())
    pytester.makepyfile(test_qwen=(directory / "test_pagebroker_qwen.py").read_text())
    pytester.makeini("[pytest]\nmarkers =\n    gpu: GPU tests\n    pagebroker_qwen: Qwen smoke tests\n")
    result = pytester.runpytest_inprocess("-q", "-p", "no:cacheprovider")
    result.assert_outcomes(errors=2)
    assert result.ret == pytest.ExitCode.TESTS_FAILED


def test_selected_context_is_passed_to_kubernetes(monkeypatch):
    calls = []
    monkeypatch.setattr(k8s, "load_config", lambda *args: calls.append(args))
    monkeypatch.setenv("SNAPSHOT_E2E_MODE", "direct")
    monkeypatch.setenv("KUBECONFIG", "/external/config")
    monkeypatch.setenv("SNAPSHOT_E2E_KUBE_CONTEXT", "selected-cluster")
    k8s.configure(k8s.E2EConfig.from_env())
    monkeypatch.delenv("SNAPSHOT_E2E_KUBE_CONTEXT")
    k8s.configure(k8s.E2EConfig.from_env())
    assert calls == [("/external/config", "selected-cluster"), ("/external/config", None)]


def test_fixture_records_ownership_before_creating_workloads(tmp_path, monkeypatch, identity):
    import test_pagebroker_qwen as suite

    path = tmp_path / "ownership.json"
    monkeypatch.setenv("SNAPSHOT_E2E_PAGEBROKER_QWEN_OWNERSHIP", str(path))
    removed = []
    monkeypatch.setattr(suite.lifecycle, "cleanup", lambda config, run: removed.append(run.suffix))
    monkeypatch.setattr(suite.qwen, "delete_app", lambda config, run: removed.append(run.suffix + "-app"))
    fixture = suite.qwen_run.__wrapped__(CONFIG)
    run = next(fixture)
    owner = json.loads(path.read_text())
    assert owner["namespace"] == CONFIG.namespace
    assert WorkloadRun(**owner["run"]) == run
    assert not removed
    with pytest.raises(StopIteration):
        next(fixture)
    assert removed == [run.suffix, run.suffix + "-app"]
    with pytest.raises(FileExistsError):
        next(suite.qwen_run.__wrapped__(CONFIG))
