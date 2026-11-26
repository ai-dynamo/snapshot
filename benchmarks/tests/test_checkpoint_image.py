# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""File inventory and dump.log markers for a captured artifact."""

import subprocess
from pathlib import Path

from snapshot_benchmarks import image, report
from snapshot_benchmarks.schema import _to_json_value

INVENTORY = """
## files
4096	8	containers/main/core-1.img
4262846464	6250000	containers/main/pages-1.img
3190000000	6226562	containers/main/memfd-1.img
1024	2	containers/main/mm-1.img
49397760	96480	containers/main/rootfs-diff.tar
12	1	containers/main/dump.log
## dump-log
./containers/main/dump.log:(00.601000) Dumping pages (type: 1 pid: 100)
./containers/main/dump.log:(03.600000) Dumping core (pid: 100)
./containers/main/dump.log:(04.100000) Dumping mm (pid: 100)
./containers/main/dump.log:(04.400000) Dumping pstree (pid: 100)
./containers/main/dump.log:(04.700000) Namespaces dump complete
./containers/main/dump.log:(04.900000) Unfreezing tasks into 3
./containers/main/dump.log:(05.040000) Writing stats
./containers/main/dump.log:(05.055000) Dumping finished successfully
## summary
pagemap count=2 first=00.700000 last=01.200000 pages=1040734 holes=12596
threads count=724 first=03.700000 last=04.050000
"""


def test_parse_inventory_names_non_page_bytes() -> None:
    parsed = image.parse_inventory(INVENTORY)

    assert parsed.parse_warnings == []
    assert parsed.file_count == 6
    assert parsed.pages_bytes == 4_262_846_464
    assert parsed.pages_allocated_bytes == 6_250_000 * 512
    assert parsed.pages_sparse_bytes == 4_262_846_464 - 6_250_000 * 512
    assert parsed.non_page_bytes == 3_190_000_000 + 49_397_760 + 4096 + 1024 + 12
    categories = {item.name: item.bytes for item in parsed.categories}
    assert categories["pages"] == 4_262_846_464
    assert categories["memfd"] == 3_190_000_000
    assert categories["rootfs-diff.tar"] == 49_397_760
    pages = next(item for item in parsed.files if item.path.endswith("pages-1.img"))
    assert pages.allocated_bytes == 6_250_000 * 512
    assert [item.path for item in parsed.files] == [
        "containers/main/pages-1.img",
        "containers/main/memfd-1.img",
        "containers/main/rootfs-diff.tar",
    ]
    assert parsed.first_dumping_pages_seconds == 0.601
    assert parsed.last_pagemap_seconds == 1.2
    assert parsed.pagemap_pages == 1_040_734
    assert parsed.pagemap_holes == 12_596
    assert not hasattr(parsed, "pagemap_hole_bytes")
    assert parsed.first_dumping_core_seconds == 3.6
    assert parsed.core_after_last_pagemap_seconds == 3.6
    assert parsed.thread_core_count == 724
    assert parsed.thread_core_first_seconds == 3.7
    assert parsed.thread_core_last_seconds == 4.05
    assert parsed.dumping_mm_seconds == 4.1
    assert parsed.dumping_pstree_seconds == 4.4
    assert parsed.namespaces_complete_seconds == 4.7
    assert parsed.unfreezing_seconds == 4.9
    assert parsed.writing_stats_seconds == 5.04
    assert parsed.dumping_finished_seconds == 5.055


def test_awk_keeps_finish_after_pagemap_flood(tmp_path: Path) -> None:
    dump_log = tmp_path / "dump.log"
    lines = ["(00.100000) Dumping pages (type: 1 pid: 100)"]
    lines.extend(
        f"(00.{index:06d}) Pagemap generated: 10 pages (0 lazy) 3 holes" for index in range(500)
    )
    lines.extend(
        f"(03.{index:06d}) Dumping core for thread (pid: {index})" for index in range(800)
    )
    lines.append("(05.200000) Dumping finished successfully")
    dump_log.write_text("\n".join(lines) + "\n")

    completed = subprocess.run(
        ["awk", image._DUMP_LOG_AWK, str(dump_log)],
        check=False,
        capture_output=True,
        text=True,
    )
    assert completed.returncode == 0, completed.stderr
    parsed = image.parse_inventory(
        "## files\n4096\t8\tcontainers/main/pages-1.img\n## dump-log\n" + completed.stdout
    )

    assert parsed.parse_warnings == []
    assert parsed.dumping_finished_seconds == 5.2
    assert parsed.pagemap_count == 500
    assert parsed.pagemap_pages == 5000
    assert parsed.pagemap_holes == 1500
    assert parsed.thread_core_count == 800
    assert not any(event.message.startswith("Pagemap generated:") for event in parsed.dump_log)
    assert not any("for thread" in event.message for event in parsed.dump_log)


def test_report_includes_image_categories() -> None:
    parsed = image.parse_inventory(INVENTORY)
    text = report.render(
        [
            {
                "schema_version": 1,
                "model": {"label": "Qwen3 0.6B"},
                "checkpoint_image": _to_json_value(parsed),
            }
        ]
    )

    assert "## What is in the checkpoint image" in text
    assert "memfd" in text
    assert "pages-1.img" in text
    assert "5.055" in text
    assert "Unfreezing tasks" in text
    assert "sparse" in text
    sparse_line = next(line for line in text.splitlines() if line.startswith("Pages apparent"))
    assert "Pagemap holes" not in sparse_line
    assert "parent image" in text
    assert "pages_skipped_parent" in text


