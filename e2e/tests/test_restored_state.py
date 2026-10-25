# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Run the restore assertions locally with the same unchecked exec output contract."""

from __future__ import annotations

import shlex
import subprocess
from pathlib import Path
from types import SimpleNamespace

import pytest

from snapshot_e2e import lifecycle


@pytest.mark.parametrize(
    "failure",
    [
        None,
        "pid-1-not-proxy",
        "restored-pid-not-workload",
        "file-token",
        "cpu-token",
        "gpu-token",
        "too-few-observations",
        "no-progress",
    ],
)
def test_restored_state_requires_every_remote_assertion_to_pass(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path, failure: str | None
) -> None:
    files = {}
    for name in ("RESTORE_DONE", "FILE_TOKEN", "OBSERVATIONS"):
        path = tmp_path / name.lower()
        monkeypatch.setattr(lifecycle, name, str(path))
        files[name] = path

    # The proxy is PID 1, and restore-complete names the restored CUDA program.
    proc = tmp_path / "proc"
    monkeypatch.setattr(lifecycle, "PROC_ROOT", str(proc))
    (proc / "1").mkdir(parents=True)
    pid_1 = "sleep\0infinity\0" if failure == "pid-1-not-proxy" else f"{lifecycle.RESTORE_PROXY}\0"
    (proc / "1" / "cmdline").write_text(pid_1)
    (proc / "42").mkdir()
    restored = "/bin/sh\0" if failure == "restored-pid-not-workload" else f"{lifecycle.CUDA_HOLD}\0"
    (proc / "42" / "cmdline").write_text(restored)
    files["RESTORE_DONE"].write_text("pid=42\n")
    files["FILE_TOKEN"].write_text("wrong" if failure == "file-token" else "source-state")
    cpu = "wrong" if failure == "cpu-token" else "source-state"
    gpu = "wrong" if failure == "gpu-token" else "source-state"
    observation = f"observation cpu={cpu} file=source-state gpu={gpu}\n"
    files["OBSERVATIONS"].write_text(observation)

    expect_failure = failure is not None

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
        assert (result.returncode != 0) is expect_failure
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
    if failure in ("pid-1-not-proxy", "restored-pid-not-workload"):
        # Agents from before the proxy: the same container passes without
        # the proxy checks.
        expect_failure = False
        lifecycle.assert_restored_state("test-ns", "restored-pod", proxy=False, **kwargs)
        expect_failure = True
    if failure:
        with pytest.raises(AssertionError, match="restored state check failed for test-ns/restored-pod"):
            lifecycle.assert_restored_state("test-ns", "restored-pod", **kwargs)
    else:
        output = lifecycle.assert_restored_state("test-ns", "restored-pod", **kwargs)
        assert "source_token=source-state restore_token=restore-state" in output


@pytest.mark.parametrize(
    "failure",
    [
        None,
        "securityfs",
        "no-mountinfo",
        "empty-dir-not-mounted",
        "empty-dir-only-as-root",
        "empty-dir-missing",
        "empty-dir-not-empty",
    ],
)
def test_restored_mounts_require_every_remote_assertion_to_pass(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path, failure: str | None
) -> None:
    restore_done = tmp_path / "restore_done"
    monkeypatch.setattr(lifecycle, "RESTORE_DONE", str(restore_done))
    restore_done.write_text("pid=42\n")
    proc = tmp_path / "proc"
    monkeypatch.setattr(lifecycle, "PROC_ROOT", str(proc))
    (proc / "42" / "root").mkdir(parents=True)
    if failure != "empty-dir-missing":
        (proc / "42" / "root" / "own").mkdir()
    if failure == "empty-dir-not-empty":
        (proc / "42" / "root" / "own" / "apparmor").mkdir()
    # Field 4 is the mount's root in its filesystem, field 5 its mount point.
    mounts = ["36 25 0:30 / / rw - overlay overlay rw"]
    if failure == "empty-dir-only-as-root":
        mounts.append("40 36 8:1 /own /other rw - ext4 /dev/sda1 rw")
    elif failure != "empty-dir-not-mounted":
        mounts.append("40 36 8:1 /data /own rw - ext4 /dev/sda1 rw")
    if failure == "securityfs":
        mounts.append("41 36 0:7 / /sys/kernel/security ro - securityfs securityfs ro")
    if failure != "no-mountinfo":
        (proc / "42" / "mountinfo").write_text("\n".join(mounts) + "\n")
    # A missing mountinfo must fail even without the empty_dir checks.
    empty_dir = None if failure in (None, "securityfs", "no-mountinfo") else "/own"

    def exec_command(namespace: str, pod: str, command: str, *, container: str | None = None) -> str:
        assert (namespace, pod, container) == ("test-ns", "restored-pod", "main")
        result = subprocess.run(["bash", "-c", command], capture_output=True, text=True, timeout=5)
        assert (result.returncode != 0) is (failure is not None)
        # Match Kubernetes' preloaded stream: nonzero exit does not raise.
        return result.stdout + result.stderr

    monkeypatch.setattr(lifecycle.k8s, "exec_command", exec_command)
    if failure:
        with pytest.raises(AssertionError, match="restored mount check failed for test-ns/restored-pod"):
            lifecycle.assert_restored_workload_mounts(
                "test-ns", "restored-pod", container="main", empty_dir=empty_dir
            )
    else:
        for empty_dir in (None, "/own"):
            lifecycle.assert_restored_workload_mounts(
                "test-ns", "restored-pod", container="main", empty_dir=empty_dir
            )


def pod_with(exit_code: int | None, phase: str = "Running") -> SimpleNamespace:
    state = SimpleNamespace(
        terminated=SimpleNamespace(exit_code=exit_code) if exit_code is not None else None
    )
    status = SimpleNamespace(name=lifecycle.CONTAINER, state=state)
    return SimpleNamespace(
        metadata=SimpleNamespace(resource_version="7"),
        status=SimpleNamespace(phase=phase, container_statuses=[status]),
    )


@pytest.mark.parametrize(
    ("events", "want"),
    [
        ([("MODIFIED", pod_with(None)), ("MODIFIED", pod_with(42, "Failed"))], 42),
        ([("MODIFIED", pod_with(None)), ("DELETED", pod_with(137, "Failed"))], 137),
        ([("MODIFIED", pod_with(None)), ("DELETED", pod_with(None))], None),
        ([], None),
    ],
)
def test_delete_pod_and_wait_for_exit_reads_the_final_status(
    monkeypatch: pytest.MonkeyPatch,
    events: list[tuple[str, SimpleNamespace]],
    want: int | None,
) -> None:
    calls = []
    monkeypatch.setattr(lifecycle.k8s, "read_pod", lambda namespace, name: pod_with(None))
    monkeypatch.setattr(
        lifecycle.k8s, "delete_pod", lambda namespace, name: calls.append(("delete", name))
    )

    def watch_pod(namespace, name, resource_version, timeout):
        # The watch starts from the Pod as it was before the delete.
        calls.append(("watch", resource_version))
        yield from ({"type": kind, "object": pod} for kind, pod in events)

    monkeypatch.setattr(lifecycle.k8s, "watch_pod", watch_pod)

    if want is None:
        with pytest.raises(AssertionError, match="without an exit code"):
            lifecycle.delete_pod_and_wait_for_exit("ns", "restore")
    else:
        assert lifecycle.delete_pod_and_wait_for_exit("ns", "restore") == want
    assert calls == [("delete", "restore"), ("watch", "7")]
