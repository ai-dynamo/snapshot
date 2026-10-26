# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Inventory a checkpoint artifact before the benchmark deletes it.

The agent's pages_written counter is only private anonymous pages. Listing
every file names the rest of the image, and `st_blocks` separates apparent
size (which counts sparse holes) from allocated size. dump.log markers are
CRIU's own log clock. Pagemap and per-thread core lines are summarized by
awk rather than stored: a vLLM dump emits hundreds of each, and a fixed cap
on those lines used to drop "Dumping finished".
"""

from __future__ import annotations

import re
import shlex
from collections import defaultdict

from snapshot_benchmarks.schema import (
    ArtifactFile,
    CheckpointImage,
    DumpLogEvent,
    ImageCategory,
)

_LARGE_FILE_BYTES = 1 << 20
_MAX_LARGE_FILES = 40
_BLOCK_SIZE = 512

# Portable awk (mawk or gawk). High-volume lines are reduced to a count and
# a first/last timestamp. Phase boundaries are printed in full so the
# post-write tail can be split: page list, core dump, thread cores, mm,
# pstree, namespace dump, the kill ("Unfreezing tasks"), stats, finish.
_DUMP_LOG_AWK = r"""
function ts(line,    s, e) {
  s = index(line, "(")
  e = index(line, ")")
  if (s > 0 && e > s) return substr(line, s + 1, e - s - 1)
  return ""
}
function num_after(line, key,    p, rest, n, i, c) {
  p = index(line, key)
  if (p == 0) return 0
  rest = substr(line, p + length(key))
  n = ""
  for (i = 1; i <= length(rest); i++) {
    c = substr(rest, i, 1)
    if (c ~ /[0-9]/) n = n c
    else if (n != "") break
  }
  return n + 0
}
function lastnum(line,    i, c, cur, found) {
  cur = ""
  found = "0"
  for (i = 1; i <= length(line); i++) {
    c = substr(line, i, 1)
    if (c ~ /[0-9]/) cur = cur c
    else if (cur != "") { found = cur; cur = "" }
  }
  if (cur != "") found = cur
  return found + 0
}
/Pagemap generated:/ {
  t = ts($0)
  if (pm_n == 0) pm_first = t
  pm_last = t
  pm_n++
  pm_pages += num_after($0, "Pagemap generated: ")
  pm_holes += lastnum($0)
  next
}
/Dumping core for thread/ {
  t = ts($0)
  if (th_n == 0) th_first = t
  th_last = t
  th_n++
  next
}
/Dumping pages \(|Dumping finished|Dumping FAILED|Writing stats|Unfreezing tasks|Dumping mm \(|Dumping pstree|Dumping task \(|Dumping core \(pid:|Namespaces dump complete|Post dump script|Lazy pages transfer/ {
  print FILENAME ":" $0
}
END {
  print "## summary"
  printf "pagemap count=%d first=%s last=%s pages=%d holes=%d\n", pm_n, pm_first, pm_last, pm_pages, pm_holes
  printf "threads count=%d first=%s last=%s\n", th_n, th_first, th_last
}
"""

_DUMP_LINE = re.compile(r"^(?P<path>.*?):\((?P<offset>\d+(?:\.\d+)?)\)\s*(?P<message>.*)$")
_PAGEMAP_SUMMARY = re.compile(
    r"^pagemap count=(?P<count>\d+) first=(?P<first>[0-9.]*) last=(?P<last>[0-9.]*) "
    r"pages=(?P<pages>\d+) holes=(?P<holes>\d+)$"
)
_THREAD_SUMMARY = re.compile(
    r"^threads count=(?P<count>\d+) first=(?P<first>[0-9.]*) last=(?P<last>[0-9.]*)$"
)
_NAMED_FILES = {
    "dump.log",
    "rootfs-diff.tar",
    "deleted-files.json",
    "stats-dump",
    "inventory.img",
}


def inventory_command(quoted_root: str) -> str:
    """Shell run inside the capture-node agent. `quoted_root` is already quoted."""
    return "\n".join(
        [
            f"cd {quoted_root}",
            "echo '## files'",
            # %s is st_size (apparent, holes included). %b is st_blocks in
            # 512-byte units (allocated).
            r"find . -type f -printf '%s\t%b\t%P\n'",
            "echo '## dump-log'",
            "find . -name dump.log -type f -exec awk "
            + shlex.quote(_DUMP_LOG_AWK)
            + " {} + || true",
        ]
    )


def parse_inventory(output: str) -> CheckpointImage:
    """Parses the marker-delimited stdout of `inventory_command`."""
    if "## files" not in output:
        return CheckpointImage(parse_warnings=["artifact inventory did not contain a file list"])
    files_text, separator, log_text = output.partition("## dump-log")
    files_text = files_text.split("## files", 1)[1]
    parsed = CheckpointImage()
    parsed.files, parsed.categories, parsed.file_count = _parse_files(files_text)
    if not separator:
        parsed.parse_warnings.append("artifact inventory did not contain dump.log markers")
        return parsed
    log_text, _, summary_text = log_text.partition("## summary")
    parsed.dump_log = _parse_dump_log(log_text, parsed)
    _apply_summary(parsed, summary_text)
    if not parsed.dump_log and parsed.pagemap_count == 0:
        parsed.parse_warnings.append("no dump.log phase markers matched")
    elif parsed.dumping_finished_seconds is None:
        parsed.parse_warnings.append("dump.log markers did not include Dumping finished")
    return parsed


def file_category(path: str) -> str:
    """Groups CRIU image files by kind. `pages-1.img` and `pages-2.img` share `pages`."""
    name = path.rsplit("/", 1)[-1]
    if name in _NAMED_FILES:
        return name
    stem = name[:-4] if name.endswith(".img") else name
    prefix: list[str] = []
    for char in stem:
        if char == "-" and prefix:
            break
        if not char.isalpha():
            break
        prefix.append(char)
    return "".join(prefix) or "other"


def _parse_files(text: str) -> tuple[list[ArtifactFile], list[ImageCategory], int]:
    totals: dict[str, list[int | None]] = defaultdict(lambda: [0, 0, 0])
    large: list[ArtifactFile] = []
    count = 0
    for line in text.splitlines():
        parsed = _parse_file_line(line)
        if parsed is None:
            continue
        size, allocated, path = parsed
        count += 1
        bucket = totals[file_category(path)]
        bucket[0] = int(bucket[0] or 0) + size
        bucket[1] = int(bucket[1] or 0) + 1
        if allocated is None:
            bucket[2] = None
        elif bucket[2] is not None:
            bucket[2] = int(bucket[2]) + allocated
        if size >= _LARGE_FILE_BYTES:
            large.append(ArtifactFile(path=path, bytes=size, allocated_bytes=allocated))
    large.sort(key=lambda item: item.bytes, reverse=True)
    categories = [
        ImageCategory(
            name=name,
            bytes=int(size_count[0] or 0),
            file_count=int(size_count[1] or 0),
            allocated_bytes=None if size_count[2] is None else int(size_count[2]),
        )
        for name, size_count in totals.items()
    ]
    categories.sort(key=lambda item: item.bytes, reverse=True)
    return large[:_MAX_LARGE_FILES], categories, count


def _parse_file_line(line: str) -> tuple[int, int | None, str] | None:
    parts = line.strip().split("\t")
    if len(parts) == 3 and parts[0].isdigit() and parts[1].isdigit() and parts[2]:
        return int(parts[0]), int(parts[1]) * _BLOCK_SIZE, parts[2].removeprefix("./")
    if len(parts) == 2 and parts[0].isdigit() and parts[1]:
        return int(parts[0]), None, parts[1].removeprefix("./")
    return None


def _parse_dump_log(text: str, parsed: CheckpointImage) -> list[DumpLogEvent]:
    events: list[DumpLogEvent] = []
    for line in text.splitlines():
        match = _DUMP_LINE.match(line.strip())
        if match is None:
            continue
        events.append(
            DumpLogEvent(
                offset_seconds=float(match.group("offset")),
                message=match.group("message"),
                path=match.group("path").removeprefix("./"),
            )
        )
    return events


def _apply_summary(parsed: CheckpointImage, text: str) -> None:
    for line in text.splitlines():
        stripped = line.strip()
        pagemap = _PAGEMAP_SUMMARY.match(stripped)
        if pagemap is not None:
            parsed.pagemap_count = int(pagemap.group("count"))
            parsed.pagemap_pages = int(pagemap.group("pages"))
            parsed.pagemap_holes = int(pagemap.group("holes"))
            parsed.pagemap_first_seconds = _optional_seconds(pagemap.group("first"))
            parsed.pagemap_last_seconds = _optional_seconds(pagemap.group("last"))
            continue
        threads = _THREAD_SUMMARY.match(stripped)
        if threads is not None:
            parsed.thread_core_count = int(threads.group("count"))
            parsed.thread_core_first_seconds = _optional_seconds(threads.group("first"))
            parsed.thread_core_last_seconds = _optional_seconds(threads.group("last"))


def _optional_seconds(text: str) -> float | None:
    return float(text) if text else None
