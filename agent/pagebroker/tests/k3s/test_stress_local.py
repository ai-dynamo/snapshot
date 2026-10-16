# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import stress_local as local
from stress_config import EvidenceOptions, Timing


class LocalQualificationTest(unittest.TestCase):
    def test_committed_aws_profiles_fit_their_declared_deadlines(self):
        for name, deadline in (("qualification", 1200), ("four-hour", 18000)):
            profile = json.loads((Path(local.__file__).parent / "profiles" / (name + ".json")).read_text())
            timing = Timing(**profile["timing"])
            timing.validate()
            evidence = EvidenceOptions(**profile["evidence"])
            evidence.validate()
            self.assertEqual(timing.worst_case, deadline)
            self.assertEqual(timing.job, deadline)
            self.assertFalse(evidence.growth_limits)
            self.assertFalse(evidence.absolute_limits)

    def test_profiles_require_multiple_real_batches_and_bounded_budget(self):
        for mode in ("filesystem", "s3"):
            spec = local.local_spec(mode, "https://127.0.0.1:1234")
            self.assertEqual(spec.timing.warmup_batches, 3)
            self.assertEqual(spec.timing.measured_batches, 2)
            self.assertLessEqual(spec.timing.worst_case, spec.timing.job)
            self.assertGreater(spec.timing.idle, 10 * spec.evidence.interval)
            if mode == "s3":
                self.assertTrue(spec.s3.prefix.endswith(spec.run_id))
                self.assertEqual(spec.s3.addressing, "path")

    def test_image_revision_and_platform_must_match(self):
        value = {"Config": {"Labels": {"org.opencontainers.image.revision": "a" * 40}},
                 "Os": "linux", "Architecture": "amd64", "Id": "sha256:" + "b" * 64}
        with patch.object(local, "command", return_value=subprocess.CompletedProcess([], 0, json.dumps([value]).encode())):
            self.assertEqual(local.image_identity("image", "a" * 40)["id"], value["Id"])
            with self.assertRaisesRegex(ValueError, "revision"):
                local.image_identity("image", "c" * 40)
            value["Architecture"] = "arm64"
        with patch.object(local, "command", return_value=subprocess.CompletedProcess([], 0, json.dumps([value]).encode())):
            with self.assertRaisesRegex(ValueError, "amd64"):
                local.image_identity("image", "a" * 40)

    def test_independent_evidence_failure_cannot_be_functional_success(self):
        with tempfile.TemporaryDirectory() as directory:
            root, spec = Path(directory), local.local_spec("filesystem")
            (root / "summary.json").write_text(json.dumps({"executionStatus": "SUCCESS"}))
            with patch.object(local, "validate_directory", return_value={"evidenceStatus": "INCOMPLETE"}):
                with self.assertRaisesRegex(ValueError, "evidence failed"):
                    local.verify_results(root, spec)

    def test_cleanup_does_not_remove_an_unowned_container(self):
        with tempfile.TemporaryDirectory() as directory:
            run = local.LocalRun(Path(directory), local.local_spec("filesystem"), {}, {})
            run.containers.append("collision")
            with patch.object(local.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, b"foreign-owner\n")) as invoke:
                run.close()
            self.assertFalse(any(call.args[0][1:3] == ["rm", "--force"] for call in invoke.call_args_list))


if __name__ == "__main__":
    unittest.main()
