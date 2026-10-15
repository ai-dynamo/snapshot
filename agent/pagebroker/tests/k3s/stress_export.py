#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Stream bounded, regular evidence files from a read-only results volume."""

import argparse
from pathlib import Path
import signal
import sys
import tarfile
import time

from stress_config import RunSpec
from stress_storage import TERMINAL, EvidenceLimitError, file_limit, files, safe_path


def inventory(root, options):
    if Path(root).is_symlink():
        raise ValueError("evidence root cannot be a link")
    ordinary = terminal = 0
    result = []
    for name, path in files(Path(root), options.max_files):
        size = path.stat().st_size
        if size > file_limit(name, options):
            raise EvidenceLimitError("export file exceeds its configured limit")
        if name in TERMINAL:
            terminal += size
        else:
            ordinary += size
        if ordinary > options.ordinary_bytes or terminal > options.terminal_bytes:
            raise EvidenceLimitError("export exceeds its evidence budget")
        result.append((name, path))
    return sorted(result)


def export(root, options, output):
    with tarfile.open(fileobj=output, mode="w|") as archive:
        for name, path in inventory(root, options):
            archive.add(path, arcname="results/" + name, recursive=False)


def extract(archive_path, destination, options):
    destination = Path(destination)
    destination.mkdir()
    seen, total = set(), 0
    archive_limit = options.ordinary_bytes + options.terminal_bytes + options.max_files * 2048 + 10240
    if Path(archive_path).stat().st_size > archive_limit:
        raise EvidenceLimitError("export archive exceeds its transfer limit")
    with tarfile.open(archive_path, mode="r|") as archive:
        for member in archive:
            name = member.name.removeprefix("results/")
            if (member.name != "results/" + name or name in seen or not member.isfile()
                    or member.sparse is not None or member.size < 0):
                raise ValueError("unexpected, linked, sparse or duplicate archive entry")
            path = safe_path(destination, name)
            if member.size > file_limit(name, options) or len(seen) >= options.max_files:
                raise EvidenceLimitError("archive entry exceeds its declared limit")
            seen.add(name)
            total += member.size
            if total > options.ordinary_bytes + options.terminal_bytes:
                raise EvidenceLimitError("archive payload exceeds its evidence budget")
            path.parent.mkdir(parents=True, exist_ok=True)
            with archive.extractfile(member) as source, path.open("xb") as output:
                remaining = member.size
                while remaining:
                    block = source.read(min(1024**2, remaining))
                    if not block:
                        raise ValueError("truncated evidence archive")
                    output.write(block)
                    remaining -= len(block)
    inventory(destination, options)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--spec", required=True)
    parser.add_argument("command", choices=("serve", "export"))
    args = parser.parse_args()
    spec = RunSpec.load(args.spec)
    if args.command == "export":
        export(spec.results, spec.evidence, sys.stdout.buffer)
        return
    stop = False
    def interrupted(_signum, _frame):
        nonlocal stop
        stop = True
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    deadline = time.monotonic() + 600
    while not stop and time.monotonic() < deadline:
        time.sleep(0.1)


if __name__ == "__main__":
    main()