def test_inventory_command_lists_allocated_blocks_and_summarizes() -> None:
    command = image.inventory_command("'/checkpoints/artifacts/uid'")

    assert "cd '/checkpoints/artifacts/uid'" in command
    assert "find . -type f -printf '%s\\t%b\\t%P\\n'" in command
    assert "Pagemap generated:" in command
    assert "Dumping finished" in command
    assert "Dumping core for thread" in command
    assert "## summary" in command


def test_awk_summarizes_a_dump_log(tmp_path: Path) -> None:
    dump_log = tmp_path / "dump.log"
    lines = ["(00.500000) Dumping pages (type: 1 pid: 100)"]
    lines.extend(
        f"(00.{index:06d}) Pagemap generated: 10 pages (0 lazy) 4 holes" for index in range(3)
    )
    lines.append("(03.100000) Dumping core (pid: 100)")
    lines.extend(f"(03.{index:06d}) Dumping core for thread (pid: {index})" for index in range(2))
    lines.extend(
        [
            "(04.200000) Dumping mm (pid: 100)",
            "(04.800000) Unfreezing tasks into 3",
            "(04.900000) Writing stats",
            "(05.000000) Dumping finished successfully",
        ]
    )
    dump_log.write_text("\n".join(lines) + "\n")

    completed = subprocess.run(
        ["awk", image._DUMP_LOG_AWK, str(dump_log)],
        check=False,
        capture_output=True,
        text=True,
    )
    assert completed.returncode == 0, completed.stderr
    parsed = image.parse_inventory("## files\n## dump-log\n" + completed.stdout)

    assert parsed.parse_warnings == []
    assert parsed.pagemap_count == 3
    assert parsed.pagemap_pages == 30
    assert parsed.pagemap_holes == 12
    assert parsed.thread_core_count == 2
    assert parsed.first_dumping_core_seconds == 3.1
    assert parsed.unfreezing_seconds == 4.8
    assert parsed.writing_stats_seconds == 4.9
    assert parsed.dumping_finished_seconds == 5.0
    assert len(parsed.dump_log) == 6


MULTI_PROCESS = """
## files
4096	8	containers/main/pages-1.img
## dump-log
./containers/main/dump.log:(00.500000) Dumping pages (type: 1 pid: 100)
./containers/main/dump.log:(01.000000) Dumping core (pid: 100)
./containers/main/dump.log:(02.000000) Dumping pages (type: 1 pid: 200)
./containers/main/dump.log:(03.000000) Dumping core (pid: 200)
./containers/main/dump.log:(04.000000) Dumping pstree
./containers/main/dump.log:(05.000000) Dumping finished successfully
## summary
pagemap count=3 first=00.600000 last=02.500000 pages=300 holes=5
"""


def test_core_after_last_pagemap_skips_an_earlier_tasks_core() -> None:
    parsed = image.parse_inventory(MULTI_PROCESS)

    assert parsed.first_dumping_core_seconds == 1.0
    assert parsed.last_pagemap_seconds == 2.5
    # pid 100's core precedes pid 200's pages, so it is not the end of the write.
    assert parsed.core_after_last_pagemap_seconds == 3.0


def test_core_after_last_pagemap_falls_back_without_a_pagemap_summary() -> None:
    parsed = image.parse_inventory(
        "## files\n## dump-log\n"
        "./containers/main/dump.log:(01.000000) Dumping core (pid: 100)\n"
    )

    assert parsed.core_after_last_pagemap_seconds == 1.0


def _result(*, pages_written, pages_written_bytes, holes=1000) -> dict:
    parsed = image.parse_inventory(INVENTORY)
    parsed.pagemap_holes = holes
    result = {
        "schema_version": 2,
        "run_id": "r",
        "model": {"label": "Qwen3 0.6B", "tp": 1, "ep": 1},
        "engine": {"name": "vllm", "version": "0.1"},
        "environment": {},
        "cold_start": {"startup_to_ready_seconds": 60.0, "measurement": None},
        "checkpoint": {"checkpoint_seconds": 8.0},
        "checkpoint_image": _to_json_value(parsed),
        "restore_attempts": [],
    }
    if pages_written is not None:
        result["checkpoint_agent_log"] = {
            "criu_pages_written": pages_written,
            "criu_pages_written_bytes": pages_written_bytes,
        }
    return result


def test_hole_bytes_use_the_page_size_the_agent_recorded() -> None:
    text = report.render([_result(pages_written=100, pages_written_bytes=100 * 65536)])

    # 1000 holes * 64 KiB, not * 4096.
    assert "Pagemap holes 1000 (0.07 GB at 64 KiB pages)" in text


def test_hole_bytes_are_omitted_without_a_page_size() -> None:
    text = report.render([_result(pages_written=None, pages_written_bytes=None)])

    assert "Pagemap holes 1000 are pages CRIU skipped" in text
    assert "KiB pages" not in text


def test_report_v2_prints_the_image_inventory_and_core_marks() -> None:
    text = report.render([_result(pages_written=100, pages_written_bytes=100 * 4096)])

    assert "## What is in the checkpoint image" in text
    assert "Dumping core at or after last pagemap" in text
    assert "Shared anonymous memory is written after `Dumping pstree`" in text
    assert "| Dump log | Offset (s) |" in text
    assert "| Category | Apparent | Allocated | Files |" in text


def test_report_v2_surfaces_image_parse_warnings() -> None:
    result = _result(pages_written=None, pages_written_bytes=None)
    result["checkpoint_image"]["parse_warnings"] = ["inventory failed: no shell"]

    text = report.render([result])

    assert "inventory failed: no shell" in text
