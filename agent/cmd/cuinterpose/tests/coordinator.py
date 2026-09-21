#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Drive the packaged coordinator through real sockets with scripted replies."""

from contextlib import ExitStack, contextmanager
from pathlib import Path
import select
import socket
import struct
import subprocess
import tempfile
import unittest

import msgpack

BINARY = Path(__file__).resolve().parents[1] / "build/cuinterpose-coordinator"
PREPARE = ("prepare_multicast", "save_allocations", "prepare_unicast")
RESTORE = ("load_allocations", "restore_unicast", "restore_multicast_creators",
           "restore_multicast_importers", "restore_multicast_devices", "restore_multicast_bindings")
ALLOCATION = {"id": bytes([1] * 16), "creator_pid": 1}
MULTICAST = {"id": bytes([2] * 16), "creator_pid": 1}


def encode(body):
    return msgpack.packb({"version": 3, "body": body}, use_bin_type=True)


def allocation(creator=1, *, size=4096, content=False, identifier=ALLOCATION["id"]):
    return {"allocation": {"allocation": {"id": identifier, "creator_pid": creator},
        "content": content, "size": size, "allocation_type": 1, "handle_types": 1,
        "location": [1, 0], "virtual_allocation_handle_count": 1}}


def mapping(size=4096, address=0x10000):
    return {"mapping": {"allocation": ALLOCATION, "address": address,
        "size": size, "offset": 0, "access": []}}


def multicast(size, devices=1):
    return {"multicast": {"allocation": MULTICAST, "devices": devices, "size": size,
        "handle_types": 1, "flags": 0, "virtual_multicast_handle_count": 1}}


def binding(size, offset=0):
    return {"multicast_binding": {"allocation": MULTICAST,
        "source": {"memory": {"allocation": ALLOCATION, "offset": 0}},
        "size": size, "offset": offset, "flags": 0, "version": "v1", "device": 0}}


