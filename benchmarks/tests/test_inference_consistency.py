# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

import ast
import asyncio
import json
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock, Mock
from uuid import uuid4

import pytest

from snapshot_benchmarks import measurement, run, cli
from snapshot_benchmarks.engines import ModelSpec
from test_benchmarks import cluster, config, cold_measurement


@pytest.mark.parametrize("framework", ["vllm", "sglang", "tensorrt-llm"])
@pytest.mark.parametrize("answer", ["a must-visit destination for anyone interested", " "])
def test_guide_precheck_records_model_answers(framework, answer, capsys):
    path = Path(__file__).parents[2] / "docs/guides" / framework / "app.py"
    # Execute the guide's actual benchmark block without importing a GPU engine.
    main = next(
        n for n in ast.parse(path.read_text()).body
        if isinstance(n, (ast.FunctionDef, ast.AsyncFunctionDef)) and n.name == "main"
    )
    precheck = next(
        n for n in ast.walk(main)
        if isinstance(n, ast.If) and isinstance(n.test, ast.Name)
        and n.test.id == "benchmark_token"
    )
    wrapper = ast.parse("async def check():\n    pass\n" if framework == "vllm" else "def check():\n    pass\n")
    wrapper.body[0].body = [precheck]
    generate = (
        AsyncMock(return_value=answer) if framework == "vllm"
        else Mock(return_value=[answer] if framework == "tensorrt-llm" else answer)
    )
    namespace = dict(
        benchmark_token="token", json=json, uuid4=uuid4,
        generate_text=generate, engine=object(), llm=object(),
    )
    exec(compile(ast.fix_missing_locations(wrapper), str(path), "exec"), namespace)

    def check():
        if framework == "vllm":
            asyncio.run(namespace["check"]())
        else:
            namespace["check"]()

    if not answer.strip():
        with pytest.raises(RuntimeError, match="Source inference check failed"):
            check()
    else:
        check()
        records = [
            json.loads(line.removeprefix("CR_SEMANTIC_PRECHECK "))
            for line in capsys.readouterr().out.splitlines()
        ]
        assert [r["prompt"] for r in records] == list(measurement.INFERENCE_PROMPTS)
        assert all(r["answer"] == answer for r in records)
        assert measurement.source_checks_pass(records)


def test_runner_passes_source_baseline_to_every_restore(tmp_path):
    with cluster() as mocks:
        run.run_benchmark(
            config(), cli.ENGINES["vllm"], ModelSpec("Test", "/hf-cache/test"),
            image="test", run_id="baseline", evidence_dir=tmp_path,
        )
    calls = mocks["measurement.restored_measurement"].call_args_list
    assert len(calls) == 3
    assert all(call.kwargs["source_checks"] == cold_measurement()["inference_checks"] for call in calls)


def test_changed_restored_answer_fails_before_timing_collection(monkeypatch, tmp_path):
    baseline = cold_measurement()["inference_checks"]
    checks = [
        dict(prompt=item["prompt"], response=dict(text=item["answer"], checkpoint_token="token"))
        for item in baseline
    ]
    checks[0]["response"]["text"] = "different answer"
    monkeypatch.setattr(
        measurement, "remote_json", Mock(return_value=dict(checks=checks, phases="")),
    )
    timings = Mock()
    monkeypatch.setattr(measurement, "restore_metrics", timings)
    with pytest.raises(ValueError, match="inference or process-token"):
        measurement.restored_measurement(
            config(), SimpleNamespace(metadata=SimpleNamespace(name="restored")),
            "snapshot", "token", tmp_path, source_checks=baseline,
        )
    assert json.loads((tmp_path / "inference.json").read_text()) == checks
    timings.assert_not_called()
