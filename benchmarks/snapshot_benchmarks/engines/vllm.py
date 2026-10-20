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
