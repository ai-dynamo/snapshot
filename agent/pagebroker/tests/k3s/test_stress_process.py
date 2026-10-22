# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

from dataclasses import replace
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import patch

import stress
from stress_config import Clock, EvidenceOptions, RunSpec, Timing
from stress_control import Channel, Peer
from stress_evidence import ResourceObservation
from stress_evidence import validate_directory
from stress_storage import EvidenceStore
from monitor import ProcessSampler
from test_run import report
from test_stress import specification


HERE = Path(__file__).resolve().parent


def wait_for(predicate, timeout=8):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        value = predicate()
        if value:
            return value
        time.sleep(0.01)
    raise TimeoutError("fixture condition did not become true")


def read_if_present(path):
    try:
        return json.loads(path.read_text())
    except (FileNotFoundError, json.JSONDecodeError):
        return None


class FixtureSampler(ProcessSampler):
    def __init__(self, *args):
        super().__init__(*args)
        self.staging_is_mount = True
        self.calls = 0

    def collect(self):
        self.calls += 1
        if os.getenv("FIXTURE_SAMPLER_FAIL") and self.calls >= 6:
            raise PermissionError("fixture sampler failure")
        value = super().collect()
        value.update(stagingFilesystemUsedBytes=0, stagingFilesystemUsedInodes=len(list(self.staging.iterdir())))
        return value


class FixtureObservation(ResourceObservation):
    def baseline(self, peer, spec):
        if not (Path(spec.staging) / "permanent").is_dir():
            raise AssertionError("baseline preceded protocol readiness")
        super().baseline(peer, spec)


def fixture_broker(spec):
    if os.getenv("FIXTURE_IGNORE_TERM"):
        signal.signal(signal.SIGTERM, signal.SIG_IGN)
    else:
        signal.signal(signal.SIGTERM, lambda *_: sys.exit(0))
    with socket.socket(socket.AF_UNIX) as server:
        server.bind(spec.socket)
        server.listen()
        server.settimeout(0.02)
        ready = time.monotonic() + float(os.getenv("FIXTURE_READY_DELAY", "0"))
        while True:
            if time.monotonic() >= ready:
                (Path(spec.staging) / "permanent").mkdir(exist_ok=True)
            try:
                connection, _ = server.accept()
                connection.close()
            except socket.timeout:
                pass


def fixture_client():
    from run import write_json
    if "-test.run=^TestPageBrokerK3sReady$" in sys.argv:
        if os.getenv("FIXTURE_SKIP_READY"):
            return 0
        wait_for(lambda: (Path(os.environ["PAGEBROKER_SMOKE_STAGING_ROOT"]) / "permanent").exists())
        write_json(Path(os.environ["PAGEBROKER_READY_REPORT"]), {
            "status": "SUCCESS", "runId": os.environ["PAGEBROKER_READY_RUN_ID"],
            "socket": os.environ["PAGEBROKER_READY_SOCKET"], "customStorageAvailable": False})
        return 0
    path = Path(os.environ["PAGEBROKER_SMOKE_REPORT"])
    (Path(os.environ["FIXTURE_CONTROL"]) / ("client-started-" + path.parent.name)).touch()
    if os.getenv("FIXTURE_CLIENT_SLEEP"):
        time.sleep(float(os.environ["FIXTURE_CLIENT_SLEEP"]))
    if os.getenv("FIXTURE_LOG_BYTES"):
        print("x" * int(os.environ["FIXTURE_LOG_BYTES"]), flush=True)
    value = report(os.environ["PAGEBROKER_SMOKE_MODE"])
    value["config"].update(socket=os.environ["PAGEBROKER_SMOKE_SOCKET"],
                           stagingRoot=os.environ["PAGEBROKER_SMOKE_STAGING_ROOT"],
                           storageRoot=os.environ["PAGEBROKER_SMOKE_STORAGE_ROOT"])
    if os.environ["PAGEBROKER_SMOKE_MODE"] == "s3":
        value["config"]["storeId"] = os.environ["PAGEBROKER_SMOKE_STORE_ID"]
        for publication in value["publications"]:
            publication["artifact"]["store_id"] = os.environ["PAGEBROKER_SMOKE_STORE_ID"]
    for index, publication in enumerate(value["publications"]):
        publication["destination"] = str(Path(value["config"]["storageRoot"]) / f"publication-{index}")
    write_json(path, value)
    return 0


