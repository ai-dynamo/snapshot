# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Exercise CI image builds without Docker or registry access."""

from __future__ import annotations

import json
import os
import shutil
import signal
import subprocess
import time
from pathlib import Path

import pytest
import yaml


ROOT = Path(__file__).resolve().parents[2]
DIGEST = "sha256:" + "1" * 64


def executable(path: Path, content: str) -> None:
    path.write_text("#!/usr/bin/env python3\n" + content)
    path.chmod(0o755)


@pytest.fixture
def build_env(tmp_path):
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    executable(
        bin_dir / "make",
        """import json, os, pathlib, shlex, sys
args = sys.argv[1:]
pathlib.Path(os.environ['MAKE_RECORD']).write_text(json.dumps(args))
if os.environ.get('MAKE_FAIL'):
    sys.exit(7)
flags = next(arg.split('=', 1)[1] for arg in args if arg.startswith('DOCKER_BUILD_ARGS='))
metadata = next(arg.split('=', 1)[1] for arg in shlex.split(flags)
                if arg.startswith('--metadata-file='))
pathlib.Path(metadata).write_text(json.dumps({
    'containerimage.digest': os.environ.get('BUILD_DIGEST', '')}))
""",
    )
    return {
        **os.environ,
        "PATH": str(bin_dir) + os.pathsep + os.environ["PATH"],
        "MAKE_RECORD": str(tmp_path / "make.json"),
        "BUILD_DIGEST": DIGEST,
        "IMAGE": "operator",
        "REGISTRY": "registry.example/snapshot",
        "TAGS": "candidate",
        "PUSH": "true",
        "PLATFORM": "linux/amd64",
        "CACHE_MODE": "read",
        "CACHE_BRANCH": "main",
        "CACHE_REF_PREFIX": "registry.example/cache",
        "CACHE_TAG_SUFFIX": "",
        "CACHE_VARIANT": "",
        "BUILD_ARGS": "",
        "PHASE": "",
        "RUNNER_TEMP": str(tmp_path),
        "GITHUB_OUTPUT": str(tmp_path / "outputs"),
    }


def build(env):
    return subprocess.run(
        ["bash", "hack/ci-build-image.sh"],
        cwd=ROOT,
        env=env,
        capture_output=True,
        text=True,
        timeout=10,
    )


@pytest.mark.parametrize("mode", ["read", "write"])
def test_variant_cache_falls_back_without_writing_the_standard_cache(build_env, mode):
    build_env.update(CACHE_VARIANT="snapshot-e2e", CACHE_MODE=mode)
    result = build(build_env)
    assert result.returncode == 0, result.stderr
    args = json.loads(Path(build_env["MAKE_RECORD"]).read_text())
    flags = next(arg for arg in args if arg.startswith("DOCKER_BUILD_ARGS="))
    prefix = "type=registry,ref=registry.example/cache:operator"
    assert f"--cache-from={prefix}-snapshot-e2e-main" in flags
    assert f"--cache-from={prefix}-main" in flags
    if mode == "write":
        assert f"--cache-to={prefix}-snapshot-e2e-main," in flags
    else:
        assert "--cache-to=" not in flags
    assert f"--cache-to={prefix}-main," not in flags
    assert Path(build_env["GITHUB_OUTPUT"]).read_text() == f"digest={DIGEST}\n"


@pytest.mark.parametrize("push", ["true", "false", "digest"])
def test_push_mode_preserves_the_make_contract_and_cleans_metadata(build_env, push):
    build_env.update(PUSH=push, PLATFORM="linux/arm64")
    result = build(build_env)
    assert result.returncode == 0, result.stderr
    args = json.loads(Path(build_env["MAKE_RECORD"]).read_text())
    assert args[:3] == [
        "docker-build-operator",
        "REGISTRY=registry.example/snapshot",
        "AGENT_PLATFORM=linux/arm64",
    ]
    assert ("TAGS=" if push == "digest" else "TAGS=candidate") in args
    flags = args[-1]
    assert "operator-arm64-main" in flags
    assert ("--push" in flags) == (push == "true")
    assert ("push-by-digest=true" in flags) == (push == "digest")
    assert not list(Path(build_env["RUNNER_TEMP"]).glob("build-image-*.json"))


@pytest.mark.parametrize(
    ("overrides", "expected"),
    [
        ({"MAKE_FAIL": "true"}, 7),
        ({"BUILD_DIGEST": ""}, 1),
        ({"PUSH": "invalid"}, 2),
        ({"CACHE_MODE": "invalid"}, 2),
        ({"CACHE_VARIANT": "bad variant"}, 2),
    ],
)
def test_failed_builds_do_not_report_a_digest(build_env, overrides, expected):
    build_env.update(overrides)
    result = build(build_env)
    assert result.returncode == expected
    assert not Path(build_env["GITHUB_OUTPUT"]).exists()


