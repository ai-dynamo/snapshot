# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""A correct prefix must not hide corrupt generated output."""

import pytest

from snapshot_e2e import inference


def test_semantic_answers_accept_valid_output(monkeypatch: pytest.MonkeyPatch) -> None:
    answers = {"capital": "Berlin.", "arithmetic": "2 + 2 = 4", "translation": "Bonjour!"}
    monkeypatch.setattr(inference, "request_generate", lambda namespace, pod, prompt: answers[prompt])
    assert inference.verify_chat_answers("ns", "pod", {key: key for key in answers}) == answers


@pytest.mark.parametrize("answer", ["Not Berlin", "Berlin own’/ 4,,Vietism T-fire/ --in"])
def test_semantic_answers_reject_incorrect_or_corrupt_output(
    monkeypatch: pytest.MonkeyPatch, answer: str,
) -> None:
    answers = {"capital": answer, "arithmetic": "4", "translation": "Bonjour"}
    monkeypatch.setattr(inference, "request_generate", lambda namespace, pod, prompt: answers[prompt])
    with pytest.raises(AssertionError, match="capital: expected"):
        inference.verify_chat_answers("ns", "pod", {key: key for key in answers})
