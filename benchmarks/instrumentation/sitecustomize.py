# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Time first heavy-package imports in each Python worker."""

import builtins
import os
import sys
from startup_timing import phase

_original_import = builtins.__import__
_roots = {
    "torch",
    "transformers",
    "vllm",
    "sglang",
    "tensorrt_llm",
    "flashinfer",
    "tilelang",
}


def _timed_import(name, globals=None, locals=None, fromlist=(), level=0):
    root = name.partition(".")[0]
    if level == 0 and root in _roots and root not in sys.modules:
        return phase("import." + root)(_original_import)(
            name, globals, locals, fromlist, level
        )
    return _original_import(name, globals, locals, fromlist, level)


if os.environ.get("BENCHMARK_DETAILED_TIMING") == "1":
    builtins.__import__ = _timed_import
