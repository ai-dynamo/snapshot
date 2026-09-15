# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Build expiring dashboard previews from untrusted benchmark artifacts."""

from __future__ import annotations

import argparse
import json
import os
import re
import shutil
import tempfile
from collections.abc import Mapping, Sequence
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any
from urllib.parse import urlparse

from snapshot_e2e.benchmark_history import (
    ResultValidationError,
    load_history,
    load_result,
    result_identity,
    store_results,
)


PREVIEW_FORMAT_VERSION = 1
DEFAULT_RETENTION_DAYS = 14
MAX_RESULT_FILES = 32
MAX_RESULT_BYTES = 1024 * 1024
PREVIEW_KEY_PATTERN = re.compile(r"(?:pr|run)-[1-9][0-9]*\Z")


class PreviewValidationError(ValueError):
    """A preview input or destination is unsafe or inconsistent."""


def prepare_preview(
    *,
    results_dir: Path,
    history_dir: Path,
    previews_dir: Path,
    key: str,
    run_id: str,
    run_attempt: int,
    source_event: str,
    source_branch: str,
    source_commit: str,
    source_run_url: str,
    pull_request: int | None = None,
    generated_at: datetime | None = None,
    retention_days: int = DEFAULT_RETENTION_DAYS,
) -> dict[str, Any]:
    """Validate one run, overlay it on history, and publish an expiring index."""
    _validate_preview_key(key)
    if not run_id.strip():
        raise PreviewValidationError("run_id must be non-empty")
    if run_attempt < 1:
        raise PreviewValidationError("run_attempt must be positive")
    if retention_days < 1:
        raise PreviewValidationError("retention_days must be positive")
    if pull_request is not None and pull_request < 1:
        raise PreviewValidationError("pull_request must be positive")
    for value, name in (
        (source_event, "source_event"),
        (source_branch, "source_branch"),
        (source_commit, "source_commit"),
    ):
        if not value.strip():
            raise PreviewValidationError(f"{name} must be non-empty")
    _validate_http_url(source_run_url)

    generated = _utc(generated_at or datetime.now(timezone.utc))
    expires = generated + timedelta(days=retention_days)
    results = _load_current_results(
        results_dir,
        run_id=run_id,
        run_attempt=run_attempt,
    )
    history_count = len(load_history(history_dir))
    stored = store_results(history_dir, results)
    metadata: dict[str, Any] = {
        "formatVersion": PREVIEW_FORMAT_VERSION,
        "key": key,
        "generatedAt": _timestamp(generated),
        "expiresAt": _timestamp(expires),
        "historyRecordCount": history_count,
        "previewRecordCount": len(results),
        "combinedRecordCount": len(stored),
        "source": {
            "event": source_event,
            "branch": source_branch,
            "commit": source_commit,
            "runId": run_id,
            "runAttempt": run_attempt,
            "runUrl": source_run_url,
            "pullRequest": pull_request,
        },
    }

    previews_dir.mkdir(parents=True, exist_ok=True)
    if previews_dir.is_symlink():
        raise PreviewValidationError(f"preview root may not be a symlink: {previews_dir}")
    with tempfile.TemporaryDirectory(prefix=f".{key}-", dir=previews_dir) as temporary:
        staged = Path(temporary) / key
        shutil.copytree(history_dir / "index", staged / "index")
        _write_json(staged / "preview.json", metadata)
        target = previews_dir / key
        if target.exists() or target.is_symlink():
            _remove_preview(previews_dir, target)
        os.replace(staged, target)

    removed = prune_expired_previews(previews_dir, now=generated, keep_key=key)
    metadata["removedExpiredPreviews"] = removed
    return metadata


def prune_expired_previews(
    previews_dir: Path,
    *,
    now: datetime | None = None,
    keep_key: str | None = None,
) -> list[str]:
    """Remove only well-formed, expired preview directories."""
    if not previews_dir.exists():
        return []
    if previews_dir.is_symlink() or not previews_dir.is_dir():
        raise PreviewValidationError(f"invalid preview root: {previews_dir}")
    current = _utc(now or datetime.now(timezone.utc))
    removed: list[str] = []
    for entry in sorted(previews_dir.iterdir()):
        if entry.name == keep_key or not PREVIEW_KEY_PATTERN.fullmatch(entry.name):
            continue
        if entry.is_symlink() or not entry.is_dir():
            continue
        try:
            value = json.loads((entry / "preview.json").read_text(encoding="utf-8"))
            if not isinstance(value, Mapping) or value.get("key") != entry.name:
                continue
            expires = _parse_timestamp(value.get("expiresAt"), "expiresAt")
        except (OSError, json.JSONDecodeError, PreviewValidationError):
            continue
        if expires <= current:
            _remove_preview(previews_dir, entry)
            removed.append(entry.name)
    return removed


