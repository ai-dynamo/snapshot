# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Nested host-wall-clock startup spans. No CUDA synchronization is introduced."""

import functools
import itertools
import json
import os
import threading
import time

_local = threading.local()
_ids = itertools.count()
_files = itertools.count()


def emit(event, **fields):
    payload = dict(
        event=event,
        pid=os.getpid(),
        tid=threading.get_ident(),
        rank=os.environ.get("RANK", os.environ.get("OMPI_COMM_WORLD_RANK")),
        wall_ns=time.time_ns(),
        monotonic_ns=time.monotonic_ns(),
        **fields,
    )
    data = (
        "STARTUP_SPAN " + json.dumps(payload, separators=(",", ":")) + "\n"
    ).encode()
    directory = os.environ.get("STARTUP_SPAN_DIRECTORY")
    if not directory:
        os.write(1, data)
        return
    identity = (payload["pid"], payload["tid"])
    if getattr(_local, "log_identity", None) != identity:
        _local.log_path = (
            f"{directory}/{identity[0]}-{identity[1]}-{next(_files)}.jsonl"
        )
        _local.log_identity = identity
    # Open descriptors become checkpoint state. Close each write so later
    # restores can append without invalidating CRIU's saved file sizes.
    with open(_local.log_path, "ab", buffering=0) as stream:
        stream.write(data)


def phase(name):
    def decorate(fn):
        @functools.wraps(fn)
        def measured(*args, **kwargs):
            stack = getattr(_local, "stack", None)
            if stack is None:
                stack = _local.stack = []
            span = f"{os.getpid()}:{next(_ids)}"
            parent = stack[-1] if stack else None
            start = time.monotonic_ns()
            emit("begin", name=name, span=span, parent=parent)
            stack.append(span)
            ok = False
            try:
                result = fn(*args, **kwargs)
                ok = True
                return result
            finally:
                stack.pop()
                emit(
                    "end",
                    name=name,
                    span=span,
                    parent=parent,
                    elapsed_ns=time.monotonic_ns() - start,
                    success=ok,
                )

        return measured

    return decorate
