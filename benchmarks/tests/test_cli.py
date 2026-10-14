# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""CLI argument validation for benchmark repetitions."""

import pytest

from snapshot_benchmarks.cli import build_parser


def test_sweep_repeats() -> None:
    args = build_parser().parse_args(
        ["sweep", "--image", "example/image", "--repeats", "3"]
    )

    assert args.repeats == 3


def test_repeats_must_be_positive() -> None:
    with pytest.raises(SystemExit):
        build_parser().parse_args(
            ["run", "--image", "example/image", "--model-label", "model", "--repeats", "0"]
        )
