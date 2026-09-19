# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""MessagePack control and virtual-handle client for process-isolated tests."""

import array
import os
import socket
import struct

import msgpack

VERSION = 1
MAX_MESSAGE_BYTES = 32 * 1024 * 1024
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


def send(connection, body):
    data = encode(body)
    connection.sendall(struct.pack("<I", len(data)) + data)


def receive_with_fd(connection):
    prefix, ancillary, flags, _ = connection.recvmsg(
        4, socket.CMSG_SPACE(8), socket.MSG_CMSG_CLOEXEC)
    received = []
    for level, kind, data in ancillary:
        if (level, kind) == (socket.SOL_SOCKET, socket.SCM_RIGHTS):
            descriptors = array.array("i")
            descriptors.frombytes(data)
            received.extend(descriptors)
    assert len(received) <= 1 and not flags & socket.MSG_CTRUNC
    while len(prefix) < 4:
        chunk = connection.recv(4 - len(prefix))
        assert chunk, "missing frame prefix"
        prefix += chunk
    length, = struct.unpack("<I", prefix)
    assert 0 < length <= MAX_MESSAGE_BYTES
    body = bytearray()
    while len(body) < length:
        chunk = connection.recv(length - len(body))
        assert chunk, "missing frame body"
        body.extend(chunk)
    return decode(body), received[0] if received else None


def receive(connection):
    response, fd = receive_with_fd(connection)
    if fd is not None:
        os.close(fd)
    assert fd is None
    return response


def request(path, operation, namespace_pid):
    body = {"kind": operation, "namespace_pid": namespace_pid}
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


def inspect(operation="inspect"):
    path = f"{os.environ['SNAPSHOT_CONTROL_DIR']}/cuinterpose-{os.getpid()}.sock"
    with request(path, operation, os.getpid()) as connection:
        return receive(connection)


def command(operation, success=True):
    response = inspect(operation)
    assert ("Ok" in response["result"]) == success, (operation, response)
    value = response["result"]["Ok" if success else "Err"]
    # Externally tagged replies deserialize directly without buffering records.
    return next(iter(value.values())) if success and isinstance(value, dict) else value


def request_export(path, reference):
    connection = socket.socket(socket.AF_UNIX)
    connection.settimeout(10)
    try:
        connection.connect(path)
        send(connection, {"kind": "export", "allocation": reference})
        response, fd = receive_with_fd(connection)
        assert connection.recv(1) == b"", "more than one response"
        return response, fd
    finally:
        connection.close()
