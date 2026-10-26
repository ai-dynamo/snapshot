# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from monitor import ResourceMonitor, fields, number, validate_timing


class ResourceMonitorTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="pb-resource-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)

    def monitor(self, pid):
        pidfd = os.pidfd_open(pid)
        self.addCleanup(os.close, pidfd)
        monitor = ResourceMonitor(pid, pidfd, self.root, self.root / "resources.jsonl", 1)
        self.addCleanup(monitor.close)
        return monitor

    def test_memory_descriptors_and_threads_capture_growth_and_release(self):
        script = """
import sys,threading
files=[]; workers=[]; memory=None; stopped=threading.Event()
print('ready',flush=True)
for line in sys.stdin:
    if line.strip()=='grow':
        memory=bytearray(24*1024*1024)
        files=[open('/dev/null') for _ in range(12)]
        workers=[threading.Thread(target=stopped.wait) for _ in range(3)]
        for worker in workers: worker.start()
    else:
        for file in files: file.close()
        stopped.set()
        for worker in workers: worker.join()
        memory=None
    print('ready',flush=True)
"""
        with subprocess.Popen([sys.executable, "-c", script], stdin=subprocess.PIPE,
                              stdout=subprocess.PIPE, text=True) as process:
            try:
                self.assertEqual(process.stdout.readline().strip(), "ready")
                monitor = self.monitor(process.pid)
                baseline = monitor.collect()
                monitor.sample("baseline", force=True)
                process.stdin.write("grow\n")
                process.stdin.flush()
                self.assertEqual(process.stdout.readline().strip(), "ready")
                grown = monitor.collect()
                monitor.sample("workload", force=True)
                self.assertGreaterEqual(grown["rssBytes"] - baseline["rssBytes"], 20 * 1024**2)
                self.assertGreaterEqual(grown["privateBytes"] - baseline["privateBytes"], 20 * 1024**2)
                self.assertEqual(grown["fileDescriptors"] - baseline["fileDescriptors"], 12)
                self.assertEqual(grown["threads"] - baseline["threads"], 3)
                process.stdin.write("release\n")
                process.stdin.flush()
                self.assertEqual(process.stdout.readline().strip(), "ready")
                released = monitor.collect()
                self.assertEqual(released["fileDescriptors"], baseline["fileDescriptors"])
                self.assertEqual(released["threads"], baseline["threads"])
                self.assertLess(released["rssBytes"], grown["rssBytes"] - 16 * 1024**2)
                monitor.sample("settled", force=True)
                summary = monitor.summary()
                self.assertEqual(summary["status"], "SUCCESS")
                self.assertEqual(summary["metrics"]["threads"]["peak"], grown["threads"])
                self.assertEqual(summary["metrics"]["fileDescriptors"]["delta"], 0)
                self.assertEqual(summary["leakAssessment"], "NOT_ASSESSED")
            finally:
                process.kill()
                process.wait(timeout=5)

    def test_filesystem_usage_units_aggregation_and_sampling_interval(self):
        monitor = self.monitor(os.getpid())
        usage = SimpleNamespace(f_blocks=1000, f_bfree=990, f_frsize=4096, f_files=100, f_ffree=95)
        with patch("monitor.os.statvfs", return_value=usage):
            monitor.sample("baseline", force=True)
            monitor.sample("workload")
            self.assertEqual(monitor.samples, 1)
            usage.f_bfree = 900
            usage.f_ffree = 80
            monitor.sample("workload", force=True)
            usage.f_bfree = 985
            usage.f_ffree = 94
            monitor.sample("settled", force=True)
        summary = monitor.summary()
        self.assertEqual(summary["metrics"]["stagingFilesystemUsedBytes"], {
            "first": 10 * 4096, "last": 15 * 4096, "peak": 100 * 4096, "delta": 5 * 4096,
        })
        self.assertEqual(summary["metrics"]["stagingFilesystemUsedInodes"], {
            "first": 5, "last": 6, "peak": 20, "delta": 1,
        })
        self.assertFalse(summary["stagingIsMountPoint"])
        records = [json.loads(line) for line in (self.root / "resources.jsonl").read_text().splitlines()]
        self.assertEqual(len(records), 3)
        self.assertEqual(len({row["processStartTicks"] for row in records}), 1)

    def test_missing_proc_metric_is_an_error_not_zero(self):
        monitor = self.monitor(os.getpid())
        monitor.sample("baseline", force=True)
        with patch.object(monitor, "read", return_value="Threads: 1\n"):
            with self.assertRaisesRegex(RuntimeError, "resource monitoring failed"):
                monitor.sample("workload", force=True)
        self.assertEqual(monitor.summary()["status"], "ERROR")
        self.assertEqual(monitor.summary()["sampleCount"], 1)

    def test_result_write_failure_is_reported(self):
        monitor = self.monitor(os.getpid())
        with patch.object(monitor.output, "write", side_effect=OSError("fixture disk full")):
            with self.assertRaisesRegex(RuntimeError, "fixture disk full"):
                monitor.sample("baseline", force=True)
        self.assertEqual(monitor.summary()["status"], "ERROR")
        self.assertEqual(monitor.summary()["sampleCount"], 0)

    def test_status_without_settled_sample_is_incomplete(self):
        monitor = self.monitor(os.getpid())
        monitor.sample("baseline", force=True)
        self.assertEqual(monitor.summary()["status"], "INCOMPLETE")

    def test_proc_units_and_invalid_timing_are_rejected(self):
        self.assertEqual(number(fields("Rss: 42 kB\n"), "Rss", "kB"), 42 * 1024)
        for value in ("42 MB", "-1 kB", "42", "unknown kB"):
            with self.subTest(value=value), self.assertRaises(ValueError):
                number(fields("Rss: " + value), "Rss", "kB")
        for interval, settle in ((0, 5), (61, 5), (float("nan"), 5), (1, -1), (1, 61), (1, float("inf"))):
            with self.subTest(interval=interval, settle=settle), self.assertRaises(ValueError):
                validate_timing(interval, settle)


if __name__ == "__main__":
    unittest.main()
