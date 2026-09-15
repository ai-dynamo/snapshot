#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""Independent MessagePack peers exercise the shipped coordinator contract."""

import json
import pathlib
import socket
import struct
import subprocess
import sys
import tempfile
import threading
import time

import msgpack

EXECUTABLE = pathlib.Path(sys.argv[1]).resolve()
ID, GROUP = b"\1" * 16, b"\2" * 16
PREPARE = ["handshake", "inspect", "prepare_multicast", "save_allocations", "prepare_unicast"]
RESTORE = ["handshake", "load_allocations", "restore_unicast",
           "restore_multicast_creators", "restore_multicast_importers",
           "restore_multicast_devices", "restore_multicast_bindings", "handshake", "inspect"]


def allocation(creator=True):
    return {"allocation": {"id": ID, "creator": creator, "content": False,
            "size": 4096, "allocation_type": 1, "handle_types": 1,
            "location_type": 1, "location_id": 0, "handles": 1}}


def mapping(size=4096, address=0x10000):
    return {"mapping": {"id": ID, "creator": True,
            "size": size, "address": address, "offset": 0, "access": []}}


def exact(connection, size):
    result = bytearray()
    while len(result) < size:
        part = connection.recv(size - len(result))
        assert part, "unexpected EOF"
        result.extend(part)
    return result


class Fixture:
    def __init__(self, count=1, rendezvous=None):
        self.temporary = tempfile.TemporaryDirectory(prefix="cpp-coordinator-")
        self.directory = pathlib.Path(self.temporary.name)
        self.models = [dict(identity=bytes([i + 1]) * 16, records=[], raw=0,
                            unsupported=0, fail=None, operations=[]) for i in range(count)]
        self.stop = threading.Event()
        self.barrier = threading.Barrier(count)
        self.released = threading.Event()
        self.rendezvous = rendezvous
        self.errors = []
        self.threads = []
        for i in range(count):
            listener = socket.socket(socket.AF_UNIX)
            listener.bind(str(self.directory / f"cuinterpose-{i + 1}.sock"))
            listener.listen()
            listener.settimeout(0.1)
            thread = threading.Thread(target=self.serve, args=(listener, i), daemon=True)
            thread.start()
            self.threads.append(thread)

    def serve(self, listener, index):
        try:
            with listener:
                while not self.stop.is_set():
                    try:
                        connection, _ = listener.accept()
                    except TimeoutError:
                        continue
                    with connection:
                        connection.settimeout(5)
                        length, = struct.unpack("<I", exact(connection, 4))
                        envelope = msgpack.unpackb(exact(connection, length), raw=False)
                        assert envelope["version"] == 4
                        request = envelope["body"]
                        model = self.models[index]
                        operation = request.get("operation", request["kind"])
                        model["operations"].append(operation)
                        if operation == "handshake":
                            reply = "handshake"
                        elif operation == "inspect":
                            reply = {"inspection": {"records": model["records"],
                                     "live_raw_imports": model["raw"],
                                     "unsupported_creations": model["unsupported"]}}
                        else:
                            reply = {"completed": {"operation": operation, "bytes": 0, "copy_us": 0}}
                        if operation == self.rendezvous:
                            self.barrier.wait(timeout=3)
                            if index == 0:
                                time.sleep(0.1)
                                self.released.set()
                        elif (self.rendezvous, operation) in [
                            ("prepare_multicast", "save_allocations"),
                            ("restore_multicast_devices", "restore_multicast_bindings"),
                        ]:
                            assert self.released.is_set(), "advanced before global barrier"
                        result = {"Err": "injected participant failure"} if operation == model["fail"] else {"Ok": reply}
                        body = msgpack.packb({"version": 4, "body": {
                            "participant": model["identity"], "result": result}}, use_bin_type=True)
                        connection.sendall(struct.pack("<I", len(body)) + body)
        except BaseException as error:
            self.errors.append(error)

    def run(self, mode, success=True):
        args = [str(EXECUTABLE), mode, "--proc-root", "", "--checkpoint-dir", str(self.directory),
                "--control-dir", str(self.directory)]
        for i in range(len(self.models)):
            args += ["--process", str(i + 1), str(i + 1)]
        result = subprocess.run(args, capture_output=True, text=True, timeout=15)
        assert (result.returncode == 0) == success, result
        assert not self.errors, self.errors
        if success:
            reports = [json.loads(line) for line in result.stdout.splitlines()]
            expected = ["inspect", "validate", "prepare_multicast", "save_allocations",
                        "prepare_unicast", "state_write"] if mode == "--prepare" else [
                        "handshake", "load_allocations", "restore_unicast", "restore_multicast", "validate"]
            assert [r["phase"] for r in reports] == expected, reports
            for report in reports:
                assert report["status"] == "ok" and report["participants"] == len(self.models)
                assert isinstance(report["elapsed_ms"], (int, float))
        return result

    def __enter__(self):
        return self

    def __exit__(self, *_):
        self.stop.set()
        for thread in self.threads:
            thread.join(timeout=6)
            assert not thread.is_alive()
        self.temporary.cleanup()
        assert not self.errors, self.errors


