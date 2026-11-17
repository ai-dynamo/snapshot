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
def test_builds_only_write_the_branch_cache_when_requested(build_env, mode):
    build_env.update(CACHE_MODE=mode)
    result = build(build_env)
    assert result.returncode == 0, result.stderr
    args = json.loads(Path(build_env["MAKE_RECORD"]).read_text())
    flags = next(arg for arg in args if arg.startswith("DOCKER_BUILD_ARGS="))
    prefix = "type=registry,ref=registry.example/cache:operator"
    assert f"--cache-from={prefix}-main" in flags
    if mode == "write":
        assert f"--cache-to={prefix}-main," in flags
    else:
        assert "--cache-to=" not in flags
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
    worker = """import json, os, pathlib, signal, subprocess, sys, time
name = os.environ.get('IMAGE', 'cluster')
print(name + ' worker output', flush=True)
if os.environ.get('IGNORE_TERM'):
    signal.signal(signal.SIGTERM, signal.SIG_IGN)
root = pathlib.Path(os.environ['WORKERS_DIR'])
(root / (name + '.config.json')).write_text(json.dumps({
    key: os.environ.get(key) for key in ('BUILD_ARGS', 'CACHE_MODE', 'CI_PHASE_LOG', 'PHASE')
}))
(root / (name + '.started')).touch()
deadline = time.monotonic() + 5
while len(list(root.glob('*.started'))) != 4:
    if time.monotonic() > deadline:
        sys.exit(9)
    time.sleep(.01)
if os.environ.get('BLOCK_WORKERS'):
    child = subprocess.Popen(['sleep', '30'])
    (root / (name + '.pid')).write_text(str(child.pid))
    if name == os.environ.get('FAIL_WORKER'):
        while len(list(root.glob('*.pid'))) != 4:
            if time.monotonic() > deadline:
                sys.exit(9)
            time.sleep(.01)
        print(name + ' failed while other workers were blocked', flush=True)
        sys.exit(int(os.environ.get('FAIL_STATUS', '7')))
    child.wait()
else:
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
    shutil.copy2(ROOT / "hack/ci-phase.sh", hack / "ci-phase.sh")
    script = tmp_path / "build-cpu.sh"
    script.write_text(step["run"])
    env = {
        **os.environ,
        "CACHE_MODE": step["env"]["CACHE_MODE"],
        "CI_PHASE_LOG": str(tmp_path / "phases.tsv"),
        "WORKERS_DIR": str(tmp_path),
        "RUNNER_TEMP": str(tmp_path),
    }
    yield tmp_path, script, env
    for path in tmp_path.glob("*.pid"):
        try:
            os.kill(int(path.read_text()), signal.SIGKILL)
        except ProcessLookupError:
            pass


@pytest.mark.skipif(shutil.which("setsid") is None, reason="CPU workflow requires Linux setsid")
def test_cpu_builds_overlap_and_wait_for_every_worker(cpu_workers):
    directory, script, env = cpu_workers
    result = subprocess.run(
        ["bash", str(script)], cwd=directory, env=env,
        capture_output=True, text=True, timeout=10,
    )
    assert result.returncode == 0, result.stderr
    assert len(list(directory.glob("*.finished"))) == 4
    operator = json.loads((directory / "operator.config.json").read_text())
    assert operator == {
        "BUILD_ARGS": "--build-arg GO_BUILD_TAGS=snapshot_e2e",
        "CACHE_MODE": "read",
        "CI_PHASE_LOG": env["CI_PHASE_LOG"] + ".builds",
        "PHASE": "Build operator image",
    }
    pagebroker = json.loads((directory / "pagebroker.config.json").read_text())
    assert pagebroker["PHASE"] == "Build PageBroker image"
    assert_prep_diagnostics(directory, result.stdout)


def assert_prep_diagnostics(directory, output):
    for worker in ("operator", "agent", "pagebroker", "cluster"):
        assert worker + " worker output" in output
    phases = (directory / "phases.tsv").read_text().splitlines()
    assert len(phases) == 1
    assert phases[0].split("\t")[0] == "Build images and create k3d cluster"


def assert_workers_stopped(directory):
    for path in directory.glob("*.pid"):
        # A terminated orphan may remain a zombie until PID 1 reaps it.
        status = Path(f"/proc/{int(path.read_text())}/stat")
        assert not status.exists() or status.read_text().split()[2] == "Z"


@pytest.mark.skipif(shutil.which("setsid") is None, reason="CPU workflow requires Linux setsid")
@pytest.mark.parametrize("failure", ["", "cluster"])
def test_cpu_collects_workers_that_finished_before_wait_started(cpu_workers, failure):
    directory, script, env = cpu_workers
    prepare = directory / "hack/ci-prepare-cpu-e2e.sh"
    source = prepare.read_text()
    # Hold the test copy before wait_for_workers until Bash has reaped the
    # fast workers. wait -n alone loses these already-completed jobs.
    call = "\nwait_for_workers\n"
    assert source.count(call) == 1
    prepare.write_text(source.replace(call, """