class Contracts(unittest.TestCase):
    def setUp(self):
        self.resources = ExitStack()
        self.addCleanup(self.resources.close)
        self.directory = Path(self.resources.enter_context(tempfile.TemporaryDirectory()))
        self.state = self.directory / "cuinterpose.state"
        self.listeners = []
        for pid in (1, 2):
            listener = self.resources.enter_context(socket.socket(socket.AF_UNIX))
            listener.bind(str(self.directory / f"cuinterpose-{pid}.sock"))
            listener.listen()
            self.listeners.append(listener)

    @contextmanager
    def coordinator(self, mode, error=None):
        command = [str(BINARY), mode, "--checkpoint-dir", str(self.directory),
                   "--control-dir", str(self.directory), "--process", "1", "--process", "2"]
        process = subprocess.Popen(command, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            yield
            _, stderr = process.communicate(timeout=5)
            self.assertFalse(select.select(self.listeners, [], [], 0)[0], "unexpected phase or retry")
            if error is None:
                self.assertEqual(process.returncode, 0, stderr)
            else:
                self.assertNotEqual(process.returncode, 0, stderr)
                self.assertIn(error, stderr)
                if mode == "--prepare":
                    self.assertFalse(self.state.exists())
        finally:
            if process.poll() is None:
                process.kill()
            process.communicate()

    def request(self, pid, kind, **fields):
        listener = self.listeners[pid - 1]
        self.assertTrue(select.select([listener], [], [], 3)[0], f"no {kind} request for {pid}")
        connection, _ = listener.accept()
        self.resources.callback(connection.close)
        connection.settimeout(3)
        with connection.makefile("rb") as stream:
            size, = struct.unpack("<I", stream.read(4))
            message = msgpack.unpackb(stream.read(size), raw=False)
        self.assertEqual(message, {"version": 3, "body": {"kind": kind, "namespace_pid": pid, **fields}})
        return connection

    def reply(self, connection, pid, result):
        body = encode({"namespace_pid": pid, "result": result})
        with connection:
            connection.sendall(struct.pack("<I", len(body)) + body)

    def inspect(self, records, begin=False):
        for pid, records in enumerate(records, 1):
            self.reply(self.request(pid, "begin_checkpoint" if begin else "inspect"), pid,
                       {"Ok": {"inspection": {"records": records}}})

    def phase(self, operation):
        # Requiring both requests before any reply detects serial dispatch.
        held, other = [self.request(pid, "execute", operation=operation) for pid in (1, 2)]
        completed = {"Ok": {"completed": {"operation": operation, "bytes": 0}}}
        self.reply(other, 2, completed)
        self.assertFalse(select.select(self.listeners, [], [], 0.1)[0], "advanced before the held reply")
        self.reply(held, 1, completed)

    def test_preflight_refusals(self):
        for records in [[allocation(2)], [allocation(), mapping(8192)],
                        [allocation(), multicast(16384),
                         {"multicast_device": {"allocation": MULTICAST, "device": 0}}, binding(8192)]]:
            with self.subTest(records=records), self.coordinator("--prepare", "AllocationReference"):
                self.inspect([records, []], begin=True)

    def test_failed_or_lost_reply_stops_without_retry(self):
        for lost in (False, True):
            error = "receive failed" if lost else "injected failure"
            with self.subTest(lost=lost), self.coordinator("--prepare", error):
                self.inspect([[], []], begin=True)
                held, failed = [self.request(pid, "execute", operation=PREPARE[0]) for pid in (1, 2)]
                if lost:
                    failed.close()
                else:
                    self.reply(failed, 2, {"Err": "injected failure"})
                self.reply(held, 1, {"Ok": {"completed": {"operation": PREPARE[0], "bytes": 0}}})

    def test_wrong_transfer_size_stops_before_teardown(self):
        with self.coordinator("--prepare", "transfer size"):
            self.inspect([[allocation(content=True)], []], begin=True)
            self.phase(PREPARE[0])
            self.phase(PREPARE[1])  # Replies claim zero bytes instead of 4096.

    def test_parallel_phases_and_canonical_state(self):
        records = [[mapping(), allocation()], [allocation()]]
        with self.coordinator("--prepare"):
            self.inspect(records, begin=True)
            for operation in PREPARE:
                self.phase(operation)
        saved = msgpack.unpackb(self.state.read_bytes(), raw=False, strict_map_key=False)
        self.assertEqual(saved["version"], 3)
        self.assertEqual(saved["body"], {1: [allocation(), mapping()], 2: [allocation()]})
        records[0].reverse()
        with self.coordinator("--restore"):
            self.inspect(records)
            for operation in RESTORE:
                self.phase(operation)
            self.inspect(records)

    def test_restore_refusals(self):
        for case, error in [("missing", "cannot parse"), ("corrupt", "cannot parse"),
                            ("identity", "namespace PID changed"), ("topology", "restored topology")]:
            self.state.unlink(missing_ok=True)
            if case == "corrupt":
                self.state.write_bytes(b"not-cuinterpose-state\n")
            elif case != "missing":
                self.state.write_bytes(encode({1: [allocation(), mapping()], 2: []}))
            with self.subTest(case=case), self.coordinator("--restore", error):
                if case == "identity":
                    self.reply(self.request(1, "inspect"), 3, {"Ok": {"inspection": {"records": []}}})
                elif case == "topology":
                    changed = [[allocation(), mapping(address=0x30000)], []]
                    self.inspect(changed)
                    for operation in RESTORE:
                        self.phase(operation)
                    self.inspect(changed)

    def test_usage_errors(self):
        for args in ["", "--prepare --checkpoint-dir /tmp",
                     "--prepare --checkpoint-dir /tmp --control-dir relative --process 1",
                     "--prepare --checkpoint-dir /tmp --control-dir /tmp --process 0",
                     "--prepare --checkpoint-dir /tmp --control-dir /tmp --process 1 --process 1",
                     "--prepare --checkpoint-dir /tmp --control-dir /tmp --process not-a-pid"]:
            with self.subTest(args=args):
                result = subprocess.run([str(BINARY), *args.split()], capture_output=True, timeout=5)
                self.assertNotEqual(result.returncode, 0)

    def test_later_participant_overflow_starts_no_save(self):
        records = [allocation(2, size=size, content=True, identifier=bytes([i] * 16))
                   for i, size in enumerate(((1 << 64) - 1, 1))]
        with self.coordinator("--prepare", "allocation size overflow"):
            self.inspect([[], records], begin=True)
            self.phase(PREPARE[0])

    def test_multicast_sizes_must_agree(self):
        for sizes in ((4096, 8192), (8192, 4096)):
            with self.subTest(sizes=sizes), self.coordinator("--prepare", "inconsistent multicast properties"):
                self.inspect([[multicast(size, 2)] for size in sizes], begin=True)

    def test_rounded_multicast_extents_preserve_creation_size(self):
        records = [[allocation(), multicast(4096),
            {"multicast_device": {"allocation": MULTICAST, "device": 0}}, binding(4096, 4096),
            {"multicast_mapping": {"allocation": MULTICAST, "address": 0x10000, "size": 8192,
                                   "offset": 0, "flags": 0, "access": []}}], []]
        with self.coordinator("--prepare"):
            self.inspect(records, begin=True)
            for operation in PREPARE:
                self.phase(operation)
        with self.coordinator("--restore"):
            self.inspect(records)
            for operation in RESTORE:
                self.phase(operation)
            self.inspect(records)


if __name__ == "__main__":
    unittest.main()
