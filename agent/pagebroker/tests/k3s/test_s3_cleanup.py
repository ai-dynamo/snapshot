# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

from dataclasses import replace
from datetime import datetime, timezone
import hashlib
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch
import uuid

from s3_cleanup import CleanupChannel, CleanupError, Helper, OwnedStore, frozen_credentials, safe_error
import stress
from stress_config import S3Options, RunSpec
from stress_control import Channel
from stress_storage import EvidenceStore
from stress_evidence import validate_cleanup
from stress_analysis import IncompleteEvidence
from test_run import report
from test_stress import FakeClock, specification


def s3_spec(root, clock=None, **options):
    spec = specification(root, clock=clock)
    settings = S3Options(bucket="local-stress-test", prefix="test/" + spec.run_id, region="us-east-1",
                         endpoint="https://127.0.0.1:12345", addressing="path",
                         credential_expiry=spec.created_at + spec.timing.job + 3600)
    return replace(spec, mode="s3", store_id="stress-" + spec.run_id, s3=replace(settings, **options)).validate()


def credentials(spec):
    return {"AWS_ACCESS_KEY_ID": "fake-access", "AWS_SECRET_ACCESS_KEY": "fake-secret",
            "AWS_SESSION_TOKEN": "fake-session", "AWS_CREDENTIAL_EXPIRATION":
                datetime.fromtimestamp(spec.s3.credential_expiry, timezone.utc).isoformat()}


