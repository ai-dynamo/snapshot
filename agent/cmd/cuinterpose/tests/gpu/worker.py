# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""The interposed workload for the GPU tests.

Started by ``harness.Workload`` with ``LD_PRELOAD=libcuinterpose.so``. The
parent forks ``WORLD_SIZE`` workers before touching CUDA (a fork after CUDA
state exists is unsupported); each worker then behaves like one rank of a
tensor-parallel server:

* creates two POSIX-shareable allocations of its own, one small one *before*
  any CUDA context exists (the driver allows that) and one large one whose
  contents must travel through the host carrier, both filled with seeded
  random bytes;
* imports a descriptor from a process without the shim (a raw import) and
  releases it again, or keeps it alive in ``hold-raw-import`` mode so the
  coordinator has something to refuse;
* shares a PyTorch symmetric-memory buffer with the other rank and captures a
  collective into a CUDA graph;
* signals ``ready``, waits for ``continue``, and after the checkpoint round
  trip verifies every byte and replays the graph.

Progress and results are communicated to the test through marker files in the
sync directory; failures print a traceback and exit non-zero.
"""

from __future__ import annotations

import os
import signal
import socket
import sys
import time
import traceback
from multiprocessing.reduction import recv_handle
from pathlib import Path

import torch
import torch.distributed as dist
import torch.distributed._symmetric_memory as symm_mem
from cuda.bindings import driver

import harness
from harness import POSIX_FD_HANDLE_TYPE, WORLD_SIZE, WORKER_TIMEOUT_SECONDS, cuda_call

NUMEL = 2048


class Options:
    def __init__(self, argv: list[str]) -> None:
        if len(argv) != 11:
            raise SystemExit(
                "usage: worker.py RAW_FD_0 RAW_FD_1 RESTORE_FD_0 RESTORE_FD_1 SYNC_DIR "
                "STORE_PATH (unicast|multicast) CARRIER_BYTES SEED "
                "(hold-raw-import|release-raw-import)"
            )
        self.raw_fds = (int(argv[1]), int(argv[2]))
        self.restore_fds = (int(argv[3]), int(argv[4]))
        self.sync_dir = Path(argv[5])
        self.store_path = Path(argv[6])
        if argv[7] not in {"unicast", "multicast"}:
            raise SystemExit("mode must be unicast or multicast")
        self.multicast = argv[7] == "multicast"
        self.carrier_bytes = int(argv[8])
        self.seed = int(argv[9])
        if argv[10] not in {"hold-raw-import", "release-raw-import"}:
            raise SystemExit("raw import handling must be hold-raw-import or release-raw-import")
        self.hold_raw_import = argv[10] == "hold-raw-import"


# --- seeded contents ----------------------------------------------------------


def _pattern(nbytes: int, seed: int, device_index: int) -> torch.Tensor:
    generator = torch.Generator(device=f"cuda:{device_index}")
    generator.manual_seed(seed)
    return torch.randint(
        0, 256, (nbytes,), dtype=torch.uint8, device=f"cuda:{device_index}", generator=generator
    )


def _fill(address: int, nbytes: int, seed: int, device_index: int) -> None:
    pattern = _pattern(nbytes, seed, device_index)
    cuda_call(driver.cuMemcpyDtoD, address, pattern.data_ptr(), nbytes)
    torch.cuda.synchronize()


def _verify(address: int, nbytes: int, seed: int, device_index: int, stage: str) -> None:
    expected = _pattern(nbytes, seed, device_index)
    actual = torch.empty(nbytes, dtype=torch.uint8, device=f"cuda:{device_index}")
    cuda_call(driver.cuMemcpyDtoD, actual.data_ptr(), address, nbytes)
    torch.cuda.synchronize()
    if not torch.equal(actual, expected):
        mismatches = actual != expected
        first = int(torch.nonzero(mismatches)[0].item())
        raise AssertionError(
            f"{stage}: {int(mismatches.sum().item())} of {nbytes} bytes differ, "
            f"first at offset {first} (seed {seed})"
        )


# --- the worker ---------------------------------------------------------------


def _wait_for_continue(sync_dir: Path) -> None:
    deadline = time.monotonic() + WORKER_TIMEOUT_SECONDS
    while not (sync_dir / "continue").exists():
        if time.monotonic() >= deadline:
            raise TimeoutError("timed out waiting for the test to continue")
        time.sleep(0.05)


def _import_raw(fd: int, size: int, device, expected: bytes, stage: str) -> tuple[object, int]:
    """Import a descriptor from an uninterposed process, map it, and check its
    contents. Returns the (raw) handle and the mapped address."""
    try:
        handle = cuda_call(driver.cuMemImportFromShareableHandle, fd, POSIX_FD_HANDLE_TYPE)
        harness.assert_handle_namespace(handle, False, stage)
    finally:
        os.close(fd)
    try:
        address = harness.map_allocation(handle, size, device)
    except Exception:
        cuda_call(driver.cuMemRelease, handle)
        raise
    try:
        harness.assert_bytes(address, expected, stage)
    except Exception:
        harness.destroy_mapped_allocation(address, size, handle)
        raise
    return handle, address


def _worker(rank: int, options: Options) -> None:
    harness.require_launch_job()
    cuda_call(driver.cuInit, 0)
    device = cuda_call(driver.cuDeviceGet, rank)
    properties = harness.allocation_properties(device)
    granularity = harness.allocation_granularity(properties)
    private_size = granularity
    bulk_size = harness.round_up(options.carrier_bytes, granularity)
    private_seed = options.seed + 2 * rank
    bulk_seed = options.seed + 2 * rank + 1

    # The driver does not need a context for cuMemCreate, so neither may the shim.
    harness.assert_no_current_context("worker before the first cuMemCreate")
    private_handle = cuda_call(driver.cuMemCreate, private_size, properties, 0)
    harness.assert_handle_namespace(private_handle, True, "tracked cuMemCreate")

    torch.cuda.set_device(rank)
    for other_rank, fd in enumerate(options.raw_fds):
        if other_rank != rank:
            os.close(fd)
    for other_rank, fd in enumerate(options.restore_fds):
        if other_rank != rank:
            os.close(fd)
    restore_socket = socket.socket(fileno=options.restore_fds[rank])

    raw_handle, raw_address = _import_raw(
        options.raw_fds[rank], granularity, device, bytes([rank + 1]) * 32, "raw import"
    )
    if not options.hold_raw_import:
        harness.destroy_mapped_allocation(raw_address, granularity, raw_handle)

    private_address = harness.map_allocation(private_handle, private_size, device)
    retained_handle = cuda_call(driver.cuMemRetainAllocationHandle, private_address)
    harness.assert_handle_namespace(retained_handle, True, "tracked retain")
    cuda_call(driver.cuMemRelease, retained_handle)
    _fill(private_address, private_size, private_seed, rank)

    bulk_handle = cuda_call(driver.cuMemCreate, bulk_size, properties, 0)
    harness.assert_handle_namespace(bulk_handle, True, "tracked bulk cuMemCreate")
    bulk_address = harness.map_allocation(bulk_handle, bulk_size, device)
    _fill(bulk_address, bulk_size, bulk_seed, rank)
    (options.sync_dir / f"carrier-{rank}").write_text(f"2 {private_size + bulk_size}\n")

    if options.hold_raw_import:
        # Nothing to restore in this mode: the test only checks that prepare is
        # refused and that the workload keeps working afterwards.
        restore_socket.close()
        (options.sync_dir / f"ready-{rank}").touch()
        _wait_for_continue(options.sync_dir)
        harness.destroy_mapped_allocation(raw_address, granularity, raw_handle)
        probe = cuda_call(driver.cuMemCreate, granularity, properties, 0)
        harness.assert_handle_namespace(probe, True, "cuMemCreate after a refused prepare")
        cuda_call(driver.cuMemRelease, probe)
        _verify(private_address, private_size, private_seed, rank, "private after refused prepare")
        _verify(bulk_address, bulk_size, bulk_seed, rank, "bulk after refused prepare")
        (options.sync_dir / f"done-{rank}").touch()
        harness.destroy_mapped_allocation(bulk_address, bulk_size, bulk_handle)
        harness.destroy_mapped_allocation(private_address, private_size, private_handle)
        return

    dist.init_process_group(
        "gloo", init_method=f"file://{options.store_path}", rank=rank, world_size=WORLD_SIZE
    )
    group_name = dist.group.WORLD.group_name
    input_tensor = symm_mem.empty(NUMEL, dtype=torch.float32, device="cuda")
    input_tensor.fill_(rank + 1)
    symm_handle = symm_mem.rendezvous(input_tensor, group=group_name)
    if options.multicast:
        if not symm_handle.has_multicast_support:
            raise AssertionError("PyTorch silently fell back from CUDA multicast")
        if int(symm_handle.multicast_ptr) == 0:
            raise AssertionError("PyTorch selected multicast without a multicast VA")
        _replace_local_binding_with_address(rank, input_tensor, symm_handle, properties)
        dist.barrier()
    output = torch.empty_like(input_tensor)

    _collective(input_tensor, group_name, output, options.multicast)
    torch.cuda.synchronize()
    graph = torch.cuda.CUDAGraph()
    with torch.cuda.graph(graph):
        _collective(input_tensor, group_name, output, options.multicast)
    graph.replay()
    torch.cuda.synchronize()
    _assert_exact_result(output, "before checkpoint")
    (options.sync_dir / f"ready-{rank}").touch()

    _wait_for_continue(options.sync_dir)

    # A raw import made after restore must work like before.
    fresh_fd = recv_handle(restore_socket)
    restore_socket.close()
    fresh_handle, fresh_address = _import_raw(
        fresh_fd, granularity, device, bytes([0x40 + rank]) * 32, "raw import after restore"
    )
    harness.destroy_mapped_allocation(fresh_address, granularity, fresh_handle)

    _verify(private_address, private_size, private_seed, rank, "private allocation after restore")
    _verify(bulk_address, bulk_size, bulk_seed, rank, "bulk allocation after restore")
    graph.replay()
    torch.cuda.synchronize()
    _assert_exact_result(output, "after restore")
    (options.sync_dir / f"done-{rank}").touch()

    dist.barrier()
    del graph, output, symm_handle, input_tensor
    torch.cuda.empty_cache()
    dist.destroy_process_group()
    harness.destroy_mapped_allocation(bulk_address, bulk_size, bulk_handle)
    harness.destroy_mapped_allocation(private_address, private_size, private_handle)


def _collective(
    input_tensor: torch.Tensor, group_name: str, output: torch.Tensor, multicast: bool
) -> None:
    operation = (
        torch.ops.symm_mem.multimem_one_shot_all_reduce_out
        if multicast
        else torch.ops.symm_mem.one_shot_all_reduce_out
    )
    operation(input_tensor, "sum", group_name, output)


def _replace_local_binding_with_address(
    rank: int, input_tensor: torch.Tensor, symm_handle, properties: driver.CUmemAllocationProp
) -> None:
    """Rebind this rank's slice of the multicast object through
    cuMulticastBindAddr, so both bind entry points are exercised."""
    granularity = int(
        cuda_call(
            driver.cuMemGetAllocationGranularity,
            properties,
            driver.CUmemAllocationGranularity_flags.CU_MEM_ALLOC_GRANULARITY_RECOMMENDED,
        )
    )
    buffer_size = input_tensor.numel() * input_tensor.element_size()
    signal_offset = (buffer_size + 15) // 16 * 16
    unrounded_size = signal_offset + symm_mem.get_signal_pad_size()
    block_size = harness.round_up(unrounded_size, granularity)
    multicast_handle = cuda_call(
        driver.cuMemRetainAllocationHandle, int(symm_handle.multicast_ptr)
    )
    harness.assert_handle_namespace(multicast_handle, True, "retained multicast handle")
    device = cuda_call(driver.cuDeviceGet, rank)
    try:
        cuda_call(driver.cuMulticastUnbind, multicast_handle, device, 0, block_size)
        cuda_call(
            driver.cuMulticastBindAddr,
            multicast_handle,
            0,
            int(symm_handle.buffer_ptrs[rank]),
            block_size,
            0,
        )
    finally:
        cuda_call(driver.cuMemRelease, multicast_handle)


def _assert_exact_result(output: torch.Tensor, stage: str) -> None:
    expected = torch.full((NUMEL,), 3.0, dtype=torch.float32)
    actual = output.cpu()
    if not torch.equal(actual, expected):
        mismatch = torch.nonzero(actual != expected)[0].item()
        raise AssertionError(
            f"{stage}: output[{mismatch}] is {actual[mismatch].item()}, expected 3.0"
        )


# --- the parent ---------------------------------------------------------------


def _fork_workers(options: Options) -> None:
    if torch.cuda.is_initialized():
        raise RuntimeError("parent initialized CUDA before forking workers")
    harness.assert_no_current_context("parent before forking workers")

    children = []
    for rank in range(WORLD_SIZE):
        child = os.fork()
        if child == 0:
            try:
                _worker(rank, options)
            except BaseException:  # noqa: BLE001 -- report child failures to the parent
                traceback.print_exc()
                os._exit(1)
            os._exit(0)
        children.append((rank, child))
        (options.sync_dir / f"pid-{rank}").write_text(f"{child}\n")

    for fd in options.raw_fds + options.restore_fds:
        os.close(fd)

    remaining = dict(children)
    while remaining:
        failures = []
        for rank, child in tuple(remaining.items()):
            waited, status = os.waitpid(child, os.WNOHANG)
            if waited == 0:
                continue
            del remaining[rank]
            exit_code = os.waitstatus_to_exitcode(status)
            if not os.WIFEXITED(status) or exit_code != 0:
                failures.append(f"rank {rank} PID {child}: {exit_code}")
        if failures:
            for child in remaining.values():
                try:
                    os.kill(child, signal.SIGTERM)
                except ProcessLookupError:
                    pass
            for child in remaining.values():
                os.waitpid(child, 0)
            raise RuntimeError(f"forked workers failed: {', '.join(failures)}")
        time.sleep(0.05)


if __name__ == "__main__":
    _fork_workers(Options(sys.argv))
