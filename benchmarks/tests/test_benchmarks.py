# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

import contextlib
import ast
import datetime
import dataclasses
import gc
import io
import importlib.util
import itertools
import threading
import json
import os
from pathlib import Path
import shlex
import subprocess
import sys
import tempfile
import unittest
import traceback
from types import SimpleNamespace
from unittest.mock import patch, Mock

from kubernetes import client

from snapshot_benchmarks import cli, measurement, report, run
from snapshot_benchmarks.engines import ModelSpec
from snapshot_benchmarks.schema import AgentLogPhases, BenchmarkEnvironment, GpuIdentity
from snapshot_benchmarks.timing import restore_metrics
from snapshot_benchmarks.workload import app_configmap, instrument_pod


def config():
    return run.BenchmarkConfig(
        "workloads",
        "snapshot",
        "snapshot",
        "checkpoints",
        None,
        "test-context",
        "gpu-node",
        "models",
        "work",
    )


def cold_measurement():
    return dict(
        weights_bytes=1503300328,
        cold_start_s=31,
        unattributed_s=0,
        phases_s={
            "Process/worker startup": 3,
            "Runtime imports": 1,
            "Model construction and weights": 2,
            "Memory profiling": 3,
            "Attention/sampler/kernel initialization": 4,
            "MHC kernel prewarm": 0,
            "Compilation and JIT module loading": 5,
            "Autotuning": 6,
            "CUDA graph capture": 7,
        },
    )


@contextlib.contextmanager
def cluster():
    """Simulate API boundaries while exercising the real runner and guide builders."""
    pods = {}
    stamp = datetime.datetime(2026, 1, 1, tzinfo=datetime.timezone.utc)

    def create(body):
        command = body["spec"]["containers"][0]["command"]
        name = body["metadata"]["name"]
        pod = client.V1Pod(
            metadata=client.V1ObjectMeta(
                name=name,
                namespace="workloads",
                uid=name + "-uid",
                creation_timestamp=stamp,
            ),
            spec=client.V1PodSpec(
                node_name="gpu-node",
                containers=[client.V1Container(name="main", command=command)],
            ),
            status=client.V1PodStatus(
                phase="Running",
                container_statuses=[],
                conditions=[
                    client.V1PodCondition(
                        type="Ready", status="True", last_transition_time=stamp
                    )
                ],
            ),
        )
        pods[name] = pod
        return pod

    snapshot = {
        "metadata": {"creationTimestamp": stamp.isoformat()},
        "status": {"boundSnapshotContentName": "content"},
    }
    content = {"metadata": {"name": "content", "uid": "content-uid"}}
    metrics = dict(
        total_s=4, agent_s=3, setup_s=1, criu_s=1, cuda_s=1, wake_handoff_s=1
    )
    boundaries = {
        "load_config": None,
        "_validate_snapshot_agent_present": "snapshot-agent:test",
        "_collect_agent_log_phases": AgentLogPhases(),
        "_wait_for_pod_condition": lambda namespace, name, condition, **kwargs: pods[
            name
        ],
        "k8s.create_pod": create,
        "k8s.delete_pod": None,
        "k8s.pod_logs": "failure log",
        "lifecycle.create_podsnapshot": snapshot,
        "lifecycle.wait_for_snapshot_ready": (snapshot, content),
        "lifecycle.wait_for_pod_deleted": None,
        "lifecycle.wait_for_pod_event": None,
        "lifecycle.delete_podsnapshot": None,
        "lifecycle.delete_podsnapshotcontent": None,
        "lifecycle.wait_for_custom_object_deleted": None,
        "lifecycle.wait_for_artifact_root_absent": None,
        "metadata.collect_environment": BenchmarkEnvironment(
            storage_class="test-storage"
        ),
        "metadata.collect_gpu_identity": GpuIdentity(
            gpu_product="Test GPU", gpu_driver_version="test-driver"
        ),
        "metadata.engine_version": "test-version",
        "measurement.source_measurement": (cold_measurement(), "preserved-token"),
        "measurement.checkpoint_measurement": (2_000_000_000, 2),
        "measurement.restored_measurement": metrics,
    }
    with contextlib.ExitStack() as stack:
        mocks = {}
        for name, value in boundaries.items():
            kwargs = (
                dict(side_effect=value) if callable(value) else dict(return_value=value)
            )
            mocks[name] = stack.enter_context(
                patch("snapshot_benchmarks.run." + name, **kwargs)
            )
        mocks["api"] = stack.enter_context(
            patch("snapshot_benchmarks.run.client.CoreV1Api")
        ).return_value
        yield mocks


