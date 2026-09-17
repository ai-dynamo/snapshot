#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Broker/session integration with a spawned, descriptor-only fake worker.

The fake replaces CUDA, not the broker, transport, manifests, or transaction
state machine. Real worker compilation and GPU execution are separate checks.
"""

import array
import os
from pathlib import Path
import socket
import struct
import subprocess
import sys
import tempfile
import time
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parent / "build"))
from v1 import pagebroker_pb2 as pb


def send(connection, message, fds=()):
    body = message.SerializeToString()
    rights = [(socket.SOL_SOCKET, socket.SCM_RIGHTS, array.array("i", fds))] if fds else []
    data = struct.pack("!I", len(body)) + body
    sent = connection.sendmsg([data], rights)
    connection.sendall(data[sent:])


def receive(connection, kind):
    prefix, ancillary, flags, _ = connection.recvmsg(4, socket.CMSG_SPACE(64 * 4))
    if not prefix:
        return None, []
    descriptors = []
    for level, tag, data in ancillary:
        assert (level, tag) == (socket.SOL_SOCKET, socket.SCM_RIGHTS)
        rights = array.array("i")
        rights.frombytes(data)
        descriptors.extend(rights)
    while len(prefix) < 4:
        prefix += connection.recv(4 - len(prefix))
    size, = struct.unpack("!I", prefix)
    body = b""
    while len(body) < size:
        part = connection.recv(size - len(body))
        if not part:
            raise EOFError("partial frame")
        body += part
    assert not flags & socket.MSG_CTRUNC
    return kind.FromString(body), descriptors


def fake_worker():
    connection = socket.socket(fileno=3)
    reply = pb.AllocationSessionReply()
    reply.completed.SetInParent()
    send(connection, reply)
    while True:
        request, fds = receive(connection, pb.AllocationWorkerRequest)
        if request is None:
            return
        count = len(request.batch.extents)
        assert len(fds) == count + 1
        assert len(request.storage_offsets) == count
        reply = pb.AllocationSessionReply()
        for index, extent in enumerate(request.batch.extents):
            source, destination = fds[index], fds[count]
            source_offset, destination_offset = 0, request.storage_offsets[index]
            if request.direction == pb.BindAllocationSession.LOAD:
                source, destination = destination, source
                source_offset, destination_offset = destination_offset, source_offset
            data = os.pread(source, extent.size, source_offset)
            assert len(data) == extent.size
            assert os.pwrite(destination, data, destination_offset) == len(data)
            os.fsync(destination)
            completed = reply.completed.extents.add()
            completed.CopyFrom(extent)
        for fd in fds:
            os.close(fd)
        send(connection, reply)


class AllocationSessions(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name)
        self.socket = str(self.root / "broker.sock")
        self.storage = self.root / "storage"
        self.storage.mkdir()
        self.log = open(self.root / "broker.log", "w")
        self.process = subprocess.Popen(
            [str(Path(__file__).resolve().parent / "pagebroker"), self.socket,
             str(self.root / "stage"), str(self.storage), "--max-concurrent-requests", "8",
             "--allocation-worker", str(Path(__file__).resolve())],
            stderr=self.log,
        )
        self.connections = []
        self.fds = []
        for _ in range(200):
            if Path(self.socket).exists():
                break
            if self.process.poll() is not None:
                self.fail("broker exited before startup")
            time.sleep(.01)
        self.participant = "a" * 32
        self.allocation = "b" * 32

    def tearDown(self):
        for connection in self.connections:
            connection.close()
        for fd in self.fds:
            os.close(fd)
        self.process.terminate()
        self.process.wait(timeout=10)
        self.log.close()
        self.temporary.cleanup()

    def connect(self):
        connection = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        connection.settimeout(5)
        connection.connect(self.socket)
        self.connections.append(connection)
        return connection

    def request(self, transaction, command):
        request = pb.Request(request_id="test", transaction_id=transaction)
        getattr(request, command).SetInParent()
        if command == "prepare_staged_checkpoint":
            request.prepare_staged_checkpoint.destination.filesystem.directory = str(self.storage / "artifact")
            request.prepare_staged_checkpoint.io_engine.posix_copy.SetInParent()
        if command in ("staged_restore", "direct_restore"):
            restore = getattr(request, command)
            restore.source.filesystem.directory = str(self.storage / "artifact")
            restore.io_engine.posix_copy.SetInParent()
        connection = self.connect()
        send(connection, request)
        reply, fds = receive(connection, pb.Response)
        self.assertFalse(fds)
        connection.close()
        return reply

    def bind(self, transaction, direction, participant=None):
        connection = self.connect()
        request = pb.Request(request_id="bind", transaction_id=transaction)
        request.bind_allocations.direction = direction
        request.bind_allocations.participant_id = participant or self.participant
        send(connection, request)
        reply, fds = receive(connection, pb.Response)
        self.assertFalse(fds)
        return connection, reply

    def batch(self, connection, data, allocation=None, descriptor_count=1):
        fd = os.memfd_create("fake-cuda", os.MFD_CLOEXEC)
        self.fds.append(fd)
        os.write(fd, data)
        request = pb.AllocationSessionRequest()
        request.batch.extents.add(allocation_id=allocation or self.allocation,
                                  size=len(data), device_uuid=bytes(16))
        send(connection, request, [fd] * descriptor_count)
        reply, fds = receive(connection, pb.AllocationSessionReply)
        self.assertFalse(fds)
        return fd, reply

    def finish(self, connection):
        request = pb.AllocationSessionRequest()
        request.finish.SetInParent()
        send(connection, request)
        reply, fds = receive(connection, pb.AllocationSessionReply)
        self.assertFalse(fds)
        return reply

    def test_packed_allocations_restore_in_reverse_order(self):
        self.request("save", "prepare_staged_checkpoint")
        connection, _ = self.bind("save", pb.BindAllocationSession.SAVE)
        for allocation, data in ((self.allocation, b"first"), ("c" * 32, b"second")):
            self.assertTrue(self.batch(connection, data, allocation)[1].HasField("completed"))
        self.assertTrue(self.finish(connection).HasField("finished"))
        self.request("save", "commit")
        directory = self.storage / "artifact" / "allocations" / self.participant
        self.assertEqual({path.name for path in directory.iterdir()}, {"content.bin", "manifest.pb"})
        self.assertEqual((directory / "content.bin").read_bytes(), b"firstsecond")
        self.request("load", "direct_restore")
        connection, _ = self.bind("load", pb.BindAllocationSession.LOAD)
        for allocation, data in (("c" * 32, b"second"), (self.allocation, b"first")):
            fd, reply = self.batch(connection, bytes(len(data)), allocation)
            self.assertTrue(reply.HasField("completed"))
            self.assertEqual(os.pread(fd, len(data), 0), data)
        self.assertTrue(self.finish(connection).HasField("finished"))
        self.assertTrue(self.request("load", "commit").HasField("commit_complete"))

    def test_save_load_commit_and_admission(self):
        self.assertTrue(self.request("save", "prepare_staged_checkpoint").HasField("staged_checkpoint_directory"))
        save, ready = self.bind("save", pb.BindAllocationSession.SAVE)
        self.assertTrue(ready.HasField("allocation_session"))
        self.assertEqual(self.request("save", "commit").failure.code, pb.Failure.TRANSACTION_CONFLICT)
        data = b"canonical allocation contents" * 100
        _, saved = self.batch(save, data)
        self.assertEqual(saved.completed.extents[0].allocation_id, self.allocation)
        # Different participants may hold sessions concurrently in one transaction.
        second, ready = self.bind("save", pb.BindAllocationSession.SAVE, "c" * 32)
        self.assertTrue(ready.HasField("allocation_session"))
        self.assertTrue(self.finish(second).HasField("finished"))
        self.assertTrue(self.finish(save).HasField("finished"))
        self.assertTrue(self.request("save", "commit").HasField("commit_complete"))
        self.assertTrue(self.request("load", "direct_restore").HasField("direct_restore_ready"))
        self.assertFalse(any((self.root / "stage" / "restore").iterdir()))
        load, ready = self.bind("load", pb.BindAllocationSession.LOAD)
        self.assertTrue(ready.HasField("allocation_session"))
        target, loaded = self.batch(load, bytes(len(data)))
        self.assertTrue(loaded.HasField("completed"))
        self.assertEqual(os.pread(target, len(data), 0), data)
        self.assertTrue(self.finish(load).HasField("finished"))
        self.assertTrue(self.request("load", "commit").HasField("commit_complete"))

    def test_incomplete_disconnect_prevents_publication(self):
        self.request("save", "prepare_staged_checkpoint")
        connection, ready = self.bind("save", pb.BindAllocationSession.SAVE)
        self.assertTrue(ready.HasField("allocation_session"))
        connection.close()
        # Commit refuses both active and disconnected-incomplete sessions.
        self.assertEqual(self.request("save", "commit").failure.code, pb.Failure.TRANSACTION_CONFLICT)
        reply = self.request("save", "abort")
        self.assertTrue(reply.HasField("abort_complete"))
        self.assertFalse(any((self.root / "stage" / "checkpoint").iterdir()))

    def test_abort_deadline_preserves_files_until_session_drains(self):
        self.request("save", "prepare_staged_checkpoint")
        connection, _ = self.bind("save", pb.BindAllocationSession.SAVE)
        reply = self.request("save", "abort")
        self.assertEqual(reply.failure.code, pb.Failure.TRANSACTION_CONFLICT)
        self.assertIn("drain", reply.failure.message)
        self.assertTrue(any((self.root / "stage" / "checkpoint").iterdir()))
        # Abort has started cancellation; no new participant or publication
        # may race cleanup, even if an existing worker has not exited yet.
        _, rejected = self.bind("save", pb.BindAllocationSession.SAVE, "c" * 32)
        self.assertTrue(rejected.HasField("failure"))
        self.assertEqual(self.request("save", "commit").failure.code, pb.Failure.TRANSACTION_CONFLICT)
        connection.close()
        self.assertTrue(self.request("save", "abort").HasField("abort_complete"))
        self.assertFalse(any((self.root / "stage" / "checkpoint").iterdir()))

    def test_descriptor_count_and_capability_restrictions(self):
        self.request("save", "prepare_staged_checkpoint")
        connection, _ = self.bind("save", pb.BindAllocationSession.SAVE)
        _, failed = self.batch(connection, b"data", descriptor_count=0)
        self.assertTrue(failed.HasField("failure"))
        self.assertEqual(self.request("save", "commit").failure.code, pb.Failure.TRANSACTION_CONFLICT)
        # Abort releases the destination reservation before another save.
        self.assertTrue(self.request("save", "abort").HasField("abort_complete"))
        self.request("other", "prepare_staged_checkpoint")
        connection, ready = self.bind("other", pb.BindAllocationSession.SAVE)
        self.assertTrue(ready.HasField("allocation_session"))
        # A bound connection cannot be used as a general broker connection.
        send(connection, pb.Request(request_id="escape", transaction_id="save", commit=pb.CommitRequest()))
        try:
            reply, _ = receive(connection, pb.AllocationSessionReply)
            self.assertTrue(reply is None or reply.HasField("failure"))
        except (ConnectionResetError, EOFError):
            pass

    def test_truncated_restore(self):
        self.request("save", "prepare_staged_checkpoint")
        connection, _ = self.bind("save", pb.BindAllocationSession.SAVE)
        self.batch(connection, b"contents")
        self.assertTrue(self.finish(connection).HasField("finished"))
        self.request("save", "commit")
        path = self.storage / "artifact" / "allocations" / self.participant / "content.bin"
        path.write_bytes(b"short")
        self.request("load", "staged_restore")
        _, failed = self.bind("load", pb.BindAllocationSession.LOAD)
        self.assertTrue(failed.HasField("failure"))
        self.assertEqual(self.request("load", "commit").failure.code, pb.Failure.TRANSACTION_CONFLICT)

    def test_duplicate_allocation_refuses_commit(self):
        self.request("save", "prepare_staged_checkpoint")
        connection, _ = self.bind("save", pb.BindAllocationSession.SAVE)
        self.assertTrue(self.batch(connection, b"data")[1].HasField("completed"))
        self.assertTrue(self.batch(connection, b"data")[1].HasField("failure"))
        self.assertEqual(self.request("save", "commit").failure.code, pb.Failure.TRANSACTION_CONFLICT)

    def test_worker_exit_releases_admission_but_never_publishes(self):
        self.request("save", "prepare_staged_checkpoint")
        connection, _ = self.bind("save", pb.BindAllocationSession.SAVE)
        descriptor = os.memfd_create("invalid-fake-cuda-size", os.MFD_CLOEXEC)
        self.fds.append(descriptor)
        request = pb.AllocationSessionRequest()
        request.batch.extents.add(allocation_id=self.allocation, size=4096, device_uuid=bytes(16))
        # The fake worker exits on a short read, exercising the real supervisor.
        send(connection, request, [descriptor])
        failed, _ = receive(connection, pb.AllocationSessionReply)
        self.assertTrue(failed.HasField("failure"))
        self.assertEqual(self.request("save", "commit").failure.code, pb.Failure.TRANSACTION_CONFLICT)
        for _ in range(100):
            if self.request("save", "abort").HasField("abort_complete"):
                break
            time.sleep(.01)
        else:
            self.fail("failed worker retained transaction admission")

    def test_oversized_descriptor_frame_closes_rights(self):
        self.request("save", "prepare_staged_checkpoint")
        connection, ready = self.bind("save", pb.BindAllocationSession.SAVE)
        self.assertTrue(ready.HasField("allocation_session"))
        descriptor = os.memfd_create("too-many-rights", os.MFD_CLOEXEC)
        self.fds.append(descriptor)
        before = len(list(Path(f"/proc/{self.process.pid}/fd").iterdir()))
        request = pb.AllocationSessionRequest()
        request.finish.SetInParent()
        send(connection, request, [descriptor] * 65)
        try:
            self.assertEqual(connection.recv(1), b"")
        except ConnectionResetError:
            pass
        for _ in range(100):
            if self.request("save", "abort").HasField("abort_complete"):
                break
            time.sleep(.01)
        after = len(list(Path(f"/proc/{self.process.pid}/fd").iterdir()))
        self.assertLessEqual(after, before)

    def test_missing_worker_refuses_binding_without_poisoning_transaction(self):
        self.process.terminate()
        self.process.wait(timeout=10)
        self.process = subprocess.Popen(
            [str(Path(__file__).resolve().parent / "pagebroker"), self.socket,
             str(self.root / "stage"), str(self.storage), "--max-concurrent-requests", "8",
             "--allocation-worker", "/does-not-exist"],
            stderr=self.log,
        )
        time.sleep(.1)
        self.request("save", "prepare_staged_checkpoint")
        _, reply = self.bind("save", pb.BindAllocationSession.SAVE)
        self.assertTrue(reply.HasField("failure"))
        self.assertTrue(self.request("save", "abort").HasField("abort_complete"))


if __name__ == "__main__":
    if sys.argv[1:] == ["--socket-fd=3"]:
        fake_worker()
    else:
        unittest.main()
