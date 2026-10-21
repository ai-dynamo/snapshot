# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Inventory measured phase intervals; preserve overlap and unattributed time explicitly."""

import datetime
import json
import re
from pathlib import Path

import argparse

parser = argparse.ArgumentParser()
parser.add_argument("directory", type=Path)
args = parser.parse_args()
P = args.directory


def category(name):
    if name.startswith("import."):
        return "Imports"
    if name.endswith(
        (".load_model", ".load_weights_only", ".load_weights", "._call_load_weights")
    ):
        return "Model loading"
    if name.startswith("vllm.compilation.") and (
        "compile" in name or "serialize_compile_artifacts" in name
    ):
        return "Torch compilation"
    if name.startswith(
        "sglang.srt.layers.deep_gemm_wrapper.compile_utils."
    ) and name.endswith(
        ("._compile_deep_gemm_one_type_all", "._maybe_compile_deep_gemm_one_type_all")
    ):
        return "JIT module build/load"
    if name == "vllm.model_executor.warmup.deep_gemm_warmup.deep_gemm_warmup":
        return "JIT module build/load"
    if (
        name in ["flashinfer.sampling.get_sampling_module"]
        or name.endswith((".build_and_load", ".run_ninja"))
        or (name.startswith(("cutlass.", "tilelang.")) and "compile" in name)
    ):
        return "JIT module build/load"
    if name.endswith("._prewarm_mhc_kernels"):
        return "MHC kernel prewarm"
    if name.endswith(("._run_autotuner_warmup", ".autotune_prefill_kernels")):
        return "Autotuning"
    if name.endswith(".GPUModelRunner.capture_model") or name.endswith(
        (
            ".PrefillCudaGraphRunner.capture",
            ".DecodeCudaGraphRunner.capture",
            ".CUDAGraphRunner.capture",
            ".capture_cuda_graphs",
        )
    ):
        return "CUDA graph capture"
    if name.endswith(("._run_attention_warmup", ".init_attention_backends")):
        return "Attention initialization"
    if name.endswith(".TorchSampler.__init__"):
        return "Sampler initialization"
    if name.endswith(
        (
            ".Worker.init_device",
            ".initialize_model_parallel",
            ".init_distributed_environment",
        )
    ):
        return "Device and distributed initialization"
    if name.endswith(".GPUModelRunner.profile_run"):
        return "Memory profiling"
    return None


results = []
for case in json.loads((P / "cold-runs.json").read_text())["cases"]:
    d = Path(case["directory"])
    if not (d / "measurement.json").exists() or not (d / "spans.json").exists():
        continue
    measurement = json.loads((d / "measurement.json").read_text())
    if not measurement["success"]:
        continue
    spans = json.loads((d / "spans.json").read_text())
    marks = {m["stage"]: m for m in measurement["markers"]}
    origin = (
        marks["process_start"]["wall_time_ns"] / 1e9
        - marks["process_start"]["elapsed_s"]
    )
    end = marks["first_inference_complete"]["elapsed_s"]
    intervals = []
    for span in spans["spans"]:
        label = category(span["name"])
        if label:
            a = max(0, span["start_wall_ns"] / 1e9 - origin)
            b = min(end, span["end_wall_ns"] / 1e9 - origin)
            if b > a:
                intervals.append(
                    dict(
                        category=label,
                        start=a,
                        end=b,
                        source=span["name"],
                        pid=span["pid"],
                    )
                )
    starts = []
    ends = []
    native = []
    for line in (d / "startup.log").read_text().splitlines():
        if "Autotuning process starts" in line or "Autotuning process ends" in line:
            match = re.search(r"(\d{4}-\d\d-\d\d \d\d:\d\d:\d\d,\d+)", line)
            if match:
                stamp = (
                    datetime.datetime.strptime(match[1], "%Y-%m-%d %H:%M:%S,%f")
                    .replace(tzinfo=datetime.timezone.utc)
                    .timestamp()
                    - origin
                )
                (starts if "starts" in line else ends).append(stamp)
        if (
            "Running FlashInfer autotune with cache:" in line
            or "FlashInfer autotune completed." in line
        ):
            match = re.search(
                r"\[(\d{4}-\d\d-\d\d \d\d:\d\d:\d\d)(?: TP(\d+))?(?: EP\d+)?\]", line
            )
            if match:
                stamp = (
                    datetime.datetime.strptime(match[1], "%Y-%m-%d %H:%M:%S")
                    .replace(tzinfo=datetime.timezone.utc)
                    .timestamp()
                    - origin
                )
                native.append(
                    dict(
                        rank=match[2],
                        event="begin" if "Running" in line else "end",
                        stamp=stamp,
                    )
                )
    # All vLLM ranks start one synchronized autotune pass; retain its wall-clock envelope.
    if starts and ends:
        intervals.append(
            dict(
                category="Autotuning",
                start=min(starts),
                end=max(ends),
                source="native FlashInfer logs (millisecond precision; concurrent rank envelope)",
                pid=None,
            )
        )
    for rank in {x["rank"] for x in native}:
        pending = None
        for event in [x for x in native if x["rank"] == rank]:
            if event["event"] == "begin":
                assert pending is None, (
                    case["case"],
                    rank,
                    "overlapping native autotune rounds",
                )
                pending = event["stamp"]
            else:
                assert pending is not None and event["stamp"] >= pending, (
                    case["case"],
                    rank,
                    "unpaired native autotune end",
                )
                intervals.append(
                    dict(
                        category="Autotuning",
                        start=pending,
                        end=event["stamp"],
                        source="native SGLang logs (one-second precision)",
                        rank=rank,
                        pid=None,
                    )
                )
                pending = None
        assert pending is None, (case["case"], rank, "unpaired native autotune begin")
    intervals.append(
        dict(
            category="First generation",
            start=marks["engine_init_end"]["elapsed_s"],
            end=end,
            source="COLD_BENCH markers",
            pid=None,
        )
    )
    results.append(dict(case=case["case"], intervals=intervals))
(P / "phase-inventory.json").write_text(json.dumps(results, indent=2) + "\n")
