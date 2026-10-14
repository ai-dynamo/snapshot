# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""CLI argument validation for benchmark repetitions."""

import pytest

from snapshot_benchmarks.cli import _reject_keep_with_repeats, build_parser, main


def test_sweep_repeats() -> None:
    args = build_parser().parse_args(
        ["sweep", "--image", "example/image", "--repeats", "3"]
    )

    assert args.repeats == 3


def test_keep_with_repeats_is_rejected(capsys: pytest.CaptureFixture[str]) -> None:
    commands = (
        ["run", "--image", "example/image", "--model-label", "model", "--repeats", "2", "--keep"],
        ["sweep", "--image", "example/image", "--repeats", "2", "--keep"],
    )
    for command in commands:
        assert main(command) == 2
        assert "--keep" in capsys.readouterr().err


def test_single_run_keep_is_allowed() -> None:
    args = build_parser().parse_args(
        ["run", "--image", "example/image", "--model-label", "model", "--keep"]
    )

    assert _reject_keep_with_repeats(args) is None


def test_repeats_must_be_positive() -> None:
    with pytest.raises(SystemExit):
        build_parser().parse_args(
            ["run", "--image", "example/image", "--model-label", "model", "--repeats", "0"]
        )
