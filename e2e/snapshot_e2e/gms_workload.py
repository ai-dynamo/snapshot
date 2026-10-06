# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Small real-CUDA GMS worker, delivered as a ConfigMap script to test pods."""

from __future__ import annotations

import argparse
import ctypes
import hashlib
import json
import os
import re
import subprocess
import sys
import tempfile
import time
from pathlib import Path

HANDSHAKE_DIR = "/gms-control"
SCRIPT_PATH = "/gms-fixture/gms_workload.py"
RESTORED_RESULT = "/tmp/gms-e2e-restored.json"
SERVER_GATE = "server-approved"
WORKER_GATE = "allocation-approved"


def wait_for_gpu_approval(handshake_dir: Path, gate: str) -> None:
    """Wait without importing CUDA until the runner proves physical isolation.

    The server is checked first, before its native-sidecar startup probe admits
    the worker/saver. The worker is checked again once all three are running.
    Approval carries the exact UUID, not merely a file-exists signal.
    """
    output = subprocess.check_output(
        ["nvidia-smi", "--query-gpu=uuid", "--format=csv,noheader"], text=True
    ).strip()
    if not re.fullmatch(r"GPU-[0-9a-fA-F-]{36}", output):
        raise AssertionError(
            f"Fixture requires exactly one physical GPU UUID: {output!r}"
        )
    (handshake_dir / f"{gate}.waiting").write_text(output, encoding="utf-8")
    print(json.dumps({"phase": "gpu-gated", "gate": gate, "uuid": output}), flush=True)
    approval = handshake_dir / gate
    while not approval.exists():
        time.sleep(0.1)
    if approval.read_text(encoding="utf-8").strip() != output:
        raise AssertionError(
            "GPU admission UUID differs from the container's visible GPU"
        )


def run_server(handshake_dir: Path, snapshot_job_uid: str | None) -> None:
    wait_for_gpu_approval(handshake_dir, SERVER_GATE)
    command = [sys.executable, "-m", "gpu_memory_service.cli.server"]
    if snapshot_job_uid is not None:
        command += [
            "--enable-loader",
            "--snapshot-storage-dir",
            "/checkpoints",
            "--snapshot-job-uid",
            snapshot_job_uid,
            "--device",
            "0",
            "--max-workers",
            "1",
        ]
    os.execv(sys.executable, command)


