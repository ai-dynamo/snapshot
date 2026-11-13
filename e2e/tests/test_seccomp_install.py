# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Cluster-free execution checks for the Helm-rendered seccomp installer."""

from __future__ import annotations

from pathlib import Path
import shutil
import subprocess

import pytest
import yaml


CHART = Path(__file__).resolve().parents[2] / "charts" / "snapshot"
SUCCESS = "Deployed seccomp profile to /var/lib/kubelet/seccomp/profiles/block-iouring.json"


@pytest.fixture(scope="module")
def seccomp_command() -> list[str]:
    rendered = subprocess.run(
        ["helm", "template", "snapshot", str(CHART), "--show-only", "templates/daemonset.yaml"],
        check=True,
        capture_output=True,
        text=True,
    )
    daemonsets = [
        item for item in yaml.safe_load_all(rendered.stdout)
        if item and item.get("kind") == "DaemonSet"
    ]
    assert len(daemonsets) == 1
    installers = [
        container
        for container in daemonsets[0]["spec"]["template"]["spec"]["initContainers"]
        if container["name"] == "deploy-seccomp"
    ]
    assert len(installers) == 1
    command = installers[0]["command"]
    # Restrict execution to the known, mockable commands in the chart.
    assert command[:2] == ["sh", "-ec"]
    assert command[2].splitlines() == [
        "mkdir -p /host-seccomp/profiles",
        "cp /seccomp-profiles/block-iouring.json /host-seccomp/profiles/block-iouring.json",
        f'echo "{SUCCESS}"',
    ]
    return command


def run_installer(
    command: list[str], tmp_path: Path, mkdir_status: int = 0, cp_status: int = 0
) -> tuple[subprocess.CompletedProcess[str], list[str]]:
    stub_dir = tmp_path / "bin"
    stub_dir.mkdir()
    trace = tmp_path / "trace"
    for name, status in (("mkdir", mkdir_status), ("cp", cp_status)):
        stub = stub_dir / name
        stub.write_text(
            "#!/bin/sh\n"
            f'printf "%s\\n" {name} >> "$INSTALL_TRACE"\n'
            f"exit {status}\n"
        )
        stub.chmod(0o755)
    shell = shutil.which(command[0])
    assert shell is not None
    result = subprocess.run(
        [shell, *command[1:]],
        env={"PATH": str(stub_dir), "INSTALL_TRACE": str(trace)},
        check=False,
        capture_output=True,
        text=True,
    )
    return result, trace.read_text().splitlines() if trace.exists() else []


@pytest.mark.parametrize(
    ("mkdir_status", "cp_status", "expected_trace", "success"),
    [
        (0, 0, ["mkdir", "cp"], True),
        (23, 0, ["mkdir"], False),
        (0, 29, ["mkdir", "cp"], False),
    ],
)
def test_seccomp_installer_exit_status(
    seccomp_command: list[str],
    tmp_path: Path,
    mkdir_status: int,
    cp_status: int,
    expected_trace: list[str],
    success: bool,
) -> None:
    result, trace = run_installer(seccomp_command, tmp_path, mkdir_status, cp_status)
    assert trace == expected_trace
    assert (result.returncode == 0) is success
    assert (SUCCESS in result.stdout) is success


def test_without_errexit_masks_failed_copy(seccomp_command: list[str], tmp_path: Path) -> None:
    # A negative control: the original -c flag incorrectly reports success.
    command = [seccomp_command[0], "-c", seccomp_command[2]]
    result, trace = run_installer(command, tmp_path, cp_status=29)
    assert trace == ["mkdir", "cp"]
    assert result.returncode == 0
    assert SUCCESS in result.stdout
