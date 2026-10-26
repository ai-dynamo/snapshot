# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Bounded, durable evidence writes and a strict in-Pod file manifest."""

from contextlib import contextmanager
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import stat
import tempfile


TERMINAL = {"summary.json", "supervisor.json", "resource-metadata.json", "resource-analysis.json", "manifest.json",
            "failure-client-tail.log", "cleanup-final.json"}
FIXED = TERMINAL | {"run-spec.json", "progress.json", "readiness.json", "readiness.log", "batches.jsonl",
                    "idle-windows.jsonl", "failure-client-tail.log", "cleanup-readiness.json", "cleanup.jsonl"}
VARIABLE = re.compile(r"(?:resources/[0-9]{6}\.jsonl|batches/[0-9]{6}/(?:client-report\.json|client\.log)|"
                      r"client-ids/[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12})\Z")


class EvidenceLimitError(RuntimeError):
    pass


def file_limit(name, options):
    if name.endswith("client-report.json"):
        return min(options.report_bytes, 1024**2)
    if name.endswith("client.log") or name == "readiness.log":
        return options.client_log_bytes
    if name.startswith("resources/"):
        return options.chunk_bytes
    if name.startswith("client-ids/"):
        return 64
    return {"manifest.json": 1024**2, "resource-analysis.json": 128 * 1024,
            "failure-client-tail.log": 4096}.get(name, 64 * 1024 if name in FIXED - {
                "batches.jsonl", "idle-windows.jsonl", "cleanup.jsonl"} else options.ordinary_bytes)


def encoded(value):
    return (json.dumps(value, allow_nan=False, separators=(",", ":")) + "\n").encode()


def strict_json(data):
    def pairs(items):
        result = {}
        for key, value in items:
            if key in result:
                raise ValueError("duplicate JSON key")
            result[key] = value
        return result
    def invalid(_value):
        raise ValueError("nonfinite JSON value")
    return json.loads(data, object_pairs_hook=pairs, parse_constant=invalid)


def safe_path(root, name):
    path = PurePosixPath(name)
    if (path.is_absolute() or str(path) != name or ".." in path.parts or "\\" in name
            or not (name in FIXED or VARIABLE.fullmatch(name))):
        raise ValueError(f"unexpected evidence path: {name}")
    target = Path(root)
    for component in path.parts:
        target /= component
        if target.is_symlink():
            raise ValueError("evidence links are forbidden")
    return target


def read_json(root, name, limit=64 * 1024):
    path = safe_path(root, name)
    with path.open("rb") as source:
        value = source.read(limit + 1)
    if len(value) > limit:
        raise EvidenceLimitError(f"{name} exceeds its JSON limit")
    return strict_json(value)


def files(root, maximum):
    count = 0
    def visit(directory):
        nonlocal count
        with os.scandir(directory) as entries:
            for entry in entries:
                count += 1
                if count > maximum:
                    raise EvidenceLimitError("evidence file/directory-count limit exceeded")
                path = Path(entry.path)
                relative = path.relative_to(root).as_posix()
                if entry.is_symlink():
                    raise ValueError("evidence links are forbidden")
                if entry.is_dir(follow_symlinks=False):
                    if not (relative in ("batches", "resources", "client-ids")
                            or re.fullmatch(r"batches/[0-9]{6}", relative)):
                        raise ValueError("unexpected evidence directory")
                    yield from visit(path)
                else:
                    safe_path(root, relative)
                    if not stat.S_ISREG(entry.stat(follow_symlinks=False).st_mode):
                        raise ValueError("evidence must contain only regular files")
                    yield relative, path
    yield from visit(root)


def digest(path, pulse=lambda: None):
    value = hashlib.sha256()
    with path.open("rb") as source:
        while block := source.read(1024**2):
            value.update(block)
            pulse()
    return value.hexdigest()


