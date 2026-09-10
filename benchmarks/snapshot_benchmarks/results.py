# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Reads and writes raw `RunResult` JSON files.

One file per run, one directory per invocation
(`benchmarks/results/<YYYYMMDD>-<git-sha7>/<model-label-slug>-<mode>.json`),
so a sweep's raw output is reviewable as a unit and diffable across
invocations. `benchmarks/results/` is gitignored -- these are local, ad-hoc,
user-generated artifacts, not the source of truth for anything published.
"""

from __future__ import annotations

import datetime
import json
import re
from pathlib import Path

from snapshot_benchmarks.schema import RunResult

DEFAULT_RESULTS_ROOT = Path(__file__).resolve().parents[1] / "results"


def _slug(label: str) -> str:
    return re.sub(r"[^a-z0-9]+", "-", label.lower()).strip("-")


def invocation_dir(root: Path, *, git_sha: str | None) -> Path:
    date = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%d")
    sha = git_sha or "nogit"
    directory = root / f"{date}-{sha}"
    directory.mkdir(parents=True, exist_ok=True)
    return directory


def write_result(result: RunResult, directory: Path) -> Path:
    path = directory / f"{_slug(result.model.label)}-{result.mode}.json"
    path.write_text(json.dumps(result.to_json_dict(), indent=2, sort_keys=False) + "\n")
    return path


def load_results(directory: Path) -> list[dict]:
    """Loads every `*.json` file directly under `directory` as a raw dict
    (not re-hydrated into `RunResult` dataclasses -- `report.py` only needs
    read access to the fields it renders, and round-tripping back into
    dataclasses would require a second, deserializing code path with no
    consumer)."""
    results = []
    for path in sorted(directory.glob("*.json")):
        results.append(json.loads(path.read_text()))
    return results
