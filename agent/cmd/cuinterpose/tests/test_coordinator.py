# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

import os
import socket
import struct
import subprocess
import threading
import time
from pathlib import Path

CUINTERPOSER_MAGIC = 0x44564D4D
CUINTERPOSER_VERSION = 2
CUINTERPOSER_IDENTIFY = 1
CUINTERPOSER_INSPECT = 2
CUINTERPOSER_PREPARE_MULTICAST = 3
CUINTERPOSER_PREPARE = 4
HEADER = struct.Struct("@IHHiIQ33s96s16s16sI64s")


def _receive_all(connection: socket.socket, size: int) -> bytes:
    chunks = []
    while size:
        chunk = connection.recv(size)
        if not chunk:
            raise RuntimeError("coordinator disconnected before sending a header")
        chunks.append(chunk)
        size -= len(chunk)
    return b"".join(chunks)


def _participant(
    endpoint: Path,
    participant_id: bytes,
    multicast_barrier: threading.Barrier,
) -> None:
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as server:
        server.bind(str(endpoint))
        server.listen()
        for _ in range(4):
            connection, _ = server.accept()
            with connection:
                request = HEADER.unpack(_receive_all(connection, HEADER.size))
                operation = request[2]
                if operation == CUINTERPOSER_PREPARE_MULTICAST:
                    multicast_barrier.wait(timeout=5)
                assert operation in {
                    CUINTERPOSER_IDENTIFY,
                    CUINTERPOSER_INSPECT,
                    CUINTERPOSER_PREPARE_MULTICAST,
                    CUINTERPOSER_PREPARE,
                }
                response_id = participant_id if operation == CUINTERPOSER_IDENTIFY else request[6]
                connection.sendall(
                    HEADER.pack(
                        CUINTERPOSER_MAGIC,
                        CUINTERPOSER_VERSION,
                        operation,
                        0,
                        0,
                        0,
                        response_id,
                        b"",
                        b"",
                        b"",
                        0,
                        b"",
                    )
                )


def test_prepare_dispatches_multicast_teardown_concurrently(tmp_path: Path) -> None:
    interposer_dir = Path(__file__).resolve().parents[1]
    build_dir = tmp_path / "build"
    coordinator = build_dir / "cuinterposer-coordinator"
    build_dir.mkdir()
    subprocess.run(
        [
            os.environ.get("CC", "cc"),
            "-std=gnu11",
            "-O2",
            "-Wall",
            "-Wextra",
            "-Werror",
            "-pthread",
            "-o",
            str(coordinator),
            str(interposer_dir / "coordinator.c"),
            str(interposer_dir / "util.c"),
        ],
        check=True,
        capture_output=True,
        text=True,
    )

    control_dir = tmp_path / "control"
    checkpoint_dir = tmp_path / "checkpoint"
    control_dir.mkdir()
    checkpoint_dir.mkdir()
    process_ids = (1001, 1002)
    barrier = threading.Barrier(len(process_ids))
    threads = []
    for index, process_id in enumerate(process_ids):
        thread = threading.Thread(
            target=_participant,
            args=(
                control_dir / f"cuinterposer-{process_id}.sock",
                f"{index + 1:032x}".encode(),
                barrier,
            ),
        )
        thread.start()
        threads.append(thread)

    deadline = time.monotonic() + 5
    while not all(
        (control_dir / f"cuinterposer-{process_id}.sock").is_socket()
        for process_id in process_ids
    ):
        if time.monotonic() >= deadline:
            raise TimeoutError("fake participant sockets did not become ready")
        time.sleep(0.01)

    command = [
        str(coordinator),
        "--prepare",
        "--proc-root",
        "",
        "--checkpoint-dir",
        str(checkpoint_dir),
    ]
    for process_id in process_ids:
        command.extend(["--process", str(process_id), str(process_id)])
    environment = os.environ.copy()
    environment["DYN_SNAPSHOT_CONTROL_DIR"] = str(control_dir)
    subprocess.run(command, env=environment, check=True, timeout=10)

    for thread in threads:
        thread.join(timeout=10)
        assert not thread.is_alive()
    assert (checkpoint_dir / "cuinterposer.state").is_file()
