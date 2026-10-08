# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Run the restore assertions locally with the same unchecked exec output contract."""

from __future__ import annotations

import shlex
import subprocess
from pathlib import Path

import pytest

from snapshot_e2e import lifecycle


@pytest.mark.parametrize(
    "failure",
    [None, "initial-token", "file-token", "cpu-token", "gpu-token", "too-few-observations", "no-progress"],
)
def test_restored_state_requires_every_remote_assertion_to_pass(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path, failure: str | None
) -> None:
    files = {}
    for name in ("RESTORE_DONE", "RESTORE_INITIAL_TOKEN", "FILE_TOKEN", "OBSERVATIONS"):
        path = tmp_path / name.lower()
        monkeypatch.setattr(lifecycle, name, str(path))
        files[name] = path

    files["RESTORE_DONE"].touch()
    files["RESTORE_INITIAL_TOKEN"].write_text("wrong" if failure == "initial-token" else "restore-state")
    files["FILE_TOKEN"].write_text("wrong" if failure == "file-token" else "source-state")
    cpu = "wrong" if failure == "cpu-token" else "source-state"
    gpu = "wrong" if failure == "gpu-token" else "source-state"
    observation = f"observation cpu={cpu} file=source-state gpu={gpu}\n"
    files["OBSERVATIONS"].write_text(observation)

    def exec_command(namespace: str, pod: str, command: str, *, container: str | None = None) -> str:
        assert (namespace, pod, container) == ("test-ns", "restored-pod", "main")
        # Advance the observation file during the script's sleep without a
        # timed background process. All assertions and their order stay intact.
        progress = (
            ":"
            if failure == "no-progress"
            else f"printf '%s' {shlex.quote(observation)} >> {shlex.quote(str(files['OBSERVATIONS']))}"
        )
        result = subprocess.run(
            ["bash", "-c", f"sleep() {{ {progress}; }}\n{command}"],
            capture_output=True,
            text=True,
            timeout=5,
        )
        assert (result.returncode != 0) is (failure is not None)
        # Match Kubernetes' preloaded stream: nonzero exit does not raise.
        return result.stdout + result.stderr

    monkeypatch.setattr(lifecycle.k8s, "exec_command", exec_command)
    kwargs = dict(
        source_token="source-state",
        restore_token="restore-state",
        checkpoint_observations=2 if failure == "too-few-observations" else 1,
        gpu=True,
        container="main",
    )
    if failure:
        with pytest.raises(AssertionError, match="restored state check failed for test-ns/restored-pod"):
            lifecycle.assert_restored_state("test-ns", "restored-pod", **kwargs)
    else:
        output = lifecycle.assert_restored_state("test-ns", "restored-pod", **kwargs)
        assert "source_token=source-state restore_token=restore-state" in output
