# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

import asyncio
import os
import traceback
from pathlib import Path
from uuid import uuid4

import uvicorn
from fastapi import FastAPI
from pydantic import BaseModel, Field
from vllm import SamplingParams
from vllm.engine.arg_utils import AsyncEngineArgs
from vllm.usage.usage_lib import UsageContext
from vllm.v1.engine.async_llm import AsyncLLM

CONTROL_DIR = Path(os.environ.get("SNAPSHOT_CONTROL_DIR", "/snapshot-control"))
MODEL = os.environ["SNAPSHOT_MODEL"]
# Small single-GPU sizing so the example fits alongside other GPU tenants and
# keeps the checkpoint artifact small. Override through the Pod template.
MAX_MODEL_LEN = int(os.environ.get("VLLM_MAX_MODEL_LEN", "2048"))
GPU_MEMORY_UTILIZATION = float(os.environ.get("VLLM_GPU_MEMORY_UTILIZATION", "0.30"))
# Off: Qwen3 needs no custom model code, and remote code execution should be
# an explicit opt-in. Set this to True only for checkpoints that ship their
# own modeling code.
TRUST_REMOTE_CODE = False


class GenerateRequest(BaseModel):
    prompt: str = Field(min_length=1)


async def generate_text(
    engine: AsyncLLM,
    prompt: str,
    request_id: str,
) -> str:
    result = None
    async for output in engine.generate(
        prompt,
        SamplingParams(temperature=0.0, max_tokens=8),
        request_id,
    ):
        result = output
    if result is None or not result.outputs:
        raise RuntimeError("vLLM produced no output")
    text = result.outputs[0].text.strip()
    if not text:
        raise RuntimeError("vLLM produced empty output")
    return text


async def serve_api(engine: AsyncLLM, restored_text: str) -> None:
    app = FastAPI()

    @app.post("/generate")
    async def generate(request: GenerateRequest) -> dict[str, str]:
        text = await generate_text(
            engine,
            request.prompt,
            f"request-{uuid4().hex}",
        )
        return {"text": text}

    server = uvicorn.Server(
        uvicorn.Config(
            app,
            host="0.0.0.0",
            port=8000,
        )
    )
    server_task = asyncio.create_task(server.serve())
    while not server.started:
        if server_task.done():
            await server_task
            raise RuntimeError("API stopped before startup")
        await asyncio.sleep(0.1)

    CONTROL_DIR.joinpath("vllm-restore-ready").write_text(
        restored_text + "\n",
        encoding="utf-8",
    )
    print("vLLM API listening on port 8000", flush=True)
    await server_task


async def main() -> None:
    if os.environ.get("SNAPSHOT_RESTORE_STANDBY") == "1":
        await asyncio.Event().wait()

    CONTROL_DIR.joinpath("ready-for-snapshot").unlink(missing_ok=True)

    engine = AsyncLLM.from_engine_args(
        AsyncEngineArgs(
            model=MODEL,
            enable_sleep_mode=True,
            max_model_len=MAX_MODEL_LEN,
            gpu_memory_utilization=GPU_MEMORY_UTILIZATION,
            trust_remote_code=TRUST_REMOTE_CODE,
        ),
        usage_context=UsageContext.LLM_CLASS,
    )

    text = await generate_text(
        engine,
        "Reply with one word: ready",
        "snapshot-preflight",
    )
    print(f"vLLM pre-checkpoint output={text!r}", flush=True)
    # Durable evidence that the engine served a generation before capture; the
    # source container is killed by the dump, so logs alone are easy to lose.
    CONTROL_DIR.joinpath("vllm-precheck").write_text(text + "\n", encoding="utf-8")

    await engine.pause_generation()
    await engine.sleep()
    CONTROL_DIR.joinpath("ready-for-snapshot").write_text(
        "ready\n",
        encoding="utf-8",
    )

    while True:
        if CONTROL_DIR.joinpath("restore-complete").exists():
            # The restored process keeps the source container's stdout, which
            # is gone; a failure here would otherwise be invisible. Record it
            # in the control directory next to the success sentinel.
            try:
                progress = CONTROL_DIR.joinpath("vllm-restore-progress")
                await engine.wake_up()
                progress.write_text("woken\n", encoding="utf-8")
                await engine.resume_generation()
                await engine.check_health()
                progress.write_text("generation-resumed\n", encoding="utf-8")
                text = await generate_text(
                    engine,
                    "Reply with one word: restored",
                    "snapshot-restore-check",
                )
                progress.write_text("generated\n", encoding="utf-8")
                print(f"vLLM restored output={text!r}", flush=True)
                await serve_api(engine, text)
            except BaseException:
                CONTROL_DIR.joinpath("vllm-restore-error").write_text(
                    traceback.format_exc(),
                    encoding="utf-8",
                )
                raise
        await asyncio.sleep(1)


if __name__ == "__main__":
    asyncio.run(main())
    os._exit(0)
