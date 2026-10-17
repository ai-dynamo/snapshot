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
    assert parsed.pagemap_hole_bytes == 12_596 * 4096
    assert parsed.first_dumping_core_seconds == 3.6
    assert parsed.thread_core_count == 724
    assert parsed.thread_core_first_seconds == 3.7
    assert parsed.thread_core_last_seconds == 4.05
    assert parsed.dumping_mm_seconds == 4.1
    assert parsed.dumping_pstree_seconds == 4.4
    assert parsed.namespaces_complete_seconds == 4.7
    assert parsed.unfreezing_seconds == 4.9
    assert parsed.writing_stats_seconds == 5.04
    assert parsed.dumping_finished_seconds == 5.055


def test_parse_inventory_keeps_finish_after_pagemap_flood() -> None:
    pagemaps = "\n".join(
        f"./containers/main/dump.log:(00.{index:06d}) Pagemap generated: 10 pages (0 lazy) 3 holes"
        for index in range(500)
    )
    threads = "\n".join(
        f"./containers/main/dump.log:(03.{index:06d}) Dumping core for thread (pid: {index})"
        for index in range(800)
    )
    output = f"""
## files
4096	8	containers/main/pages-1.img
## dump-log
./containers/main/dump.log:(00.100000) Dumping pages (type: 1 pid: 100)
{pagemaps}
{threads}
./containers/main/dump.log:(05.200000) Dumping finished successfully
## summary
pagemap count=500 first=00.000000 last=00.000499 pages=5000 holes=1500
threads count=800 first=03.000000 last=03.000799
"""
    parsed = image.parse_inventory(output)

    assert parsed.parse_warnings == []
    assert parsed.dumping_finished_seconds == 5.2
    assert parsed.pagemap_count == 500
    assert parsed.pagemap_pages == 5000
    assert parsed.pagemap_holes == 1500
    assert parsed.thread_core_count == 800
    assert not any(event.message.startswith("Pagemap generated:") for event in parsed.dump_log)
    assert not any("for thread" in event.message for event in parsed.dump_log)


def test_parse_inventory_accepts_size_only_lines() -> None:
    parsed = image.parse_inventory(
        """
## files
4096	containers/main/pages-1.img
## dump-log
./containers/main/dump.log:(01.000000) Dumping finished successfully
"""
    )

    assert parsed.pages_bytes == 4096
    assert parsed.pages_allocated_bytes is None
    assert parsed.pages_sparse_bytes is None
    assert parsed.dumping_finished_seconds == 1.0


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