@pytest.fixture
def cpu_workers(tmp_path):
    hack = tmp_path / "hack"
    hack.mkdir()
    worker = """import json, os, pathlib, sys, time
name = os.environ.get('IMAGE', 'cluster')
root = pathlib.Path(os.environ['WORKERS_DIR'])
(root / (name + '.config.json')).write_text(json.dumps({
    key: os.environ.get(key) for key in ('CACHE_VARIANT', 'BUILD_ARGS', 'CACHE_MODE')
}))
(root / (name + '.started')).touch()
deadline = time.monotonic() + 5
while len(list(root.glob('*.started'))) != 4:
    if time.monotonic() > deadline:
        sys.exit(9)
    time.sleep(.01)
if os.environ.get('BLOCK_WORKERS'):
    subprocess = __import__('subprocess')
    child = subprocess.Popen(['sleep', '30'])
    (root / (name + '.pid')).write_text(str(child.pid))
    child.wait()
else:
    time.sleep(.05)
    (root / (name + '.finished')).touch()
    sys.exit(7 if name == os.environ.get('FAIL_WORKER') else 0)
"""
    # The workflow invokes bash scripts; exec keeps the worker in its group.
    executable(hack / "worker.py", worker)
    for name in ("ci-build-image.sh", "k3d-cpu-e2e.sh"):
        (hack / name).write_text('exec python3 hack/worker.py\n')
    workflow = yaml.safe_load((ROOT / ".github/workflows/e2e-cpu.yaml").read_text())
    step = next(
        s for s in workflow["jobs"]["cpu-e2e"]["steps"]
        if s.get("name") == "Build images and create k3d cluster"
    )
    (hack / "ci-prepare-cpu-e2e.sh").write_text(
        (ROOT / "hack/ci-prepare-cpu-e2e.sh").read_text()
    )
    script = tmp_path / "build-cpu.sh"
    script.write_text(step["run"])
    env = {
        **os.environ,
        "CACHE_MODE": step["env"]["CACHE_MODE"],
        "WORKERS_DIR": str(tmp_path),
        "RUNNER_TEMP": str(tmp_path),
    }
    return tmp_path, script, env


@pytest.mark.skipif(shutil.which("setsid") is None, reason="CPU workflow requires Linux setsid")
@pytest.mark.parametrize("failure", ["", "agent", "operator", "pagebroker", "cluster"])
def test_cpu_builds_overlap_and_wait_for_every_worker(cpu_workers, failure):
    directory, script, env = cpu_workers
    result = subprocess.run(
        ["bash", str(script)], cwd=directory, env={**env, "FAIL_WORKER": failure},
        capture_output=True, text=True, timeout=10,
    )
    assert result.returncode == (1 if failure else 0), result.stderr
    assert len(list(directory.glob("*.finished"))) == 4
    operator = json.loads((directory / "operator.config.json").read_text())
    workflow = yaml.safe_load((ROOT / ".github/workflows/push-artifacts.yaml").read_text())
    producer = next(
        s for s in workflow["jobs"]["build-image"]["steps"]
        if s.get("name") == "Warm CPU operator cache"
    )
    assert operator["CACHE_VARIANT"] == producer["with"]["cache_variant"]
    assert operator["BUILD_ARGS"] == producer["with"]["build_args"]
    assert operator["CACHE_MODE"] == "read"
    assert producer["with"]["cache_mode"] == "write"


@pytest.mark.skipif(shutil.which("setsid") is None, reason="CPU workflow requires Linux setsid")
def test_cancelling_cpu_builds_terminates_their_child_processes(cpu_workers):
    directory, script, env = cpu_workers
    process = subprocess.Popen(
        ["bash", str(script)], cwd=directory, env={**env, "BLOCK_WORKERS": "true"},
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
    )
    child_pids = []
    try:
        deadline = time.monotonic() + 5
        while len(list(directory.glob("*.pid"))) != 4:
            assert process.poll() is None
            assert time.monotonic() < deadline
            time.sleep(.01)
        child_pids = [int(p.read_text()) for p in directory.glob("*.pid")]
        process.send_signal(signal.SIGTERM)
        assert process.wait(timeout=5) == 143
        for pid in child_pids:
            # A terminated orphan may remain a zombie until PID 1 reaps it.
            status = Path(f"/proc/{pid}/stat")
            assert not status.exists() or status.read_text().split()[2] == "Z"
    finally:
        if process.poll() is None:
            process.kill()
            process.wait()
        for pid in child_pids:
            try:
                os.kill(pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
