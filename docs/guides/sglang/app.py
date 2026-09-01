# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

import argparse
import json
import os
import time
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from typing import Any

CONTROL_DIR = Path(os.environ.get("SNAPSHOT_CONTROL_DIR", "/snapshot-control"))


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--mode", choices=("prime", "snapshot"), default="snapshot")
    return parser.parse_args()


def configure_capture_environment() -> None:
    os.environ.update(
        {
            "NCCL_CUMEM_ENABLE": "0",
            "NCCL_NVLS_ENABLE": "0",
            "NCCL_IB_DISABLE": "1",
            "NCCL_RAS_ENABLE": "0",
            "TORCH_NCCL_ENABLE_MONITORING": "0",
            "TORCH_NCCL_DUMP_ON_TIMEOUT": "0",
            "HF_HUB_OFFLINE": "1",
        }
    )


def create_engine(snapshot_mode: bool) -> Any:
    import sglang as sgl

    return sgl.Engine(
        model_path=os.environ["SNAPSHOT_MODEL"],
        context_length=int(os.environ.get("SGLANG_CONTEXT_LENGTH", "10240")),
        enable_memory_saver=snapshot_mode,
        enable_weights_cpu_backup=snapshot_mode,
        log_level="info",
    )


def generate_text(engine: Any, prompt: str) -> str:
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

    engine.loop.run_until_complete(
        engine.tokenizer_manager.pause_generation(
            PauseGenerationReqInput(mode="abort")
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

            body = json.dumps({"text": text}).encode()
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
    if os.environ.get("DYN_SNAPSHOT_RESTORE_STANDBY") == "1":
        while True:
            time.sleep(3600)

    args = parse_args()
    snapshot_mode = args.mode == "snapshot"
    if snapshot_mode:
        configure_capture_environment()
        CONTROL_DIR.joinpath("ready-for-snapshot").unlink(missing_ok=True)

    engine = create_engine(snapshot_mode)
    try:
        text = generate_text(engine, "The capital city of France is")
        print(f"SGLang pre-checkpoint output={text!r}", flush=True)

        if not snapshot_mode:
            return

        pause_generation(engine)
        try:
            engine.release_memory_occupation()
        except BaseException:
            continue_generation(engine)
            raise

        CONTROL_DIR.joinpath("ready-for-snapshot").write_text(
            "ready\n",
            encoding="utf-8",
        )

        while not CONTROL_DIR.joinpath("restore-complete").exists():
            time.sleep(1)

        engine.resume_memory_occupation()
        continue_generation(engine)
        os.environ.pop("HF_HUB_OFFLINE", None)

        text = generate_text(engine, "The capital city of Germany is")
        print(f"SGLang restored output={text!r}", flush=True)
        serve_api(engine, text)
    finally:
        engine.shutdown()


if __name__ == "__main__":
    main()