class CleanupTest(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name)
        self.spec = s3_spec(self.root)
        self.clock = FakeClock()

    def store(self, client=None):
        client = client or Mock()
        value = OwnedStore(client, self.spec, 120, self.clock)
        client.get_object.side_effect = lambda **_: {"Body": io.BytesIO(value.owner)}
        client.get_bucket_versioning.return_value = {}
        client.list_objects_v2.return_value = {}
        client.list_multipart_uploads.return_value = {}
        return value

    def test_owned_prefix_and_store_rejections(self):
        for prefix in ("", "/", "shared", "a/../" + self.spec.run_id, self.spec.s3.prefix + "/"):
            with self.subTest(prefix=prefix), self.assertRaises(ValueError):
                replace(self.spec, s3=replace(self.spec.s3, prefix=prefix)).validate()
        with self.assertRaises(ValueError):
            replace(self.spec, store_id="shared").validate()
        for endpoint in ("http://localhost", "https://user:secret@host", "https://host?token=secret"):
            with self.assertRaises(ValueError):
                replace(self.spec, s3=replace(self.spec.s3, endpoint=endpoint)).validate()

    def test_credentials_cover_full_job_not_just_measured_time(self):
        expiry = self.spec.created_at + self.spec.timing.measured + 900
        with self.assertRaisesRegex(ValueError, "full Job"):
            replace(self.spec, s3=replace(self.spec.s3, credential_expiry=expiry)).validate()
        frozen_credentials(self.spec, credentials(self.spec), self.clock.time())
        with self.assertRaisesRegex(ValueError, "full Job"):
            frozen_credentials(self.spec, credentials(self.spec), self.clock.time() + 3600)

    def test_static_credentials_have_no_expiry_and_roundtrip_without_secrets(self):
        spec = replace(self.spec, s3=replace(self.spec.s3, credential_provider="static-environment",
                                           credential_expiry=None)).validate()
        values = {"AWS_ACCESS_KEY_ID": "fake-static-access", "AWS_SECRET_ACCESS_KEY": "fake-static-secret"}
        self.assertEqual(frozen_credentials(spec, values, self.clock.time() + 86400), values)
        self.assertEqual(RunSpec.from_dict(spec.as_dict()), spec)
        self.assertIsNone(spec.as_dict()["s3"]["credential_expiry"])
        for value in values.values():
            self.assertNotIn(value, json.dumps(spec.as_dict()))
        # Static credentials do not relax the run's independent admission deadline.
        late = FakeClock()
        late.sleep(spec.timing.startup)
        with self.assertRaises(TimeoutError):
            spec.deadlines(late)

    def test_static_credentials_reject_missing_keys_and_temporary_metadata(self):
        spec = replace(self.spec, s3=replace(self.spec.s3, credential_provider="static-environment", credential_expiry=None))
        values = {"AWS_ACCESS_KEY_ID": "fake-static-access", "AWS_SECRET_ACCESS_KEY": "fake-static-secret"}
        for name in values:
            with self.subTest(missing=name), self.assertRaises(CleanupError):
                frozen_credentials(spec, {key: value for key, value in values.items() if key != name}, self.clock.time())
        for name in ("AWS_SESSION_TOKEN", "AWS_SECURITY_TOKEN", "AWS_CREDENTIAL_EXPIRATION"):
            with self.subTest(metadata=name), self.assertRaises(CleanupError) as result:
                frozen_credentials(spec, values | {name: "fake-sensitive-metadata"}, self.clock.time())
            self.assertNotIn("fake-sensitive-metadata", str(result.exception))
        with self.assertRaisesRegex(ValueError, "must not declare"):
            replace(spec, s3=replace(spec.s3, credential_expiry=10000)).validate()

    def test_temporary_credentials_cannot_omit_or_forge_expiry(self):
        for expiry in (None, True, float("nan"), float("inf"), "10000"):
            with self.subTest(expiry=expiry), self.assertRaises(ValueError):
                replace(self.spec, s3=replace(self.spec.s3, credential_expiry=expiry)).validate()

    def test_unknown_or_mismatched_expiry_and_provider_rejected(self):
        for field in ("AWS_SESSION_TOKEN", "AWS_CREDENTIAL_EXPIRATION"):
            environment = credentials(self.spec)
            del environment[field]
            with self.assertRaises(CleanupError):
                frozen_credentials(self.spec, environment, self.clock.time())
        environment = credentials(self.spec) | {"AWS_CREDENTIAL_EXPIRATION": "invalid-secret-value"}
        with self.assertRaises(CleanupError) as result:
            frozen_credentials(self.spec, environment, self.clock.time())
        self.assertNotIn("invalid-secret", str(result.exception))
        with self.assertRaises(ValueError):
            replace(self.spec, s3=replace(self.spec.s3, credential_provider="assumed-refresh")).validate()

    def test_run_spec_roundtrip_contains_only_expiry_metadata(self):
        path = self.root / "spec.json"
        path.write_text(json.dumps(self.spec.as_dict()))
        self.assertEqual(RunSpec.load(path), self.spec)
        for value in credentials(self.spec).values():
            if value.startswith("fake-"):
                self.assertNotIn(value, path.read_text())
        runtime = stress.LocalRuntime(self.spec, [])
        with patch.dict("os.environ", credentials(self.spec)):
            self.assertFalse(any(key.startswith("AWS_") for key in runtime.environment()))

    def test_foreign_listing_is_never_deleted(self):
        store = self.store()
        store.client.list_objects_v2.return_value = {"Contents": [{"Key": self.spec.s3.prefix + "-other/key", "Size": 1}]}
        with self.assertRaisesRegex(CleanupError, "escaped"):
            store.sweep()
        store.client.delete_objects.assert_not_called()

    def test_wrong_marker_prevents_listing_or_deletion(self):
        store = self.store()
        store.client.get_object.side_effect = lambda **_: {"Body": io.BytesIO(b"foreign")}
        with self.assertRaisesRegex(CleanupError, "ownership"):
            store.sweep()
        store.client.list_objects_v2.assert_not_called()
        store.client.delete_objects.assert_not_called()

    def test_partial_delete_and_missing_per_object_ack_fail(self):
        for reply in ({"Errors": [{"Code": "AccessDenied"}]}, {"Deleted": []}):
            store = self.store()
            store.client.list_objects_v2.return_value = {"Contents": [{"Key": store.prefix + "batch/file", "Size": 5}]}
            store.client.delete_objects.return_value = reply
            with self.assertRaises(CleanupError):
                store.sweep()
            self.assertEqual(store.client.delete_objects.call_count, 1)

    def test_suspended_null_versions_and_delete_markers(self):
        store = self.store()
        store.client.get_bucket_versioning.return_value = {"Status": "Suspended"}
        key = store.prefix + "batch/file"
        store.client.list_object_versions.side_effect = [
            {"Versions": [{"Key": key, "VersionId": "null", "Size": 5},
                          {"Key": store.marker, "VersionId": "owner", "Size": len(store.owner)}],
             "DeleteMarkers": [{"Key": key, "VersionId": "old-marker"}]}, {}]
        store.client.delete_objects.side_effect = lambda **args: {"Deleted": args["Delete"]["Objects"]}
        result = store.sweep()
        self.assertEqual((result["versions"], result["deleteMarkers"], result["bytes"]), (2, 1, 5))
        self.assertEqual(store.client.delete_objects.call_args.kwargs["Delete"]["Objects"], [
            {"Key": key, "VersionId": "null"}, {"Key": key, "VersionId": "old-marker"}])

    def test_lost_reply_is_replayed_without_repeating_deletion(self):
        Path(self.spec.control).mkdir()
        helper = Helper(self.spec, "spec", self.clock)
        helper.validate_request = Mock(return_value=1)
        helper.operation = Mock(return_value={"status": "SUCCESS", "incompleteUploads": 0})
        request = {"batch": 1, "token": str(uuid.uuid4()), "sha256": "digest", "clientRunId": str(uuid.uuid4())}
        helper.respond(request)
        original = helper.channel.read("cleanup-reply.json")
        (Path(self.spec.control) / "cleanup-reply.json").unlink()
        helper.respond(request)
        self.assertEqual(helper.channel.read("cleanup-reply.json"), original)
        self.assertEqual(helper.operation.call_count, 1)

    def test_failed_batch_is_replayed_but_next_batch_is_refused(self):
        Path(self.spec.control).mkdir()
        helper = Helper(self.spec, "spec", self.clock)
        helper.validate_request = Mock(return_value=1)
        helper.operation = Mock(return_value={"status": "ERROR", "error": "UnexpectedMultipartUploads"})
        request = {"batch": 1, "token": str(uuid.uuid4()), "sha256": "digest", "clientRunId": str(uuid.uuid4())}
        helper.respond(request)
        helper.respond(request)
        with self.assertRaisesRegex(CleanupError, "another batch"):
            helper.respond(request | {"batch": 2, "token": str(uuid.uuid4())})
        self.assertEqual(helper.operation.call_count, 1)

    def test_multipart_failure_is_reported_before_recovery(self):
        store = self.store()
        store.client.list_multipart_uploads.return_value = {"Uploads": [{"Key": store.prefix + "batch/file", "UploadId": "fake-id"}]}
        store.client.list_parts.return_value = {"Parts": [{"PartNumber": 1, "Size": 7}]}
        result = store.sweep()
        self.assertEqual(result["status"], "ERROR")
        self.assertEqual(result["multipartBytes"], 7)
        self.assertEqual(result["recoveryCleanup"], "NOT_STARTED")
        store.client.abort_multipart_upload.assert_not_called()
        store.client.delete_objects.assert_not_called()

    def test_pagination_and_inventory_limits(self):
        store = self.store()
        store.client.list_objects_v2.return_value = {"IsTruncated": True, "NextContinuationToken": "same"}
        with self.assertRaisesRegex(CleanupError, "advance"):
            store.inventory()
        store.options = replace(store.options, max_pages=1)
        with self.assertRaisesRegex(CleanupError, "page limit"):
            store.inventory()
        store.options = replace(store.options, max_items=1, max_pages=10)
        store.client.list_objects_v2.return_value = {"Contents": [{"Key": store.prefix + str(n), "Size": 1} for n in range(2)]}
        with self.assertRaisesRegex(CleanupError, "budget"):
            store.inventory()

    def test_upload_and_part_pagination(self):
        store = self.store()
        store.options = replace(store.options, page_size=1)
        first, second = store.prefix + "a", store.prefix + "b"
        store.client.list_multipart_uploads.side_effect = [
            {"Uploads": [{"Key": first, "UploadId": "upload-a"}], "IsTruncated": True,
             "NextKeyMarker": first, "NextUploadIdMarker": "upload-a"},
            {"Uploads": [{"Key": second, "UploadId": "upload-b"}]}]
        store.client.list_parts.side_effect = [
            {"Parts": [{"PartNumber": 1, "Size": 2}], "IsTruncated": True, "NextPartNumberMarker": 1},
            {"Parts": [{"PartNumber": 2, "Size": 3}]}, {"Parts": []}]
        _, uploads, _ = store.inventory()
        self.assertEqual(len(uploads), 2)
        self.assertEqual((uploads[0]["parts"], uploads[0]["bytes"]), (2, 5))
        self.assertEqual(store.client.list_parts.call_args_list[1].kwargs["PartNumberMarker"], 1)

    def test_preflight_refuses_populated_prefix_and_inspection_denial(self):
        store = self.store()
        store.client.list_objects_v2.return_value = {"Contents": [{"Key": store.prefix + "foreign", "Size": 1}]}
        with self.assertRaisesRegex(CleanupError, "populated"):
            store.ready()
        store.client.put_object.assert_not_called()
        store.client.delete_objects.assert_not_called()
        store = self.store()
        store.client.get_bucket_versioning.side_effect = PermissionError("fake access denial")
        with self.assertRaises(PermissionError):
            store.ready()
        store.client.put_object.assert_not_called()

    def test_failure_ack_is_saved_even_when_journal_quota_exhausts(self):
        spec = self.spec
        Path(spec.control).mkdir()
        Path(spec.results).mkdir()
        runtime = stress.LocalRuntime(spec, [], cleanup=Mock(), clock=self.clock)
        runtime.broker = Mock()
        runtime.broker.record.return_value = {"pid": 123, "startTicks": 456}
        runtime.evidence = EvidenceStore(spec.results, spec.evidence)
        runtime.event = runtime.pulse = runtime.wait_message = Mock()
        runtime.channel.write("client.json", active=False, token="inactive")
        append = runtime.evidence.append
        def quota(name, value):
            if value.get("event") == "ACK":
                raise RuntimeError("ordinary budget exhausted")
            append(name, value)
        runtime.evidence.append = quota
        reply = {"runId": spec.run_id, "status": "ERROR", "error": "UnexpectedMultipartUploads"}
        runtime.cleanup.poll.return_value = reply
        with self.assertRaisesRegex(RuntimeError, "budget"):
            runtime.finish_batch(1, report("s3"), 2)
        final = json.loads((Path(spec.results) / "cleanup-final.json").read_text())
        self.assertEqual(final["error"], "UnexpectedMultipartUploads")

    def test_deadline_during_sdk_response_is_not_success(self):
        store = self.store()
        def slow(**_):
            self.clock.sleep(121)
            return {}
        store.client.get_bucket_versioning.side_effect = slow
        with self.assertRaises(TimeoutError):
            store.inventory()
        store.client.delete_objects.assert_not_called()

    def test_sdk_errors_never_echo_diagnostics(self):
        self.assertEqual(safe_error(RuntimeError("fake-secret URL signature")), "S3RequestFailed")

    def test_protocol_rejects_nonquiescent_client_missing_report_and_wrong_hash(self):
        Path(self.spec.control).mkdir()
        helper = Helper(self.spec, "spec", self.clock)
        helper.expiry = 90
        helper.peers = [Mock(), Mock(), Mock()]
        helper.peers[2].record.return_value = {"pid": 123, "startTicks": 456}
        request = {"batch": 1, "quiescent": False, "broker": helper.peers[2].record(), "deadline": 1}
        with self.assertRaisesRegex(CleanupError, "quiescent"):
            helper.validate_request(request)
        request["quiescent"] = True
        helper.channel.write("client.json", active=True, token="fake-token")
        with self.assertRaisesRegex(CleanupError, "reaped"):
            helper.validate_request(request)
        helper.channel.write("client.json", active=False, token="fake-token")
        helper.channel.write("client-ack.json", token="fake-token")
        path = Path(self.spec.results) / "batches/000001/client-report.json"
        path.parent.mkdir(parents=True)
        value = report("s3")
        value["config"].update(storeId=self.spec.store_id, socket=self.spec.socket, stagingRoot=self.spec.staging)
        for publication in value["publications"]:
            publication["artifact"]["store_id"] = self.spec.store_id
        path.write_text(json.dumps(value))
        request.update(report="batches/000001/client-report.json", sha256="wrong", clientRunId=value["runId"])
        with self.assertRaisesRegex(CleanupError, "digest"):
            helper.validate_request(request)
        request["sha256"] = hashlib.sha256(path.read_bytes()).hexdigest()
        self.assertEqual(helper.validate_request(request), 1)

    def test_channel_ignores_lost_or_old_reply_and_rejects_wrong_binding(self):
        Path(self.spec.control).mkdir()
        adapter = CleanupChannel(self.spec)
        adapter.peer = Mock()
        adapter.peer.record.return_value = {"pid": 123, "startTicks": 456}
        adapter.check = Mock()
        request = {"token": str(uuid.uuid4()), "batch": 1, "sha256": "digest", "clientRunId": str(uuid.uuid4())}
        adapter.request(request)
        self.assertIsNone(adapter.poll(self.spec.run_id, 1))
        adapter.channel.write("cleanup-reply.json", **(request | {"token": "old"}))
        self.assertIsNone(adapter.poll(self.spec.run_id, 1))
        adapter.channel.write("cleanup-reply.json", **(request | {"sha256": "wrong"}))
        with self.assertRaisesRegex(CleanupError, "match"):
            adapter.poll(self.spec.run_id, 1)
        adapter.channel.write("cleanup-reply.json", **request, status="SUCCESS", helper=adapter.peer.record())
        self.assertEqual(adapter.poll(self.spec.run_id, 1)["status"], "SUCCESS")

    def test_failed_cleanup_or_timeout_retains_report_and_stops_admission(self):
        for outcome in ("ERROR", None):
            with self.subTest(outcome=outcome), tempfile.TemporaryDirectory() as directory:
                spec = s3_spec(Path(directory))
                Path(spec.control).mkdir()
                Path(spec.results).mkdir()
                path = Path(spec.results) / "batches/000001/client-report.json"
                path.parent.mkdir(parents=True)
                value = report("s3")
                runtime = stress.LocalRuntime(spec, [], clock=self.clock, cleanup=Mock())
                runtime.broker = Mock()
                runtime.broker.record.return_value = {"pid": 123, "startTicks": 456}
                runtime.process = None
                runtime.evidence = EvidenceStore(spec.results, spec.evidence)
                runtime.event = runtime.pulse = Mock()
                runtime.wait_message = Mock()
                runtime.channel.write("client.json", active=False, token="inactive")
                runtime.cleanup.poll.return_value = None if outcome is None else {
                    "runId": spec.run_id, "status": "ERROR", "error": "UnexpectedMultipartUploads"}
                with self.assertRaises((RuntimeError, TimeoutError)):
                    runtime.finish_batch(1, value, self.clock.monotonic() + 2)
                self.assertEqual(json.loads(path.read_text()), value)
                self.assertEqual(runtime.cleanup.request.call_count, 1)

    def test_cleanup_evidence_rejects_missing_reordered_or_changed_ack(self):
        root = Path(self.spec.results)
        root.mkdir()
        store = EvidenceStore(root, self.spec.evidence)
        helper = {"pid": 123, "startTicks": 456}
        supervisor = {"broker": {"pid": 321, "startTicks": 654},
                      "helper": {"helper": helper, "status": "STOPPED", "remoteCleanup": "VERIFIED"}}
        store.write("cleanup-readiness.json", {"runId": self.spec.run_id, "status": "READY", "helper": helper,
                    "credentialExpiry": self.spec.s3.credential_expiry, "credentialProvider": self.spec.s3.credential_provider,
                    "incompleteUploads": 0})
        value = report("s3")
        name = "batches/000001/client-report.json"
        store.write(name, value)
        request = {"runId": self.spec.run_id, "event": "REQUEST", "batch": 1, "token": str(uuid.uuid4()),
                   "quiescent": True, "broker": supervisor["broker"], "report": name,
                   "clientRunId": value["runId"], "sha256": hashlib.sha256((root / name).read_bytes()).hexdigest(), "deadline": 2}
        ack = {key: request[key] for key in ("runId", "batch", "token", "clientRunId", "sha256")}
        ack.update(event="ACK", status="SUCCESS", helper=helper, incompleteUploads=0, started=1, finished=1.5,
                   objects=3, versions=0, deleteMarkers=0, bytes=5)
        store.write("batches.jsonl", {"event": "COMPLETE", "batch": 1, "clientRunId": value["runId"], "started": 0, "finished": 1.6})
        store.write("cleanup.jsonl", b"")
        store.append("cleanup.jsonl", request)
        with self.assertRaises(IncompleteEvidence):
            validate_cleanup(root, self.spec, supervisor)
        store.append("cleanup.jsonl", ack)
        store.write("cleanup-final.json", ack)
        validate_cleanup(root, self.spec, supervisor)
        store.write("cleanup-final.json", ack | {"status": "ERROR"})
        with self.assertRaises(IncompleteEvidence):
            validate_cleanup(root, self.spec, supervisor)
        store.write("cleanup-final.json", ack)
        store.append("cleanup.jsonl", request)
        with self.assertRaises(ValueError):
            validate_cleanup(root, self.spec, supervisor)


if __name__ == "__main__":
    unittest.main()
