# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

from snapshot_benchmarks.engines.common import GuideEngine


class SGLangEngine(GuideEngine):
    name = "sglang"
    guide = "sglang"
    version_probe_command = "python3 -c 'from importlib.metadata import version; print(version(\"sglang\"))'"

    def benchmark_env(self):
        return dict(
            TRITON_CACHE_DIR="/snapshot-triton-cache",
            SGLANG_CACHE_DIR="/compile-cache/sglang",
            SGLANG_JIT_CACHE_DIR="/compile-cache/sglang-jit",
        )

    def constructor_args(self, model):
        args = dict(model.engine_args)
        args.setdefault("weight_loader_prefetch_checkpoints", True)
        args["ep_size"] = model.ep
        args.update(model_path=model.hf_id_or_path, tp_size=model.tp)
        return args
