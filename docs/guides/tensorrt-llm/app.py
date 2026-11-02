# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

from contextlib import nullcontext

# The benchmark wrapper supplies these hooks. Normal guide runs need no helper files.
phase = globals().get("benchmark_phase", lambda _: nullcontext())
mark = globals().get("benchmark_mark", lambda _: None)
benchmark_token = globals().get("benchmark_token")

import gc
import json
import os
import sys
import time
import traceback
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path

# Forked compiler workers can inherit CUDA mappings that CRIU cannot restore.
os.environ["TORCHINDUCTOR_COMPILE_THREADS"] = "1"
from tensorrt_llm import LLM, SamplingParams

CONTROL_DIR = Path(os.environ.get("SNAPSHOT_CONTROL_DIR", "/snapshot-control"))
# Small single-GPU sizing so the example fits alongside other GPU tenants and
# keeps the checkpoint artifact small. Override through the Pod template.
MAX_NUM_TOKENS = int(os.environ.get("TRTLLM_MAX_NUM_TOKENS", "1024"))
MAX_BATCH_SIZE = int(os.environ.get("TRTLLM_MAX_BATCH_SIZE", "1"))
FREE_GPU_MEMORY_FRACTION = float(
    os.environ.get("TRTLLM_FREE_GPU_MEMORY_FRACTION", "0.10")
)
TRUST_REMOTE_CODE = False


def generate_text(llm: LLM, prompts: list[str]) -> list[str]:
    with phase("generation"):
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
    CONTROL_DIR.joinpath("trtllm-restore-ready").write_text(
        restored_text + "\n",
        encoding="utf-8",
    )
    print("TensorRT-LLM API listening on port 8000", flush=True)
    server.serve_forever()


def main() -> None:
    CONTROL_DIR.joinpath("ready-for-snapshot").unlink(missing_ok=True)

    engine_args = {
        "backend": "pytorch",
        "dtype": "auto",
        "trust_remote_code": TRUST_REMOTE_CODE,
        "max_num_tokens": MAX_NUM_TOKENS,
        "max_seq_len": 512,
        "max_batch_size": MAX_BATCH_SIZE,
        "enable_chunked_prefill": False,
        "kv_cache_config": {"free_gpu_memory_fraction": FREE_GPU_MEMORY_FRACTION},
    }
    # JSON keys are LLM API keyword arguments, not CLI flags.
    engine_args.update(json.loads(sys.argv[1]))
    revision = engine_args.pop("revision", None)
    if revision is not None:
        from huggingface_hub import snapshot_download

        # rc24 does not forward revision to every tokenizer/config read.
        engine_args["model"] = snapshot_download(
            repo_id=engine_args["model"], revision=revision, local_files_only=True
        )
    # rc24 supports KV-only sleep through its executor collective RPC.
    # Discard KV contents and keep weights resident, without a CPU backup.
    engine_args["kv_cache_config"]["use_kv_cache_manager_v2"] = False
    engine_args["kv_cache_config"]["enable_block_reuse"] = False
    engine_args["sleep_config"] = {"restore_modes": {"kv_cache": "NONE"}}
    mark("engine_init_begin")
    llm = LLM(**engine_args)
    mark("engine_init_end")

    for text in generate_text(
        llm,
        [
            "Summarize why checkpoint and restore testing matters.",
            "Continue this sequence with four numbers: 1, 2, 3, 4,",
        ],
    ):
        print(f"TensorRT-LLM pre-checkpoint output={text!r}", flush=True)
    mark("first_inference_complete")
    if benchmark_token:
        for prompt in [
            "The capital city of France is",
            "The capital city of Germany is",
            "The capital city of Japan is",
        ]:
            answer = generate_text(llm, [prompt])[0]
            print(
                "CR_SEMANTIC_PRECHECK "
                + json.dumps(dict(prompt=prompt, answer=answer)),
                flush=True,
            )
            if not answer.strip():
                raise RuntimeError("Source inference check failed: " + repr(answer))

    with phase("sleep"):
        llm._collective_rpc("sleep", args=(["kv_cache"],))
    gc.collect()
    CONTROL_DIR.joinpath("ready-for-snapshot").write_text(
        "ready\n",
        encoding="utf-8",
    )

    while True:
        if CONTROL_DIR.joinpath("restore-complete").exists():
            # The restore connects this process's output to the restore
            # container's log. Also record a failure in the control directory,
            # next to the success sentinel, where tools can read it.
            try:
                progress = CONTROL_DIR.joinpath("trtllm-restore-progress")
                with phase("wake"):
                    llm._collective_rpc("wakeup", args=(["kv_cache"],))
                text = generate_text(llm, ["The capital city of Germany is"])[0]
                progress.write_text("generated\n", encoding="utf-8")
                print(f"TensorRT-LLM restored output={text!r}", flush=True)
                serve_api(llm, text)
            except Exception:
                CONTROL_DIR.joinpath("trtllm-restore-error").write_text(
                    traceback.format_exc(),
                    encoding="utf-8",
                )
                raise
        time.sleep(1)


if __name__ == "__main__":
    main()
