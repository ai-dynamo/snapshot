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
    fd = 1
    directory = os.environ.get("STARTUP_SPAN_DIRECTORY")
    if directory:
        identity = (payload["pid"], payload["tid"])
        if getattr(_local, "log_identity", None) != identity:
            _local.log_file = open(
                f"{directory}/{identity[0]}-{identity[1]}-{next(_files)}.jsonl",
                "xb",
                buffering=0,
            )
            _local.log_identity = identity
        fd = _local.log_file.fileno()
    os.write(
        fd,
        ("STARTUP_SPAN " + json.dumps(payload, separators=(",", ":")) + "\n").encode(),
    )


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
