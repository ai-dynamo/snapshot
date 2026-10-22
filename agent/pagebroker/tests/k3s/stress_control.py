# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Small shared control messages and process identities for endurance peers."""

from dataclasses import dataclass
import json
import os
from pathlib import Path
import signal
import tempfile

from run import exited, wait_for_peer


def start_ticks(pid):
    return int(Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()[19])


@dataclass
class Peer:
    pid: int
    ticks: int
    fd: int

    @classmethod
    def open(cls, record):
        pid = record["pid"]
        if type(pid) is not int or pid <= 1 or pid == os.getpid():
            raise ValueError("invalid peer PID")
        fd = os.pidfd_open(pid)
        try:
            peer = cls(pid, start_ticks(pid), fd)
            status = Path(f"/proc/{pid}/status").read_text().splitlines()
            uid = next(line for line in status if line.startswith("Uid:")).split()[2]
            if peer.ticks != record["startTicks"] or int(uid) != os.geteuid():
                raise RuntimeError("peer identity or UID changed")
            peer.check()
            return peer
        except BaseException:
            os.close(fd)
            raise

    @classmethod
    def discover(cls, socket, timeout):
        pid, fd = wait_for_peer(socket, timeout)
        try:
            peer = cls(pid, start_ticks(pid), fd)
            peer.check()
            return peer
        except BaseException:
            os.close(fd)
            raise

    def record(self):
        return {"pid": self.pid, "startTicks": self.ticks}

    def check(self):
        if exited(self.fd):
            raise RuntimeError(f"registered process {self.pid} exited")

    def verify_socket(self, socket):
        self.check()
        other = self.discover(socket, 0.05)
        try:
            if other.record() != self.record():
                raise RuntimeError("PageBroker socket identifies a replacement process")
        finally:
            other.close()
        self.check()

    def send(self, sig):
        if not exited(self.fd):
            signal.pidfd_send_signal(self.fd, sig)

    def close(self):
        os.close(self.fd)


def own_record():
    return {"pid": os.getpid(), "startTicks": start_ticks(os.getpid())}


class Channel:
    def __init__(self, directory, run_id):
        self.directory = Path(directory)
        self.run_id = run_id

    def write(self, name, **value):
        data = (json.dumps(value | {"runId": self.run_id}, allow_nan=False) + "\n").encode()
        if len(data) > 64 * 1024:
            raise ValueError("control message exceeds 64 KiB")
        with tempfile.NamedTemporaryFile(dir=self.directory, delete=False) as output:
            temporary = Path(output.name)
            try:
                output.write(data)
                output.flush()
                os.fsync(output.fileno())
                os.replace(temporary, self.directory / name)
                directory = os.open(self.directory, os.O_RDONLY | os.O_DIRECTORY)
                try:
                    os.fsync(directory)
                finally:
                    os.close(directory)
            finally:
                temporary.unlink(missing_ok=True)

    def read(self, name):
        path = self.directory / name
        try:
            if path.stat().st_size > 64 * 1024:
                raise ValueError(f"control message {name} exceeds 64 KiB")
            value = json.loads(path.read_text())
        except FileNotFoundError:
            return None
        if value.get("runId") != self.run_id:
            raise ValueError(f"foreign run in control message {name}")
        return value

    def beat(self, role, clock):
        self.write(f"{role}-heartbeat.json", monotonic=clock.monotonic())

    def check_heartbeat(self, role, clock, stale):
        message = self.read(f"{role}-heartbeat.json")
        if message is None:
            raise RuntimeError(f"missing {role} heartbeat")
        age = clock.monotonic() - message["monotonic"]
        if not 0 <= age <= stale:
            raise RuntimeError(f"stale {role} heartbeat")
