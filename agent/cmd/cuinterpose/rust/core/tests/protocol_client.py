# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""MessagePack control/ticket client shared by the process-isolated tests."""

import array
import fcntl
import os
import socket
import struct

import msgpack

VERSION = 3
MAX_BYTES = 32 * 1024 * 1024
LIFECYCLE = (
    "prepare_multicast", "save_allocations", "prepare_unicast",
    "load_allocations", "restore_unicast", "restore_multicast_creators",
    "restore_multicast_importers", "restore_multicast_devices",
    "restore_multicast_bindings",
)


def encode(body):
    return msgpack.packb({"version": VERSION, "body": body}, use_bin_type=True)


def decode(data):
    envelope = msgpack.unpackb(data, raw=False)
    assert envelope["version"] == VERSION
    return envelope["body"]


def send(connection, body, fd=None):
    data = encode(body)
    frame = struct.pack("<I", len(data)) + data
    ancillary = [] if fd is None else [
        (socket.SOL_SOCKET, socket.SCM_RIGHTS, array.array("i", [fd]))]
    sent = connection.sendmsg([frame], ancillary)
    assert sent
    connection.sendall(frame[sent:])


def receive(connection):
    # These clients never consume export FDs. Close unexpected rights before
    # asserting, including when a malformed response interrupts a test.
    prefix, ancillary, flags, _ = connection.recvmsg(
        4, socket.CMSG_SPACE(8), socket.MSG_CMSG_CLOEXEC)
    for level, kind, data in ancillary:
        if (level, kind) == (socket.SOL_SOCKET, socket.SCM_RIGHTS):
            descriptors = array.array("i")
            descriptors.frombytes(data)
            for fd in descriptors:
                os.close(fd)
    assert not ancillary and not flags & socket.MSG_CTRUNC
    while len(prefix) < 4:
        chunk = connection.recv(4 - len(prefix))
        assert chunk, "missing frame prefix"
        prefix += chunk
    length, = struct.unpack("<I", prefix)
    assert 0 < length <= MAX_BYTES
    body = bytearray()
    while len(body) < length:
        chunk = connection.recv(length - len(body))
        assert chunk, "missing frame body"
        body.extend(chunk)
    return decode(body)


def request(path, operation, identity=None):
    body = {"kind": operation}
    if operation != "handshake":
        body["participant"] = identity
        if operation != "inspect":
            body.update(kind="execute", operation=operation)
    connection = socket.socket(socket.AF_UNIX)
    connection.settimeout(10)
    try:
        connection.connect(path)
        send(connection, body)
        return connection
    except BaseException:
        connection.close()
        raise


def reply(connection, success=True):
    with connection:
        response = receive(connection)
        assert ("Ok" in response["result"]) == success, response
        assert connection.recv(1) == b"", "more than one response"
        return response


def inspect(operation="handshake"):
    path = f"{os.environ['SNAPSHOT_CONTROL_DIR']}/cuinterpose-{os.getpid()}.sock"
    # Identify before opening the operation connection: an idle first
    # connection would occupy the listener while we awaited the handshake.
    identity = None if operation == "handshake" else inspect()["participant"]
    with request(path, operation, identity) as connection:
        return receive(connection)


def command(operation, success=True):
    response = inspect(operation)
    assert ("Ok" in response["result"]) == success, (operation, response)
    return response["result"]["Ok" if success else "Err"]


def seal_ticket(ticket):
    fd = os.memfd_create("cuinterpose-test-ticket", os.MFD_ALLOW_SEALING)
    try:
        os.write(fd, b"CMVD" + encode(ticket))
        fcntl.fcntl(fd, fcntl.F_ADD_SEALS, fcntl.F_SEAL_SEAL | fcntl.F_SEAL_WRITE |
                    fcntl.F_SEAL_GROW | fcntl.F_SEAL_SHRINK)
        return fd
    except BaseException:
        os.close(fd)
        raise
