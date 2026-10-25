# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""CLI argument validation for benchmark repetitions."""

from unittest.mock import MagicMock, patch

import pytest

from snapshot_benchmarks.cli import CleanupError, _reject_keep_with_repeats, build_parser, cmd_sweep, main


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


def _sweep_args(*, repeats: int = 2, fail_fast: bool = False) -> MagicMock:
    args = MagicMock()
    args.repeats = repeats
    args.fail_fast = fail_fast
    args.keep = False
    args.output_dir = "/tmp/benchmark-results"
    args.models = "models.yaml"
    return args


def _model(label: str = "Qwen3 0.6B") -> MagicMock:
    model = MagicMock()
    model.label = label
    return model


def test_sweep_aborts_after_cleanup_error() -> None:
    args = _sweep_args()
    run_calls: list[str] = []

    def run_one(_cfg, _engine, model, _args):
        run_calls.append(model.label)
        raise CleanupError("cleanup failed")

    with (
        patch("snapshot_benchmarks.cli._setup", return_value=(MagicMock(), MagicMock())),
        patch("snapshot_benchmarks.cli._load_models", return_value=[_model(), _model("Qwen3 14B")]),
        patch("snapshot_benchmarks.cli._run_one", side_effect=run_one),
    ):
        assert cmd_sweep(args) == 1
    assert run_calls == ["Qwen3 0.6B"]


def test_sweep_continues_after_run_failure() -> None:
    args = _sweep_args(repeats=1)
    success = MagicMock(git_sha="abc")
    outcomes = iter([RuntimeError("boom"), success])

    def run_one(_cfg, _engine, model, _args):
        outcome = next(outcomes)
        if isinstance(outcome, Exception):
            raise outcome
        return outcome

    with (
        patch("snapshot_benchmarks.cli._setup", return_value=(MagicMock(), MagicMock())),
        patch(
            "snapshot_benchmarks.cli._load_models",
            return_value=[_model("first"), _model("second")],
        ),
        patch("snapshot_benchmarks.cli._run_one", side_effect=run_one),
        patch("snapshot_benchmarks.cli.results.invocation_dir", return_value="/tmp/out"),
        patch("snapshot_benchmarks.cli.results.write_result", return_value="/tmp/out/run.json"),
    ):
        assert cmd_sweep(args) == 1


def test_sweep_fail_fast_reraises() -> None:
    args = _sweep_args(repeats=2, fail_fast=True)

    with (
        patch("snapshot_benchmarks.cli._setup", return_value=(MagicMock(), MagicMock())),
        patch("snapshot_benchmarks.cli._load_models", return_value=[_model()]),
        patch("snapshot_benchmarks.cli._run_one", side_effect=RuntimeError("stop here")),
    ):
        with pytest.raises(RuntimeError, match="stop here"):
            cmd_sweep(args)
