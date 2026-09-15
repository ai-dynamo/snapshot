#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Representative carrier failure boundaries; normal restore uses the C fixtures."""

import ctypes as c
import os
import sys

from fork import cuda, props, stats
from protocol_client import command


def main():
    mode = sys.argv[1]
    assert mode in ("save-copy", "load-copy", "save-pending", "load-pending")
    length = 1 << 20
    cuda.cuMemMap.argtypes = [c.c_uint64, c.c_size_t, c.c_size_t, c.c_uint64, c.c_uint64]
    cuda.fakeFailNext.argtypes = [c.c_char_p]
    handles, tickets = [], []
    for index in range(3):
        handle, ticket = c.c_uint64(), c.c_int(-1)
        assert cuda.cuMemCreate(c.byref(handle), length, c.byref(props), 0) == 0
        assert cuda.cuMemMap(0x10000000 + index * length, length, 0, handle, 0) == 0
        assert cuda.cuMemExportToShareableHandle(c.byref(ticket), handle, 1, 0) == 0
        handles.append(handle)
        tickets.append(ticket.value)
    command("prepare_multicast")
    saving = mode.startswith("save")
    if not saving:
        command("save_allocations")
        command("prepare_unicast")
        cuda.fakeForgetHostRegistrations()
    if mode.endswith("pending"):
        cuda.carrier_pending_copies()
        command("save_allocations" if saving else "load_allocations")
        raise AssertionError("unknown copy completion returned instead of terminating")

    cuda.fakeFailNext(b"cuMemcpyDtoHAsync_v2" if saving else b"cuMemcpyHtoDAsync_v2")
    command("save_allocations" if saving else "load_allocations", False)
    assert stats().phase == 5
    assert cuda.fakeRegisteredHostRanges() == 0 and cuda.fakePrimaryContextsHeld() == 0
    # Failed save retains the original driver state. Failed load must release
    # all freshly created backing and staging mappings.
    assert cuda.fakeLiveAllocations() == (3 if saving else 0)
    assert cuda.fakeMappedCount() == (3 if saving else 0)
    assert cuda.cuMemRelease(handles[0]) == 600, "failed generation resumed"
    for fd in tickets:
        os.close(fd)
    print("PASS carrier", mode)


if __name__ == "__main__":
    main()
