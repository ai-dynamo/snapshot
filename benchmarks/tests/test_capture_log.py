# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Parser and report coverage for the capture breakdown."""

import pytest

from snapshot_benchmarks import logs, report
from snapshot_benchmarks.schema import CheckpointAgentLog, _to_json_value

SUMMARY = """
2026-10-12T19:07:00.000000000Z  INFO  Checkpoint timing summary  {"content":"other","checkpoint":{"duration":"1s","phases":{}}}
2026-10-12T19:07:07.289000000Z  INFO  Checkpoint timing summary  {"content":"vllm-content","checkpoint":{"duration":"7.289s","phases":{"cuda_checkpoint":"1.665s","criu_dump":"5.197s","overlay_capture":"343ms","artifact_accounting":"20ms","unaccounted":"48ms"},"cuda":{"lock":"100ms","copy":"1.5s","job_file":"65ms"},"criu":{"freezing":"12ms","frozen":"2s","memdump":"4.5s","memwrite":"400ms","pages_written":1708984,"pages_written_bytes":7000000000,"checkpoint_dir_bytes_before_overlay":7000100000},"overlay":{"diff_bytes":49000000}}}
"""


def test_parse_checkpoint_agent_log_splits_criu_and_cuda() -> None:
    parsed = logs.parse_checkpoint_agent_log(SUMMARY, log_source_pod="agent", content="vllm-content")

    assert parsed.log_line_found
    assert parsed.parse_warnings == []
    assert parsed.duration == 7.289
    assert parsed.phases["criu_dump"] == 5.197
    assert parsed.cuda_lock == 0.1
    assert parsed.cuda_copy == 1.5
    assert parsed.criu_memdump == 4.5
    assert parsed.criu_memwrite == 0.4
    assert parsed.criu_read == pytest.approx(4.1)
    assert parsed.criu_pages_written_bytes == 7_000_000_000
    assert parsed.checkpoint_dir_bytes_before_overlay == 7_000_100_000
    assert parsed.overlay_diff_bytes == 49_000_000
    assert parsed.memwrite_gigabytes_per_second == 7_000_000_000 / 0.4 / 1e9
    assert not hasattr(parsed, "criu_image_bytes")


def test_parse_checkpoint_agent_log_reads_pre_rename_image_bytes() -> None:
    logs_text = (
        "2026-10-12T19:07:07Z INFO Checkpoint timing summary "
        '{"content":"wanted","checkpoint":{"duration":"6.8s","phases":{},'
        '"criu":{"memdump":"2s","memwrite":"1s","image_bytes":123}}}'
    )

    parsed = logs.parse_checkpoint_agent_log(logs_text, log_source_pod=None, content="wanted")

    assert parsed.checkpoint_dir_bytes_before_overlay == 123


def test_criu_read_is_missing_when_timers_are_not_nested() -> None:
    assert CheckpointAgentLog(criu_memdump=0.4, criu_memwrite=4.5).criu_read is None
    assert CheckpointAgentLog(criu_memdump=0.4).criu_read is None


def test_parse_checkpoint_agent_log_warns_for_old_agent() -> None:
    logs_text = (
        "2026-10-12T19:07:07Z INFO Checkpoint timing summary "
        '{"content":"wanted","checkpoint":'
        '{"duration":"6.8s","phases":{"criu_dump":"4.95s","cuda_checkpoint":"1.45s"}}}'
    )

    parsed = logs.parse_checkpoint_agent_log(logs_text, log_source_pod=None, content="wanted")

    assert parsed.log_line_found
    assert parsed.phases["criu_dump"] == 4.95
    assert parsed.criu_memwrite is None
    assert any("predates capture sub-phase" in warning for warning in parsed.parse_warnings)


def test_report_includes_capture_tables() -> None:
    parsed = CheckpointAgentLog(
        log_line_found=True,
        duration=7.289,
        phases={"criu_dump": 5.197, "overlay_capture": 0.343},
        cuda_lock=0.1,
        cuda_copy=1.5,
        criu_freezing=0.012,
        criu_memdump=4.5,
        criu_memwrite=0.4,
        criu_pages_written_bytes=7_000_000_000,
        checkpoint_dir_bytes_before_overlay=7_000_100_000,
        overlay_diff_bytes=49_000_000,
    )
    text = report.render(
        [
            {
                "schema_version": 1,
                "model": {"label": "Qwen3 0.6B"},
                "checkpoint": {"checkpoint_seconds": 8.0},
                "checkpoint_agent_log": _to_json_value(parsed),
            }
        ]
    )

    _assert_capture_tables(text)


def _v2_result(parsed: CheckpointAgentLog) -> dict:
    return {
        "schema_version": 2,
        "run_id": "r",
        "model": {"label": "Qwen3 0.6B", "tp": 1, "ep": 1},
        "engine": {"name": "vllm", "version": "0.1"},
        "environment": {},
        "cold_start": {"startup_to_ready_seconds": 60.0, "measurement": None},
        "checkpoint": {"checkpoint_seconds": 8.0},
        "checkpoint_agent_log": _to_json_value(parsed),
        "restore_attempts": [],
    }


def test_report_v2_includes_capture_tables() -> None:
    parsed = CheckpointAgentLog(
        log_line_found=True,
        duration=7.289,
        phases={"criu_dump": 5.197, "overlay_capture": 0.343},
        cuda_lock=0.1,
        cuda_copy=1.5,
        criu_freezing=0.012,
        criu_memdump=4.5,
        criu_memwrite=0.4,
        criu_pages_written_bytes=7_000_000_000,
        checkpoint_dir_bytes_before_overlay=7_000_100_000,
        overlay_diff_bytes=49_000_000,
    )

    _assert_capture_tables(report.render([_v2_result(parsed)]))


def test_report_v2_surfaces_capture_parse_warnings() -> None:
    parsed = CheckpointAgentLog(parse_warnings=["criu stats unavailable: no stats-dump"])

    text = report.render([_v2_result(parsed)])

    assert "## Warnings" in text
    assert "criu stats unavailable: no stats-dump" in text


def test_report_v2_skips_capture_tables_without_agent_log() -> None:
    result = _v2_result(CheckpointAgentLog())
    del result["checkpoint_agent_log"]

    text = report.render([result])

    assert "## Where the capture time goes" not in text


def _assert_capture_tables(text: str) -> None:
    assert "## Where the capture time goes" in text
    assert "## How the pages were written" in text
    assert "Qwen3 0.6B" in text
    assert "of which memwrite" in text
    assert "CRIU read" in text
    assert "4.100" in text  # memdump 4.5 - memwrite 0.4
    assert "17.500" in text  # 7e9 / 0.4 / 1e9
    assert "7.00 GB" in text
    assert "Image GB/s" not in text
    assert "Midpoint" not in text


def test_report_skips_capture_tables_without_agent_log() -> None:
    text = report.render(
        [
            {
                "schema_version": 1,
                "model": {"label": "older"},
                "checkpoint": {"checkpoint_seconds": 8.0},
            }
        ]
    )

    assert "## Where the capture time goes" not in text
    assert "## How the pages were written" not in text
