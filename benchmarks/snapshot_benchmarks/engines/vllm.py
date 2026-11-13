# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""vLLM pods reuse the deployment templates in the manual guide."""

from snapshot_benchmarks.engines.common import GuideEngine


class VLLMEngine(GuideEngine):
    name = "vllm"
    guide = "vllm"
    version_probe_command = (
        "python3 -c 'from importlib.metadata import version; print(version(\"vllm\"))'"
    )

    def benchmark_env(self):
        return dict(
            VLLM_CACHE_ROOT="/compile-cache/vllm",
            VLLM_FLASHINFER_AUTOTUNE_CACHE_DIR="/compile-cache/vllm/flashinfer_autotune",
            PYTORCH_ALLOC_CONF="pinned_max_round_threshold_mb:1,pinned_max_cached_size_mb:1",
        )

    def constructor_args(self, model):
        if model.ep not in (1, model.tp):
            raise ValueError("vLLM expert parallelism must be 1 or TP")
        args = dict(model.engine_args)
        args["enable_expert_parallel"] = model.ep > 1
        args.update(model=model.hf_id_or_path, tensor_parallel_size=model.tp)
        return args