class BenchmarkTests(unittest.TestCase):
    def test_patcher_limits_constructor_timers_to_startup_modules(self):
        path = Path(__file__).parents[1] / "instrumentation/patch_sources_common.py"
        spec = importlib.util.spec_from_file_location("test_patch_sources", path)
        patcher = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(patcher)
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            for name in ("model_runner.py", "quantization.py"):
                (root / name).write_text(
                    "class Model:\n    def __init__(self): pass\n    def load_weights(self): pass\n"
                )
            with (
                patch.object(patcher, "PACKAGES", ["test_engine"]),
                patch.object(
                    patcher.importlib.util,
                    "find_spec",
                    return_value=SimpleNamespace(submodule_search_locations=[tmp]),
                ),
                contextlib.redirect_stdout(io.StringIO()),
            ):
                patcher.instrument("base")
            for name, timed_init in (
                ("model_runner.py", True),
                ("quantization.py", False),
            ):
                methods = {
                    n.name: n
                    for n in ast.parse((root / name).read_text()).body[-1].body
                }
                self.assertEqual(bool(methods["__init__"].decorator_list), timed_init)
                self.assertTrue(methods["load_weights"].decorator_list)

    def test_trt_guide_serializes_compilation_without_benchmark_hooks(self):
        path = Path(__file__).parents[2] / "docs/guides/tensorrt-llm/app.py"
        nodes = ast.parse(path.read_text()).body
        stop = next(
            i
            for i, n in enumerate(nodes)
            if isinstance(n, ast.ImportFrom) and n.module == "tensorrt_llm"
        )
        for token in (None, "benchmark-token"):
            with patch.dict(os.environ, TORCHINDUCTOR_COMPILE_THREADS="9"):
                exec(
                    compile(
                        ast.Module(body=nodes[:stop], type_ignores=[]),
                        str(path),
                        "exec",
                    ),
                    {"benchmark_token": token},
                )
                self.assertEqual(os.environ["TORCHINDUCTOR_COMPILE_THREADS"], "1")

    def test_sglang_guide_keeps_kv_only_policy(self):
        path = Path(__file__).parents[2] / "docs/guides/sglang/app.py"
        fn = next(
            n
            for n in ast.parse(path.read_text()).body
            if isinstance(n, ast.FunctionDef) and n.name == "create_engine"
        )
        for token, backup in [(None, False), ("benchmark-token", False)]:
            factory = Mock()
            globals_ = dict(os=os, json=json, Any=object, benchmark_token=token)
            with (
                patch.dict(sys.modules, sglang=SimpleNamespace(Engine=factory)),
                patch.dict(os.environ, SNAPSHOT_MODEL="test"),
            ):
                exec(
                    compile(ast.Module(body=[fn], type_ignores=[]), str(path), "exec"),
                    globals_,
                )
                globals_["create_engine"]({"model_path": "test", "tp_size": 2}, True)
            self.assertEqual(
                factory.call_args.kwargs["enable_weights_cpu_backup"], backup
            )

    def test_trt_guide_keeps_kv_sleep_policy(self):
        path = Path(__file__).parents[2] / "docs/guides/tensorrt-llm/app.py"
        fn = next(
            n
            for n in ast.parse(path.read_text()).body
            if isinstance(n, ast.FunctionDef) and n.name == "main"
        )
        for token in (None, "benchmark-token"):
            with tempfile.TemporaryDirectory() as tmp:
                control = Path(tmp)
                (control / "restore-complete").touch()
                llm = Mock()
                factory = Mock(return_value=llm)
                globals_ = dict(
                    os=os,
                    sys=SimpleNamespace(
                        argv=["app.py", '{"model": "test", "tensor_parallel_size": 2}']
                    ),
                    json=json,
                    gc=gc,
                    traceback=traceback,
                    CONTROL_DIR=control,
                    MODEL="test",
                    MAX_NUM_TOKENS=1024,
                    MAX_BATCH_SIZE=1,
                    FREE_GPU_MEMORY_FRACTION=0.1,
                    TRUST_REMOTE_CODE=False,
                    benchmark_token=token,
                    phase=lambda _: contextlib.nullcontext(),
                    mark=lambda _: None,
                    LLM=factory,
                    generate_text=lambda *_: ["paris berlin tokyo"],
                    serve_api=Mock(side_effect=RuntimeError("stop-server")),
                )
                exec(
                    compile(ast.Module(body=[fn], type_ignores=[]), str(path), "exec"),
                    globals_,
                )
                with (
                    contextlib.redirect_stdout(io.StringIO()),
                    self.assertRaisesRegex(RuntimeError, "stop-server"),
                ):
                    globals_["main"]()
                calls = [c.args[0] for c in llm._collective_rpc.call_args_list]
                self.assertEqual(calls, ["sleep", "wakeup"])
                self.assertEqual(factory.call_args.kwargs["dtype"], "auto")

    def test_startup_trace_keeps_no_file_descriptors_between_writes(self):
        path = Path(__file__).parents[1] / "instrumentation/startup_timing.py"
        spec = importlib.util.spec_from_file_location("test_startup_timing", path)
        timing = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(timing)
        with (
            tempfile.TemporaryDirectory() as tmp,
            patch.dict(os.environ, STARTUP_SPAN_DIRECTORY=tmp),
        ):
            for repetition in range(3):
                timing.emit("begin", span=str(repetition))
                for descriptor in Path("/proc/self/fd").iterdir():
                    try:
                        target = os.readlink(descriptor)
                    except FileNotFoundError:
                        continue
                    self.assertFalse(target.startswith(tmp + "/"), target)
            # A restored process can create a thread with a reused identity/counter.
            timing._local = threading.local()
            timing._files = itertools.count()
            timing.emit("begin", span="new-thread-after-restore")
            files = list(Path(tmp).glob("*.jsonl"))
            self.assertEqual(len(files), 1)
            self.assertEqual(len(files[0].read_text().splitlines()), 4)

    def test_source_analysis_counts_overlapping_ranks_once_and_excludes_inference(self):
        lines = []
        for stage, elapsed in [
            ("process_start", 0),
            ("engine_init_begin", 1),
            ("engine_init_end", 10),
            ("first_inference_complete", 15),
        ]:
            lines.append(
                "COLD_BENCH "
                + json.dumps(
                    dict(stage=stage, elapsed_s=elapsed, wall_time_ns=elapsed * 10**9)
                )
            )
        for expected in ("paris", "berlin", "tokyo"):
            lines.append(
                "CR_SEMANTIC_PRECHECK "
                + json.dumps(dict(expected=expected, answer=expected))
            )
        lines.append('CR_PHASE {"checkpoint_token":"token"}')
        spans = []
        for index, (name, start, end) in enumerate(
            [
                ("vllm.ModelRunner.load_model", 2, 6),
                ("vllm.ModelRunner.load_model", 3, 7),
                ("vllm.GPUModelRunner.capture_model", 7, 10),
            ]
        ):
            for event, elapsed in [("begin", start), ("end", end)]:
                spans.append(
                    "STARTUP_SPAN "
                    + json.dumps(
                        dict(
                            event=event,
                            span=str(index),
                            pid=index,
                            rank=index,
                            tid=1,
                            name=name,
                            parent=None,
                            wall_ns=elapsed * 10**9,
                            monotonic_ns=elapsed * 10**9,
                            elapsed_ns=(end - start) * 10**9,
                            success=True,
                        )
                    )
                )
        pod = client.V1Pod(metadata=client.V1ObjectMeta(name="source"))
        with tempfile.TemporaryDirectory() as tmp:
            span_dir = Path(tmp) / "spans"
            span_dir.mkdir()
            (span_dir / "worker-0.jsonl").write_text("\n".join(spans[:2]))
            (span_dir / "worker-1.jsonl").write_text("\n".join(spans[2:]))
            (span_dir / "cache-control.json").write_text(
                json.dumps(
                    dict(
                        success=True,
                        node_page_cache_dropped=True,
                        weights_bytes=1503300328,
                        total_pages=1,
                        resident_pages_after=0,
                    )
                )
            )

            def execute(_namespace, _pod, command, **_kwargs):
                script = shlex.split(command)[2].replace(
                    "/startup-spans", str(span_dir)
                )
                output = io.StringIO()
                with contextlib.redirect_stdout(output):
                    exec(script, {})
                return output.getvalue()

            with (
                patch.object(measurement.k8s, "exec_command", side_effect=execute),
                patch.object(
                    measurement.k8s, "pod_logs", return_value="\n".join(lines)
                ),
            ):
                value, token = measurement.source_measurement(
                    dataclasses.replace(config(), detailed_timing=True), pod, Path(tmp)
                )
                detailed_log = (Path(tmp) / "startup.log").read_text()
                for span_file in span_dir.glob("*.jsonl"):
                    span_file.unlink()
                with patch.object(measurement.subprocess, "run") as analysis:
                    basic, basic_token = measurement.source_measurement(
                        config(), pod, Path(tmp)
                    )
                analysis.assert_not_called()
                self.assertEqual(basic["cold_start_s"], 10)
                self.assertEqual(basic_token, token)
                self.assertFalse(basic["detailed_timing"])
                self.assertNotIn("phases_s", basic)
            self.assertEqual(
                detailed_log,
                "\n".join(lines) + "\n" + "\n".join(spans),
            )
            self.assertEqual(token, "token")
            self.assertEqual(value["weights_bytes"], 1503300328)
            self.assertEqual(value["cold_start_s"], 10)
            self.assertEqual(value["phases_s"]["Model construction and weights"], 5)
            self.assertEqual(value["phases_s"]["CUDA graph capture"], 3)
            self.assertEqual(value["phases_s"]["First generation"], 0)

    def test_restore_timing_requires_wake_and_stops_before_inference(self):
        epoch = 1767225600 * 10**9
        events = [
            dict(phase="wake", event="begin", wall_ns=epoch + 3 * 10**9),
            dict(
                phase="wake",
                event="end",
                wall_ns=epoch + 4 * 10**9,
                success=True,
                elapsed_ns=10**9,
            ),
            dict(phase="generation", event="begin", wall_ns=epoch + 5 * 10**9),
            dict(phase="generation", event="end", wall_ns=epoch + 50 * 10**9),
        ]
        with tempfile.TemporaryDirectory() as tmp:
            folder = Path(tmp)
            measurement.save(
                folder / "pod-final.json",
                dict(metadata=dict(name="restored", namespace="workloads")),
            )
            (folder / "agent.log").write_text(
                '2026-01-01T00:00:00.000000000Z\tINFO === Starting external restore === {"pod":"workloads/restored"}\n'
            )
            measurement.save(
                folder / "timing.json",
                dict(duration="3s", phases=dict(criu_restore="1s", cuda_restore="1s")),
            )

            def write_events():
                (folder / "phases.jsonl").write_text(
                    "".join(json.dumps(e) + "\n" for e in events)
                )

            write_events()
            self.assertEqual(restore_metrics(folder)["total_s"], 4)
            events.pop(1)
            write_events()
            with self.assertRaisesRegex(ValueError, "wake completion"):
                restore_metrics(folder)

    def test_import_tracing_is_disabled_by_default(self):
        for enabled in (False, True):
            output = subprocess.check_output(
                [
                    sys.executable,
                    "-c",
                    "import builtins; print(builtins.__import__.__module__)",
                ],
                env=dict(
                    os.environ,
                    PYTHONPATH=str(Path(__file__).parents[1] / "instrumentation"),
                    BENCHMARK_DETAILED_TIMING="1" if enabled else "0",
                ),
                text=True,
            )
            self.assertEqual(output.strip(), "sitecustomize" if enabled else "builtins")

    def test_detailed_timing_toggle_selects_launcher_and_import_tracing(self):
        for engine in cli.ENGINES.values():
            for enabled in (False, True):
                with self.subTest(engine=engine.name, enabled=enabled):
                    model = ModelSpec("Test", "/hf-cache/test", tp=2)
                    pod = engine.build_source_pod(
                        name="test", namespace="workloads", image="test", model=model
                    )
                    instrument_pod(
                        pod,
                        dataclasses.replace(config(), detailed_timing=enabled),
                        engine,
                        model,
                        "test",
                    )
                    main = next(
                        c
                        for c in pod["spec"]["containers"]
                        if c["name"] == engine.container_name
                    )
                    self.assertEqual(
                        main["command"][-1],
                        "/snapshot-app/launch.py"
                        if enabled
                        else "/snapshot-app/bench.py",
                    )
                    env = {e["name"]: e.get("value") for e in main["env"]}
                    self.assertEqual(
                        env["BENCHMARK_DETAILED_TIMING"], "1" if enabled else "0"
                    )
                    self.assertEqual(
                        Path(main["command"][0]).name, "cuinterpose-launch"
                    )

    def test_basic_run_report_keeps_totals_without_cold_start_breakdown(self):
        with tempfile.TemporaryDirectory() as tmp, cluster() as mocks:
            mocks["measurement.source_measurement"].return_value = (
                dict(cold_start_s=31, detailed_timing=False, weights_bytes=1503300328),
                "preserved-token",
            )
            result = run.run_benchmark(
                config(),
                cli.ENGINES["vllm"],
                ModelSpec("Test", "/hf-cache/test"),
                image="test",
                evidence_dir=Path(tmp),
                run_id="basic",
                progress=lambda _: None,
            )
            text = report.render([result.to_json_dict()])
            self.assertIn("7.75×", text)
            self.assertIn("Where the restore time goes", text)
            self.assertNotIn("Where the cold start time goes", text)

    def test_three_engines_use_guide_contract_and_explicit_topology(self):
        for engine in cli.ENGINES.values():
            for tp, ep in ((1, 1), (2, 1), (4, 1), (8, 8)):
                with self.subTest(engine=engine.name, tp=tp):
                    model = ModelSpec("Test", "/hf-cache/test", tp=tp, ep=ep)
                    args = dict(
                        name="test",
                        namespace="workloads",
                        image="engine:pinned",
                        model=model,
                    )
                    source = instrument_pod(
                        engine.build_source_pod(**args), config(), engine, model, "run"
                    )
                    restore = instrument_pod(
                        engine.build_restore_pod(snapshot_name="snapshot", **args),
                        config(),
                        engine,
                        model,
                        "run",
                        restore=True,
                    )
                    for pod in (source, restore):
                        self.assertEqual(
                            pod["metadata"]["labels"], {"snapshot-benchmark": "run"}
                        )
                        spec = pod["spec"]
                        self.assertEqual(spec["runtimeClassName"], "nvidia")
                        self.assertEqual(
                            spec["securityContext"]["seccompProfile"][
                                "localhostProfile"
                            ],
                            "profiles/block-iouring.json",
                        )
                        self.assertEqual(
                            spec["nodeSelector"]["kubernetes.io/hostname"], "gpu-node"
                        )
                        main = spec["containers"][0]
                        env = {e["name"]: e["value"] for e in main["env"]}
                        span_mount = next(
                            m
                            for m in main["volumeMounts"]
                            if m["mountPath"] == env["STARTUP_SPAN_DIRECTORY"]
                        )
                        span_volume = next(
                            v for v in spec["volumes"] if v["name"] == span_mount["name"]
                        )
                        self.assertEqual(span_volume["emptyDir"]["medium"], "Memory")
                        self.assertIn("sizeLimit", span_volume["emptyDir"])
                        cache_init = [
                            c for c in spec.get("initContainers", [])
                            if c["name"] == "drop-page-cache"
                        ]
                        if pod is restore:
                            self.assertEqual(cache_init, [])
                        else:
                            self.assertEqual(len(cache_init), 1)
                            self.assertTrue(cache_init[0]["securityContext"]["privileged"])
                            self.assertEqual(cache_init[0]["args"][0], model.hf_id_or_path)
                            self.assertEqual(spec["initContainers"][-1], cache_init[0])
                        if engine.name == "trtllm":
                            self.assertEqual(
                                env["FLASHINFER_WORKSPACE_BASE"],
                                "/snapshot-flashinfer-cache",
                            )
                            self.assertFalse(
                                any(
                                    m["mountPath"].startswith(
                                        "/snapshot-flashinfer-cache"
                                    )
                                    for m in main["volumeMounts"]
                                )
                            )
                        self.assertNotIn(
                            "PYTHONPROFILEIMPORTTIME", {e["name"] for e in main["env"]}
                        )
                        if pod is restore:
                            self.assertEqual(main["command"], ["sleep", "infinity"])
                            self.assertEqual(main["args"], [])
                        else:
                            constructor = json.loads(main["args"][0])
                            if engine.name == "sglang":
                                self.assertTrue(
                                    constructor["weight_loader_prefetch_checkpoints"]
                                )
                                self.assertEqual(
                                    constructor["model_path"], model.hf_id_or_path
                                )
                                self.assertEqual(constructor["tp_size"], tp)
                                self.assertEqual(constructor["ep_size"], ep)
                            else:
                                self.assertEqual(constructor["model"], model.hf_id_or_path)
                                self.assertEqual(constructor["tensor_parallel_size"], tp)
                                if engine.name == "vllm":
                                    self.assertEqual(
                                        constructor["enable_expert_parallel"], ep > 1
                                    )
                                else:
                                    self.assertEqual(
                                        constructor["moe_expert_parallel_size"], ep
                                    )
                        self.assertEqual(
                            main["resources"]["limits"]["nvidia.com/gpu"], str(tp)
                        )
                        self.assertIn(
                            dict(
                                name="model-cache", mountPath="/hf-cache", readOnly=True
                            ),
                            main["volumeMounts"],
                        )
                        self.assertFalse(
                            any(
                                c["name"] == "model-cache"
                                for c in spec.get("initContainers", [])
                            )
                        )
                        if tp > 1:
                            self.assertEqual(
                                pod["metadata"]["annotations"][
                                    "nvidia.com/snapshot-multi-gpu"
                                ],
                                "enabled",
                            )
                    if tp > 1:
                        self.assertEqual(
                            Path(source["spec"]["containers"][0]["command"][0]).name,
                            "cuinterpose-launch",
                        )
                        installer = next(
                            c
                            for c in source["spec"]["initContainers"]
                            if c["name"] == "snapshot-cuda-install"
                        )
                        self.assertIn(
                            "/usr/local/bin/cuinterpose-launch", installer["args"]
                        )
                    self.assertEqual(
                        [
                            m
                            for m in source["spec"]["containers"][0]["volumeMounts"]
                            if m["name"] != "snapshot-cuda"
                        ],
                        restore["spec"]["containers"][0]["volumeMounts"],
                    )
                    self.assertFalse(
                        any(
                            c["name"] == "fresh-cache"
                            for c in restore["spec"]["initContainers"]
                        )
                    )
                    for filename, code in app_configmap(engine, "workloads", "app")[
                        "data"
                    ].items():
                        compile(code, filename, "exec")

    def test_sglang_prefetch_can_be_disabled(self):
        engine = cli.ENGINES["sglang"]
        model = ModelSpec(
            "Test",
            "/hf-cache/test",
            engine_args={"weight_loader_prefetch_checkpoints": False},
        )
        pod = engine.build_source_pod(
            name="test", namespace="workloads", image="engine:pinned", model=model
        )
        instrument_pod(pod, config(), engine, model, "run")
        constructor = json.loads(pod["spec"]["containers"][0]["args"][0])
        self.assertFalse(constructor["weight_loader_prefetch_checkpoints"])

    def test_restore_waits_without_starting_a_second_engine(self):
        for engine in cli.ENGINES.values():
            for tp in (1, 8):
                with self.subTest(engine=engine.name, tp=tp):
                    model = ModelSpec("Test", "/hf-cache/test", tp=tp)
                    pod = instrument_pod(
                        engine.build_restore_pod(
                            name="restored",
                            namespace="workloads",
                            image="engine:pinned",
                            model=model,
                            snapshot_name="snapshot",
                        ),
                        config(),
                        engine,
                        model,
                        "run",
                        restore=True,
                    )
                    main = pod["spec"]["containers"][0]
                    process = subprocess.Popen(
                        main["command"] + main.get("args", []),
                        stdout=subprocess.DEVNULL,
                        stderr=subprocess.DEVNULL,
                    )
                    try:
                        with self.assertRaises(subprocess.TimeoutExpired):
                            process.wait(timeout=0.1)
                    finally:
                        process.terminate()
                        process.wait(timeout=5)

    def test_cache_initializer_rejects_a_reused_run(self):
        engine = cli.ENGINES["trtllm"]
        model = ModelSpec("Test", "/hf-cache/test")
        pod = instrument_pod(
            engine.build_source_pod(
                name="test", namespace="workloads", image="test", model=model
            ),
            config(),
            engine,
            model,
            "run",
        )
        initializer = next(
            c for c in pod["spec"]["initContainers"] if c["name"] == "fresh-cache"
        )
        script = initializer["command"][-1]
        with tempfile.TemporaryDirectory() as tmp:
            script = script.replace("/work", tmp)
            subprocess.run([sys.executable, "-c", script], check=True)
            self.assertTrue((Path(tmp) / "run/compile-cache").is_dir())
            self.assertFalse((Path(tmp) / "run/compile-cache/startup-spans").exists())
            repeated = subprocess.run(
                [sys.executable, "-c", script], capture_output=True
            )
            self.assertNotEqual(repeated.returncode, 0)

    def test_original_runner_captures_once_and_checks_three_restores(self):
        for engine in cli.ENGINES.values():
            with (
                self.subTest(engine=engine.name),
                tempfile.TemporaryDirectory() as tmp,
                cluster() as mocks,
            ):
                result = run.run_benchmark(
                    config(),
                    engine,
                    ModelSpec("Test", "/hf-cache/test", tp=2),
                    image="test",
                    evidence_dir=Path(tmp),
                    run_id="test",
                    progress=lambda _: None,
                )
                self.assertEqual(result.model.reported_weights_bytes, 1503300328)
                self.assertEqual(result.cold_start.startup_to_ready_seconds, 31)
                self.assertEqual(result.restore.startup_to_ready_seconds, 4)
                self.assertEqual(len(result.restore_attempts), 3)
                self.assertEqual(mocks["lifecycle.create_podsnapshot"].call_count, 1)
                self.assertEqual(
                    mocks["measurement.restored_measurement"].call_count, 3
                )
                mocks["load_config"].assert_called_once_with(
                    str(Path.home() / ".kube/config"), "test-context"
                )
                mocks["lifecycle.delete_podsnapshotcontent"].assert_called_once_with(
                    "content"
                )
                self.assertTrue((Path(tmp) / "test/result.json").is_file())
                text = report.render([result.to_json_dict()])
                self.assertIn("7.75×", text)
                self.assertIn("| 4.00 | 2.00 | 7.00 | 18.00 | 31.00 |", text)

    def test_failure_retains_evidence_and_cleans_owned_resources(self):
        with tempfile.TemporaryDirectory() as tmp, cluster() as mocks:
            mocks["measurement.restored_measurement"].side_effect = ValueError(
                "missing wake"
            )
            with self.assertRaisesRegex(ValueError, "missing wake"):
                run.run_benchmark(
                    config(),
                    cli.ENGINES["vllm"],
                    ModelSpec("Test", "/hf-cache/test", tp=2),
                    image="test",
                    evidence_dir=Path(tmp),
                    run_id="failed",
                    progress=lambda _: None,
                )
            self.assertTrue((Path(tmp) / "failed/failure.json").is_file())
            self.assertFalse((Path(tmp) / "failed/result.json").exists())
            mocks["lifecycle.delete_podsnapshotcontent"].assert_called_once_with(
                "content"
            )
            mocks["api"].delete_namespaced_config_map.assert_called_once_with(
                "bench-app-failed", "workloads"
            )

    def test_dry_run_never_loads_cluster_config(self):
        with (
            tempfile.TemporaryDirectory() as tmp,
            patch("snapshot_benchmarks.run.load_config") as load,
        ):
            code = cli.main(
                [
                    "run",
                    "--context",
                    "test",
                    "--namespace",
                    "test",
                    "--node",
                    "test",
                    "--model-pvc",
                    "models",
                    "--work-pvc",
                    "work",
                    "--engine",
                    "sglang",
                    "--model",
                    "/hf-cache/test",
                    "--image",
                    "test",
                    "--tp",
                    "8",
                    "--ep",
                    "8",
                    "--dry-run",
                    "--output-dir",
                    tmp,
                ]
            )
            self.assertEqual(code, 0)
            load.assert_not_called()
            self.assertEqual(len(list(Path(tmp).rglob("source.json"))), 1)

    def test_bad_topology_and_constructor_overrides_are_rejected(self):
        for kwargs in (
            dict(tp=0),
            dict(tp=4, ep=3),
            dict(engine_args={"tensor_parallel_size": 8}),
        ):
            with self.subTest(kwargs=kwargs), self.assertRaises(ValueError):
                ModelSpec("Test", "test", **kwargs)

    def test_remote_failure_cannot_look_like_valid_json(self):
        with patch.object(
            measurement.k8s, "exec_command", return_value='{"apparently":"valid"}'
        ):
            with self.assertRaisesRegex(RuntimeError, "collection failed"):
                measurement.remote_json("test", "test", "raise RuntimeError('failed')")

    def test_checkpoint_requires_custom_storage_and_matches_content(self):
        text = 'Checkpoint timing summary {"content":"content", "checkpoint":{"duration":"1.2s"}}'
        with (
            tempfile.TemporaryDirectory() as tmp,
            patch.object(
                measurement.lifecycle, "checkpoint_agent_pod", return_value="agent"
            ),
            patch.object(
                measurement.k8s,
                "exec_command",
                return_value="gpu/123/manifest.txt\0" + "12\0BENCHMARK_FIND_COMPLETE\0",
            ) as execute,
            patch.object(measurement, "agent_log", return_value=text),
        ):
            self.assertEqual(
                measurement.checkpoint_measurement(
                    config(), "gpu-node", "uid", "content", Path(tmp)
                ),
                (12, 1.2),
            )
            self.assertNotIn("python", execute.call_args.args[2])
            execute.return_value = "pages.img\0" + "12\0BENCHMARK_FIND_COMPLETE\0"
            with self.assertRaisesRegex(ValueError, "CustomStorage"):
                measurement.checkpoint_measurement(
                    config(), "gpu-node", "uid", "content", Path(tmp)
                )

    def test_checkpoint_inventory_uses_available_agent_tools(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            file = root / "gpu/123/manifest.txt"
            file.parent.mkdir(parents=True)
            file.write_text("gpu manifest")
            (root / "a name\twith\nwhitespace").write_bytes(b"abcd")
            (root / "symlink").symlink_to(file)

            def execute(namespace, pod, command, **kwargs):
                return subprocess.check_output(["bash", "-c", command], text=True)

            with (
                patch.object(
                    measurement.lifecycle, "checkpoint_agent_pod", return_value="agent"
                ),
                patch.object(
                    measurement.lifecycle,
                    "checkpoint_artifact_path",
                    return_value=shlex.quote(tmp),
                ),
                patch.object(measurement.k8s, "exec_command", side_effect=execute),
                patch.object(
                    measurement,
                    "agent_log",
                    return_value='Checkpoint timing summary {"content":"content", "checkpoint":{"duration":"1s"}}',
                ),
            ):
                size, duration = measurement.checkpoint_measurement(
                    config(), "node", "uid", "content", root
                )
                self.assertEqual((size, duration), (16, 1))
            with (
                patch.object(
                    measurement.lifecycle, "checkpoint_agent_pod", return_value="agent"
                ),
                patch.object(
                    measurement.k8s, "exec_command", return_value="find: failure"
                ),
                patch.object(measurement, "agent_log", return_value=""),
                self.assertRaisesRegex(RuntimeError, "inventory did not complete"),
            ):
                measurement.checkpoint_measurement(
                    config(), "node", "uid", "content", root
                )

    def test_schema_one_results_are_still_readable_and_separate(self):
        self.assertIn(
            "Cold start vs.",
            report.render([dict(schema_version=1, model={}, environment={})]),
        )
        with self.assertRaisesRegex(ValueError, "unsupported schema_version"):
            report.render([dict(schema_version=True)])

    def test_version_probe_does_not_import_the_engine(self):
        for engine in cli.ENGINES.values():
            command = shlex.split(engine.version_probe_command)
            self.assertEqual(command[:2], ["python3", "-c"])
            self.assertIn("importlib.metadata", command[2])


if __name__ == "__main__":
    unittest.main()
