# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Cluster-free regressions for the required-case guard in ``conftest.py``.

The CPU end-to-end check exists to prove a checkpoint and restore round trip
runs. Without this guard it proves only that pytest exited 0, which it also
does when the case was deselected by a marker change or skipped itself. These
run a small inner pytest session through ``pytester`` so each of those
outcomes is covered without a cluster.
"""

from __future__ import annotations

from pathlib import Path

import pytest

CONFTEST = Path(__file__).with_name("conftest.py")

INNER_TESTS = """
import pytest


@pytest.mark.cpu
def test_required_case():
    pass


@pytest.mark.cpu
def test_required_case_that_skips():
    pytest.skip("nothing to do here")


@pytest.mark.gpu
def test_other_case():
    pass


@pytest.mark.cpu
@pytest.mark.parametrize("case", ["one", "two"])
def test_required_parametrized(case):
    pass
"""


def run_inner(
    pytester: pytest.Pytester,
    monkeypatch: pytest.MonkeyPatch,
    *args: str,
    required: str | None = None,
) -> pytest.RunResult:
    if required is None:
        monkeypatch.delenv("SNAPSHOT_E2E_REQUIRED_TESTS", raising=False)
    else:
        monkeypatch.setenv("SNAPSHOT_E2E_REQUIRED_TESTS", required)
    pytester.makeconftest(CONFTEST.read_text(encoding="utf-8"))
    pytester.makepyfile(test_inner=INNER_TESTS)
    pytester.makeini(
        """
        [pytest]
        markers =
            cpu: tests that run on a CPU-only cluster
            gpu: tests that require a GPU workload
        """
    )
    return pytester.runpytest_inprocess("-p", "no:cacheprovider", *args)


def test_required_case_that_runs_is_left_alone(pytester, monkeypatch):
    run = run_inner(
        pytester,
        monkeypatch,
        "-m",
        "cpu",
        "-k",
        "test_required_case and not skips",
        required="test_required_case",
    )
    run.assert_outcomes(passed=1)
    assert run.ret == pytest.ExitCode.OK


def test_deselected_required_case_fails_the_run(pytester, monkeypatch):
    """The marker-drop regression: selecting only GPU leaves the CPU case out."""
    run = run_inner(pytester, monkeypatch, "-m", "gpu", required="test_required_case")
    assert run.ret != pytest.ExitCode.OK
    run.stderr.fnmatch_lines(["*did not select: test_required_case*"])


def test_skipped_required_case_fails_the_run(pytester, monkeypatch):
    run = run_inner(
        pytester,
        monkeypatch,
        "-m",
        "cpu",
        "-k",
        "skips",
        required="test_required_case_that_skips",
    )
    run.assert_outcomes(failed=1)
    run.stdout.fnmatch_lines(["*required to run, but it was skipped*"])


def test_skip_is_still_a_skip_when_not_required(pytester, monkeypatch):
    """Local runs set nothing, and a skip there is not a failure."""
    run = run_inner(pytester, monkeypatch, "-m", "cpu", "-k", "skips")
    run.assert_outcomes(skipped=1)
    assert run.ret == pytest.ExitCode.OK


def test_requiring_a_name_that_does_not_exist_fails_the_run(pytester, monkeypatch):
    """A typo in the required list must not read as a satisfied requirement."""
    run = run_inner(pytester, monkeypatch, "-m", "cpu", required="test_renamed_case")
    assert run.ret != pytest.ExitCode.OK
    run.stderr.fnmatch_lines(["*did not select: test_renamed_case*"])


def test_one_entry_covers_every_case_of_a_parametrized_test(pytester, monkeypatch):
    run = run_inner(
        pytester,
        monkeypatch,
        "-m",
        "cpu",
        "-k",
        "parametrized",
        required="test_required_parametrized",
    )
    run.assert_outcomes(passed=2)
    assert run.ret == pytest.ExitCode.OK
