# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

import contextlib
import dataclasses
import io
import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from snapshot_benchmarks import cli, measurement, run
from snapshot_benchmarks.analysis.phase_inventory import (
    category,
    native_autotune_intervals,
)
from snapshot_benchmarks.engines import ModelSpec
from test_benchmarks import cluster, config


class ReviewFixTests(unittest.TestCase):
    def test_restore_count_cli_and_validation(self):
        for command in ("run", "sweep"):
            args = cli.build_parser().parse_args(
                [
                    command,
                    "--context",
                    "test",
                    "--node",
                    "node",
                    "--image",
                    "test",
                    "--model-pvc",
                    "models",
                    "--work-pvc",
                    "work",
                    "--restore-count",
                    "2",
                ]
            )
            self.assertEqual(cli._benchmark_config(args).restore_count, 2)
        self.assertEqual(config().restore_count, 3)
        for count in (0, -1, True):
            with self.subTest(count=count), self.assertRaises(ValueError):
                dataclasses.replace(config(), restore_count=count)

    def test_restore_count_and_keep_free_gpus_before_next_attempt(self):
        for count in (1, 2):
            for keep in (False, True):
                with (
                    self.subTest(count=count, keep=keep),
                    tempfile.TemporaryDirectory() as tmp,
                    cluster() as mocks,
                ):
                    result = run.run_benchmark(
                        dataclasses.replace(config(), restore_count=count),
                        cli.ENGINES["vllm"],
                        ModelSpec("Test", "/hf-cache/test"),
                        image="test",
                        keep=keep,
                        evidence_dir=Path(tmp),
                        run_id="count",
                        progress=lambda _: None,
                    )
                    self.assertEqual(len(result.restore_attempts), count)
                    self.assertEqual(
                        mocks["measurement.restored_measurement"].call_count, count
                    )
                    expected = ["bench-source-count"] + [
                        f"bench-restore-count-{i}" for i in range(1, count + (not keep))
                    ]
                    self.assertEqual(
                        [c.args[1] for c in mocks["k8s.delete_pod"].call_args_list],
                        expected,
                    )

    def test_failed_cleanup_stops_sweep_when_diagnostic_disk_is_full(self):
        save = measurement.save

        def disk_full(path, value):
            if Path(path).name == "cleanup-errors.json":
                raise OSError("disk full")
            save(path, value)

        with tempfile.TemporaryDirectory() as tmp, cluster() as mocks:
            root = Path(tmp)
            models = root / "models.yaml"
            models.write_text(
                json.dumps(
                    dict(
                        models=[
                            dict(label=label, hf_id_or_path="/hf-cache/test")
                            for label in ("one", "two")
                        ]
                    )
                )
            )
            mocks["lifecycle.delete_podsnapshotcontent"].side_effect = OSError(
                "delete failed"
            )
            error = io.StringIO()
            with (
                patch.object(measurement, "save", side_effect=disk_full),
                contextlib.redirect_stdout(io.StringIO()),
                contextlib.redirect_stderr(error),
            ):
                code = cli.main(
                    [
                        "sweep",
                        "--context",
                        "test-context",
                        "--namespace",
                        "workloads",
                        "--node",
                        "gpu-node",
                        "--image",
                        "test",
                        "--model-pvc",
                        "models",
                        "--work-pvc",
                        "work",
                        "--models",
                        str(models),
                        "--output-dir",
                        str(root / "results"),
                    ]
                )
            self.assertEqual(code, 1)
            self.assertIn("disk full", error.getvalue())
            mocks["measurement.source_measurement"].assert_called_once()

    def test_inference_helpers_require_source_answers_and_the_same_token(self):
        source = [
            dict(prompt=prompt, answer=answer)
            for prompt, answer in zip(measurement.INFERENCE_PROMPTS, [
                "a must-visit destination for anyone interested", "Berlin", "Tokyo",
            ])
        ]
        restored = [
            dict(prompt=x["prompt"], response=dict(text=x["answer"], checkpoint_token="token"))
            for x in source
        ]
        self.assertTrue(measurement.source_checks_pass(source))
        self.assertTrue(measurement.restore_checks_pass(restored, "token", source))
        self.assertFalse(measurement.restore_checks_pass(restored, "wrong", source))
        for checks in ([], source[:2], source[:2] + source[:1]):
            self.assertFalse(measurement.source_checks_pass(checks))
        for checks in ([], restored[:2], restored[:2] + restored[:1]):
            self.assertFalse(measurement.restore_checks_pass(checks, "token", source))
        # A factual answer still fails if the restored model changed its response.
        restored[0]["response"]["text"] = "Paris"
        self.assertFalse(measurement.restore_checks_pass(restored, "token", source))
        for empty in ("", "  ", None):
            source[0]["answer"] = empty
            restored[0]["response"]["text"] = empty
            self.assertFalse(measurement.source_checks_pass(source))
            self.assertFalse(measurement.restore_checks_pass(restored, "token", source))

    def test_checkpoint_helpers_reject_bad_inventory_and_ambiguous_timing(self):
        self.assertTrue(measurement.is_gpu_manifest("gpu/123/manifest.txt"))
        for path in (
            "gpu/rank/manifest.txt",
            "gpu/123/other.txt",
            "prefix/gpu/123/manifest.txt",
        ):
            self.assertFalse(measurement.is_gpu_manifest(path))
        inventory = measurement.parse_checkpoint_inventory(
            "name\twith\nspaces\0" + "12\0BENCHMARK_FIND_COMPLETE\0"
        )
        self.assertEqual(inventory["total_bytes"], 12)
        for output in ("find failed", "name\0BENCHMARK_FIND_COMPLETE\0"):
            with self.assertRaises(RuntimeError):
                measurement.parse_checkpoint_inventory(output)
        with self.assertRaises(ValueError):
            measurement.parse_checkpoint_inventory("BENCHMARK_FIND_COMPLETE\0")
        other = 'Checkpoint timing summary {"content":"other","checkpoint":{"duration":"9s"}}'
        wanted = 'Checkpoint timing summary {"content":"wanted","checkpoint":{"duration":"1s"}}'
        self.assertEqual(
            measurement.find_checkpoint_timing(other + "\n" + wanted, "wanted"),
            dict(duration="1s"),
        )
        for text in (other, wanted + "\n" + wanted):
            with self.assertRaises(ValueError):
                measurement.find_checkpoint_timing(text, "wanted")

    def test_native_autotune_pairing_and_classification(self):
        start = (
            "[2026-01-01 00:00:00 TP0 EP0] Running FlashInfer autotune with cache: test"
        )
        end = "[2026-01-01 00:00:02 TP0 EP0] FlashInfer autotune completed."
        intervals = native_autotune_intervals([start, end], 0, "test")
        self.assertEqual(intervals[0]["end"] - intervals[0]["start"], 2)
        for lines in ([start], [end], [start, start, end]):
            with self.assertRaises(AssertionError):
                native_autotune_intervals(lines, 0, "test")
        intervals = native_autotune_intervals(
            [
                "2026-01-01 00:00:00,000 Autotuning process starts",
                "2026-01-01 00:00:02,500 Autotuning process ends",
            ],
            0,
            "test",
        )
        self.assertEqual(intervals[0]["end"] - intervals[0]["start"], 2.5)
        for name, expected in [
            ("vllm.compilation.backend.compile_model", "Torch compilation"),
            ("cutlass.compiler.compile", "JIT module build/load"),
            ("flashinfer.sampling.get_sampling_module", "JIT module build/load"),
            (
                "sglang.srt.layers.deep_gemm_wrapper.compile_utils._compile_deep_gemm_one_type_all",
                "JIT module build/load",
            ),
        ]:
            self.assertEqual(category(name), expected)
