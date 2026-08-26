# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

import asyncio
from pathlib import Path
from typing import Literal

from vllm.v1.engine.async_llm import AsyncLLM

CONTROL_DIR = Path("/snapshot-control")


async def quiesce_for_snapshot(
    engine: AsyncLLM,
) -> Literal["snapshot", "restore"]:
    await engine.pause_generation(mode="wait")
    await engine.sleep()
    CONTROL_DIR.joinpath("ready-for-snapshot").write_text(
        "ready\n",
        encoding="utf-8",
    )

    while True:
        if CONTROL_DIR.joinpath("snapshot-complete").exists():
            return "snapshot"
        if CONTROL_DIR.joinpath("restore-complete").exists():
            return "restore"
        await asyncio.sleep(1)


async def resume_after_restore(engine: AsyncLLM) -> None:
    await engine.wake_up()
    await engine.resume_generation()
    CONTROL_DIR.joinpath("vllm-restore-ready").write_text(
        "ready\n",
        encoding="utf-8",
    )
