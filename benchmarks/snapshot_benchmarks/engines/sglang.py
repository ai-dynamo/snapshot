# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

from snapshot_benchmarks.engines.common import GuideEngine


class SGLangEngine(GuideEngine):
    name = "sglang"
    guide = "sglang"
    version_probe_command = "python3 -c 'from importlib.metadata import version; print(version(\"sglang\"))'"
