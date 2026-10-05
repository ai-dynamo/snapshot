# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Run the same library-path probe used before capture and after real restore."""

import hashlib
import json
import subprocess
import sys
from pathlib import Path

import pytest

from snapshot_e2e import framework_workloads as fw


def guide_process(proc: Path, pid: int = 42, *, unbuffered: bool = False) -> Path:
    process = proc / str(pid)
    libraries = process / "root/tmp/snapshot-cuda"
    libraries.mkdir(parents=True)
    args = [b"/usr/bin/python3"]
    if unbuffered:
        args.append(b"-u")
    (process / "cmdline").write_bytes(b"\0".join([*args, b"/snapshot-app/app.py", b""]))
    (libraries / "libcuinterpose.so").write_bytes(b"frontend")
    (libraries / "libcuinterpose_core.so").write_bytes(b"core")
    return libraries


def probe(proc: Path) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [sys.executable, "-c", fw.CUINTERPOSE_LIBRARY_PROBE, str(proc)],
        capture_output=True, text=True, timeout=10,
    )


@pytest.mark.parametrize("unbuffered", [False, True])
def test_library_probe_reads_guide_root(tmp_path: Path, unbuffered: bool) -> None:
    guide_process(tmp_path, unbuffered=unbuffered)
    placeholder = tmp_path / "1"
    placeholder.mkdir()
    (placeholder / "cmdline").write_bytes(b"sleep\0infinity\0")
    result = probe(tmp_path)
    assert result.returncode == 0, result.stderr
    assert json.loads(result.stdout) == {
        "libcuinterpose.so": hashlib.sha256(b"frontend").hexdigest(),
        "libcuinterpose_core.so": hashlib.sha256(b"core").hexdigest(),
    }


@pytest.mark.parametrize("missing", ["libcuinterpose.so", "libcuinterpose_core.so"])
def test_library_probe_rejects_missing_file(tmp_path: Path, missing: str) -> None:
    libraries = guide_process(tmp_path)
    (libraries / missing).unlink()
    result = probe(tmp_path)
    assert result.returncode != 0
    assert missing in result.stderr


def test_library_probe_requires_live_guide(tmp_path: Path) -> None:
    result = probe(tmp_path)
    assert result.returncode != 0
    assert "no running /snapshot-app/app.py" in result.stderr


def test_library_probe_detects_changed_restored_core(tmp_path: Path) -> None:
    libraries = guide_process(tmp_path)
    source = probe(tmp_path)
    (libraries / "libcuinterpose_core.so").write_bytes(b"different core")
    restored = probe(tmp_path)
    assert source.returncode == restored.returncode == 0
    assert json.loads(source.stdout) != json.loads(restored.stdout)


def test_library_probe_requires_consistent_guide_roots(tmp_path: Path) -> None:
    guide_process(tmp_path)
    other = guide_process(tmp_path, pid=43)
    (other / "libcuinterpose_core.so").write_bytes(b"different core")
    result = probe(tmp_path)
    assert result.returncode != 0
    assert "guide processes have different libraries" in result.stderr
