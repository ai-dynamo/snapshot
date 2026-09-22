# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Reads and writes raw `RunResult` JSON files.

One file per run, one directory per invocation
(`benchmarks/results/<YYYYMMDD>-<git-sha7>-<random-suffix>/<model-label-slug>-<mode>-<run-id>.json`),
so a sweep's raw output is reviewable as a unit. Each invocation gets its own
directory (the random suffix guarantees this even for two invocations on the
same day against the same commit, e.g. rerunning a single model after a fix)
rather than reusing an existing one, so a later run can never silently
overwrite an earlier one's result files. The filename itself includes
`run_id` for the same reason at the single-file level: the label slug alone
is not collision-free (`"Foo Bar"` and `"Foo-Bar"` both normalize to
`foo-bar`), and `run_id` is what actually guarantees a unique path.
`benchmarks/results/` is gitignored -- these are local, ad-hoc,
user-generated artifacts, not the source of truth for anything published.
"""

from __future__ import annotations

import datetime
import json
import re
import uuid
from pathlib import Path

from snapshot_benchmarks.schema import RunResult

DEFAULT_RESULTS_ROOT = Path(__file__).resolve().parents[1] / "results"


def _slug(label: str) -> str:
    return re.sub(r"[^a-z0-9]+", "-", label.lower()).strip("-")


def invocation_dir(root: Path, *, git_sha: str | None) -> Path:
    date = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%d")
    sha = git_sha or "nogit"
    directory = root / f"{date}-{sha}-{uuid.uuid4().hex[:8]}"
    directory.mkdir(parents=True)
    return directory


def write_result(result: RunResult, directory: Path) -> Path:
    path = directory / f"{_slug(result.model.label)}-{result.mode}-{result.run_id}.json"
    if path.exists():
        # `run_id` is generated per run (see `run.py`'s `run_benchmark`), so
        # this should be unreachable in practice -- but a silent overwrite
        # here would be exactly the failure mode this filename scheme exists
        # to prevent, so a same-run_id collision must be a loud error, not a
        # quiet clobber.
        raise FileExistsError(f"refusing to overwrite existing result file: {path}")
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