class EvidenceStore:
    def __init__(self, root, options):
        self.root, self.options = Path(root), options
        self.sizes = {}
        self.reserved = 0

    def used(self, terminal):
        return sum(size for name, size in self.sizes.items() if (name in TERMINAL) == terminal)

    def admit(self, name, size, temporary=False):
        safe_path(self.root, name)
        if size > file_limit(name, self.options):
            raise EvidenceLimitError(f"{name} exceeds its per-file limit")
        if name not in self.sizes and len(self.sizes) >= self.options.max_files:
            raise EvidenceLimitError("evidence file-count limit exceeded")
        terminal = name in TERMINAL
        previous = self.sizes.get(name, 0)
        maximum = self.options.terminal_bytes if terminal else self.options.ordinary_bytes
        reservation = 0 if terminal else self.reserved
        addition = size if temporary else size - previous
        if self.used(terminal) + reservation + addition > maximum:
            raise EvidenceLimitError("terminal evidence reserve exhausted" if terminal else "ordinary evidence budget exhausted")

    @contextmanager
    def reserve(self, amount):
        if self.used(False) + self.reserved + amount > self.options.ordinary_bytes:
            raise EvidenceLimitError("insufficient evidence capacity to admit another client")
        self.reserved += amount
        try:
            yield
        finally:
            self.reserved -= amount

    def register(self, name, limit):
        path = safe_path(self.root, name)
        info = path.lstat()
        if not stat.S_ISREG(info.st_mode) or info.st_size > limit:
            raise EvidenceLimitError(f"{name} is not a regular file within its size limit")
        self.admit(name, info.st_size)
        self.sizes[name] = info.st_size

    def write(self, name, value, exclusive=False):
        data = value if isinstance(value, bytes) else encoded(value)
        path = safe_path(self.root, name)
        if exclusive and path.exists():
            raise FileExistsError(name)
        self.admit(name, len(data), temporary=True)
        path.parent.mkdir(parents=True, exist_ok=True)
        with tempfile.NamedTemporaryFile(dir=path.parent, delete=False) as output:
            temporary = Path(output.name)
            try:
                output.write(data)
                output.flush()
                os.fsync(output.fileno())
                os.replace(temporary, path)
                directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
                try:
                    os.fsync(directory)
                finally:
                    os.close(directory)
            finally:
                temporary.unlink(missing_ok=True)
        self.sizes[name] = len(data)

    def append(self, name, value):
        data = value if isinstance(value, bytes) else encoded(value)
        path = safe_path(self.root, name)
        size = self.sizes.get(name, 0) + len(data)
        self.admit(name, size)
        path.parent.mkdir(parents=True, exist_ok=True)
        flags = os.O_WRONLY | os.O_CREAT | os.O_APPEND | os.O_NOFOLLOW
        with os.fdopen(os.open(path, flags, 0o600), "ab") as output:
            output.write(data)
            output.flush()
            os.fsync(output.fileno())
        self.sizes[name] = size

    def manifest(self, run_id, pulse=lambda: None):
        entries = []
        for name, path in files(self.root, self.options.max_files):
            if name == "manifest.json":
                continue
            self.register(name, file_limit(name, self.options))
            # The terminal summary carries the verdict, so its hash cannot include itself.
            if name != "summary.json":
                entries.append({"path": name, "size": path.stat().st_size, "sha256": digest(path, pulse)})
            pulse()
        value = {"schemaVersion": 1, "runId": run_id, "scope": "IN_POD", "files": sorted(entries, key=lambda item: item["path"])}
        if len(encoded(value)) > 1024**2:
            raise EvidenceLimitError("manifest exceeds 1 MiB")
        self.write("manifest.json", value)
        return value


def validate_manifest(root, spec, pulse=lambda: None):
    manifest = read_json(root, "manifest.json", 1024**2)
    if manifest.get("schemaVersion") != 1 or manifest.get("runId") != spec.run_id or manifest.get("scope") != "IN_POD":
        raise ValueError("manifest belongs to a different run or schema")
    entries = manifest["files"]
    if len(entries) > spec.evidence.max_files:
        raise EvidenceLimitError("manifest has too many entries")
    actual = dict(files(Path(root), spec.evidence.max_files))
    ordinary = sum(path.stat().st_size for name, path in actual.items() if name not in TERMINAL)
    terminal = sum(path.stat().st_size for name, path in actual.items() if name in TERMINAL)
    if any(path.stat().st_size > file_limit(name, spec.evidence) for name, path in actual.items()):
        raise EvidenceLimitError("evidence file exceeds its declared per-file limit")
    if ordinary > spec.evidence.ordinary_bytes or terminal > spec.evidence.terminal_bytes:
        raise EvidenceLimitError("evidence exceeds its writer budget or terminal reserve")
    seen = set()
    for item in entries:
        name = item["path"]
        path = safe_path(root, name)
        if name in seen or name in ("manifest.json", "summary.json"):
            raise ValueError("duplicate or mutable manifest entry")
        seen.add(name)
        if path.stat().st_size != item["size"] or digest(path, pulse) != item["sha256"]:
            raise ValueError(f"evidence size/hash mismatch: {name}")
    if seen != set(actual) - {"manifest.json", "summary.json"}:
        raise ValueError("manifest does not cover the exact evidence file set")
    return {"fileCount": len(actual), "ordinaryBytes": ordinary, "terminalBytes": terminal,
            "diagnosticReserveBytes": spec.evidence.diagnostic_bytes}
