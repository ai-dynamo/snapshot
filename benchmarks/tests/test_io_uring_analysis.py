# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

import hashlib
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch


ANALYSIS = Path(__file__).resolve().parents[1] / "io-uring" / "analysis"
SPEC = importlib.util.spec_from_file_location("tail_report", ANALYSIS / "tail_report.py")
tail_report = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(tail_report)


class TailAnalysisTests(unittest.TestCase):
    @staticmethod
    def request_record(start_seconds, latency):
        """Build a synthetic request with no observed chunk gaps."""
        return {
            "metadata": {"request_start_ns": int(start_seconds * 1e9)},
            "metrics": {
                name: {"unit": "ms", "value": latency}
                for name in ("request_latency", "time_to_first_token", "decode_duration")
            },
        }

    def test_window_boundaries_and_missing_gaps(self):
        """Include the low boundary, exclude high, and keep absent gaps missing."""
        records = [
            self.request_record(0, 1),
            self.request_record(120, 10),
            self.request_record(299, 30),
            self.request_record(300, 999),
        ]
        result = tail_report.window_metrics(records, 120, 300)
        self.assertEqual(result["requests"], 2)
        self.assertEqual(result["request_start_window_s"], [120, 300])
        self.assertAlmostEqual(result["request_start_rate_per_second"], 2 / 180)
        self.assertTrue(result["sparse_p99"])
        for name in ("request_latency", "time_to_first_token", "decode_duration"):
            self.assertAlmostEqual(result[name + "_p99_ms"], 29.8)
        self.assertAlmostEqual(result["request_latency_p95_ms"], 29)
        self.assertIsNone(result["largest_chunk_gap_p99_ms"])
        self.assertEqual(result["requests_with_observed_chunk_gaps"], 0)
        self.assertEqual(result["requests_without_observed_chunk_gaps"], 2)

        records[1]["metrics"]["inter_chunk_latency"] = {"unit": "ms", "value": [2, 4]}
        records[2]["metrics"]["inter_chunk_latency"] = {"unit": "ms", "value": []}
        result = tail_report.window_metrics(records, 120, 300)
        self.assertEqual(result["largest_chunk_gap_p99_ms"], 4)
        self.assertEqual(result["requests_with_observed_chunk_gaps"], 1)
        self.assertEqual(result["requests_without_observed_chunk_gaps"], 1)

    def test_invalid_window_and_units(self):
        """Reject an empty window and latency values with unexpected units."""
        record = self.request_record(0, 10)
        with self.assertRaisesRegex(ValueError, "No requests in prespecified window"):
            tail_report.window_metrics([record], 1, 2)
        with self.assertRaisesRegex(ValueError, "Empty percentile sample"):
            tail_report.percentile([], 0.99)
        record["metrics"]["request_latency"]["unit"] = "seconds"
        with self.assertRaisesRegex(ValueError, "Unexpected latency units: request_latency"):
            tail_report.window_metrics([record], 0, 1)

    def test_sparse_sample_threshold(self):
        """Mark p99 as sparse below 1,000 requests, including at 999."""
        for count, sparse in ((999, True), (1000, False)):
            with self.subTest(count=count):
                result = tail_report.window_metrics([self.request_record(0, 10)] * count, 0, 1)
                self.assertEqual(result["sparse_p99"], sparse)

    def test_confirmation_requires_all_planned_pairs(self):
        """Keep partial primary samples inconclusive while allowing secondary CIs."""
        partial = [1.0] * (tail_report.PLANNED_PAIRS - 1)
        self.assertIsNone(tail_report.interval(partial, confirmatory=True)["ci95"])
        self.assertIsNotNone(tail_report.interval(partial)["ci95"])
        complete = [1.0] * tail_report.PLANNED_PAIRS
        self.assertEqual(tail_report.interval(complete, confirmatory=True)["ci95"], [1.0, 1.0])

    def test_paired_summary_from_packaged_run_metrics(self):
        """Exercise pairing across segments without claiming raw-request validation."""
        saved = json.loads((ANALYSIS / "tail-results.json").read_text(encoding="utf-8"))
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            for index, segment in enumerate(saved["execution_segments"]):
                directory = root / segment["directory"]
                directory.mkdir()
                cells = {}
                for run in saved["runs"]:
                    if (run["repeat"] <= 5) != (index == 0):
                        continue
                    cells[run["cell"]] = {
                        "concurrency": 128,
                        "repeat": run["repeat"],
                        "condition": run["condition"],
                        "guard": {"gpu_uuid": run["gpu_uuid"]},
                        "measurement": {
                            key: run[key]
                            for key in ("requests_per_second", "telemetry")
                        },
                    }
                    export = directory / "results" / run["cell"]
                    export.mkdir(parents=True)
                    # Precomputed windows isolate run-level pairing and inference.
                    record = {
                        "metadata": {"benchmark_phase": "profiling"},
                        "windows": run["windows"],
                    }
                    (export / "profile_export.jsonl").write_text(
                        json.dumps(record) + "\n", encoding="utf-8"
                    )
                for conditioning in range(30):
                    cells[f"conditioning-{conditioning}"] = {"concurrency": 1}
                progress = {
                    "state": saved["state"],
                    "cells": cells,
                    "execution_segments": segment["events"],
                    "startup_failures": segment["startup_failures"],
                }
                (directory / "formal-progress.json").write_text(
                    json.dumps(progress), encoding="utf-8"
                )

            previous = root / saved["execution_segments"][0]["directory"]
            current = root / saved["execution_segments"][1]["directory"]
            source = {
                "directory": previous.name,
                "progress_sha256": hashlib.sha256(
                    (previous / "formal-progress.json").read_bytes()
                ).hexdigest(),
            }
            (current / "continuation-source.json").write_text(
                json.dumps(source), encoding="utf-8"
            )

            def precomputed_windows(records, low, high):
                """Return packaged windows instead of simulating raw latencies."""
                label = next(
                    key for key, bounds in tail_report.WINDOWS.items()
                    if bounds == (low, high)
                )
                return records[0]["windows"][label]

            with patch.object(tail_report, "ROOT", current), patch.object(
                tail_report, "window_metrics", precomputed_windows
            ):
                actual = tail_report.generate()
            self.assertEqual(actual, saved)

        idle_repeats = {
            event["repeat"]
            for segment in saved["execution_segments"]
            for event in segment["events"]
            if "prolonged idle" in event.get("note", "").lower()
        }
        self.assertEqual(set(tail_report.RECOVERY_IDLE_REPEATS), idle_repeats)
        primary = next(row for row in actual["comparisons"] if row["primary"])
        self.assertEqual(primary["pairs"], 10)
        self.assertIn("interpretation", primary)
        for key, pairs in (
            ("same_gpu_steady_p99_sensitivity", 4),
            ("excluding_recovery_pairs_steady_p99_exploratory", 8),
        ):
            self.assertEqual(actual[key]["pairs"], pairs)
            self.assertNotIn("interpretation", actual[key])


if __name__ == "__main__":
    unittest.main()
