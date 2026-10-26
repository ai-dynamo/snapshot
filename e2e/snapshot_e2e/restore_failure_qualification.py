# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Verify that the manual CPU negative control failed at the intended restore."""

from __future__ import annotations

import argparse
import re
import xml.etree.ElementTree as ET
from pathlib import Path


PROBE_TEST = "test_restore_from_a_damaged_checkpoint_fails_and_keeps_the_snapshot"
EXPECTED_FAILURE = re.compile(
    r"\AAssertionError: restore reached unexpected terminal condition for "
    r"[^\s:]+/[^\s:]+: RestoreFailed:"
)


def verify_junit(report: Path, serial_report: Path, pytest_outcome: str) -> None:
    if pytest_outcome != "failure":
        raise ValueError(f"CPU pytest outcome was {pytest_outcome!r}, not failure")
    cases = []
    for path in (report, serial_report):
        try:
            root = ET.parse(path).getroot()
        except (OSError, ET.ParseError) as exc:
            raise ValueError(f"CPU JUnit report is missing or invalid: {path}") from exc
        report_cases = root.findall(".//testcase")
        if not report_cases:
            raise ValueError(f"CPU JUnit report contains no test cases: {path}")
        cases.extend(report_cases)
    probe = [
        case for case in cases
        if case.get("classname", "").rsplit(".", 1)[-1] == "test_snapshot_lifecycle"
        and case.get("name") == PROBE_TEST
    ]
    if len(probe) != 1:
        raise ValueError(f"expected exactly one damaged-checkpoint case, found {len(probe)}")

    errors = [case for case in cases if case.find("error") is not None]
    failures = [case for case in cases if case.find("failure") is not None]
    if errors or failures != probe:
        raise ValueError(
            f"CPU suite had {len(errors)} errors and {len(failures)} failures; "
            "only the damaged-checkpoint case may fail"
        )
    failure = probe[0].find("failure")
    assert failure is not None
    # Match the reported exception, not source lines in the traceback that
    # could mention RestoreFailed even for an unrelated assertion.
    if not EXPECTED_FAILURE.search(failure.get("message", "")):
        raise ValueError("damaged-checkpoint case did not fail on terminal RestoreFailed")


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("report", type=Path)
    parser.add_argument("serial_report", type=Path)
    parser.add_argument("--pytest-outcome", required=True)
    args = parser.parse_args()
    try:
        verify_junit(args.report, args.serial_report, args.pytest_outcome)
    except ValueError as exc:
        parser.exit(1, f"restore-failure qualification rejected: {exc}\n")
    print(
        "Restore-failure negative control verified from JUnit: "
        "only the damaged-checkpoint case failed on RestoreFailed."
    )


if __name__ == "__main__":
    main()
