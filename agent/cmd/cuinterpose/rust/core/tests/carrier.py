#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Headless carrier failure boundaries; normal restore uses lifecycle.py."""

import ctypes as c
import os
import sys

from support import driver, props

cuda = driver()
from protocol_client import command


def main():
    mode = sys.argv[1]
    assert mode in ("save-copy", "load-copy", "save-pending", "load-pending")
    length = 1 << 20
    cuda.cuMemMap.argtypes = [c.c_uint64, c.c_size_t, c.c_size_t, c.c_uint64, c.c_uint64]
    cuda.fakeFailNext.argtypes = [c.c_char_p]
    handles, virtual_shareable_handles = [], []
    for index in range(3):
        handle, virtual_shareable_handle = c.c_uint64(), c.c_int(-1)
        assert cuda.cuMemCreate(c.byref(handle), length, c.byref(props), 0) == 0
        assert cuda.cuMemMap(0x10000000 + index * length, length, 0, handle, 0) == 0
        assert (
            cuda.cuMemExportToShareableHandle(
                c.byref(virtual_shareable_handle), handle, 1, 0
            )
            == 0
        )
        handles.append(handle)
        virtual_shareable_handles.append(virtual_shareable_handle.value)
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
    # Checkpoint mutation failure poisons the process. Failed save keeps the
    # original driver objects; failed load may leak freshly created backing.
    assert cuda.cuMemRelease(handles[0]) == 600, "failed generation resumed"
    if saving:
        assert cuda.fakeRegisteredHostRanges() == 0 and cuda.fakePrimaryContextsHeld() == 0
        assert cuda.fakeLiveAllocations() == 3
        assert cuda.fakeMappedCount() == 3
    for fd in virtual_shareable_handles:
        os.close(fd)
    print("PASS carrier", mode)


if __name__ == "__main__":
    main()
