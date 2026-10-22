# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

import os
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest

from stress_cgroup import BrokerCgroup


class CgroupAttributionTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.group = self.root / "root/sys/fs/cgroup"
        self.group.mkdir(parents=True)
        self.membership = "0::/\n"
        proc = os.open(self.root, os.O_RDONLY | os.O_DIRECTORY)
        self.addCleanup(os.close, proc)
        def read(name):
            return self.membership if name == "cgroup" else "1 0 0:1 / /sys/fs/cgroup rw - cgroup2 cgroup rw\n"
        self.sampler = SimpleNamespace(pid=999999, proc=proc, read=read, check_alive=lambda: None)
        for name, value in {"cgroup.procs": "999999\n", "memory.current": "42\n", "memory.max": "max\n",
                            "memory.stat": "anon 20\nfile 22\n", "memory.events": "oom 0\noom_kill 0\n"}.items():
            (self.group / name).write_text(value)

    def observer(self):
        value = BrokerCgroup(self.sampler)
        self.addCleanup(value.close)
        return value

    def test_broker_membership_and_runner_exclusion_are_required(self):
        value = self.observer().collect()
        self.assertEqual(value["status"], "VERIFIED")
        self.assertEqual(value["currentBytes"], 42)
        self.assertIsNone(value["limitBytes"])
        self.assertEqual(value["breakdown"], {"anon": 20, "file": 22})

    def test_client_or_parent_cgroup_is_never_reported_as_broker_memory(self):
        for members in (f"999999\n{os.getpid()}\n", f"{os.getpid()}\n", "1111\n"):
            (self.group / "cgroup.procs").write_text(members)
            value = self.observer().collect()
            self.assertEqual(value["status"], "UNSUPPORTED")
            self.assertNotIn("currentBytes", value)

    def test_membership_change_invalidates_verified_counters(self):
        observer = self.observer()
        self.membership = "0::/different\n"
        value = observer.collect()
        self.assertEqual(value["status"], "ERROR")
        self.assertNotIn("currentBytes", value)

    def test_supervisor_or_helper_membership_is_also_excluded(self):
        (self.group / "cgroup.procs").write_text("999999\n888888\n")
        observer = BrokerCgroup(self.sampler, excluded_pids=(888888,))
        self.addCleanup(observer.close)
        self.assertEqual(observer.collect()["status"], "UNSUPPORTED")

    def test_disappearing_or_malformed_optional_counters_are_explicit(self):
        observer = self.observer()
        (self.group / "memory.current").write_text("-1\n")
        self.assertEqual(observer.collect()["status"], "ERROR")
        self.assertEqual(observer.collect()["status"], "ERROR")


if __name__ == "__main__":
    unittest.main()
