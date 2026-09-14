#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Exchange sealed tickets with the unmodified, pinned C stack reader/writer."""

import os
from pathlib import Path
import subprocess
import tempfile


REFERENCE = "21008b50b93a9879a805665e331e777bb93abf49"
PREFIX = "agent/cmd/cuinterpose/"
SOURCES = (
    "posix.c", "posix.h", "protocol.c", "protocol.h",
    "util/io.c", "util/io.h", "util/id.c", "util/id.h",
    "util/cleanup.h", "util/misc.h",
)


def main():
    workspace = Path(__file__).resolve().parents[2]
    repo = workspace.parents[3]
    env = os.environ.copy()
    env.pop("LD_PRELOAD", None)
    env.setdefault("CARGO_TARGET_X86_64_UNKNOWN_LINUX_GNU_LINKER", "/usr/bin/gcc")
    # Fail rather than quietly using a different reader when the draft commit
    # is absent. The runner never fetches or modifies the original PR branches.
    subprocess.run(["git", "cat-file", "-e", f"{REFERENCE}^{{commit}}"], cwd=repo, check=True)
    with tempfile.TemporaryDirectory(prefix="cuinterpose-ticket-") as directory:
        build = Path(directory)
        for source in SOURCES:
            output = build / source
            output.parent.mkdir(parents=True, exist_ok=True)
            output.write_bytes(subprocess.check_output(
                ["git", "show", f"{REFERENCE}:{PREFIX}{source}"], cwd=repo,
            ))
        helper = build / "ticket-interop"
        # Function sections discard unrelated peer-request/random-ID functions;
        # only the exact ticket reader/writer and their helpers enter the binary.
        subprocess.run([
            "/usr/bin/gcc", "-std=gnu11", "-O2", "-Wall", "-Wextra", "-Werror",
            "-ffunction-sections", "-fdata-sections", "-Wl,--gc-sections",
            "-I", str(build), str(Path(__file__).with_suffix(".c")),
            str(build / "posix.c"), str(build / "protocol.c"),
            str(build / "util/io.c"), str(build / "util/id.c"),
            "-o", str(helper),
        ], env=env, check=True)
        env["CUINTERPOSE_C_TICKET_HELPER"] = str(helper)
        subprocess.run([
            "cargo", "test", "--target", "x86_64-unknown-linux-gnu",
            "-p", "cuinterpose-core", "ticket::tests::c_v2_sealed_ticket_interoperability",
            "--", "--ignored", "--exact",
        ], cwd=workspace, env=env, timeout=120, check=True)
    print(f"PASS Rust → C → Rust sealed unicast ticket, C reference {REFERENCE}")


if __name__ == "__main__":
    main()