def fixture_runner(spec):
    from s3_cleanup import CleanupChannel
    os.environ["FIXTURE_CONTROL"] = spec.control
    runtime = stress.LocalRuntime(spec, [sys.executable, str(Path(__file__).resolve()), "--fixture-client"],
                                  observation=None if os.getenv("FIXTURE_NO_OBSERVER") else
                                  FixtureObservation(spec, sampler_factory=FixtureSampler),
                                  cleanup=CleanupChannel(spec) if spec.mode == "s3" and not os.getenv("FIXTURE_NO_CLEANUP") else None)
    def interrupted(_signum, _frame):
        runtime.signal_received = True
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    # These process tests use a temporary directory; real admission requires a mount.
    with patch("stress.staging_usage", lambda path: (0, len(list(Path(path).iterdir())))):
        return stress.run(spec, runtime)


class ProcessTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="pb-stress-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        timing = Timing(startup=3, warmup_min=0.2, warmup_max=1, warmup_batches=1,
                        measured=0.9, measured_batches=1, cadence=0.12, idle=0.06, cooldown=0.1,
                        batch=1, cleanup=0.1, drain=1.1, shutdown=2, margin=0.2, job=9,
                        poll=0.01, heartbeat=0.05, stale=0.7, broker_term=0.15,
                        kill_wait=0.1, client_term=0.1)
        self.spec = specification(self.root, timing, Clock(), evidence=EvidenceOptions(interval=0.1))
        self.processes = []
        self.logs = []
        self.addCleanup(self.cleanup)
        Path(self.spec.staging).mkdir()
        Path(self.spec.storage).mkdir()
        self.path = self.root / "spec.json"
        self.save_spec()

    def save_spec(self):
        self.path.write_text(json.dumps(self.spec.as_dict()))

    def cleanup(self):
        for process in self.processes:
            if process.poll() is None:
                process.kill()
            process.wait(timeout=3)
        # A killed runner can leave a gated client until supervision notices it.
        registration = read_if_present(Path(self.spec.control) / "client.json")
        if registration and registration.get("active"):
            try:
                peer = Peer.open(registration["peer"])
                peer.send(signal.SIGKILL)
                peer.close()
            except (ProcessLookupError, RuntimeError, FileNotFoundError):
                pass
        for log in self.logs:
            log.close()

    def spawn(self, role, **environment):
        log = (self.root / f"{role}-{len(self.processes)}.log").open("w+")
        self.logs.append(log)
        command = ([sys.executable, str(HERE / "stress_supervisor.py"), "--spec", str(self.path)]
                   if role == "supervisor" else
                   [sys.executable, str(Path(__file__).resolve()), "--fixture-" + role, str(self.path)])
        process = subprocess.Popen(command, stdout=log, stderr=subprocess.STDOUT,
                                   env=dict(os.environ) | {"PYTHONDONTWRITEBYTECODE": "1"} | environment)
        self.processes.append(process)
        return process

    def start(self, broker_environment=None, runner_environment=None):
        self.broker = self.spawn("broker", **(broker_environment or {}))
        wait_for(lambda: Path(self.spec.socket).exists())
        self.supervisor = self.spawn("supervisor")
        self.runner = self.spawn("runner", **(runner_environment or {}))

    def summary(self):
        return json.loads((Path(self.spec.results) / "summary.json").read_text())

    def assert_finished(self, success):
        expected = 0 if success else 1
        code = self.runner.wait(timeout=8)
        if code != expected:
            self.fail(f"runner exited {code}: {self.summary()}; logs: {self.read_logs()}")
        self.supervisor.wait(timeout=3)
        self.broker.wait(timeout=3)
        return self.summary()

    def read_logs(self):
        result = []
        for log in self.logs:
            log.seek(0)
            result.append(log.read())
        return result

    def active_client(self):
        directory = Path(self.spec.control)
        wait_for(lambda: list(directory.glob("client-started-*")))
        value = read_if_present(Path(self.spec.control) / "client.json")
        self.assertTrue(value["active"])
        return Peer.open(value["peer"])

    def test_one_broker_across_multiple_clients_and_delayed_readiness(self):
        self.start(broker_environment={"FIXTURE_READY_DELAY": "0.15"})
        summary = self.assert_finished(True)
        self.assertEqual(summary["executionStatus"], "SUCCESS")
        self.assertEqual(summary["evidenceStatus"], "COMPLETE")
        self.assertEqual(summary["resourceAssessment"], "INCONCLUSIVE")
        self.assertEqual(summary["kubernetesCollection"], "PENDING_COLLECTION")
        self.assertEqual(summary["shutdown"]["broker"]["pid"], self.broker.pid)
        self.assertGreater(summary["counts"]["measured"], 1)
        reports = list((Path(self.spec.results) / "batches").glob("*/client-report.json"))
        ids = [json.loads(path.read_text())["runId"] for path in reports]
        self.assertEqual(len(ids), len(set(ids)))
        self.assertEqual(self.supervisor.returncode, 0)
        self.assertEqual(self.broker.returncode, 0)

    def test_missing_observation_fails_before_batch_admission(self):
        self.start(runner_environment={"FIXTURE_NO_OBSERVER": "1"})
        summary = self.assert_finished(False)
        self.assertIn("step 2", summary["errors"][0])
        self.assertEqual(list((Path(self.spec.results) / "batches").iterdir()), [])

    def test_absolute_limit_returns_nonzero_with_separate_functional_success(self):
        self.spec = replace(self.spec, evidence=replace(self.spec.evidence, absolute_limits={"rssBytes": 1}))
        self.save_spec()
        self.start()
        self.assertEqual(self.runner.wait(timeout=8), 2)
        self.supervisor.wait(timeout=3)
        self.broker.wait(timeout=3)
        summary = self.summary()
        self.assertEqual(summary["executionStatus"], "SUCCESS")
        self.assertEqual(summary["evidenceStatus"], "COMPLETE")
        self.assertEqual(summary["resourceAssessment"], "INCONCLUSIVE")
        self.assertTrue(summary["resourceLimitsExceeded"])
        self.assertEqual(self.supervisor.returncode, 0)
        self.assertEqual(self.broker.returncode, 0)

    def test_s3_requires_cleanup_helper_before_admission(self):
        from stress_config import S3Options
        settings = S3Options(bucket="local-stress-test", prefix="test/" + self.spec.run_id,
                             region="us-east-1", endpoint="https://127.0.0.1:1",
                             credential_expiry=self.spec.created_at + 7200)
        self.spec = replace(self.spec, mode="s3", store_id="stress-" + self.spec.run_id, s3=settings)
        self.save_spec()
        self.start(runner_environment={"FIXTURE_NO_CLEANUP": "1"})
        summary = self.assert_finished(False)
        self.assertIn("cleanup helper", summary["errors"][0])
        self.assertFalse((Path(self.spec.results) / "readiness.json").exists())

    def test_skipped_readiness_is_not_success(self):
        self.start(runner_environment={"FIXTURE_SKIP_READY": "1"})
        summary = self.assert_finished(False)
        self.assertEqual(summary["counts"]["warmup"], 0)
        self.assertTrue(summary["errors"])

    def test_incomplete_client_report_cannot_pass_independent_validation(self):
        self.start()
        summary = self.assert_finished(True)
        directory = Path(self.spec.results)
        path = next((directory / "batches").glob("*/client-report.json"))
        value = json.loads(path.read_text())
        value["status"] = "RUNNING"
        path.write_text(json.dumps(value))
        EvidenceStore(directory, self.spec.evidence).manifest(self.spec.run_id)
        self.assertEqual(validate_directory(directory, self.spec, summary)["evidenceStatus"], "ERROR")
        value["status"] = "SUCCESS"
        path.write_text(json.dumps(value))
        metadata_path = directory / "resource-metadata.json"
        metadata = json.loads(metadata_path.read_text())
        metadata["status"] = "RUNNING"
        metadata_path.write_text(json.dumps(metadata))
        EvidenceStore(directory, self.spec.evidence).manifest(self.spec.run_id)
        self.assertEqual(validate_directory(directory, self.spec, summary)["evidenceStatus"], "INCOMPLETE")
        altered = replace(self.spec, evidence=replace(self.spec.evidence, growth_limits={"rssBytes": 0}))
        self.assertEqual(validate_directory(directory, altered, summary)["evidenceStatus"], "ERROR")
        summary["executionStatus"] = "RUNNING"
        self.assertEqual(validate_directory(directory, self.spec, summary)["evidenceStatus"], "INCOMPLETE")

    def test_client_log_limit_preserves_bounded_failure_evidence(self):
        self.spec = replace(self.spec, evidence=replace(self.spec.evidence, client_log_bytes=1024))
        self.save_spec()
        self.start(runner_environment={"FIXTURE_LOG_BYTES": "65536"})
        summary = self.assert_finished(False)
        self.assertIn("log exceeded", summary["errors"][0])
        directory = Path(self.spec.results)
        self.assertLessEqual((directory / "failure-client-tail.log").stat().st_size, 4096)
        self.assertTrue(all(path.stat().st_size <= 1024 for path in (directory / "batches").glob("*/client.log")))

    def test_sampler_failure_stops_peers_and_cannot_report_complete_evidence(self):
        self.start(runner_environment={"FIXTURE_SAMPLER_FAIL": "1"})
        summary = self.assert_finished(False)
        self.assertIn("sampling failed", summary["errors"][0])
        self.assertNotEqual(summary["evidenceStatus"], "COMPLETE")
        metadata = json.loads((Path(self.spec.results) / "resource-metadata.json").read_text())
        self.assertEqual(metadata["status"], "ERROR")

    def test_evidence_budget_rejects_admission_and_preserves_terminal_report(self):
        self.spec = replace(self.spec, evidence=replace(self.spec.evidence, total_bytes=262144,
                            diagnostic_bytes=65536, terminal_bytes=65536, chunk_bytes=4096))
        self.save_spec()
        self.start()
        summary = self.assert_finished(False)
        self.assertIn("evidence capacity", summary["errors"][0])
        self.assertEqual(summary["executionStatus"], "ERROR")
        self.assertFalse(list((Path(self.spec.results) / "batches").glob("*/client-report.json")))

    def test_client_timeout_terminates_registered_activity(self):
        self.start(runner_environment={"FIXTURE_CLIENT_SLEEP": "3"})
        client = self.active_client()
        self.addCleanup(client.close)
        summary = self.assert_finished(False)
        self.assertIn("deadline", summary["errors"][0])
        with self.assertRaises(RuntimeError):
            client.check()

    def test_cooperative_stop_drains_active_batch(self):
        self.start(runner_environment={"FIXTURE_CLIENT_SLEEP": "0.15"})
        client = self.active_client()
        self.addCleanup(client.close)
        Channel(self.spec.control, self.spec.run_id).write("stop.json")
        summary = self.assert_finished(False)
        self.assertEqual(summary["executionStatus"], "INTERRUPTED")
        self.assertEqual(summary["counts"]["warmup"], 1)
        self.assertEqual(len(list((Path(self.spec.results) / "batches").iterdir())), 1)
        with self.assertRaises(RuntimeError):
            client.check()

    def test_sigterm_skips_cooperative_drain(self):
        self.start(runner_environment={"FIXTURE_CLIENT_SLEEP": "3"})
        client = self.active_client()
        self.addCleanup(client.close)
        self.runner.terminate()
        summary = self.assert_finished(False)
        self.assertEqual(summary["executionStatus"], "INTERRUPTED")
        self.assertEqual(summary["counts"]["warmup"], 0)

    def test_runner_death_stops_client_and_broker(self):
        self.start(runner_environment={"FIXTURE_CLIENT_SLEEP": "3"})
        client = self.active_client()
        self.addCleanup(client.close)
        self.runner.kill()
        self.runner.wait(timeout=3)
        self.assertEqual(self.supervisor.wait(timeout=3), 1)
        self.broker.wait(timeout=3)
        with self.assertRaises(RuntimeError):
            client.check()
        self.assertEqual(self.summary()["executionStatus"], "RUNNING")
        final = json.loads((Path(self.spec.results) / "supervisor.json").read_text())
        self.assertEqual(final["status"], "ERROR")

    def test_stalled_runner_is_not_masked_by_background_heartbeat(self):
        self.start(runner_environment={"FIXTURE_CLIENT_SLEEP": "3"})
        client = self.active_client()
        self.addCleanup(client.close)
        os.kill(self.runner.pid, signal.SIGSTOP)
        self.assertEqual(self.supervisor.wait(timeout=4), 1)
        self.runner.wait(timeout=3)
        self.broker.wait(timeout=3)
        final = json.loads((Path(self.spec.results) / "supervisor.json").read_text())
        self.assertIn("stale runner heartbeat", final["errors"])

    def test_supervisor_death_causes_emergency_shutdown(self):
        self.start(runner_environment={"FIXTURE_CLIENT_SLEEP": "3"})
        client = self.active_client()
        self.addCleanup(client.close)
        self.supervisor.kill()
        summary = self.assert_finished(False)
        self.assertEqual(summary["executionStatus"], "ERROR")
        self.assertIn("process", summary["errors"][0])
        with self.assertRaises(RuntimeError):
            client.check()

    def test_supervisor_stall_causes_emergency_shutdown(self):
        self.start(runner_environment={"FIXTURE_CLIENT_SLEEP": "3"})
        client = self.active_client()
        self.addCleanup(client.close)
        os.kill(self.supervisor.pid, signal.SIGSTOP)
        summary = self.assert_finished(False)
        self.assertIn("stale supervisor heartbeat", summary["errors"][0])
        self.assertEqual(self.supervisor.returncode, -signal.SIGKILL)

    def test_gate_cannot_execute_when_registration_owner_disappears(self):
        marker = self.root / "unexpected-work"
        process = subprocess.Popen([sys.executable, "-c", stress.GATE, sys.executable,
                                    "-c", f"from pathlib import Path; Path({str(marker)!r}).touch()"],
                                   stdin=subprocess.PIPE)
        self.processes.append(process)
        time.sleep(0.05)
        self.assertFalse(marker.exists())
        process.stdin.close()
        self.assertEqual(process.wait(timeout=2), 125)
        self.assertFalse(marker.exists())

    def test_forced_broker_shutdown_is_failure(self):
        self.start(broker_environment={"FIXTURE_IGNORE_TERM": "1"})
        summary = self.assert_finished(False)
        self.assertEqual(summary["executionStatus"], "ERROR")
        self.assertIn("PageBroker required SIGKILL", summary["shutdown"]["errors"])
        self.assertEqual(self.broker.returncode, -signal.SIGKILL)

    def test_socket_replacement_does_not_rebind_existing_peer(self):
        self.broker = self.spawn("broker")
        wait_for(lambda: Path(self.spec.socket).exists())
        peer = Peer.discover(self.spec.socket, 1)
        self.addCleanup(peer.close)
        Path(self.spec.socket).rename(self.root / "old.sock")
        replacement = self.spawn("broker")
        wait_for(lambda: Path(self.spec.socket).exists())
        with self.assertRaisesRegex(RuntimeError, "replacement"):
            peer.verify_socket(self.spec.socket)
        self.assertIsNone(self.broker.poll())
        self.assertIsNone(replacement.poll())


if __name__ == "__main__":
    if len(sys.argv) > 1 and sys.argv[1].startswith("--fixture-"):
        role = sys.argv[1][len("--fixture-"):]
        if role == "client":
            raise SystemExit(fixture_client())
        spec = RunSpec.load(sys.argv[2])
        raise SystemExit(fixture_broker(spec) if role == "broker" else fixture_runner(spec))
    unittest.main()
