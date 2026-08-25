# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

import gc
import time
from pathlib import Path

from tensorrt_llm import LLM, SamplingParams

CONTROL_DIR = Path("/snapshot-control")


def quiesce_for_snapshot() -> None:
    gc.collect()
    CONTROL_DIR.joinpath("ready-for-snapshot").write_text(
        "ready\n",
        encoding="utf-8",
    )
    while not CONTROL_DIR.joinpath("restore-complete").exists():
        time.sleep(1)


def resume_after_restore(llm: LLM) -> str:
    outputs = llm.generate(
        ["Reply with one word: ready"],
        SamplingParams(temperature=0.0, max_tokens=16),
        use_tqdm=False,
    )
    text = outputs[0].outputs[0].text.strip()
    if not text:
        raise RuntimeError("TensorRT-LLM produced empty output after restore")
    CONTROL_DIR.joinpath("trtllm-restore-ready").write_text(
        text + "\n",
        encoding="utf-8",
    )
    return text
