# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import pytest

from snapshot_e2e import k8s


class FakeSession:
    def __init__(self, *, output: str, returncode: object, exits: bool = True) -> None:
        self.output = output
        self._returncode = returncode
        self.exits = exits
        self.closed = False

    def run_forever(self, timeout=None) -> None:
        self.timeout = timeout

    def is_open(self) -> bool:
        return not self.exits

    @property
    def returncode(self) -> int:
        if isinstance(self._returncode, Exception):
            raise self._returncode
        return self._returncode

    def read_all(self) -> str:
        return self.output

    def close(self) -> None:
        self.closed = True


def run(monkeypatch: pytest.MonkeyPatch, session: FakeSession) -> str:
    monkeypatch.setattr(k8s.client, "CoreV1Api", lambda: type("Api", (), {
        "connect_get_namespaced_pod_exec": None,
    })())
    monkeypatch.setattr(k8s, "stream", lambda *args, **kwargs: session)
    return k8s.exec_argv("ns", "pod", ["/bin/tool", "argument"], timeout=5)


def test_exec_argv_returns_the_output_of_a_finished_program(monkeypatch) -> None:
    session = FakeSession(output="done\n", returncode=0)
    assert run(monkeypatch, session) == "done\n"
    assert session.timeout == 5
    assert session.closed


def test_exec_argv_raises_on_a_failed_program(monkeypatch) -> None:
    session = FakeSession(output="directory not found", returncode=3)
    with pytest.raises(k8s.ExecFailed) as failure:
        run(monkeypatch, session)
    assert failure.value.returncode == 3
    assert failure.value.output == "directory not found"
    assert session.closed


def test_exec_argv_raises_when_the_program_outlasts_the_deadline(monkeypatch) -> None:
    session = FakeSession(output="partial", returncode=0, exits=False)
    with pytest.raises(TimeoutError):
        run(monkeypatch, session)
    assert session.closed


def test_exec_argv_fails_with_minus_one_when_the_exit_status_is_missing(monkeypatch) -> None:
    session = FakeSession(output="cut off", returncode=TypeError("no status"))
    with pytest.raises(k8s.ExecFailed) as failure:
        run(monkeypatch, session)
    assert failure.value.returncode == -1
    assert failure.value.output == "cut off"
    assert session.closed