while true; do
  finished=("${RUNNER_TEMP}"/*.finished)
  ((${#finished[@]} == 4)) && break
  sleep .01
done
sleep .05
wait_for_workers
"""))
    result = subprocess.run(
        ["bash", str(script)], cwd=directory, env={**env, "FAIL_WORKER": failure},
        capture_output=True, text=True, timeout=5,
    )
    assert result.returncode == (7 if failure else 0), result.stderr
    if failure:
        assert "CPU worker cluster-up exited with status 7" in result.stderr
    assert_prep_diagnostics(directory, result.stdout)


@pytest.mark.skipif(shutil.which("setsid") is None, reason="CPU workflow requires Linux setsid")
@pytest.mark.parametrize("worker, status", [("operator", 7), ("agent", 17), ("pagebroker", 23), ("cluster", 31)])
def test_cpu_worker_failure_stops_blocked_peers_and_reports_diagnostics(cpu_workers, worker, status):
    directory, script, env = cpu_workers
    result = subprocess.run(
        ["bash", str(script)], cwd=directory,
        env={**env, "BLOCK_WORKERS": "true", "FAIL_WORKER": worker,
             "FAIL_STATUS": str(status), "IGNORE_TERM": "true"},
        capture_output=True, text=True, timeout=5,
    )
    assert result.returncode == status, result.stderr
    name = "cluster-up" if worker == "cluster" else f"build-{worker}"
    assert f"CPU worker {name} exited with status {status}" in result.stderr
    assert f"{worker} failed while other workers were blocked" in result.stdout
    assert not list(directory.glob("*.finished"))
    assert_prep_diagnostics(directory, result.stdout)
    assert_workers_stopped(directory)


@pytest.mark.skipif(shutil.which("setsid") is None, reason="CPU workflow requires Linux setsid")
@pytest.mark.parametrize("sent, expected", [(signal.SIGINT, 130), (signal.SIGTERM, 143)])
def test_cancelling_cpu_builds_terminates_their_child_processes(cpu_workers, sent, expected):
    directory, script, env = cpu_workers
    process = subprocess.Popen(
        ["bash", str(script)], cwd=directory,
        env={**env, "BLOCK_WORKERS": "true", "IGNORE_TERM": "true"},
        stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
    )
    try:
        deadline = time.monotonic() + 5
        while len(list(directory.glob("*.pid"))) != 4:
            assert process.poll() is None
            assert time.monotonic() < deadline
            time.sleep(.01)
        process.send_signal(sent)
        output, errors = process.communicate(timeout=5)
        assert process.returncode == expected, errors
        assert_prep_diagnostics(directory, output)
        assert_workers_stopped(directory)
    finally:
        if process.poll() is None:
            process.kill()
            process.wait()


@pytest.mark.skipif(shutil.which("setsid") is None, reason="CPU workflow requires Linux setsid")
@pytest.mark.parametrize("sent, expected", [(signal.SIGINT, 130), (signal.SIGTERM, 143)])
def test_cancellation_before_setsid_still_stops_launchers_and_children(cpu_workers, sent, expected):
    directory, script, env = cpu_workers
    bin_dir = directory / "bin"
    bin_dir.mkdir()
    executable(bin_dir / "setsid", """import os, pathlib, signal, sys, time
def form_session(*_):
    time.sleep(.1)
    os.execv(os.environ['REAL_SETSID'], ['setsid', *sys.argv[1:]])
signal.signal(signal.SIGTERM, form_session)
print('launcher waiting for session', flush=True)
pathlib.Path(os.environ['RUNNER_TEMP'], str(os.getpid()) + '.launcher').touch()
while True:
    signal.pause()
""")
    process = subprocess.Popen(
        ["bash", str(script)], cwd=directory,
        env={**env, "PATH": str(bin_dir) + os.pathsep + env["PATH"],
             "REAL_SETSID": shutil.which("setsid"), "BLOCK_WORKERS": "true"},
        stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
    )
    try:
        deadline = time.monotonic() + 5
        while len(list(directory.glob("*.launcher"))) != 4:
            assert process.poll() is None
            assert time.monotonic() < deadline
            time.sleep(.01)
        process.send_signal(sent)
        output, errors = process.communicate(timeout=5)
        assert process.returncode == expected, errors
        assert output.count("launcher waiting for session") == 4
        for marker in directory.glob("*.launcher"):
            assert not Path(f"/proc/{marker.stem}").exists()
        assert_workers_stopped(directory)
        assert (directory / "phases.tsv").exists()
    finally:
        if process.poll() is None:
            process.kill()
            process.wait()
        for marker in directory.glob("*.launcher"):
            pid = int(marker.stem)
            for target in (pid, -pid):
                try:
                    os.kill(target, signal.SIGKILL)
                except ProcessLookupError:
                    pass


def test_phase_summary_reports_partial_build_details_without_a_phase_total(tmp_path):
    phase_log = tmp_path / "phases.tsv"
    Path(f"{phase_log}.builds").write_text("Build agent image\t60\t\n")
    result = subprocess.run(
        ["sh", "hack/ci-phase-summary.sh"], cwd=ROOT,
        env={**os.environ, "CI_PHASE_LOG": str(phase_log)},
        capture_output=True, text=True, check=True,
    )
    assert "No phases were recorded" in result.stdout
    assert "| Build agent image | 1m 00s | — |" in result.stdout
    assert "Total instrumented" not in result.stdout


@pytest.mark.parametrize("build_details", [False, True])
def test_phase_summary_excludes_overlapping_builds_from_totals(tmp_path, build_details):
    phase_log = tmp_path / "phases.tsv"
    phase_log.write_text("Build images and create k3d cluster\t600\t\nRun CPU tests\t300\t\n")
    if build_details:
        Path(f"{phase_log}.builds").write_text(
            "Build operator image\t200\t1073741824\n"
            "Build agent image\t600\t2147483648\n"
            "Build PageBroker image\t100\t\n"
        )
    result = subprocess.run(
        ["sh", "hack/ci-phase-summary.sh", "cold"], cwd=ROOT,
        env={**os.environ, "CI_PHASE_LOG": str(phase_log)},
        capture_output=True, text=True, check=True,
    )
    assert "Cache: cold." in result.stdout
    assert "| **Total instrumented** | **15m 00s** | |" in result.stdout
    assert "| Build images and create k3d cluster | 10m 00s (67%) |" in result.stdout
    assert "| Run CPU tests | 5m 00s (33%) |" in result.stdout
    if build_details:
        assert "Their durations are not added to the total above." in result.stdout
        assert "| Build operator image | 3m 20s | 1.00 GiB |" in result.stdout
        assert "| Build agent image | 10m 00s | 2.00 GiB |" in result.stdout
        assert "| Build PageBroker image | 1m 40s | — |" in result.stdout
    else:
        assert "Concurrent image builds" not in result.stdout
