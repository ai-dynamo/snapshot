# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

import asyncio
import os
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
    if os.environ.get("DYN_SNAPSHOT_RESTORE_STANDBY") == "1":
        await asyncio.Event().wait()

    CONTROL_DIR.joinpath("ready-for-snapshot").unlink(missing_ok=True)

    engine = AsyncLLM.from_engine_args(
        AsyncEngineArgs(
            model=MODEL,
            enable_sleep_mode=True,
        ),
        usage_context=UsageContext.LLM_CLASS,
    )

    text = await generate_text(
        engine,
        "Reply with one word: ready",
        "snapshot-preflight",
    )
    print(f"vLLM pre-checkpoint output={text!r}", flush=True)

    await engine.pause_generation()
    await engine.sleep()
    CONTROL_DIR.joinpath("ready-for-snapshot").write_text(
        "ready\n",
        encoding="utf-8",
    )

    while True:
        if CONTROL_DIR.joinpath("restore-complete").exists():
            await engine.wake_up()
            await engine.resume_generation()
            await engine.check_health()
            text = await generate_text(
                engine,
                "Reply with one word: restored",
                "snapshot-restore-check",
            )
            print(f"vLLM restored output={text!r}", flush=True)
            await serve_api(engine, text)
        await asyncio.sleep(1)


if __name__ == "__main__":
    asyncio.run(main())
    os._exit(0)
