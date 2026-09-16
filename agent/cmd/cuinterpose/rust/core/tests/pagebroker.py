#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Packaged shim/coordinator -> real broker -> fake CUDA worker integration.

Run after `make -C agent/pagebroker test-allocations`, with the packaged shim
and fake CUDA preloaded as for headless.py. The worker copies descriptor bytes;
this verifies protocol/ownership/barriers, not physical GPU byte restoration.
"""

import ctypes as c
import json
import hashlib
import os
from pathlib import Path
import subprocess
import sys
import unittest

from support import driver, props, stats
from protocol_client import command

agent = Path(__file__).resolve().parents[5]
sys.path.insert(0, str(agent / "pagebroker"))
from allocation_session_test import AllocationSessions, pb


class ShimSessions(AllocationSessions):
    # Reuse broker lifecycle setup, not its independent test methods.
    def runTest(self):
        cuda = driver()
        cuda.fakeCopiedToHost.restype = c.c_uint64
        cuda.fakeCopiedToDevice.restype = c.c_uint64
        cuda.cuMemMap.argtypes = [c.c_uint64, c.c_size_t, c.c_size_t, c.c_uint64, c.c_uint64]
        handles = []
        empty = sys.argv[1:] == ["empty"]
        corrupt = sys.argv[1:] == ["corrupt"]
        count = 0 if empty else 33
        # Initialize a participant whose native-owned allocation is never shared.
        private = c.c_uint64()
        assert cuda.cuMemCreate(c.byref(private), 4096, c.byref(props), 0) == 0
        # 33 extents cross the bounded 32-FD batch boundary.
        for index in range(count):
            handle = c.c_uint64()
            assert cuda.cuMemCreate(c.byref(handle), 4096, c.byref(props), 0) == 0
            assert cuda.cuMemMap(0x20000000 + index * 4096, 4096, 0, handle, 0) == 0
            ticket = c.c_int(-1)
            assert cuda.cuMemExportToShareableHandle(c.byref(ticket), handle, 1, 0) == 0
            os.close(ticket.value)
            handles.append(handle)
        # Released logical handles must be recovered from the mapping on SAVE.
        if handles:
            assert cuda.cuMemRelease(handles.pop()) == 0
        before_fds = len(os.listdir("/proc/self/fd"))
        args = [
            os.environ["CUINTERPOSE_COORDINATOR"], "--proc-root", "",
            "--checkpoint-dir", str(self.root),
            "--control-dir", os.environ["SNAPSHOT_CONTROL_DIR"],
            "--process", str(os.getpid()), str(os.getpid()),
        ]
        ids = json.loads(subprocess.check_output(args + ["--identify"]))
        assert len(ids) == 1
        participant = ids[0]
        prepared = self.request("save", "prepare_staged_checkpoint")
        staging = Path(prepared.staged_checkpoint_directory.image_directory)
        source, reply = self.bind("save", pb.BindAllocationSession.SAVE, participant)
        assert reply.HasField("allocation_session"), reply
        mismatch = subprocess.run(args + ["--prepare", "--content-storage", "pagebroker",
                                         "--allocation-session", "0" * 32, str(source.fileno())],
                                  pass_fds=(source.fileno(),), capture_output=True)
        assert mismatch.returncode != 0
        assert stats(cuda).phase == 1 and cuda.fakeCopiedToHost() == 0
        subprocess.run(args + ["--prepare", "--content-storage", "pagebroker",
                              "--allocation-session", participant, str(source.fileno())],
                       pass_fds=(source.fileno(),), check=True)
        source.close()
        assert stats(cuda).phase == 3
        assert cuda.fakeMappedCount() == 0
        assert cuda.fakeCopiedToHost() == 0
        manifest = pb.AllocationManifest.FromString(
            (staging / "allocations" / participant / "manifest.pb").read_bytes())
        assert len(manifest.extents) == count
        assert all(item.size == 4096 and len(item.sha256) == 64 for item in manifest.extents)
        for item in manifest.extents:
            data = (staging / "allocations" / participant / item.allocation_id).read_bytes()
            assert hashlib.sha256(data).hexdigest() == item.sha256
        assert self.request("save", "commit").HasField("commit_complete")
        staged = self.request("load", "staged_restore")
        assert staged.HasField("staged_restore_directory")
        destination, reply = self.bind("load", pb.BindAllocationSession.LOAD, participant)
        assert reply.HasField("allocation_session"), reply
        if corrupt:
            allocation = Path(staged.staged_restore_directory.image_directory) / "allocations" / participant / manifest.extents[0].allocation_id
            allocation.write_bytes(b"X" * 4096)
        restored = subprocess.run(args + ["--restore", "--content-storage", "pagebroker",
                              "--allocation-session", participant, str(destination.fileno())],
                       pass_fds=(destination.fileno(),))
        destination.close()
        if corrupt:
            assert restored.returncode != 0
            assert stats(cuda).phase == 5
            assert cuda.fakeCopiedToDevice() == 0
            assert self.request("load", "commit").HasField("failure")
            print("PASS PageBroker corrupt LOAD: phase poisoned, no host fallback, publication refused")
            return
        restored.check_returncode()
        assert stats(cuda).phase == 1
        assert cuda.fakeMappedCount() == count
        assert cuda.fakeCopiedToDevice() == 0
        assert self.request("load", "commit").HasField("commit_complete")
        # Both broker transfers complete before topology inspection is legal.
        assert len(command("inspect")["records"]) == count * 2 + 1
        assert len(os.listdir("/proc/self/fd")) == before_fds
        print(f"PASS PageBroker shim: {count} allocations, no host arena, restore barriers, session FD cleanup")


if __name__ == "__main__":
    result = unittest.TextTestRunner().run(unittest.TestSuite([ShimSessions()]))
    sys.exit(not result.wasSuccessful())