def weight_bytes(token: str, size: int) -> bytes:
    """Fill the complete physical slab, including allocator padding."""
    pattern = hashlib.sha256(token.encode()).digest()
    return (pattern * ((size + len(pattern) - 1) // len(pattern)))[:size]


def open_ghost_file(size: int) -> int:
    """Hold an unlinked file with allocated bytes above the CRIU ghost limit."""
    fd, path = tempfile.mkstemp(prefix="gms-e2e-ghost-")
    try:
        # CRIU checks st_blocks * 512, so sparse length alone cannot force failure.
        with os.fdopen(fd, "wb", closefd=False) as output:
            remaining = size
            while remaining:
                chunk = os.urandom(min(remaining, 1024 * 1024))
                output.write(chunk)
                remaining -= len(chunk)
            output.flush()
            os.fsync(fd)
        if os.fstat(fd).st_blocks * 512 < size:
            raise RuntimeError("Ghost file has insufficient allocated disk blocks")
        os.unlink(path)
    except BaseException:
        os.close(fd)
        Path(path).unlink(missing_ok=True)
        raise
    return fd


def run_source(token: str, handshake_dir: Path, failure_ghost_size: int) -> None:
    wait_for_gpu_approval(handshake_dir, WORKER_GATE)
    # Runtime imports keep cluster-free manifest tests independent of GMS/CUDA.
    from gpu_memory_service.common.locks import RequestedLockType
    from gpu_memory_service.common.vmm import VMMDeviceType, get_vmm, init_vmm
    from gpu_memory_service.v1.client.memory_manager import GMSClientMemoryManager
    from gpu_memory_service.v1.device import get_socket_path

    control_dir = Path(os.environ["SNAPSHOT_CONTROL_DIR"])
    init_vmm(VMMDeviceType.CUDA)
    vmm = get_vmm()
    vmm.ensure_initialized()
    vmm.runtime_set_device(0)
    granularity = int(vmm.get_allocation_granularity(0))
    manager = GMSClientMemoryManager(
        get_socket_path(0, "weights"), vmm, 0, slab_size=granularity
    )
    manager.connect(RequestedLockType.RW)
    va = manager.create_mapping(granularity)
    expected = weight_bytes(token, granularity)
    host = ctypes.create_string_buffer(expected, granularity)
    stream = vmm.stream_create_nonblocking()
    try:
        vmm.memcpy_h2d_async(va, ctypes.addressof(host), granularity, stream)
        vmm.stream_synchronize(stream)
        source_bytes = ctypes.create_string_buffer(granularity)
        vmm.memcpy_d2h_async(ctypes.addressof(source_bytes), va, granularity, stream)
        vmm.stream_synchronize(stream)
    finally:
        vmm.stream_destroy(stream)
    if source_bytes.raw != expected:
        raise AssertionError("Source GMS CUDA bytes differ from the token pattern")
    manager.commit()
    source_mappings = [
        (mapping.allocation_id, mapping.aligned_size, mapping.base)
        for mapping in manager.mappings
    ]
    manager.unmap_all_vas()
    manager.disconnect()
    (handshake_dir / "quiesced").write_text("quiesced\n", encoding="utf-8")

    while not (handshake_dir / "published").exists():
        time.sleep(0.1)
    # This descriptor deliberately stays open across the failed dump attempt.
    ghost_fd = open_ghost_file(failure_ghost_size) if failure_ghost_size else None
    print(
        json.dumps(
            {
                "phase": "quiesced",
                "token": token,
                "mappings": source_mappings,
                "bytes": granularity,
                "sha256": hashlib.sha256(source_bytes.raw).hexdigest(),
            }
        ),
        flush=True,
    )
    (control_dir / "ready-for-snapshot").write_text("ready\n", encoding="utf-8")
    while not (control_dir / "restore-complete").exists():
        if (handshake_dir / "release").exists():
            if ghost_fd is not None:
                os.close(ghost_fd)
            raise SystemExit(42)
        time.sleep(0.1)

    # RO admission waits for the fresh server's loader to publish the saved IDs.
    manager.connect(RequestedLockType.RO)
    manager.remap_all_vas()
    restored_mappings = [
        (mapping.allocation_id, mapping.aligned_size, mapping.base)
        for mapping in manager.mappings
    ]
    if restored_mappings != source_mappings:
        raise AssertionError("GMS allocation IDs, sizes or virtual addresses changed")
    host = ctypes.create_string_buffer(granularity)
    stream = vmm.stream_create_nonblocking()
    try:
        vmm.memcpy_d2h_async(ctypes.addressof(host), va, granularity, stream)
        vmm.stream_synchronize(stream)
    finally:
        vmm.stream_destroy(stream)
    if host.raw != expected:
        raise AssertionError(
            "Restored GMS bytes differ from the source CUDA allocation"
        )
    result = {
        "phase": "restored",
        "token": token,
        "mappings": restored_mappings,
        "bytes": granularity,
        "sha256": hashlib.sha256(host.raw).hexdigest(),
    }
    Path(RESTORED_RESULT).write_text(json.dumps(result) + "\n", encoding="utf-8")
    print(json.dumps(result), flush=True)
    while True:
        time.sleep(3600)


def main(argv: list[str] | None = None) -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("role", choices=("source", "standby", "server"))
    parser.add_argument("--token")
    parser.add_argument("--handshake-dir", type=Path, default=Path(HANDSHAKE_DIR))
    parser.add_argument("--failure-ghost-size", type=int, default=0)
    parser.add_argument("--snapshot-job-uid")
    args = parser.parse_args(argv)
    if args.failure_ghost_size < 0:
        parser.error("--failure-ghost-size must be nonnegative")
    if args.role == "server":
        run_server(args.handshake_dir, args.snapshot_job_uid)
        return
    if args.role == "standby":
        while True:
            time.sleep(3600)
    if not args.token:
        parser.error("source requires --token")
    run_source(args.token, args.handshake_dir, args.failure_ghost_size)


if __name__ == "__main__":
    main()
