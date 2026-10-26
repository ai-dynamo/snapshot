# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

from dataclasses import replace
import json
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import stress
from stress_analysis import analyze_resources
from stress_config import EvidenceOptions, RESOURCE_METRICS, Timing
from stress_evidence import ResourceObservation
from stress_storage import EvidenceLimitError, EvidenceStore, read_json, validate_manifest
from test_stress import FakeClock, specification


class ResourceFixture:
    def __init__(self, root, growth="stable", options=None):
        self.root = root
        self.clock = FakeClock()
        self.spec = specification(root, Timing(), evidence=options or EvidenceOptions(interval=10, retention_revision="fixture-revision"))
        self.results = Path(self.spec.results)
        self.results.mkdir()
        self.store = EvidenceStore(self.results, self.spec.evidence)
        self.peer = SimpleNamespace(pid=123, ticks=456, fd=-1, record=lambda: {"pid": 123, "startTicks": 456})
        fixture = self
        class Sampler:
            start_ticks = 456
            staging_is_mount = True
            def collect(self):
                now = fixture.clock.now
                increase = 0 if growth in ("stable", "handles") else 100 * (min(now, 4200) if growth == "plateau" else now)
                result = {name: int(1000000 + increase) for name in RESOURCE_METRICS}
                result.update(fileDescriptors=12, threads=4, stagingFilesystemUsedBytes=0, stagingFilesystemUsedInodes=3)
                if growth == "handles":
                    result.update(fileDescriptors=12 + int(now / 300), threads=4 + int(now / 600))
                return result
            def read(self, _name):
                return ""
            def diagnostics(self):
                return {"descriptorTypes": {"file": 12}, "threadNames": {"worker": 4}}
            def close(self):
                pass
        self.sampler = Sampler()
        self.observer = ResourceObservation(self.spec, self.clock, lambda *_: self.sampler)
        self.observer.bind(self.store)
        self.observer.attach(self.peer, self.spec)
        self.observer.baseline(self.peer, self.spec)
        self.current = None
        self.window_start = self.previous = 0
        self.summary = {"runId": self.spec.run_id, "executionStatus": "SUCCESS", "measuredStarted": 600}

    def sample(self, when, phase, batch):
        key = (phase, batch)
        if self.current is not None and key != self.current and self.current[0] in ("warmup-idle", "idle", "final-idle"):
            self.observer.idle(*self.current, self.window_start, self.previous)
        if key != self.current:
            self.current, self.window_start = key, when
        self.clock.now = when
        self.observer.sample(phase, batch, when)
        self.previous = when

    def build(self, missing_hour=None, gap=False, short=False):
        for when in range(10, 601, 10):
            index = when // 200
            phase = "warmup-idle" if when % 200 <= 110 else "warmup"
            self.sample(when, phase, index)
        duration = 300 if short else 14400
        self.spec = replace(self.spec, timing=replace(self.spec.timing, measured=duration))
        for when in range(610, 601 + duration, 10):
            if gap and 10000 <= when <= 10100:
                continue
            elapsed = when - 600
            phase = "idle" if elapsed % 300 <= 120 else "measured"
            if missing_hour and (missing_hour - 1) * 3600 <= elapsed < missing_hour * 3600:
                phase = "measured"
            self.sample(when, phase, elapsed // 300 + 1)
        for when in range(610 + duration, 1201 + duration, 10):
            self.sample(when, "final-idle", 0)
        self.observer.idle("final-idle", 0, self.window_start, self.previous)
        self.observer.final()
        return self

    def analyze(self):
        return analyze_resources(self.results, self.spec, self.summary)


class ResourceEvidenceTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        sync = patch("stress_storage.os.fsync")
        sync.start()
        self.addCleanup(sync.stop)

    def fixture(self, **kwargs):
        value = ResourceFixture(self.root, **kwargs)
        self.addCleanup(value.observer.close)
        return value

    def test_stable_diagnostic_run_does_not_claim_leak_freedom(self):
        value = self.fixture().build()
        result = value.analyze()
        self.assertEqual(result["evidenceStatus"], "COMPLETE")
        self.assertEqual(result["resourceAssessment"], "INCONCLUSIVE")
        self.assertEqual(result["finalMinusLater"]["rssBytes"], 0)
        self.assertEqual(result["hours"]["4"]["metrics"]["rssBytes"]["slopePerHour"], 0)

    def test_warmup_and_cache_fill_followed_by_plateau(self):
        options = EvidenceOptions(interval=10, retention_revision="fixture", growth_limits={"rssBytes": 0})
        value = self.fixture(growth="plateau", options=options).build()
        result = value.analyze()
        self.assertGreater(result["finalMinusInitial"]["rssBytes"], 0)
        self.assertEqual(result["finalMinusLater"]["rssBytes"], 0)
        self.assertEqual(result["resourceAssessment"], "WITHIN_LIMITS")
        self.assertTrue(result["initialGrowthIncludesCacheFill"])

    def test_sustained_growth_with_and_without_predeclared_limits(self):
        value = self.fixture(growth="ongoing").build()
        result = value.analyze()
        self.assertEqual(result["resourceAssessment"], "INCONCLUSIVE")
        self.assertGreater(result["hours"]["4"]["metrics"]["rssBytes"]["slopePerHour"], 0)
        value.spec = replace(value.spec, evidence=replace(value.spec.evidence, growth_limits={"rssBytes": 1000}))
        result = value.analyze()
        self.assertEqual(result["resourceAssessment"], "GROWTH_DETECTED")
        self.assertEqual(result["breaches"][0]["metric"], "rssBytes")

    def test_absolute_ceilings_are_separate_from_idle_growth(self):
        value = self.fixture().build()
        value.spec = replace(value.spec, evidence=replace(value.spec.evidence, absolute_limits={"threads": 3}))
        result = value.analyze()
        self.assertEqual(result["resourceAssessment"], "INCONCLUSIVE")
        self.assertTrue(result["limitsExceeded"])
        self.assertEqual(result["breaches"][0]["kind"], "absolute")

    def test_optional_cgroup_failure_cannot_hide_observed_growth(self):
        value = self.fixture(growth="ongoing", options=EvidenceOptions(interval=10, retention_revision="fixture",
                             growth_limits={"rssBytes": 1000}))
        with patch.object(value.observer.cgroup, "collect", return_value={"status": "ERROR", "reason": "fixture loss"}):
            value.build()
        result = value.analyze()
        self.assertEqual(result["evidenceStatus"], "COMPLETE")
        self.assertEqual(result["resourceAssessment"], "GROWTH_DETECTED")
        self.assertTrue(result["limitsExceeded"])

    def test_short_run_enforces_absolute_limit_without_claiming_growth_coverage(self):
        value = self.fixture(options=EvidenceOptions(interval=10, absolute_limits={"threads": 3})).build(short=True)
        result = value.analyze()
        self.assertTrue(result["limitsExceeded"])
        self.assertEqual(result["resourceAssessment"], "INCONCLUSIVE")
        self.assertEqual(result["evidenceStatus"], "COMPLETE")

    def test_missing_growth_limits_do_not_produce_within_limits(self):
        value = self.fixture(options=EvidenceOptions(interval=10, retention_revision="fixture", absolute_limits={"threads": 100})).build()
        result = value.analyze()
        self.assertFalse(result["limitsExceeded"])
        self.assertEqual(result["resourceAssessment"], "INCONCLUSIVE")

    def test_descriptor_and_thread_growth_use_count_limits(self):
        value = self.fixture(growth="handles", options=EvidenceOptions(interval=10, retention_revision="fixture",
                             growth_limits={"fileDescriptors": 0, "threads": 0})).build()
        result = value.analyze()
        self.assertEqual(result["resourceAssessment"], "GROWTH_DETECTED")
        self.assertEqual({item["metric"] for item in result["breaches"]}, {"fileDescriptors", "threads"})

    def test_missing_late_windows_cannot_pass_resource_limits(self):
        value = self.fixture(options=EvidenceOptions(interval=10, retention_revision="fixture", growth_limits={"rssBytes": 0})).build(missing_hour=3)
        result = value.analyze()
        self.assertEqual(result["evidenceStatus"], "INCOMPLETE")
        self.assertEqual(result["resourceAssessment"], "INCONCLUSIVE")
        self.assertEqual(result["hours"]["3"]["windows"], 0)

    def test_short_profile_and_unknown_retention_remain_inconclusive(self):
        value = self.fixture(options=EvidenceOptions(interval=10, growth_limits={"rssBytes": 0})).build(short=True)
        result = value.analyze()
        self.assertEqual(result["evidenceStatus"], "COMPLETE")
        self.assertEqual(result["resourceAssessment"], "INCONCLUSIVE")
        self.assertTrue(any("short profile" in reason for reason in result["reasons"]))

    def test_sampling_gap_remains_visible(self):
        value = self.fixture().build(gap=True)
        result = value.analyze()
        self.assertGreater(result["samplingGaps"], 0)
        self.assertEqual(result["evidenceStatus"], "INCOMPLETE")

    def test_missing_reordered_or_replaced_process_samples_are_rejected(self):
        value = self.fixture().build(short=True)
        path = value.results / "resources/000001.jsonl"
        original = path.read_text().splitlines(keepends=True)
        for mutation in ("missing", "reordered", "identity"):
            lines = list(original)
            if mutation == "missing":
                del lines[20]
            elif mutation == "reordered":
                lines[20], lines[21] = lines[21], lines[20]
            else:
                record = json.loads(lines[20])
                record["processStartTicks"] += 1
                lines[20] = json.dumps(record) + "\n"
            path.write_text("".join(lines))
            with self.subTest(mutation=mutation), self.assertRaises(ValueError):
                value.analyze()

    def test_idle_medians_are_independently_recomputed(self):
        value = self.fixture().build(short=True)
        path = value.results / "idle-windows.jsonl"
        lines = path.read_text().splitlines()
        item = json.loads(lines[0])
        item["medians"]["rssBytes"] += 100
        lines[0] = json.dumps(item)
        path.write_text("\n".join(lines) + "\n")
        with self.assertRaisesRegex(ValueError, "medians"):
            value.analyze()

    def test_staging_growth_between_batches_is_always_an_error(self):
        value = self.fixture().build(short=True)
        path = value.results / "resources/000001.jsonl"
        lines = path.read_text().splitlines()
        for index, line in enumerate(lines):
            record = json.loads(line)
            if record["phase"] == "idle":
                record["metrics"]["stagingFilesystemUsedBytes"] = 4096
                lines[index] = json.dumps(record)
                break
        path.write_text("\n".join(lines) + "\n")
        with self.assertRaisesRegex(ValueError, "staging did not return"):
            value.analyze()

    def test_sampler_failure_is_preserved_in_terminal_reserve(self):
        value = self.fixture()
        value.clock.sleep(10)
        with patch.object(value.sampler, "collect", side_effect=PermissionError("fixture denial")):
            with self.assertRaisesRegex(RuntimeError, "sampling failed"):
                value.observer.sample("warmup", 1, 10)
        self.assertEqual(read_json(value.results, "resource-metadata.json")["status"], "ERROR")
        with self.assertRaisesRegex(ValueError, "sampler reported"):
            value.analyze()

    def test_unavailable_optional_diagnostics_do_not_replace_required_metrics(self):
        value = self.fixture()
        with patch.object(value.sampler, "diagnostics", side_effect=ValueError("fixture thread name")):
            value.build(short=True)
        self.assertEqual(value.analyze()["evidenceStatus"], "COMPLETE")
        windows = (value.results / "idle-windows.jsonl").read_text().splitlines()
        self.assertTrue(all(json.loads(line)["diagnostics"]["status"] == "UNAVAILABLE" for line in windows))

    def test_resource_chunks_rotate_and_are_bounded(self):
        value = self.fixture(options=EvidenceOptions(interval=10, chunk_bytes=2048)).build(short=True)
        chunks = list((value.results / "resources").glob("*.jsonl"))
        self.assertGreater(len(chunks), 1)
        self.assertTrue(all(path.stat().st_size <= 2048 for path in chunks))
        self.assertEqual(value.analyze()["evidenceStatus"], "COMPLETE")


class EvidenceStorageTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.options = EvidenceOptions(total_bytes=16384, diagnostic_bytes=4096, terminal_bytes=4096,
                                       chunk_bytes=4096, report_bytes=256, client_log_bytes=1024)
        self.store = EvidenceStore(self.root, self.options)

    def test_ordinary_writers_cannot_consume_failure_reserve(self):
        self.store.write("batches.jsonl", b"x" * self.options.ordinary_bytes)
        with self.assertRaises(EvidenceLimitError):
            self.store.append("batches.jsonl", b"one byte too many")
        self.store.write("summary.json", {"executionStatus": "ERROR", "reason": "quota"})
        self.assertEqual(read_json(self.root, "summary.json")["executionStatus"], "ERROR")
        self.assertEqual((self.root / "batches.jsonl").stat().st_size, self.options.ordinary_bytes)

    def test_client_reservation_prevents_oversubscription(self):
        with self.store.reserve(8000):
            with self.assertRaises(EvidenceLimitError):
                self.store.write("readiness.log", b"x" * 500)
        self.store.write("readiness.log", b"x" * 500)

    def test_invalid_limits_and_evidence_profiles_are_rejected(self):
        for change in ({"interval": True}, {"interval": float("nan")}, {"max_files": 99999},
                       {"growth_limits": {"unknown": 1}}, {"growth_limits": {"rssBytes": -1}},
                       {"absolute_limits": {"rssBytes": float("inf")}}, {"retention_revision": 123}):
            with self.subTest(change=change), self.assertRaises(ValueError):
                replace(self.options, **change).validate()

    def test_manifest_rejects_mutation_extra_files_links_and_traversal(self):
        spec = specification(self.root, evidence=self.options)
        self.store.write("readiness.json", {"status": "SUCCESS"})
        self.store.write("summary.json", {})
        self.store.manifest(spec.run_id)
        validate_manifest(self.root, spec)
        self.store.write("readiness.json", {"status": "ERROR"})
        with self.assertRaisesRegex(ValueError, "mismatch"):
            validate_manifest(self.root, spec)
        self.store.write("readiness.json", {"status": "SUCCESS"})
        self.store.write("progress.json", {})
        with self.assertRaisesRegex(ValueError, "file set"):
            validate_manifest(self.root, spec)
        with self.assertRaises(ValueError):
            self.store.write("../outside", b"bad")
        (self.root / "resources").symlink_to(self.root, target_is_directory=True)
        with self.assertRaises(ValueError):
            self.store.manifest(spec.run_id)

    def test_progress_is_checkpointed_without_waiting_for_a_batch(self):
        clock = FakeClock()
        spec = specification(self.root)
        store = EvidenceStore(self.root, spec.evidence)
        Path(spec.control).mkdir()
        runtime = stress.LocalRuntime(spec, ["unused"], clock=clock)
        runtime.configure_evidence(store, {"executionStatus": "RUNNING", "counts": {"warmup": 0, "measured": 0, "drain": 0}})
        runtime.pulse()
        first = read_json(self.root, "progress.json")
        clock.sleep(10)
        runtime.pulse()
        second = read_json(self.root, "progress.json")
        self.assertEqual(second["monotonic"] - first["monotonic"], 10)
        self.assertEqual(second["executionStatus"], "RUNNING")


if __name__ == "__main__":
    unittest.main()
