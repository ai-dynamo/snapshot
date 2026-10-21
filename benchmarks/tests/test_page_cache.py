# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

import contextlib
import dataclasses
import importlib.util
import io
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from snapshot_benchmarks import cli
from snapshot_benchmarks.engines import ModelSpec
from snapshot_benchmarks.run import BenchmarkConfig
from snapshot_benchmarks.workload import instrument_pod

spec = importlib.util.spec_from_file_location(
    "page_cache", Path(__file__).parents[1] / "instrumentation/page_cache.py"
)
page_cache = importlib.util.module_from_spec(spec)
spec.loader.exec_module(page_cache)


class PageCacheTests(unittest.TestCase):
    def test_sync_and_drop_precede_weight_eviction_and_saved_probe(self):
        with (
            tempfile.TemporaryDirectory() as tmp,
            contextlib.redirect_stdout(io.StringIO()),
        ):
            root = Path(tmp)
            (root / "model.safetensors").write_bytes(b"x" * 8192)
            control = root / "drop-caches"
            output = root / "cache-control.json"
            events = []

            def sync():
                events.append("sync")

            def advise(*args):
                self.assertEqual(control.read_text(), "1\n")
                self.assertEqual(events, ["sync"])
                events.append("fadvise")

            with (
                patch.object(page_cache, "DROP_CACHES", control),
                patch.object(page_cache.os, "sync", side_effect=sync),
                patch.object(page_cache.os, "posix_fadvise", side_effect=advise),
                patch.object(
                    page_cache, "resident_pages", side_effect=[(2, 2), (0, 2)]
                ),
            ):
                result = page_cache.prepare(str(root), output=output)
            self.assertTrue(result["success"])
            self.assertEqual(events, ["sync", "fadvise"])
            self.assertEqual(result["resident_pages_after"], 0)
            self.assertEqual(json.loads(output.read_text()), result)

    def test_permission_failure_stops_before_sync_and_saves_reason(self):
        with (
            tempfile.TemporaryDirectory() as tmp,
            contextlib.redirect_stdout(io.StringIO()),
        ):
            root = Path(tmp)
            (root / "model.safetensors").write_bytes(b"weights")
            control = unittest.mock.Mock()
            control.open.side_effect = PermissionError("read-only sysctl")
            output = root / "cache-control.json"
            with (
                patch.object(page_cache, "DROP_CACHES", control),
                patch.object(page_cache.os, "sync") as sync,
                patch.object(page_cache.os, "posix_fadvise") as advise,
            ):
                with self.assertRaisesRegex(
                    PermissionError, "privileged init container"
                ):
                    page_cache.prepare(str(root), output=output)
            sync.assert_not_called()
            advise.assert_not_called()
            result = json.loads(output.read_text())
            self.assertFalse(result["success"])
            self.assertFalse(result["node_page_cache_dropped"])

    def test_resident_weights_fail_instead_of_claiming_a_cold_cache(self):
        with (
            tempfile.TemporaryDirectory() as tmp,
            contextlib.redirect_stdout(io.StringIO()),
        ):
            root = Path(tmp)
            (root / "model.safetensors").write_bytes(b"weights")
            output = root / "cache-control.json"
            with (
                patch.object(page_cache, "DROP_CACHES", root / "drop-caches"),
                patch.object(page_cache.os, "sync"),
                patch.object(page_cache.os, "posix_fadvise"),
                patch.object(page_cache, "resident_pages", return_value=(1, 1)),
            ):
                with self.assertRaisesRegex(RuntimeError, "remain resident"):
                    page_cache.prepare(str(root), output=output)
            result = json.loads(output.read_text())
            self.assertFalse(result["success"])
            self.assertEqual(result["resident_pages_after"], 1)

    def test_mincore_observes_a_read_page_without_faulting_the_mapping(self):
        with tempfile.TemporaryFile() as stream:
            stream.write(b"x" * os.sysconf("SC_PAGE_SIZE"))
            stream.flush()
            os.fsync(stream.fileno())
            self.assertEqual(
                page_cache.resident_pages(stream.fileno(), stream.tell()), (1, 1)
            )

    def test_opt_out_removes_privileged_init_and_is_recorded(self):
        parser = cli.build_parser()
        for command in ("run", "sweep"):
            args = parser.parse_args(
                [
                    command,
                    "--context",
                    "test",
                    "--node",
                    "test",
                    "--image",
                    "test",
                    "--model-pvc",
                    "models",
                    "--work-pvc",
                    "work",
                    "--no-drop-page-cache",
                ]
            )
            self.assertTrue(args.no_drop_page_cache)
            self.assertFalse(cli._benchmark_config(args).drop_page_cache)
        cfg = BenchmarkConfig(
            "work",
            "snap",
            "snap",
            "checkpoints",
            None,
            "test",
            "test",
            "models",
            "work",
        )
        self.assertTrue(cfg.drop_page_cache)
        cfg = dataclasses.replace(cfg, drop_page_cache=False)
        model = ModelSpec("Test", "/hf-cache/test")
        for engine in cli.ENGINES.values():
            pod = engine.build_source_pod(
                name="test",
                namespace="work",
                image="test",
                model=model,
            )
            instrument_pod(pod, cfg, engine, model, "test")
            self.assertFalse(
                any(
                    c["name"] == "drop-page-cache"
                    for c in pod["spec"]["initContainers"]
                )
            )
