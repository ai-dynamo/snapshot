# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

import copy
from dataclasses import replace
import json
from types import SimpleNamespace
from unittest.mock import Mock

import pytest

from snapshot_benchmarks import cli, measurement, metadata, report, run
from snapshot_benchmarks.engines import ModelSpec
from test_benchmarks import cluster, config


SETTINGS = {
    "storeId": "store-test", "bucket": "test-bucket", "region": "us-west-2",
    "endpoint": "https://s3.us-west-2.amazonaws.com", "prefix": "benchmarks",
}


@pytest.fixture
def config_api(monkeypatch):
    api = Mock()
    api.list_namespaced_config_map.return_value.items = [
        SimpleNamespace(data={"pagebroker-s3-config.json": json.dumps({
            **SETTINGS, "credentialsFile": "/private/credentials", "caFile": "/private/ca",
        })}),
        SimpleNamespace(data={"unrelated": "ignore"}),
    ]
    monkeypatch.setattr(metadata.client, "CoreV1Api", lambda: api)
    monkeypatch.setattr(metadata, "k8s_server_version", lambda: "test-kubernetes")
    return api


def test_s3_environment_skips_pvc_and_records_only_public_fields(config_api, monkeypatch):
    pvc = Mock(side_effect=AssertionError("must not inspect a PVC"))
    monkeypatch.setattr(metadata, "storage_backend", pvc)
    env = metadata.collect_environment(
        pvc_namespace="system", pvc_name="unused", storage_type="s3", release="test-release",
    )
    assert env.s3 == SETTINGS
    assert env.storage_type == "s3"
    assert env.storage_class is None and env.storage_provisioner is None
    config_api.list_namespaced_config_map.assert_called_once_with(
        "system", label_selector="app.kubernetes.io/instance=test-release",
    )
    pvc.assert_not_called()


@pytest.mark.parametrize("configs", [[], [SETTINGS, SETTINGS], [{"storeId": "missing-fields"}], [None]])
def test_invalid_s3_config_fails(config_api, configs):
    config_api.list_namespaced_config_map.return_value.items = [
        SimpleNamespace(data={"pagebroker-s3-config.json": json.dumps(value)}) for value in configs
    ]
    with pytest.raises(RuntimeError):
        metadata.s3_settings("system", "release")


def test_pvc_environment_keeps_existing_collection(config_api, monkeypatch):
    pvc = Mock(return_value=("vast", "vast-csi"))
    monkeypatch.setattr(metadata, "storage_backend", pvc)
    env = metadata.collect_environment(pvc_namespace="system", pvc_name="checkpoints")
    assert (env.storage_type, env.storage_class, env.storage_provisioner) == ("pvc", "vast", "vast-csi")
    assert env.s3 is None
    pvc.assert_called_once_with("system", "checkpoints")
    config_api.list_namespaced_config_map.assert_not_called()


@pytest.fixture
def publication():
    return {
        "metadata": {"name": "content", "uid": "content-uid"},
        "spec": {"storage": {"storeID": "store-test"}, "source": {"podRef": {"containers": ["main"]}}},
        "status": {"storage": {"artifacts": [{"containerName": "main", "artifactHandle": "opaque"}]}},
    }


@pytest.fixture
def benchmark(monkeypatch, config_api, publication, tmp_path):
    collect_environment = metadata.collect_environment
    with cluster() as mocks:
        mocks["metadata.collect_environment"].side_effect = collect_environment
        mocks["api"].list_namespaced_config_map.side_effect = config_api.list_namespaced_config_map
        mocks["lifecycle.wait_for_snapshot_ready"].return_value = ({}, publication)
        mocks["measurement.restored_measurement"].return_value["staging_s"] = 0.5
        monkeypatch.setattr(metadata, "storage_backend", Mock(return_value=("vast", "vast-csi")))

        def execute(storage, engine="vllm", tp=1):
            cfg = replace(config(), storage_type=storage)
            mocks["measurement.checkpoint_measurement"].return_value = (
                1234, 2, {"publication_s": 1, "publication_gbps": 0.000001234},
            )
            return run.run_benchmark(
                cfg, cli.ENGINES[engine], ModelSpec("test-model", "/hf-cache/test", tp=tp),
                image="test/image", run_id="test", evidence_dir=tmp_path,
            )

        yield execute, mocks


@pytest.mark.parametrize("storage", ["pvc", "s3"])
@pytest.mark.parametrize("engine", ["vllm", "sglang", "trtllm"])
@pytest.mark.parametrize("tp", [1, 4])
def test_checkpoint_restore_and_report_for_each_backend(benchmark, storage, engine, tp):
    execute, mocks = benchmark
    result = execute(storage, engine, tp)
    mocks["measurement.checkpoint_measurement"].assert_called_once()
    if storage == "s3":
        assert result.model.checkpoint_artifact_bytes == 1234
        assert not result.warnings
        assert result.environment.s3 == SETTINGS
        mocks["lifecycle.wait_for_artifact_root_absent"].assert_not_called()
    else:
        assert result.model.checkpoint_artifact_bytes == 1234
        mocks["lifecycle.wait_for_artifact_root_absent"].assert_called_once()
    assert result.checkpoint.agent_seconds == 2
    assert len(result.restore_attempts) == 3
    assert f"| {storage if storage == 's3' else 'vast'} |" in report.render([result.to_json_dict()])
    assert mocks["measurement.restored_measurement"].call_count == 3
    assert mocks["lifecycle.wait_for_pod_event"].call_count == 3
    mocks["lifecycle.delete_podsnapshotcontent"].assert_called_once_with("content")
    assert mocks["k8s.delete_pod"].call_count == 4
    for call in mocks["k8s.create_pod"].call_args_list:
        main = call.args[0]["spec"]["containers"][0]
        assert int(main["resources"]["limits"]["nvidia.com/gpu"]) == tp


