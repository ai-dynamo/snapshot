# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

from snapshot_benchmarks.engines.common import GuideEngine


class TRTLLMEngine(GuideEngine):
    name = "trtllm"
    guide = "tensorrt-llm"
    version_probe_command = "python3 -c 'from importlib.metadata import version; print(version(\"tensorrt_llm\"))'"

    def benchmark_env(self):
        return dict(
            OMPI_MCA_pml="ob1",
            OMPI_MCA_btl="tcp,self",
            # Loaded/deleted extension files must survive independently per restore.
            FLASHINFER_WORKSPACE_BASE="/snapshot-flashinfer-cache",
        )

    def constructor_args(self, model):
        args = dict(model.engine_args)
        args["moe_expert_parallel_size"] = model.ep
        args.update(model=model.hf_id_or_path, tensor_parallel_size=model.tp)
        return args