for case in ("raw", "unsupported", "missing-creator", "mapping", "member"):
    with Fixture() as fixture:
        model = fixture.models[0]
        if case in ("raw", "unsupported"):
            model[case] = 3
        elif case == "missing-creator":
            model["records"] = [allocation(False)]
        elif case == "mapping":
            model["records"] = [allocation(), mapping(8192)]
        else:
            model["records"] = [allocation(),
                {"multicast": dict(id=GROUP, creator=ID, owned=True, size=16384,
                     handles=1, handle_types=1, flags=0, devices=1)},
                {"multicast_device": dict(id=GROUP, device=0)},
                {"multicast_binding": dict(id=GROUP,
                     source={"memory": {"allocation": ID, "offset": 0}},
                     size=8192, offset=0, flags=0, version="v1", device=0)}]
        result = fixture.run("--prepare", False)
        assert model["operations"] == ["handshake", "inspect"], (case, result, model)
        assert not (fixture.directory / "cuinterpose.state").exists()

with Fixture(2) as fixture:
    fixture.models[1]["fail"] = "prepare_multicast"
    fixture.run("--prepare", False)
    assert all(m["operations"] == PREPARE[:3] for m in fixture.models)
    assert not (fixture.directory / "cuinterpose.state").exists()

for rendezvous in ("prepare_multicast", "restore_multicast_devices"):
    with Fixture(2, rendezvous) as fixture:
        fixture.models[0]["records"] = [mapping(), allocation()]
        fixture.models[1]["records"] = [allocation(False)]
        fixture.run("--prepare")
        state = msgpack.unpackb((fixture.directory / "cuinterpose.state").read_bytes(), raw=False)
        assert state["version"] == 4 and len(state["body"]) == 2
        for participant, model in zip(state["body"], fixture.models):
            assert participant["id"] == model["identity"]
            assert all(record in model["records"] for record in participant["records"])
        fixture.models[0]["records"].reverse()
        fixture.run("--restore")
        assert all(m["operations"] == PREPARE + RESTORE for m in fixture.models)

for case in ("missing", "corrupt", "identity", "topology"):
    with Fixture() as fixture:
        model = fixture.models[0]
        if case != "missing":
            model["records"] = [allocation(), mapping()]
            fixture.run("--prepare")
            model["operations"].clear()
        if case == "corrupt":
            (fixture.directory / "cuinterpose.state").write_bytes(b"cuinterpose-state-v2\n")
        elif case == "identity":
            model["identity"] = b"\3" * 16
        elif case == "topology":
            model["records"] = [allocation(), mapping(address=0x30000)]
        fixture.run("--restore", False)
        if case in ("missing", "corrupt"):
            assert not model["operations"]
        elif case == "identity":
            assert model["operations"] == ["handshake"]
        else:
            assert model["operations"][-1] == "inspect"

for args in ([], ["--prepare"], ["--prepare", "--restore"], ["--process", "1"],
             ["--process", "0", "1"], ["--process", "not-a-pid", "1"]):
    assert subprocess.run([EXECUTABLE, *args], capture_output=True).returncode != 0
print("PASS C++ coordinator: independent v4 peers, reports, topology, failure boundaries, and parallel barriers")