@pytest.mark.parametrize("valid_timing", [True, False])
def test_s3_checkpoint_timing_without_filesystem_inventory(monkeypatch, tmp_path, valid_timing):
    cfg = replace(config(), storage_type="s3")
    text = 'Checkpoint timing summary {"content":"content","checkpoint":{"duration":"1.2s"}}'
    monkeypatch.setattr(measurement, "agent_log", Mock(return_value=text if valid_timing else ""))
    execute = Mock(side_effect=AssertionError("S3 has no local checkpoint inventory"))
    monkeypatch.setattr(measurement.k8s, "exec_command", execute)
    monkeypatch.setattr(measurement.storage, "s3_measurement", Mock(return_value={"payload_bytes": 1234}))
    if valid_timing:
        assert measurement.checkpoint_measurement(cfg, "node", "uid", "content", tmp_path)[:2] == (1234, 1.2)
        assert json.loads((tmp_path / "checkpoint-timing.json").read_text())["duration"] == "1.2s"
    else:
        with pytest.raises(ValueError):
            measurement.checkpoint_measurement(cfg, "node", "uid", "content", tmp_path)
    execute.assert_not_called()
    assert (tmp_path / "artifacts.json").exists()


@pytest.mark.parametrize("failure", ["wrong-store", "missing", "wrong-container", "empty-handle", "duplicate"])
def test_invalid_publication_stops_restore_and_cleans_up(benchmark, publication, failure):
    execute, mocks = benchmark
    artifacts = publication["status"]["storage"]["artifacts"]
    if failure == "wrong-store":
        publication["spec"]["storage"]["storeID"] = "another-store"
    elif failure == "missing":
        artifacts.clear()
    elif failure == "wrong-container":
        artifacts[0]["containerName"] = "other"
    elif failure == "empty-handle":
        artifacts[0]["artifactHandle"] = " "
    else:
        artifacts.append(copy.deepcopy(artifacts[0]))
    with pytest.raises(RuntimeError, match="checkpoint"):
        execute("s3")
    mocks["lifecycle.wait_for_pod_event"].assert_not_called()
    mocks["lifecycle.delete_podsnapshotcontent"].assert_called_once_with("content")
    mocks["k8s.delete_pod"].assert_called_once_with("workloads", "bench-source-test")


@pytest.mark.parametrize("command", ["metadata", "run", "sweep"])
def test_cli_passes_storage_choice(command):
    arguments = [command, "--context", "test", "--storage-type", "s3"]
    if command != "metadata":
        arguments += ["--image", "test/image", "--node", "gpu", "--model-pvc", "models", "--work-pvc", "work"]
    if command == "run":
        arguments += ["--model-label", "Qwen3 8B"]
    args = cli.build_parser().parse_args(arguments)
    assert cli._benchmark_config(args).storage_type == "s3"
    assert cli._benchmark_config(cli.build_parser().parse_args(["metadata", "--context", "test"])).storage_type == "pvc"


def test_metadata_command_uses_s3(config_api, monkeypatch, capsys):
    monkeypatch.setattr(run.BenchmarkConfig, "configure", Mock())
    args = cli.build_parser().parse_args(["metadata", "--context", "test", "--storage-type", "s3"])
    assert cli.cmd_metadata(args) == 0
    assert json.loads(capsys.readouterr().out)["s3"] == SETTINGS


def test_agent_phase_logs_select_agent_container(monkeypatch):
    monkeypatch.setattr(run.lifecycle, "checkpoint_agent_pod", Mock(return_value="agent-pod"))
    logs = Mock(return_value='Restore timing summary {"pod":"workload/restored","snapshot":"checkpoint",'
                             '"restore":{"duration":"2s","phases":{}}}')
    monkeypatch.setattr(run.k8s, "pod_logs", logs)
    cfg = replace(config(), workload_namespace="workload", snapshot_namespace="system", storage_type="s3")
    phases = run._collect_agent_log_phases(cfg, "node", restore_name="restored", snapshot_name="checkpoint")
    logs.assert_called_once_with("system", "agent-pod", tail_lines=2000, container="agent")
    assert phases.log_line_found and phases.duration == 2


def test_report_accepts_existing_pvc_results_without_storage_type():
    assert "| vast |" in report.render([{
        "schema_version": 1, "model": {"label": "previous-run"},
        "environment": {"storage_class": "vast"},
    }])


def test_missing_s3_configuration_fails_before_creating_workload(benchmark, config_api):
    execute, mocks = benchmark
    config_api.list_namespaced_config_map.return_value.items = []
    with pytest.raises(RuntimeError, match="S3 configuration"):
        execute("s3")
    mocks["k8s.create_pod"].assert_not_called()
