# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

import contextlib
import json
import os
import time
import uuid
from pathlib import Path

RUN_TOKEN = uuid.uuid4().hex


@contextlib.contextmanager
def phase(name):
    path = (
        Path(os.environ.get("SNAPSHOT_CONTROL_DIR", "/snapshot-control"))
        / "phase-events.jsonl"
    )

    def emit(event, **extra):
        record = dict(
            phase=name,
            event=event,
            wall_ns=time.time_ns(),
            monotonic_ns=time.monotonic_ns(),
            pid=os.getpid(),
            checkpoint_token=RUN_TOKEN,
            **extra,
        )
        with path.open("a") as stream:
            stream.write(json.dumps(record) + "\n")
        print("CR_PHASE " + json.dumps(record), flush=True)

    start = time.monotonic_ns()
    emit("begin")
    try:
        yield
    except BaseException:
        emit("end", success=False, elapsed_ns=time.monotonic_ns() - start)
        raise
    else:
        emit("end", success=True, elapsed_ns=time.monotonic_ns() - start)
