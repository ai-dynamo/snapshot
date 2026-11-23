#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
"""Attribute the multicast groups in a cuinterpose.state to their owning process.

``cuinterpose-coordinator --prepare`` writes ``cuinterpose.state`` into the
checkpoint directory: a msgpack envelope (``{version, body}``) whose body maps
each namespace PID to the list of CUDA records persisted for that process
(see ``agent/cmd/cuinterpose/rust/protocol/src/record.rs``). The file carries
only the participant/allocation topology, not device memory bytes.

When a restore fails inside ``RestoreMulticastDevices`` — for example
ai-dynamo/snapshot#510, where driver 580 returns ``CUDA_ERROR_INVALID_DEVICE``
from ``cuMulticastAddDevice`` after a freeze/restore cycle — the decisive
triage step is knowing which multicast groups each TP worker holds, with their
size and device count, so the groups can be eliminated or accounted for.
Reading that out of the raw msgpack stream by hand is tedious; this tool does
it directly.

For a default vLLM TP2 checkpoint it surfaces the three groups created by:

* ``CustomAllreduce.__init__`` via ``torch._symmetric_memory`` (devices=2,
  created unconditionally),
* ``VLLM_ALLREDUCE_USE_SYMM_MEM``,
* ``NCCL_NVLS_ENABLE``.

The state format is internal to cuinterpose and may change between releases.
This tool reads the ``Record::Multicast`` variant
(``{allocation, properties{devices, size, handle_types, flags}, ...}``) on a
best-effort basis: unknown record variants are skipped, missing optional
fields are reported as ``None`` (``size``/``devices`` default to 0), and
traversal is depth-limited so a malformed or hostile file cannot exhaust the
Python stack. A file that cannot be decoded at all produces a clean error.

Usage::

    python3 hack/dump-multicast-groups.py CHECKPOINT_DIR/cuinterpose.state
    python3 hack/dump-multicast-groups.py CHECKPOINT_DIR/cuinterpose.state --json

Requires the ``msgpack`` Python package.
"""

from __future__ import annotations

import argparse
import json
import sys

import msgpack

# Real coordinator output is shallow (the Rust decoder caps nesting at 32).
# Bound traversal well above that so malformed input cannot hit Python's
# recursion limit, while never truncating a legitimate manifest.
_MAX_DEPTH = 256
_SUPPORTED_VERSION = 1


def _as_int(value):
    """Coerce a numeric field to int, tolerating null/non-numeric input."""
    if isinstance(value, bool):
        return int(value)
    if isinstance(value, int):
        return value
    return 0


def iter_participants(path):
    """Yield ``(pid, records)`` for each participant in the state file.

    The coordinator wraps the manifest in a ``{version, body}`` envelope; older
    or hand-built dumps may be the bare manifest map. Accept both, and warn on
    an unexpected envelope version rather than silently misreading it.
    """
    warned = False
    decoded = 0
    with open(path, "rb") as fh:
        unpacker = msgpack.Unpacker(fh, raw=False, strict_map_key=False)
        for obj in unpacker:
            decoded += 1
            if isinstance(obj, dict) and "body" in obj:
                version = obj.get("version")
                if not warned and version != _SUPPORTED_VERSION:
                    print(
                        f"warning: envelope version {version!r} != "
                        f"{_SUPPORTED_VERSION}; output may be unreliable",
                        file=sys.stderr,
                    )
                    warned = True
                body = obj["body"]
            else:
                body = obj
            if not isinstance(body, dict):
                continue
            for pid, records in body.items():
                yield pid, records
    if decoded == 0:
        raise ValueError("no msgpack objects decoded (empty or truncated file)")


def multicast_groups(records):
    """Return the multicast groups found in one participant's record list."""
    groups = []

    def walk(node, depth=0):
        if depth > _MAX_DEPTH:
            return
        if isinstance(node, dict):
            mc = node.get("multicast")
            if isinstance(mc, dict) and isinstance(mc.get("properties"), dict):
                props = mc["properties"]
                alloc = mc.get("allocation")
                if not isinstance(alloc, dict):
                    alloc = {}
                groups.append(
                    {
                        "size_bytes": _as_int(props.get("size")),
                        "devices": _as_int(props.get("devices")),
                        "creator_pid": alloc.get("creator_pid"),
                        "handle_types": props.get("handle_types"),
                        "flags": props.get("flags"),
                    }
                )
            for value in node.values():
                walk(value, depth + 1)
        elif isinstance(node, list):
            for value in node:
                walk(value, depth + 1)

    walk(records)
    return groups


def collect(path):
    per_process = {}
    for pid, records in iter_participants(path):
        groups = multicast_groups(records)
        if groups:
            per_process[pid] = groups
    return per_process


def main(argv=None):
    parser = argparse.ArgumentParser(
        description="List per-process multicast groups in a cuinterpose.state."
    )
    parser.add_argument("state", help="path to CHECKPOINT_DIR/cuinterpose.state")
    parser.add_argument(
        "--json", action="store_true", help="emit machine-readable JSON"
    )
    args = parser.parse_args(argv)

    try:
        per_process = collect(args.state)
    except (OSError, ValueError, TypeError, RecursionError) as exc:
        print(f"error: cannot read {args.state}: {exc}", file=sys.stderr)
        return 1

    if args.json:
        print(json.dumps(per_process, indent=2, sort_keys=True, default=str))
        return 0

    total_bytes = 0
    for pid in sorted(per_process, key=lambda p: (str(type(p)), p)):
        groups = per_process[pid]
        print(f"pid={pid} multicast_groups={len(groups)}")
        for g in groups:
            mib = g["size_bytes"] / 1048576.0
            print(
                f"  size={mib:.1f}MiB devices={g['devices']} "
                f"creator_pid={g['creator_pid']}"
            )
            total_bytes += g["size_bytes"]
    print(
        f"total: processes={len(per_process)} "
        f"multicast_size={total_bytes / 1048576.0:.1f}MiB"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