def _load_current_results(
    results_dir: Path,
    *,
    run_id: str,
    run_attempt: int,
) -> list[dict[str, Any]]:
    if results_dir.is_symlink() or not results_dir.is_dir():
        raise PreviewValidationError(f"results directory is missing or unsafe: {results_dir}")
    entries = sorted(results_dir.iterdir())
    if not entries:
        raise PreviewValidationError("preview artifact contains no benchmark results")
    if len(entries) > MAX_RESULT_FILES:
        raise PreviewValidationError(
            f"preview artifact contains more than {MAX_RESULT_FILES} files"
        )

    results: list[dict[str, Any]] = []
    identities: set[tuple[str, str, str, str, int]] = set()
    for path in entries:
        if path.is_symlink() or not path.is_file() or path.suffix != ".json":
            raise PreviewValidationError(f"unexpected preview artifact entry: {path.name}")
        if path.stat().st_size > MAX_RESULT_BYTES:
            raise PreviewValidationError(
                f"preview result exceeds {MAX_RESULT_BYTES} bytes: {path.name}"
            )
        try:
            result = load_result(path)
        except ResultValidationError as exc:
            raise PreviewValidationError(str(exc)) from exc
        identity = result["identity"]
        if str(identity["runId"]) != run_id or int(identity["runAttempt"]) != run_attempt:
            raise PreviewValidationError(
                f"{path.name} does not belong to workflow run {run_id}-{run_attempt}"
            )
        canonical_identity = result_identity(result)
        if canonical_identity in identities:
            raise PreviewValidationError(
                f"preview artifact contains duplicate identity {canonical_identity!r}"
            )
        identities.add(canonical_identity)
        results.append(result)
    return results


def _remove_preview(previews_dir: Path, target: Path) -> None:
    _validate_preview_key(target.name)
    if target.parent.resolve() != previews_dir.resolve():
        raise PreviewValidationError(f"preview path escapes its root: {target}")
    if target.is_symlink():
        raise PreviewValidationError(f"preview destination may not be a symlink: {target}")
    shutil.rmtree(target)


def _validate_preview_key(value: str) -> str:
    if not PREVIEW_KEY_PATTERN.fullmatch(value):
        raise PreviewValidationError(f"invalid preview key: {value!r}")
    return value


def _validate_http_url(value: str) -> None:
    parsed = urlparse(value)
    if parsed.scheme not in {"http", "https"} or not parsed.netloc:
        raise PreviewValidationError("source_run_url must be an absolute HTTP(S) URL")


def _parse_timestamp(value: object, name: str) -> datetime:
    if not isinstance(value, str) or not value:
        raise PreviewValidationError(f"{name} must be an RFC3339 timestamp")
    try:
        parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError as exc:
        raise PreviewValidationError(f"{name} must be an RFC3339 timestamp") from exc
    if parsed.tzinfo is None:
        raise PreviewValidationError(f"{name} must include a timezone")
    return parsed.astimezone(timezone.utc)


def _utc(value: datetime) -> datetime:
    if value.tzinfo is None:
        raise PreviewValidationError("timestamps must include a timezone")
    return value.astimezone(timezone.utc)


def _timestamp(value: datetime) -> str:
    return _utc(value).isoformat(timespec="milliseconds").replace("+00:00", "Z")


def _write_json(path: Path, value: object) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n", encoding="utf-8")


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--results-dir", type=Path, required=True)
    parser.add_argument("--history-dir", type=Path, required=True)
    parser.add_argument("--previews-dir", type=Path, required=True)
    parser.add_argument("--key", required=True)
    parser.add_argument("--run-id", required=True)
    parser.add_argument("--run-attempt", type=int, required=True)
    parser.add_argument("--source-event", required=True)
    parser.add_argument("--source-branch", required=True)
    parser.add_argument("--source-commit", required=True)
    parser.add_argument("--source-run-url", required=True)
    parser.add_argument("--pull-request", type=int)
    parser.add_argument("--retention-days", type=int, default=DEFAULT_RETENTION_DAYS)
    args = parser.parse_args(argv)
    metadata = prepare_preview(
        results_dir=args.results_dir,
        history_dir=args.history_dir,
        previews_dir=args.previews_dir,
        key=args.key,
        run_id=args.run_id,
        run_attempt=args.run_attempt,
        source_event=args.source_event,
        source_branch=args.source_branch,
        source_commit=args.source_commit,
        source_run_url=args.source_run_url,
        pull_request=args.pull_request,
        retention_days=args.retention_days,
    )
    print(json.dumps(metadata, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
