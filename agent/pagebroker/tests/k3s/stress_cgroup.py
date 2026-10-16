# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Read optional cgroup v2 counters only after verifying broker membership."""

import os
from pathlib import PurePosixPath


class BrokerCgroup:
    def __init__(self, sampler, excluded_pids=()):
        self.sampler = sampler
        self.excluded_pids = {os.getpid(), *excluded_pids}
        self.fd = None
        self.status = "UNSUPPORTED"
        self.reason = "broker cgroup v2 mount is unavailable"
        try:
            membership = sampler.read("cgroup")
            group = next(line[3:] for line in membership.splitlines() if line.startswith("0::"))
            for line in sampler.read("mountinfo").splitlines():
                before, after = line.split(" - ", 1)
                if after.split()[0] != "cgroup2":
                    continue
                root, mount = before.split()[3:5]
                if "\\" in root + mount + group or ".." in PurePosixPath(group).parts:
                    continue
                relative = PurePosixPath(group).relative_to(root)
                location = "root/" + str(PurePosixPath(mount).relative_to("/") / relative)
                fd = os.open(location, os.O_RDONLY | os.O_DIRECTORY, dir_fd=sampler.proc)
                self.fd = fd
                try:
                    self.members()
                except (OSError, ValueError, RuntimeError):
                    os.close(fd)
                    self.fd = None
                    continue
                self.membership = membership
                self.status, self.reason = "VERIFIED", "broker is a member; runner is not"
                break
        except (OSError, ValueError, StopIteration):
            self.reason = "broker cgroup attribution could not be verified"

    def read(self, name):
        with os.fdopen(os.open(name, os.O_RDONLY, dir_fd=self.fd)) as source:
            value = source.read(64 * 1024 + 1)
        if len(value) > 64 * 1024:
            raise ValueError("cgroup counter exceeds its read limit")
        return value

    def members(self):
        members = {int(value) for value in self.read("cgroup.procs").split()}
        if self.sampler.pid not in members or self.excluded_pids.intersection(members):
            raise RuntimeError("cgroup is not attributable to the broker")

    def collect(self):
        if self.fd is None:
            return {"status": self.status, "reason": self.reason}
        try:
            self.sampler.check_alive()
            if self.sampler.read("cgroup") != self.membership:
                raise RuntimeError("broker changed cgroup")
            self.members()
            maximum = self.read("memory.max").strip()
            breakdown = {key: int(value) for key, value in (line.split() for line in self.read("memory.stat").splitlines())
                         if key in ("anon", "file", "kernel", "sock", "shmem", "slab")}
            events = {key: int(value) for key, value in (line.split() for line in self.read("memory.events").splitlines())
                      if key in ("oom", "oom_kill", "oom_group_kill")}
            values = {"currentBytes": int(self.read("memory.current")), "breakdown": breakdown, "events": events,
                      "limitBytes": None if maximum == "max" else int(maximum)}
            numbers = [values["currentBytes"], *breakdown.values(), *events.values()]
            if values["limitBytes"] is not None:
                numbers.append(values["limitBytes"])
            if any(number < 0 for number in numbers) or not {"oom", "oom_kill"} <= events.keys():
                raise ValueError("invalid cgroup memory counters")
            self.members()
            self.sampler.check_alive()
            return {"status": "VERIFIED", **values}
        except (OSError, ValueError, RuntimeError):
            self.status, self.reason = "ERROR", "verified broker cgroup counters became unavailable"
            os.close(self.fd)
            self.fd = None
            return {"status": self.status, "reason": self.reason}

    def close(self):
        if self.fd is not None:
            os.close(self.fd)
            self.fd = None
