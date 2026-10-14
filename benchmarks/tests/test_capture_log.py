# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Parser and report coverage for the capture breakdown."""

from snapshot_benchmarks import logs, report
from snapshot_benchmarks.schema import CheckpointAgentLog, ImageGrowthSample, _to_json_value

SUMMARY = """
2026-10-12T19:07:00.000000000Z  INFO  Checkpoint timing summary  {"content":"other","checkpoint":{"duration":"1s","phases":{}}}
2026-10-12T19:07:07.289000000Z  INFO  Checkpoint timing summary  {"content":"vllm-content","checkpoint":{"duration":"7.289s","phases":{"cuda_checkpoint":"1.665s","criu_dump":"5.197s","overlay_capture":"343ms","artifact_accounting":"20ms","unaccounted":"48ms"},"cuda":{"lock":"100ms","copy":"1.5s","job_file":"65ms"},"criu":{"freezing":"12ms","frozen":"2s","memdump":"400ms","memwrite":"4.5s","pages_written":1708984,"pages_written_bytes":7000000000,"image_bytes":7000000000,"image_growth":[{"offset":"0s","bytes":1000},{"offset":"2.5s","bytes":3500000000},{"offset":"5s","bytes":7000000000},{"offset":"5.2s","bytes":7000000000}]},"overlay":{"diff_bytes":49000000}}}
"""


def test_parse_checkpoint_agent_log_splits_criu_and_cuda() -> None:
    parsed = logs.parse_checkpoint_agent_log(SUMMARY, log_source_pod="agent", content="vllm-content")

    assert parsed.log_line_found
    assert parsed.parse_warnings == []
    assert parsed.duration == 7.289
    assert parsed.phases["criu_dump"] == 5.197
    assert parsed.cuda_lock == 0.1
    assert parsed.cuda_copy == 1.5
    assert parsed.criu_memdump == 0.4
    assert parsed.criu_memwrite == 4.5
    assert parsed.criu_pages_written_bytes == 7_000_000_000
    assert parsed.overlay_diff_bytes == 49_000_000
    assert len(parsed.image_growth) == 4
    assert parsed.memwrite_gigabytes_per_second == 7_000_000_000 / 4.5 / 1e9
    assert parsed.image_growth_midpoint_fraction == 0.5
    assert parsed.image_growth_max_stall_seconds is not None
    assert abs(parsed.image_growth_max_stall_seconds - 0.2) < 1e-9


def test_parse_checkpoint_agent_log_warns_for_old_agent() -> None:
    logs_text = (
        'Checkpoint timing summary\t{"content":"wanted","checkpoint":'
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
        criu_memdump=0.4,
        criu_memwrite=4.5,
        criu_pages_written_bytes=7_000_000_000,
        criu_image_bytes=7_000_000_000,
        overlay_diff_bytes=49_000_000,
        image_growth=[
            ImageGrowthSample(0.0, 1000),
            ImageGrowthSample(2.5, 3_500_000_000),
            ImageGrowthSample(5.0, 7_000_000_000),
        ],
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

    assert "## Where the capture time goes" in text
    assert "## How the CRIU image was written" in text
    assert "Qwen3 0.6B" in text
    assert "1.556" in text  # 7e9/4.5/1e9
