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
import sys
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
    @unittest.skipUnless(sys.platform == "linux", "Linux page-cache APIs")
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
            self.assertEqual(result["files"][0]["resident_pages_after_node_drop"], 2)
            self.assertEqual(json.loads(output.read_text()), result)

    def test_missing_indexed_shard_fails_before_node_cache_drop(self):
        with (
            tempfile.TemporaryDirectory() as tmp,
            contextlib.redirect_stdout(io.StringIO()),
        ):
            root = Path(tmp)
            (root / "model-00001.safetensors").write_bytes(b"weights")
            (root / "model.safetensors.index.json").write_text(
                json.dumps(
                    dict(
                        weight_map={
                            "layer1": "model-00001.safetensors",
                            "layer2": "model-00002.safetensors",
                        }
                    )
                )
            )
            with patch.object(page_cache, "DROP_CACHES") as control:
                with self.assertRaisesRegex(ValueError, "Missing or empty"):
                    page_cache.prepare(str(root), output=root / "cache-control.json")
            control.open.assert_not_called()
            (root / "model-00002.safetensors").write_bytes(b"weights")
            (root / "unused.safetensors").write_bytes(b"unused")
            result = page_cache.prepare(
                str(root), output=root / "cache-control.json", drop_page_cache=False
            )
            self.assertEqual(result["weights_bytes"], 14)
            self.assertEqual(len(result["files"]), 2)

    def test_pinned_helper_uses_directory_resolved_by_unprivileged_inventory(self):
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
        engine = cli.ENGINES["vllm"]
        model = ModelSpec("Test", "/hf-cache/test")
        pod = engine.build_source_pod(
            name="test", namespace="work", image="engine:test", model=model
        )
        instrument_pod(pod, cfg, engine, model, "test")
        helper = next(
            c for c in pod["spec"]["initContainers"] if c["name"] == "drop-page-cache"
        )
        with tempfile.TemporaryDirectory() as tmp:
            inventory = Path(tmp) / "cache-control.json"
            inventory.write_text(
                json.dumps(dict(model_directory="/hf-cache/resolved-snapshot"))
            )
            script = helper["command"][-1].replace(
                "/startup-spans/cache-control.json", str(inventory)
            )
            with (
                patch.dict(sys.modules, page_cache=page_cache),
                patch.object(page_cache, "prepare") as prepare,
            ):
                exec(script, {})
            prepare.assert_called_once_with("/hf-cache/resolved-snapshot")

    def test_inventory_measures_logical_sizes_without_cache_operations(self):
        with (
            tempfile.TemporaryDirectory() as tmp,
            contextlib.redirect_stdout(io.StringIO()),
        ):
            root = Path(tmp)
            blob = root / "blob"
            with blob.open("wb") as stream:
                stream.truncate(1048576)
            (root / "model-00001.safetensors").symlink_to(blob)
            (root / "model-00002.safetensors").write_bytes(b"weights")
            (root / "config.json").write_text("{}")
            output = root / "cache-control.json"
            with (
                patch.object(page_cache, "DROP_CACHES") as control,
                patch.object(page_cache.os, "sync") as sync,
                patch.object(page_cache.os, "posix_fadvise", create=True) as advise,
                patch.object(page_cache, "resident_pages") as residency,
            ):
                result = page_cache.prepare(
                    str(root), output=output, drop_page_cache=False
                )
            self.assertTrue(result["success"])
            self.assertTrue(result["eviction_skipped"])
            self.assertEqual(result["weights_bytes"], 1048576 + 7)
            self.assertEqual(len(result["files"]), 2)
            self.assertEqual(json.loads(output.read_text()), result)
            control.open.assert_not_called()
            sync.assert_not_called()
            advise.assert_not_called()
            residency.assert_not_called()

    @unittest.skipUnless(sys.platform == "linux", "Linux page-cache APIs")
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
                patch.object(page_cache.os, "posix_fadvise", create=True) as advise,
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

    @unittest.skipUnless(sys.platform == "linux", "Linux page-cache APIs")
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

    @unittest.skipUnless(sys.platform == "linux", "Linux page-cache APIs")
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
            self.assertFalse(args.detailed_timing)
            self.assertFalse(cli._benchmark_config(args).detailed_timing)
            detailed = parser.parse_args(
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
                    "--detailed-timing",
                ]
            )
            self.assertTrue(cli._benchmark_config(detailed).detailed_timing)
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
            inventory = next(
                c for c in pod["spec"]["initContainers"] if c["name"] == "model-weights"
            )
            self.assertFalse(inventory["securityContext"].get("privileged", False))
            self.assertIn("--no-drop-page-cache", inventory["args"])
            self.assertFalse(
                any(
                    c["name"] == "drop-page-cache"
                    for c in pod["spec"]["initContainers"]
                )
            )
