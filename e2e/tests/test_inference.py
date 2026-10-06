# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Semantic checks reject coherent prefixes followed by corrupt model output."""

import json
import sys
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import Mock

import pytest

from snapshot_e2e import inference


@pytest.mark.parametrize("arithmetic", ["4", " ４.\n", "Four", "2 + 2 = 4"])
def test_semantic_answers_allow_formatting(monkeypatch: pytest.MonkeyPatch, arithmetic: str) -> None:
    answers = {"capital": '"Berlin."', "arithmetic": arithmetic, "translation": "Bonjour!"}
    request = Mock(side_effect=lambda namespace, pod, prompt: answers[prompt])
    monkeypatch.setattr(inference, "request_generate", request)
    assert inference.verify_chat_answers("ns", "pod", {key: key for key in answers}) == answers
    assert [call.args[2] for call in request.call_args_list] == list(answers)


@pytest.mark.parametrize("name,answer", [
    ("capital", "Berlin own’/ 4,,Vietism T-fire/ --in"),
    ("capital", "Not Berlin"),
    ("translation", "BonjourCar PR , st/ the/countists- landS national"),
    ("arithmetic", "2A: ( Awardability sit hacker\\n =ee prism (  (、"),
    ("arithmetic", "14"),
    ("arithmetic", "-4"),
    ("arithmetic", "2 + 2 = 5"),
    ("arithmetic", "4, but maybe 5"),
    ("arithmetic", "<think>2 + 2 is"),
])
def test_semantic_answers_reject_wrong_or_corrupt_output(
    monkeypatch: pytest.MonkeyPatch, name: str, answer: str,
) -> None:
    answers = {"capital": "Berlin", "arithmetic": "4", "translation": "Bonjour"}
    answers[name] = answer
    monkeypatch.setattr(inference, "request_generate", lambda namespace, pod, prompt: answers[prompt])
    with pytest.raises(AssertionError, match=f"{name}: expected"):
        inference.verify_chat_answers("ns", "pod", {key: key for key in answers})


@pytest.mark.parametrize("model_type", ["qwen3", "deepseek_v41", "glm_moe_dsa"])
@pytest.mark.parametrize("revision", [None, "a" * 40])
def test_prompt_script_uses_offline_source_cache_and_template(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str],
    model_type: str, revision: str | None,
) -> None:
    (tmp_path / "config.json").write_text(json.dumps({"model_type": model_type}))
    download = Mock(return_value=str(tmp_path))
    render = Mock(return_value="model-rendered prompt")
    load = Mock(return_value=SimpleNamespace(apply_chat_template=render))
    monkeypatch.setitem(sys.modules, "huggingface_hub", SimpleNamespace(snapshot_download=download))
    monkeypatch.setitem(sys.modules, "transformers", SimpleNamespace(AutoTokenizer=SimpleNamespace(from_pretrained=load)))
    monkeypatch.setenv("SNAPSHOT_MODEL", "example/model")
    if revision:
        monkeypatch.setenv("SNAPSHOT_MODEL_REVISION", revision)
    else:
        monkeypatch.delenv("SNAPSHOT_MODEL_REVISION", raising=False)
    monkeypatch.setattr(sys, "argv", ["prompt-script", json.dumps({"arithmetic": "What is 2 + 2?"})])

    exec(compile(inference._CHAT_PROMPTS, "chat-prompts", "exec"), {})

    download.assert_called_once_with(repo_id="example/model", revision=revision, local_files_only=True)
    load.assert_called_once_with(str(tmp_path), local_files_only=True, trust_remote_code=False)
    messages = [{"role": "user", "content": "What is 2 + 2?"}]
    if model_type == "glm_moe_dsa":
        messages.append({"role": "assistant", "content": "", "reasoning_content": ""})
    render.assert_called_once_with(
        messages, tokenize=False, enable_thinking=False,
        add_generation_prompt=model_type != "glm_moe_dsa",
    )
    assert json.loads(capsys.readouterr().out.split("__snapshot_e2e_chat_prompts__")[1]) == {
        "arithmetic": "model-rendered prompt",
    }


def test_prompt_collection_rejects_failed_exec(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(inference.k8s, "exec_command", lambda *args: "tokenizer cache missing")
    with pytest.raises(AssertionError, match="tokenizer cache missing"):
        inference.chat_prompts("ns", "pod")
