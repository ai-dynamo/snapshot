# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Run the guide application with optional timing and state-preservation hooks."""

import json
import runpy
import time
from pathlib import Path

start = time.monotonic()


def mark(stage):
    print(
        "COLD_BENCH "
        + json.dumps(
            dict(
                stage=stage,
                elapsed_s=time.monotonic() - start,
                wall_time_ns=time.time_ns(),
            )
        ),
        flush=True,
    )


mark("process_start")
print(
    "BENCHMARK_MOUNTS "
    + json.dumps(
        [
            line
            for line in Path("/proc/mounts").read_text().splitlines()
            if " /hf-cache " in line or " /compile-cache " in line
        ]
    ),
    flush=True,
)
from cr_timing import phase, RUN_TOKEN

runpy.run_path(
    "/snapshot-app/app.py",
    run_name="__main__",
    init_globals=dict(
        benchmark_mark=mark,
        benchmark_phase=phase,
        benchmark_token=RUN_TOKEN,
    ),
)
