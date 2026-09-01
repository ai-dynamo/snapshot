# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

import gc
import json
import os
import time
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path

from tensorrt_llm import LLM, SamplingParams

CONTROL_DIR = Path(os.environ.get("SNAPSHOT_CONTROL_DIR", "/snapshot-control"))
MODEL = os.environ["SNAPSHOT_MODEL"]


def generate_text(llm: LLM, prompts: list[str]) -> list[str]:
    outputs = llm.generate(
        prompts,
        SamplingParams(temperature=0.0, max_tokens=16),
        use_tqdm=False,
    )
    texts = []
    for output in outputs:
        if not output.outputs:
            raise RuntimeError("TensorRT-LLM produced no output")
        text = output.outputs[0].text.strip()
        if not text:
            raise RuntimeError("TensorRT-LLM produced empty output")
        texts.append(text)
    return texts


def serve_api(llm: LLM, restored_text: str) -> None:
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
                text = generate_text(llm, [prompt])[0]
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
    CONTROL_DIR.joinpath("trtllm-restore-ready").write_text(
        restored_text + "\n",
        encoding="utf-8",
    )
    print("TensorRT-LLM API listening on port 8000", flush=True)
    server.serve_forever()


def main() -> None:
    if os.environ.get("DYN_SNAPSHOT_RESTORE_STANDBY") == "1":
        while True:
            time.sleep(3600)

    CONTROL_DIR.joinpath("ready-for-snapshot").unlink(missing_ok=True)

    llm = LLM(
        model=MODEL,
        backend="pytorch",
        dtype="float16",
        trust_remote_code=True,
        tensor_parallel_size=1,
        max_num_tokens=1024,
        max_seq_len=512,
        max_batch_size=1,
        enable_chunked_prefill=False,
        kv_cache_config={"free_gpu_memory_fraction": 0.10},
    )

    for text in generate_text(
        llm,
        [
            "Summarize why checkpoint and restore testing matters.",
            "Continue this sequence with four numbers: 1, 2, 3, 4,",
        ],
    ):
        print(f"TensorRT-LLM pre-checkpoint output={text!r}", flush=True)

    gc.collect()
    CONTROL_DIR.joinpath("ready-for-snapshot").write_text(
        "ready\n",
        encoding="utf-8",
    )

    while True:
        if CONTROL_DIR.joinpath("restore-complete").exists():
            text = generate_text(llm, ["Reply with one word: restored"])[0]
            print(f"TensorRT-LLM restored output={text!r}", flush=True)
            serve_api(llm, text)
        time.sleep(1)


if __name__ == "__main__":
    main()
