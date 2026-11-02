# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

from contextlib import nullcontext

# The benchmark wrapper supplies these hooks. Normal guide runs need no helper files.
phase = globals().get("benchmark_phase", lambda _: nullcontext())
mark = globals().get("benchmark_mark", lambda _: None)
benchmark_token = globals().get("benchmark_token")

import argparse
import json
import os
import time
import traceback
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from typing import Any

CONTROL_DIR = Path(os.environ.get("SNAPSHOT_CONTROL_DIR", "/snapshot-control"))


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--mode", choices=("prime", "snapshot"), default="snapshot")
    # JSON keys are Engine API keyword arguments, not CLI flags.
    parser.add_argument("engine_args", type=json.loads)
    return parser.parse_args()


def configure_capture_environment() -> None:
    os.environ.update(
        {
            "NCCL_IB_DISABLE": "1",
            "NCCL_RAS_ENABLE": "0",
            "TORCH_NCCL_ENABLE_MONITORING": "0",
            "TORCH_NCCL_DUMP_ON_TIMEOUT": "0",
            "HF_HUB_OFFLINE": "1",
        }
    )


def create_engine(overrides: dict[str, Any], snapshot_mode: bool) -> Any:
    import sglang as sgl

    engine_args = {
        "context_length": int(os.environ.get("SGLANG_CONTEXT_LENGTH", "10240")),
        "page_size": int(os.environ.get("SGLANG_PAGE_SIZE", "16")),
        "trust_remote_code": False,
        "log_level": "info",
        **overrides,
        # Capture releases KV through the memory saver and keeps no CPU weight copy.
        "enable_memory_saver": snapshot_mode,
        "enable_weights_cpu_backup": False,
    }
    return sgl.Engine(**engine_args)


def generate_text(engine: Any, prompt: str) -> str:
    with phase("generation"):
        result = engine.generate(
            prompt,
            sampling_params={"temperature": 0, "max_new_tokens": 16},
        )
        text = result.get("text", "").strip()
        if not text:
            raise RuntimeError("SGLang produced empty output")
        return text


def pause_generation(engine: Any) -> None:
    from sglang.srt.managers.io_struct import PauseGenerationReqInput

    # "retract" drains the scheduler before pausing: it processes any pending
    # overlap result, clears last_batch and running_batch, and re-queues
    # unfinished requests (there are none here; the warm-up generation has
    # returned). release_memory_occupation() asserts is_fully_idle(), which
    # also requires last_batch to be empty. The default "in_place" mode freezes
    # scheduler state untouched, so pausing right after a generation leaves a
    # stale last_batch and the release fails with "should be called only when
    # server is idle" (SGLang 0.5.17).
    engine.loop.run_until_complete(
        engine.tokenizer_manager.pause_generation(
            PauseGenerationReqInput(mode="retract")
        )
    )


def continue_generation(engine: Any) -> None:
    from sglang.srt.managers.io_struct import ContinueGenerationReqInput

    engine.loop.run_until_complete(
        engine.tokenizer_manager.continue_generation(ContinueGenerationReqInput())
    )


def serve_api(engine: Any, restored_text: str) -> None:
    class GenerateHandler(BaseHTTPRequestHandler):
        def do_POST(self) -> None:
            if self.path != "/generate":
                self.send_error(404)
                return

            try:
                length = int(self.headers.get("Content-Length", "0"))
                payload = json.loads(self.rfile.read(length))
                prompt = payload["prompt"]
                if not isinstance(prompt, str) or not prompt.strip():
                    raise ValueError("prompt must be a non-empty string")
                text = generate_text(engine, prompt)
            except (json.JSONDecodeError, KeyError, TypeError, ValueError) as error:
                self.send_error(400, str(error))
                return
            except Exception as error:
                self.send_error(500, str(error))
                return

            body = json.dumps(
                {
                    "text": text,
                    **(
                        {"checkpoint_token": benchmark_token} if benchmark_token else {}
                    ),
                }
            ).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

    server = HTTPServer(("0.0.0.0", 8000), GenerateHandler)
    CONTROL_DIR.joinpath("sglang-restore-ready").write_text(
        restored_text + "\n",
        encoding="utf-8",
    )
    print("SGLang API listening on port 8000", flush=True)
    server.serve_forever()


def main() -> None:
    args = parse_args()
    snapshot_mode = args.mode == "snapshot"
    if snapshot_mode:
        configure_capture_environment()
        CONTROL_DIR.joinpath("ready-for-snapshot").unlink(missing_ok=True)

    mark("engine_init_begin")
    engine = create_engine(args.engine_args, snapshot_mode)
    mark("engine_init_end")
    try:
        text = generate_text(engine, "The capital city of France is")
        print(f"SGLang pre-checkpoint output={text!r}", flush=True)
        mark("first_inference_complete")
        if benchmark_token:
            for prompt in [
                "The capital city of France is",
                "The capital city of Germany is",
                "The capital city of Japan is",
            ]:
                answer = generate_text(engine, prompt)
                print(
                    "CR_SEMANTIC_PRECHECK "
                    + json.dumps(dict(prompt=prompt, answer=answer)),
                    flush=True,
                )
                if not answer.strip():
                    raise RuntimeError("Source inference check failed: " + repr(answer))

        if not snapshot_mode:
            return

        with phase("pause_generation"):
            pause_generation(engine)
        try:
            with phase("release_kv_cache"):
                engine.release_memory_occupation(tags=["kv_cache"])
        except BaseException:
            continue_generation(engine)
            raise

        CONTROL_DIR.joinpath("ready-for-snapshot").write_text(
            "ready\n",
            encoding="utf-8",
        )

        while not CONTROL_DIR.joinpath("restore-complete").exists():
            time.sleep(1)

        # The restore connects this process's output to the restore
        # container's log. Also record a failure in the control directory,
        # next to the success sentinel, where tools can read it.
        try:
            progress = CONTROL_DIR.joinpath("sglang-restore-progress")
            with phase("wake"):
                engine.resume_memory_occupation(tags=["kv_cache"])
            progress.write_text("memory-resumed\n", encoding="utf-8")
            with phase("resume_generation"):
                continue_generation(engine)
            progress.write_text("generation-continued\n", encoding="utf-8")

            text = generate_text(engine, "The capital city of Germany is")
            progress.write_text("generated\n", encoding="utf-8")
            print(f"SGLang restored output={text!r}", flush=True)
            serve_api(engine, text)
        except Exception:
            CONTROL_DIR.joinpath("sglang-restore-error").write_text(
                traceback.format_exc(),
                encoding="utf-8",
            )
            raise
    finally:
        engine.shutdown()


if __name__ == "__main__":
    main()
