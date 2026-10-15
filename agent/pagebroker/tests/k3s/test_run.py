# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

import copy
import json
import os
from pathlib import Path
from unittest.mock import patch
import subprocess
import sys
import tempfile
import time
import unittest
import uuid

import run


def report(mode):
    engines = ["model-streamer"] if mode == "s3" else ["posix-copy", "model-streamer"]
    return {
        "runId": str(uuid.uuid4()), "status": "SUCCESS", "payloadBytes": (128 << 20) + 37,
        "config": {"mode": mode, "storeId": "smoke-store"},
        "steps": [{"name": name, "status": "SUCCESS"} for name in run.expected_steps(mode)],
        "publications": [{"engine": engine, "destination": f"/storage/{engine}-{cycle}",
                          "artifact": {"store_id": "smoke-store", "artifact_handle": str(uuid.uuid4())}}
                         for engine in engines for cycle in range(3)],
    }


class ReportTest(unittest.TestCase):
    def test_required_steps_and_publications(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "report.json"
            for mode, count in (("filesystem", 21), ("s3", 15)):
                value = report(mode)
                self.assertEqual(len(value["steps"]), count)
                path.write_text(json.dumps(value))
                self.assertEqual(run.validate_report(path, mode, "smoke-store"), value)
                for mutation in ("incomplete", "skipped", "duplicate", "failed", "wrong-mode", "no-publications", "small-payload"):
                    with self.subTest(mode=mode, mutation=mutation):
                        broken = copy.deepcopy(value)
                        if mutation == "incomplete":
                            broken["status"] = "RUNNING"
                        elif mutation == "skipped":
                            broken["steps"].pop()
                        elif mutation == "duplicate":
                            broken["steps"][-1] = broken["steps"][0]
                        elif mutation == "failed":
                            broken["steps"][0]["status"] = "ERROR"
                        elif mutation == "wrong-mode":
                            broken["config"]["mode"] = "wrong-mode"
                        elif mutation == "no-publications":
                            broken["publications"] = []
                        elif mutation == "small-payload":
                            broken["payloadBytes"] = 1
                        path.write_text(json.dumps(broken))
                        with self.assertRaises(ValueError):
                            run.validate_report(path, mode, "smoke-store")

    def test_wrong_store_and_duplicate_artifacts(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "report.json"
            for mutation in ("store", "handle"):
                value = report("s3")
                if mutation == "store":
                    value["publications"][0]["artifact"]["store_id"] = "another-store"
                else:
                    value["publications"][0]["artifact"] = value["publications"][1]["artifact"]
                path.write_text(json.dumps(value))
                with self.assertRaises(ValueError):
                    run.validate_report(path, "s3", "smoke-store")


class LauncherTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="pb-smoke-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.socket = self.root / "pb.sock"
        (self.root / "staging").mkdir()
        self.environment = dict(os.environ) | {
            "PAGEBROKER_SMOKE_SOCKET": str(self.socket), "PAGEBROKER_SMOKE_MODE": "filesystem",
            "PAGEBROKER_SMOKE_STAGING_ROOT": str(self.root / "staging"),
        }
        self.broker = None

    def start_broker(self, ignore_term=False):
        script = """
import signal,socket,sys
signal.signal(signal.SIGTERM, signal.SIG_IGN if sys.argv[2]=='ignore' else lambda *_: sys.exit(0))
server=socket.socket(socket.AF_UNIX)
server.bind(sys.argv[1]); server.listen()
while True:
    connection,_=server.accept(); connection.close()
"""
        self.broker = subprocess.Popen([sys.executable, "-c", script, str(self.socket),
                                        "ignore" if ignore_term else "exit"])
        self.addCleanup(self.stop_broker)
        deadline = time.monotonic() + 5
        while not self.socket.exists():
            if time.monotonic() >= deadline or self.broker.poll() is not None:
                self.fail("fixture broker did not start")
            time.sleep(0.01)

    def stop_broker(self):
        if self.broker.poll() is None:
            self.broker.kill()
        self.broker.wait(timeout=5)

    def execute(self, script, **limits):
        return run.run([sys.executable, "-c", script], self.environment, self.root / "results",
                       startup_timeout=0.2, client_timeout=limits.get("client_timeout", 5),
                       shutdown_timeout=limits.get("shutdown_timeout", 1),
                       sample_interval=0.1, settle_seconds=limits.get("settle_seconds", 0.15))

    def successful_client(self):
        return ("import os; from pathlib import Path; "
                f"Path(os.environ['PAGEBROKER_SMOKE_REPORT']).write_text({json.dumps(report('filesystem'))!r})")

    def summary(self):
        return json.loads((self.root / "results/summary.json").read_text())

    def test_success_stops_the_exact_peer(self):
        self.start_broker()
        self.assertEqual(self.execute(self.successful_client()), 0)
        self.assertEqual(self.broker.wait(timeout=5), 0)
        self.assertEqual(self.summary()["pagebrokerPid"], self.broker.pid)
        self.assertEqual(self.summary()["stepsVerified"], 21)
        self.assertEqual(self.summary()["status"], "SUCCESS")
        resources = self.summary()["resources"]
        self.assertEqual(resources["status"], "SUCCESS")
        self.assertEqual(resources["leakAssessment"], "NOT_ASSESSED")
        records = [json.loads(line) for line in (self.root / "results/resources.jsonl").read_text().splitlines()]
        self.assertEqual(resources["sampleCount"], len(records))
        self.assertEqual(records[0]["phase"], "baseline")
        self.assertEqual(records[-1]["phase"], "settled")
        self.assertGreaterEqual(resources["durationSeconds"], 0.15)
        self.assertTrue(all(record["pid"] == self.broker.pid for record in records))

    def test_missing_report_fails_even_when_client_exits_zero(self):
        self.start_broker()
        self.assertEqual(self.execute("pass"), 1)
        self.assertEqual(self.broker.wait(timeout=5), 0)
        self.assertEqual(self.summary()["status"], "ERROR")

    def test_resource_permission_failure_fails_and_stops_peer(self):
        self.start_broker()
        with patch("monitor.ResourceMonitor.collect", side_effect=PermissionError("fixture proc denial")):
            self.assertEqual(self.execute(self.successful_client()), 1)
        self.assertEqual(self.broker.wait(timeout=5), 0)
        self.assertEqual(self.summary()["resources"]["status"], "ERROR")
        self.assertIn("fixture proc denial", self.summary()["errors"][0])
        self.assertFalse((self.root / "results/client.log").exists())

    def test_monitoring_error_during_workload_preserves_earlier_samples(self):
        self.start_broker()
        original = run.ResourceMonitor.collect
        calls = 0

        def collect(monitor):
            nonlocal calls
            calls += 1
            if calls == 3:
                raise OSError("fixture staging failure")
            return original(monitor)

        with patch("monitor.ResourceMonitor.collect", collect):
            self.assertEqual(self.execute("import time; time.sleep(60)"), 1)
        self.assertEqual(self.broker.wait(timeout=5), 0)
        self.assertEqual(self.summary()["resources"]["sampleCount"], 2)
        records = [json.loads(line) for line in (self.root / "results/resources.jsonl").read_text().splitlines()]
        self.assertEqual([record["status"] for record in records], ["SUCCESS", "SUCCESS", "ERROR"])

    def test_stale_resource_log_is_not_overwritten(self):
        self.start_broker()
        results = self.root / "results"
        results.mkdir()
        (results / "resources.jsonl").write_text("previous run\n")
        self.assertEqual(self.execute(self.successful_client()), 1)
        self.assertEqual(self.broker.wait(timeout=5), 0)
        self.assertEqual((results / "resources.jsonl").read_text(), "previous run\n")
        self.assertEqual(self.summary()["status"], "ERROR")

    def test_client_failure_preserves_logs_and_stops_peer(self):
        self.start_broker()
        self.assertEqual(self.execute("print('fixture failure'); raise SystemExit(7)"), 1)
        self.assertEqual(self.summary()["clientExitCode"], 7)
        self.assertIn("fixture failure", (self.root / "results/client.log").read_text())
        self.assertEqual(self.broker.wait(timeout=5), 0)

    def test_client_timeout_stops_both_processes(self):
        self.start_broker()
        self.assertEqual(self.execute("import time; time.sleep(60)", client_timeout=0.1), 1)
        self.assertIn("deadline", self.summary()["errors"][0])
        self.assertEqual(self.broker.wait(timeout=5), 0)

    def test_broker_crash_is_not_hidden_by_client_success(self):
        self.start_broker()
        self.assertEqual(self.execute(f"import os,signal; os.kill({self.broker.pid},signal.SIGKILL)"), 1)
        self.assertEqual(self.summary()["status"], "ERROR")

    def test_broker_shutdown_timeout_fails(self):
        self.start_broker(ignore_term=True)
        self.assertEqual(self.execute(self.successful_client(), shutdown_timeout=0.1), 1)
        self.assertIn("SIGKILL", self.summary()["errors"][-1])
        self.assertEqual(self.broker.wait(timeout=5), -9)

    def test_startup_failure_is_persisted(self):
        self.assertEqual(self.execute("pass"), 1)
        self.assertEqual(self.summary()["status"], "ERROR")

    def test_old_result_is_not_overwritten(self):
        results = self.root / "results"
        results.mkdir()
        original = '{"status":"SUCCESS","old":true}'
        (results / "summary.json").write_text(original)
        self.assertEqual(self.execute("pass"), 1)
        self.assertEqual((results / "summary.json").read_text(), original)


if __name__ == "__main__":
    unittest.main()
