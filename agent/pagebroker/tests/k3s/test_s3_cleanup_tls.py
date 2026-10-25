# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Local TLS emulator coverage; never use the host's AWS credentials or endpoints."""

from dataclasses import replace
import importlib.util
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from s3_cleanup import CleanupError, OwnedStore, frozen_credentials, sdk_client
from stress_config import Clock
from stress_control import Peer
from stress_evidence import IncompleteEvidence, validate_cleanup
from test_s3_cleanup import credentials, s3_spec
import test_stress_process as processes


AVAILABLE = importlib.util.find_spec("moto") is not None and importlib.util.find_spec("boto3") is not None


@unittest.skipUnless(AVAILABLE, "install hack/pagebroker-integration/requirements.txt for the local TLS emulator")
class TLSCleanupTest(unittest.TestCase):
    def setUp(self):
        sys.path.insert(0, str(Path(__file__).resolve().parents[4] / "hack/pagebroker-integration"))
        from s3_integration import local_environment, local_service
        self.environment = local_environment()
        self.service = local_service(SimpleNamespace(tls=True), self.environment, service_factory=lambda app: app)
        self.service.__enter__()
        self.addCleanup(self.service.__exit__, None, None, None)
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.spec = s3_spec(self.root, Clock(), endpoint=self.environment["AWS_ENDPOINT_URL"], page_size=2)
        self.environment.update(credentials(self.spec))
        with patch.dict(os.environ, self.environment, clear=True):
            self.client = sdk_client(self.spec, credentials(self.spec))
        self.addCleanup(self.client.close)
        self.client.create_bucket(Bucket=self.spec.s3.bucket)

    def store(self):
        return OwnedStore(self.client, self.spec, time.monotonic() + 30)

    def test_readiness_pagination_and_prefix_isolation(self):
        store = self.store()
        self.assertEqual(store.ready()["status"], "READY")
        outside = self.spec.s3.prefix + "-foreign/file"
        self.client.put_object(Bucket=self.spec.s3.bucket, Key=outside, Body=b"keep")
        for index in range(7):
            self.client.put_object(Bucket=self.spec.s3.bucket, Key=store.prefix + str(index), Body=b"data")
        reply = store.sweep()
        self.assertEqual((reply["objects"], reply["bytes"]), (7, 28))
        self.assertEqual(self.client.get_object(Bucket=self.spec.s3.bucket, Key=outside)["Body"].read(), b"keep")
        with self.assertRaisesRegex(CleanupError, "already owned"):
            self.store().ready()

    def test_enabled_versions_and_delete_markers(self):
        self.client.put_bucket_versioning(Bucket=self.spec.s3.bucket, VersioningConfiguration={"Status": "Enabled"})
        store = self.store()
        self.assertEqual(store.ready()["status"], "READY")
        for index in range(4):
            key = store.prefix + str(index)
            for _ in range(2):
                self.client.put_object(Bucket=self.spec.s3.bucket, Key=key, Body=b"data")
            self.client.delete_object(Bucket=self.spec.s3.bucket, Key=key)
        reply = store.sweep()
        self.assertEqual(reply["status"], "SUCCESS")
        self.assertEqual(reply["deleteMarkers"], 4)
        versions = self.client.list_object_versions(Bucket=self.spec.s3.bucket, Prefix=store.prefix)
        self.assertEqual([item["Key"] for item in versions["Versions"]], [store.marker])
        self.assertFalse(versions.get("DeleteMarkers"))

    def test_incomplete_upload_preserves_failed_batch_and_inspects_parts(self):
        store = self.store()
        store.ready()
        key = store.prefix + "failed/object"
        self.client.put_object(Bucket=self.spec.s3.bucket, Key=key, Body=b"retain")
        upload = self.client.create_multipart_upload(Bucket=self.spec.s3.bucket, Key=key)["UploadId"]
        self.client.upload_part(Bucket=self.spec.s3.bucket, Key=key, UploadId=upload, PartNumber=1, Body=b"part")
        reply = store.sweep()
        self.assertEqual(reply["status"], "ERROR")
        self.assertEqual((reply["incompleteUploads"], reply["multipartParts"], reply["multipartBytes"]), (1, 1, 4))
        self.assertEqual(self.client.get_object(Bucket=self.spec.s3.bucket, Key=key)["Body"].read(), b"retain")
        self.assertEqual(len(self.client.list_multipart_uploads(Bucket=self.spec.s3.bucket, Prefix=store.prefix)["Uploads"]), 1)

    def test_static_sdk_requests_do_not_inherit_a_session_token(self):
        spec = replace(self.spec, s3=replace(self.spec.s3, credential_provider="static-environment", credential_expiry=None))
        values = {name: self.environment[name] for name in ("AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY")}
        tokens = []
        with patch.dict(os.environ, {"AWS_SESSION_TOKEN": "fake-inherited-token"}, clear=True):
            client = sdk_client(spec, frozen_credentials(spec, values, spec.created_at), self.environment["AWS_CA_BUNDLE"])
        self.addCleanup(client.close)
        client.meta.events.register("before-send.s3", lambda request, **_: tokens.append(request.headers.get("X-Amz-Security-Token")))
        store = OwnedStore(client, spec, time.monotonic() + 30)
        ready = store.ready()
        self.assertEqual(ready["credentialProvider"], "static-environment")
        self.assertIsNone(ready["credentialExpiry"])
        self.assertTrue(tokens)
        self.assertTrue(all(token is None for token in tokens))

    def harness(self, client_sleep=None, static=False):
        harness = processes.ProcessTest(methodName="runTest")
        harness.setUp()
        self.addCleanup(harness.doCleanups)
        timing = replace(harness.spec.timing, startup=15, warmup_min=0.3, warmup_max=5,
                         measured=3, cadence=0.6, batch=2, cleanup=3, drain=5, job=45)
        settings = replace(self.spec.s3, prefix="test/" + harness.spec.run_id,
                           credential_expiry=harness.spec.created_at + 7200)
        if static:
            settings = replace(settings, credential_provider="static-environment", credential_expiry=None)
        harness.spec = replace(harness.spec, mode="s3", store_id="stress-" + harness.spec.run_id, s3=settings,
                               timing=timing, startup_deadline=harness.spec.created_at + timing.startup,
                               expires_at=harness.spec.created_at + timing.job).validate()
        harness.save_spec()
        harness.start(runner_environment={"FIXTURE_CLIENT_SLEEP": client_sleep} if client_sleep else None)
        log = (harness.root / "helper.log").open("w+")
        harness.logs.append(log)
        if static:
            environment = {key: value for key, value in self.environment.items()
                           if key not in ("AWS_SESSION_TOKEN", "AWS_SECURITY_TOKEN", "AWS_CREDENTIAL_EXPIRATION")}
        else:
            environment = self.environment | credentials(harness.spec)
        environment["PYTHONDONTWRITEBYTECODE"] = "1"
        helper = subprocess.Popen([sys.executable, str(processes.HERE / "s3_cleanup.py"), "--spec", str(harness.path)],
                                  env=environment, stdout=log, stderr=subprocess.STDOUT)
        harness.processes.append(helper)
        harness.helper = helper
        return harness

    def test_static_helper_finishes_and_records_explicit_nonexpiring_evidence(self):
        harness = self.harness(static=True)
        harness.runner.wait(timeout=25)
        summary = harness.assert_finished(True)
        harness.helper.wait(timeout=3)
        self.assertEqual(harness.helper.returncode, 0)
        self.assertEqual(summary["evidenceStatus"], "COMPLETE")
        root = Path(harness.spec.results)
        supervisor = json.loads((root / "supervisor.json").read_text())
        validate_cleanup(root, harness.spec, supervisor)
        path = root / "cleanup-readiness.json"
        ready = json.loads(path.read_text())
        self.assertEqual(ready["credentialProvider"], "static-environment")
        self.assertIsNone(ready["credentialExpiry"])
        del ready["credentialExpiry"]
        path.write_text(json.dumps(ready))
        with self.assertRaises(IncompleteEvidence):
            validate_cleanup(root, harness.spec, supervisor)

    def test_persistent_runner_and_helper_full_handshake(self):
        harness = self.harness()
        harness.runner.wait(timeout=25)
        ready = processes.read_if_present(Path(harness.spec.control) / "cleanup-ready.json")
        self.assertEqual(ready and ready.get("status"), "READY", ready)
        summary = harness.assert_finished(True)
        harness.helper.wait(timeout=3)
        self.assertEqual(harness.helper.returncode, 0)
        self.assertEqual(summary["evidenceStatus"], "COMPLETE")
        self.assertGreaterEqual(summary["counts"]["measured"], 1)
        root = Path(harness.spec.results)
        supervisor = json.loads((root / "supervisor.json").read_text())
        validate_cleanup(root, harness.spec, supervisor)
        for path in root.rglob("*"):
            if path.is_file():
                self.assertNotIn(b"fake-secret", path.read_bytes())
                self.assertNotIn(b"fake-session", path.read_bytes())

    def test_helper_death_stops_broker_and_client_without_cleanup(self):
        harness = self.harness(client_sleep="0.8")
        processes.wait_for(lambda: list(Path(harness.spec.control).glob("client-started-*")), timeout=15)
        harness.helper.kill()
        for process in (harness.helper, harness.broker, harness.supervisor, harness.runner):
            process.wait(timeout=8)
        self.assertNotEqual(harness.supervisor.returncode, 0)
        failure = json.loads((Path(harness.spec.control) / "failure.json").read_text())
        self.assertIn(failure["remoteCleanup"], ("NOT_STARTED", "UNCERTAIN"))
        journal = Path(harness.spec.results) / "cleanup.jsonl"
        self.assertFalse(journal.exists())

    def test_runner_death_cancels_active_sdk_worker(self):
        self.observer_death_during_sdk("runner")

    def test_helper_death_cancels_active_sdk_worker(self):
        self.observer_death_during_sdk("helper")

    def observer_death_during_sdk(self, role):
        harness = self.harness()
        def active_worker():
            message = processes.read_if_present(Path(harness.spec.control) / "cleanup-worker.json")
            return message if message and message["active"] else None
        registration = processes.wait_for(active_worker, timeout=15)
        peer = Peer.open(registration["peer"])
        self.addCleanup(peer.close)
        peer.send(signal.SIGSTOP)
        getattr(harness, role).kill()
        for process in (harness.runner, harness.broker, harness.supervisor, harness.helper):
            process.wait(timeout=8)
        self.assertNotEqual(harness.supervisor.returncode, 0)
        from run import exited
        self.assertTrue(exited(peer.fd))
        self.assertFalse((Path(harness.spec.results) / "cleanup.jsonl").exists())

    def test_failed_batch_retention_and_no_next_batch(self):
        harness = self.harness(client_sleep="0.8")
        processes.wait_for(lambda: list(Path(harness.spec.control).glob("client-started-*")), timeout=15)
        key = harness.spec.s3.prefix + "/failed/object"
        self.client.put_object(Bucket=self.spec.s3.bucket, Key=key, Body=b"keep")
        self.client.create_multipart_upload(Bucket=self.spec.s3.bucket, Key=key)
        harness.runner.wait(timeout=10)
        summary = harness.assert_finished(False)
        self.assertEqual(summary["counts"], {"warmup": 0, "measured": 0, "drain": 0})
        self.assertEqual(len(list((Path(harness.spec.results) / "batches").iterdir())), 1)
        final = json.loads((Path(harness.spec.results) / "cleanup-final.json").read_text())
        self.assertEqual(final["error"], "UnexpectedMultipartUploads")
        self.assertEqual(final["recoveryCleanup"], "NOT_STARTED")
        self.assertEqual(summary["shutdown"]["helper"]["remoteCleanup"], "RETAINED")
        self.assertEqual(self.client.get_object(Bucket=self.spec.s3.bucket, Key=key)["Body"].read(), b"keep")

    def test_sdk_worker_timeout_stops_run_and_retains_saved_report(self):
        harness = self.harness()
        processes.wait_for(lambda: (Path(harness.spec.results) / "cleanup.jsonl").exists(), timeout=15)
        def active_worker():
            message = processes.read_if_present(Path(harness.spec.control) / "cleanup-worker.json")
            return message if message and message["active"] else None
        registration = processes.wait_for(active_worker)
        peer = Peer.open(registration["peer"])
        self.addCleanup(peer.close)
        peer.send(signal.SIGSTOP)
        for process in (harness.runner, harness.helper, harness.broker, harness.supervisor):
            process.wait(timeout=8)
        from run import exited
        self.assertTrue(exited(peer.fd))
        self.assertNotEqual(harness.runner.returncode, 0)
        self.assertTrue((Path(harness.spec.results) / "batches/000001/client-report.json").exists())
        self.assertEqual(len(list((Path(harness.spec.results) / "batches").iterdir())), 1)


if __name__ == "__main__":
    unittest.main()
