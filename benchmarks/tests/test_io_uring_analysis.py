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
    def test_paired_summary_from_packaged_run_metrics(self):
        """Exercise pairing across segments without claiming raw-request validation."""
        saved = json.loads((ANALYSIS / "tail-results.json").read_text())
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
                    (export / "profile_export.jsonl").write_text(json.dumps(record) + "\n")
                for conditioning in range(30):
                    cells[f"conditioning-{conditioning}"] = {"concurrency": 1}
                progress = {
                    "state": saved["state"],
                    "cells": cells,
                    "execution_segments": segment["events"],
                    "startup_failures": segment["startup_failures"],
                }
                (directory / "formal-progress.json").write_text(json.dumps(progress))

            previous = root / saved["execution_segments"][0]["directory"]
            current = root / saved["execution_segments"][1]["directory"]
            source = {
                "directory": previous.name,
                "progress_sha256": hashlib.sha256(
                    (previous / "formal-progress.json").read_bytes()
                ).hexdigest(),
            }
            (current / "continuation-source.json").write_text(json.dumps(source))

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
