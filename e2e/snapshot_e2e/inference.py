# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Inference requests against a framework guide program, from inside its pod.

The guide programs expose `POST /generate` with `{"prompt": ...}` on port 8000
and answer `{"text": ...}`. There is no Service in front of them, so the
request is issued from inside the pod through exec. Python's standard library
is used rather than curl because every framework runtime image ships Python
and none is guaranteed to ship curl.
"""

from __future__ import annotations

import json
import shlex
import unicodedata

from snapshot_e2e import k8s
from snapshot_e2e.frameworks import API_PORT
from snapshot_e2e.frameworks import REQUEST_TIMEOUT_SECONDS

# Short answers fit the guide APIs' existing generation limits.
_CHAT_CHECKS = {
    "capital": ("What is the capital of Germany? Reply with the city name only.", {"berlin"}),
    "arithmetic": ("What is 2 + 2? Reply with the number only.", {"4", "four", "2+2=4"}),
    "translation": ("Translate hello into French. Reply with the translation only.", {"bonjour"}),
}

_CHAT_PROMPTS = """
import json, os, sys
from pathlib import Path
from huggingface_hub import snapshot_download
from transformers import AutoTokenizer

model = os.environ["SNAPSHOT_MODEL"]
revision = os.environ.get("SNAPSHOT_MODEL_REVISION")
path = model if Path(model).is_dir() else snapshot_download(
    repo_id=model, revision=revision, local_files_only=True,
)
tokenizer = AutoTokenizer.from_pretrained(path, local_files_only=True, trust_remote_code=False)
config = json.loads((Path(path) / "config.json").read_text())
prompts = {}
for name, question in json.loads(sys.argv[1]).items():
    messages = [{"role": "user", "content": question}]
    kwargs = {"add_generation_prompt": True, "enable_thinking": False}
    if config.get("model_type") == "glm_moe_dsa":
        # This pinned template ignores enable_thinking. Render an empty assistant
        # turn so the template itself closes reasoning within the short token budget.
        messages.append({"role": "assistant", "content": "", "reasoning_content": ""})
        kwargs["add_generation_prompt"] = False
    prompts[name] = tokenizer.apply_chat_template(messages, tokenize=False, **kwargs)
print("__snapshot_e2e_chat_prompts__" + json.dumps(prompts))
"""


def chat_prompts(namespace: str, pod: str) -> dict[str, str]:
    """Render the source model's cached template before its rootfs is captured."""
    questions = {name: question for name, (question, _) in _CHAT_CHECKS.items()}
    command = (
        f"timeout 60s python3 -c {shlex.quote(_CHAT_PROMPTS)} "
        f"{shlex.quote(json.dumps(questions))}"
    )
    output = k8s.exec_command(namespace, pod, command)
    marker = "__snapshot_e2e_chat_prompts__"
    if marker not in output:
        raise AssertionError(f"could not render source chat prompts: {output}")
    prompts = json.loads(output.rsplit(marker, 1)[1])
    if prompts.keys() != _CHAT_CHECKS.keys() or not all(
        isinstance(prompt, str) and prompt.strip() for prompt in prompts.values()
    ):
        raise AssertionError(f"invalid source chat prompts: {prompts!r}")
    return prompts


def verify_chat_answers(namespace: str, pod: str, prompts: dict[str, str]) -> dict[str, str]:
    """Check whole answers, allowing case, whitespace and surrounding punctuation."""
    answers = {}
    for name, (_, expected) in _CHAT_CHECKS.items():
        answer = request_generate(namespace, pod, prompts[name])
        normalized = unicodedata.normalize("NFKC", answer).casefold().strip(" \t\r\n.!?\"'")
        normalized = "".join(normalized.split())
        if normalized not in expected:
            raise AssertionError(f"{name}: expected {sorted(expected)!r}, got {answer!r}")
        answers[name] = answer
    return answers


_CLIENT = """
import json, sys, urllib.request
payload = json.dumps({"prompt": sys.argv[1]}).encode()
request = urllib.request.Request(
    sys.argv[2],
    data=payload,
    headers={"Content-Type": "application/json"},
    method="POST",
)
with urllib.request.urlopen(request, timeout=float(sys.argv[3])) as response:
    body = response.read().decode()
print("__snapshot_e2e_generate__" + body)
"""

_MARKER = "__snapshot_e2e_generate__"


def request_generate(
    namespace: str,
    pod: str,
    prompt: str,
    *,
    timeout: int = REQUEST_TIMEOUT_SECONDS,
    port: int = API_PORT,
    container: str | None = None,
) -> str:
    """POSTs the prompt to the program's /generate and returns the generated text.

    Raises AssertionError if the endpoint does not answer with non-empty text;
    the raw exec output is included so a framework error is visible verbatim.
    """
    url = f"http://127.0.0.1:{port}/generate"
    command = (
        f"python3 -c {shlex.quote(_CLIENT)} {shlex.quote(prompt)} "
        f"{shlex.quote(url)} {timeout}"
    )
    output = k8s.exec_command(namespace, pod, command, container=container)
    marker_at = output.rfind(_MARKER)
    if marker_at < 0:
        raise AssertionError(
            f"/generate on {namespace}/{pod} produced no response; exec output:\n{output}"
        )
    body = output[marker_at + len(_MARKER):].strip()
    try:
        text = json.loads(body)["text"]
    except (ValueError, KeyError, TypeError) as exc:
        raise AssertionError(
            f"/generate on {namespace}/{pod} returned malformed body {body!r}: {exc}"
        ) from exc
    if not isinstance(text, str) or not text.strip():
        raise AssertionError(f"/generate on {namespace}/{pod} returned empty text: {body!r}")
    return text
