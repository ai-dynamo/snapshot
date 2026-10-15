# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

from dataclasses import replace
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import uuid

import stress
from stress_config import RunSpec, Timing
from stress_control import Channel
from stress_supervisor import Supervisor
from test_run import report


class FakeClock:
    def __init__(self):
        self.now = 0.0

    def monotonic(self):
        return self.now

    def time(self):
        return 1000 + self.now

    def sleep(self, seconds):
        if seconds < 0:
            raise ValueError("sleep length must be non-negative")
        self.now += seconds


def specification(root, timing=None, clock=None, **changes):
    clock = clock or FakeClock()
    created_at = clock.time()
    timing = timing or Timing(startup=10, warmup_min=6, warmup_max=20, warmup_batches=2,
                              measured=30, measured_batches=3, cadence=5, idle=2, cooldown=3,
                              batch=4, cleanup=1, drain=5, shutdown=5, margin=2, job=90,
                              poll=0.01, heartbeat=0.05, stale=1, broker_term=0.2,
                              kill_wait=0.1, client_term=0.2)
    value = RunSpec(run_id=str(uuid.uuid4()), mode="filesystem", socket=str(root / "pb.sock"),
                    staging=str(root / "staging"), storage=str(root / "storage"),
                    results=str(root / "results"), control=str(root / "control"),
                    created_at=created_at, startup_deadline=created_at + timing.startup,
                    expires_at=created_at + timing.job, timing=timing)
    return replace(value, **changes)


class FakeRuntime:
    def __init__(self, spec):
        self.clock = FakeClock()
        self.spec = spec
        self.starts, self.idles = [], []
        self.shutdowns = 0
        self.durations = []
        self.stop_at = None
        self.duplicate = False
        self.dead_at = None
        self.closed = False

    def initialize(self, startup, expiry):
        self.startup, self.expiry = startup, expiry

    def event(self, phase, number=0):
        if self.dead_at is not None and self.clock.now >= self.dead_at:
            raise RuntimeError("registered broker exited")

    def wait_until(self, deadline, phase, number=0):
        if self.stop_at is not None and deadline >= self.stop_at:
            self.clock.now = max(self.clock.now, self.stop_at)
            raise InterruptedError("cooperative stop requested")
        if self.dead_at is not None and deadline >= self.dead_at:
            self.clock.now = self.dead_at
            raise RuntimeError("registered broker exited")
        started = self.clock.now
        self.clock.now = max(self.clock.now, deadline)
        if "idle" in phase:
            self.idles.append((phase, started, self.clock.now))

    def batch(self, number, phase, deadline):
        self.event(phase, number)
        self.starts.append((phase, self.clock.now))
        duration = self.durations.pop(0) if self.durations else 1
        if self.clock.now + duration >= deadline:
            self.clock.now = deadline
            raise TimeoutError("client execution exceeded its deadline")
        self.clock.sleep(duration)
        value = report("filesystem")
        value["runId"] = str(uuid.uuid5(uuid.UUID(self.spec.run_id), "duplicate" if self.duplicate else str(number)))
        directory = Path(self.spec.results) / "batches" / f"{number:06d}"
        directory.mkdir()
        (directory / "client-report.json").write_text(json.dumps(value))
        return value

    def final(self):
        self.event("final-sample")

    def shutdown(self, status):
        self.shutdowns += 1
        return {"status": "SUCCESS"}

    def complete(self):
        pass

    def close(self):
        self.closed = True


class ConfigTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.spec = specification(self.root)

    def test_default_budget_and_spec_round_trip(self):
        self.assertEqual(Timing().worst_case, 18000)
        Timing().validate()
        path = self.root / "spec.json"
        path.write_text(json.dumps(self.spec.as_dict()))
        self.assertEqual(RunSpec.load(path), self.spec)

    def test_invalid_profiles_fail_before_admission(self):
        changes = [dict(job=20), dict(idle=float("nan")), dict(batch=-1), dict(warmup_batches=1.5),
                   dict(poll=2), dict(stale=0.01), dict(warmup_min=30), dict(drain=0.1),
                   dict(shutdown=0.1), dict(measured_batches=100)]
        for change in changes:
            with self.subTest(change=change), self.assertRaises(ValueError):
                replace(self.spec, timing=replace(self.spec.timing, **change)).validate()

    def test_paths_id_and_absolute_budgets(self):
        for change in (dict(run_id="bad"), dict(socket="relative"), dict(results=self.spec.control),
                       dict(staging=self.spec.storage + "/nested"), dict(schema_version=2),
                       dict(startup_deadline=2000), dict(expires_at=1020), dict(created_at=float("nan"))):
            with self.subTest(change=change), self.assertRaises(ValueError):
                replace(self.spec, **change).validate()

    def test_late_start_does_not_reset_deadlines(self):
        clock = FakeClock()
        clock.sleep(4)
        self.assertEqual(self.spec.deadlines(clock), (10, 90))
        clock.sleep(6)
        with self.assertRaisesRegex(TimeoutError, "expired"):
            self.spec.deadlines(clock)


class LifecycleTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.spec = specification(Path(self.directory.name))
        self.runtime = FakeRuntime(self.spec)

    def summary(self):
        return json.loads((Path(self.spec.results) / "summary.json").read_text())

    def test_persistent_lifecycle_pacing_and_fresh_reports(self):
        self.assertEqual(stress.run(self.spec, self.runtime), 0)
        summary = self.summary()
        self.assertEqual(summary["executionStatus"], "SUCCESS")
        self.assertEqual(summary["resourceAssessment"], "INCONCLUSIVE")
        self.assertEqual(summary["evidenceStatus"], "INCOMPLETE")
        self.assertGreaterEqual(summary["counts"]["warmup"], 2)
        self.assertGreaterEqual(summary["counts"]["measured"], 3)
        starts = [value for _, value in self.runtime.starts]
        self.assertTrue(all(second - first >= 5 for first, second in zip(starts, starts[1:])))
        self.assertTrue(all(end - start >= 2 for phase, start, end in self.runtime.idles if phase == "warmup-idle"))
        reports = list((Path(self.spec.results) / "batches").glob("*/client-report.json"))
        ids = [json.loads(path.read_text())["runId"] for path in reports]
        self.assertEqual(len(ids), len(set(ids)))
        self.assertEqual(len(ids), len(list((Path(self.spec.results) / "client-ids").iterdir())))
        self.assertEqual(self.runtime.shutdowns, 1)
        self.assertTrue(self.runtime.closed)

    def test_slow_batches_do_not_catch_up(self):
        self.runtime.durations = [3] * 30
        self.assertEqual(stress.run(self.spec, self.runtime), 0)
        starts = [value for _, value in self.runtime.starts]
        self.assertTrue(all(second - first >= 5 for first, second in zip(starts, starts[1:])))

    def test_warmup_can_finish_while_waiting_for_next_cadence(self):
        spec = replace(self.spec, timing=replace(self.spec.timing, warmup_min=9, warmup_max=9.5))
        self.assertEqual(stress.run(spec, self.runtime), 0)
        self.assertEqual(self.summary()["counts"]["warmup"], 2)

    def test_duplicate_client_id_fails(self):
        self.runtime.duplicate = True
        self.assertEqual(stress.run(self.spec, self.runtime), 1)
        self.assertEqual(self.summary()["executionStatus"], "ERROR")
        self.assertEqual(self.runtime.shutdowns, 1)

    def test_timeout_and_warmup_deadline(self):
        self.runtime.durations = [100]
        self.assertEqual(stress.run(self.spec, self.runtime), 1)
        self.assertIn("deadline", self.summary()["errors"][0])
        self.assertEqual(self.runtime.shutdowns, 1)

    def test_insufficient_warmup_work_is_not_a_pass(self):
        timing = replace(self.spec.timing, warmup_min=1, warmup_max=4, warmup_batches=2)
        spec = replace(self.spec, timing=timing)
        self.assertEqual(stress.run(spec, self.runtime), 1)
        self.assertIn("warm-up", self.summary()["errors"][0])

    def test_interruption_stops_admission_and_shuts_down_once(self):
        self.runtime.stop_at = 15
        self.assertEqual(stress.run(self.spec, self.runtime), 1)
        self.assertEqual(self.summary()["executionStatus"], "INTERRUPTED")
        self.assertEqual(self.runtime.shutdowns, 1)
        self.assertTrue(all(start < 15 for _, start in self.runtime.starts))

    def test_exit_during_idle_cannot_be_repaired(self):
        self.runtime.dead_at = 2
        self.assertEqual(stress.run(self.spec, self.runtime), 1)
        self.assertIn("broker exited", self.summary()["errors"][0])
        self.assertEqual(len(self.runtime.starts), 1)

    def test_final_overlapping_batch_is_drain_work(self):
        timing = replace(self.spec.timing, measured=8, measured_batches=1, batch=8, drain=9)
        spec = replace(self.spec, timing=timing)
        self.runtime.durations = [1, 1, 1, 3]
        self.assertEqual(stress.run(spec, self.runtime), 0)
        self.assertEqual(self.summary()["counts"]["measured"], 1)
        self.assertEqual(self.summary()["counts"]["drain"], 1)

    def test_measured_work_minimum_is_required(self):
        self.runtime.durations = [3.5] * 30
        spec = replace(self.spec, timing=replace(self.spec.timing, measured_batches=6))
        self.assertEqual(stress.run(spec, self.runtime), 1)
        self.assertIn("minimum measured", self.summary()["errors"][0])

    def test_existing_result_is_not_overwritten(self):
        path = Path(self.spec.results)
        path.mkdir()
        (path / "summary.json").write_text("old evidence")
        self.assertEqual(stress.run(self.spec, self.runtime), 1)
        self.assertEqual((path / "summary.json").read_text(), "old evidence")
        self.assertEqual(self.runtime.shutdowns, 0)

    def test_missing_completion_acknowledgment_is_failure(self):
        def incomplete():
            raise TimeoutError("supervisor completion was not confirmed")
        self.runtime.complete = incomplete
        self.assertEqual(stress.run(self.spec, self.runtime), 1)
        self.assertEqual(self.summary()["executionStatus"], "ERROR")
        self.assertIn("completion", self.summary()["errors"][-1])
        self.assertTrue(self.runtime.closed)

    def test_reference_collection_cannot_extend_warmup(self):
        original = self.runtime.event
        def event(phase, number=0):
            if phase == "initial-reference":
                self.runtime.clock.sleep(100)
            original(phase, number)
        self.runtime.event = event
        self.assertEqual(stress.run(self.spec, self.runtime), 1)
        self.assertIn("warm-up deadline", self.summary()["errors"][0])

    def test_unconfirmed_exit_and_forced_kill_are_reported(self):
        supervisor = Supervisor(self.spec, self.runtime.clock)
        Path(self.spec.control).mkdir()
        signals = []
        class UnresponsivePeer:
            fd = 99
            send = staticmethod(signals.append)
        with patch("stress_supervisor.exited", return_value=False):
            supervisor.terminate(UnresponsivePeer(), "PageBroker", 0.2, 1)
        self.assertEqual(len(signals), 2)
        self.assertIn("PageBroker required SIGKILL", supervisor.errors)
        self.assertIn("PageBroker exit was not confirmed", supervisor.errors)
        self.assertLessEqual(self.runtime.clock.now, 1)

    def test_control_ownership_and_stale_heartbeat(self):
        Path(self.spec.control).mkdir()
        channel = Channel(self.spec.control, self.spec.run_id)
        channel.beat("runner", self.runtime.clock)
        channel.check_heartbeat("runner", self.runtime.clock, 1)
        self.runtime.clock.sleep(2)
        with self.assertRaisesRegex(RuntimeError, "stale"):
            channel.check_heartbeat("runner", self.runtime.clock, 1)
        with self.assertRaisesRegex(ValueError, "foreign"):
            Channel(self.spec.control, str(uuid.uuid4())).read("runner-heartbeat.json")

    def test_stop_and_expired_admission_cannot_spawn_a_client(self):
        Path(self.spec.control).mkdir()
        runtime = stress.LocalRuntime(self.spec, ["unused"], clock=self.runtime.clock)
        runtime.channel.write("stop.json")
        with patch("stress.subprocess.Popen") as spawn:
            with self.assertRaisesRegex(InterruptedError, "before client admission"):
                runtime.invoke(["unused"], {}, Path(self.spec.results) / "unused.log", 5)
            with self.assertRaisesRegex(TimeoutError, "admission"):
                runtime.invoke(["unused"], {}, Path(self.spec.results) / "unused.log", 0)
            spawn.assert_not_called()

    def test_control_poll_crossing_idle_deadline_does_not_sleep_backwards(self):
        runtime = stress.LocalRuntime(self.spec, ["unused"], clock=self.runtime.clock)
        runtime.pulse = lambda: self.runtime.clock.sleep(0.11)
        runtime.wait_until(0.1, "idle")
        self.assertGreaterEqual(self.runtime.clock.now, 0.1)


if __name__ == "__main__":
    unittest.main()
