# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

import contextlib
import io
import os
from pathlib import Path
import tempfile
import threading
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch
from urllib.error import HTTPError
from urllib.request import urlopen

from moto import mock_aws

import s3_integration


class SessionTest(unittest.TestCase):
    def test_ignores_inherited_aws_configuration(self):
        with tempfile.TemporaryDirectory() as directory:
            config = Path(directory) / "config"
            config.write_text("invalid configuration, not an INI file")
            for config_path in (config, Path(directory) / "missing-config"):
                with self.subTest(config=config_path), patch.dict(os.environ, {
                    "AWS_PROFILE": "nonexistent-profile",
                    "AWS_DEFAULT_PROFILE": "another-nonexistent-profile",
                    "AWS_CONFIG_FILE": str(config_path),
                    "AWS_SHARED_CREDENTIALS_FILE": str(Path(directory) / "missing-credentials"),
                    "AWS_ACCESS_KEY_ID": "inherited-test-access-key",
                    "AWS_SECRET_ACCESS_KEY": "inherited-test-secret-key",
                    "AWS_SESSION_TOKEN": "inherited-test-session-token",
                    "AWS_DEFAULT_REGION": "us-west-2",
                    "AWS_ENDPOINT_URL": "https://inherited.example.invalid",
                    "AWS_ENDPOINT_URL_S3": "https://inherited-s3.example.invalid",
                    "AWS_CA_BUNDLE": "/nonexistent/ca.pem",
                    "PAGEBROKER_S3_TEST_BUCKET": "inherited-test-bucket",
                    "PAGEBROKER_S3_TEST_TLS_REJECT": "1",
                    "UNCHANGED": "value",
                }, clear=True):
                    inherited = dict(os.environ)
                    environment = s3_integration.local_environment()
                    session = s3_integration.create_session(environment)
                    credentials = session.get_credentials().get_frozen_credentials()
                    self.assertEqual(credentials.access_key, "local-test-access-key")
                    self.assertEqual(credentials.secret_key, "local-test-secret-key")
                    self.assertIsNone(credentials.token)
                    self.assertEqual(session.region_name, "us-east-1")
                    for name in ("AWS_PROFILE", "AWS_DEFAULT_PROFILE", "AWS_SESSION_TOKEN",
                                 "AWS_ENDPOINT_URL", "AWS_ENDPOINT_URL_S3", "AWS_CA_BUNDLE",
                                 "PAGEBROKER_S3_TEST_BUCKET", "PAGEBROKER_S3_TEST_TLS_REJECT"):
                        self.assertNotIn(name, environment)
                    self.assertEqual(environment["UNCHANGED"], "value")
                    self.assertEqual(dict(os.environ), inherited)


class S3FixtureTest(unittest.TestCase):
    def test_resource_upload_checks_size_and_bytes_and_closes_response(self):
        for name, contents, reported, expected_error in (
                ("valid", b"m" * 131073, 131073, None),
                ("wrong-size", b"mmm", 4, "size mismatch"),
                ("corrupt", b"mxm", 3, "contents mismatch"),
                ("truncated", b"mm", 3, "truncated"),
                ("extra-bytes", b"mmmm", 3, "contents mismatch")):
            with self.subTest(name=name):
                body = io.BytesIO(contents)
                client = Mock()
                client.get_object.return_value = dict(Body=body, ContentLength=reported)
                size = 131073 if name == "valid" else 3
                if expected_error:
                    with self.assertRaisesRegex(AssertionError, expected_error):
                        s3_integration.verify_resource_upload(client, "bucket", "key", size)
                else:
                    s3_integration.verify_resource_upload(client, "bucket", "key", size)
                self.assertTrue(body.closed)
                client.get_object.assert_called_once_with(Bucket="bucket", Key="key")

    def test_native_failure_removes_local_fixture(self):
        with patch.dict(os.environ, {}, clear=True), mock_aws(), contextlib.ExitStack() as stack:
            environment = s3_integration.local_environment()
            environment["AWS_ENDPOINT_URL"] = "http://127.0.0.1:1"
            session = s3_integration.create_session(environment)
            client = session.client("s3")
            stack.enter_context(patch.object(session, "client", return_value=client))
            stack.enter_context(patch.object(s3_integration, "create_session", return_value=session))
            failure = RuntimeError("native restore failed")
            stack.enter_context(patch.object(s3_integration, "run_native", side_effect=failure))
            args = SimpleNamespace(tls=False, resources=False, image=None, binary="unused-test-binary")
            with self.assertRaises(RuntimeError) as raised:
                s3_integration.run(args, environment)
            self.assertIs(raised.exception, failure)
            self.assertFalse(Path(environment["PAGEBROKER_S3_TEST_FIXTURE"]).parent.exists())


class CheckedServiceTest(unittest.TestCase):
    def test_records_application_iteration_and_close_errors(self):
        for stage in ("application", "iteration", "close"):
            with self.subTest(stage=stage):
                failure = RuntimeError("unexpected fixture failure")

                class Body:
                    def __iter__(self):
                        if stage == "iteration":
                            raise failure
                        yield b"ok"

                    def close(self):
                        if stage == "close":
                            raise failure

                def app(environment, start_response):
                    if stage == "application":
                        raise failure
                    return Body()

                service = s3_integration.CheckedService(app)
                environment = dict(REQUEST_METHOD="POST", PATH_INFO="/bucket/roundtrip/memory/512",
                                   QUERY_STRING="uploadId=test-id&partNumber=2&signature=private-value")
                with self.assertRaises(RuntimeError) as raised:
                    list(service(environment, Mock()))
                self.assertIs(raised.exception, failure)
                with self.assertRaises(AssertionError) as raised:
                    service.verify()
                message = str(raised.exception)
                for value in ("POST", "/bucket/roundtrip/memory/512", "test-id", "RuntimeError"):
                    self.assertIn(value, message)
                self.assertNotIn("private-value", message)

    def test_injected_http_failure_is_not_an_unexpected_exception(self):
        service = s3_integration.CheckedService(s3_integration.FaultService(Mock()))
        statuses = []
        body = list(service(dict(REQUEST_METHOD="PUT", PATH_INFO="/bucket/roundtrip/denied/data"),
                            lambda status, headers: statuses.append(status)))
        self.assertEqual(statuses, ["403 Forbidden"])
        self.assertIn(b"AccessDenied", b"".join(body))
        service.verify()

    def test_worker_exception_fails_service_exit_after_successful_request(self):
        def app(environment, start_response):
            if environment["PATH_INFO"] == "/resource":
                raise KeyError("test-upload-id")
            start_response("200 OK", [("Content-Length", "2")])
            return [b"ok"]

        environment = {}
        args = SimpleNamespace(tls=False)
        with self.assertLogs("werkzeug", level="ERROR"), \
                self.assertRaisesRegex(AssertionError, "Unexpected fixture exceptions.*resource.*KeyError"):
            with s3_integration.local_service(args, environment, lambda backend: app):
                with urlopen(environment["AWS_ENDPOINT_URL"], timeout=5) as response:
                    self.assertEqual(response.read(), b"ok")
                with self.assertRaises(HTTPError) as raised:
                    urlopen(environment["AWS_ENDPOINT_URL"] + "/resource", timeout=5)
                self.assertEqual(raised.exception.code, 500)
                raised.exception.close()

    def test_service_exit_waits_for_late_worker_failure(self):
        from werkzeug.serving import make_server

        closing = threading.Event()

        def app(environment, start_response):
            start_response("200 OK", [("Content-Length", "2")])
            yield b"ok"
            if not closing.wait(timeout=5):
                raise RuntimeError("server did not close")
            raise KeyError("late-upload-id")

        def server_factory(*args, **kwargs):
            server = make_server(*args, **kwargs)
            close = server.server_close

            def finish():
                closing.set()
                close()

            server.server_close = finish
            return server

        environment = {}
        with patch("werkzeug.serving.make_server", side_effect=server_factory), \
                self.assertLogs("werkzeug", level="ERROR"), \
                self.assertRaisesRegex(AssertionError, "Unexpected fixture exceptions.*KeyError"):
            with s3_integration.local_service(SimpleNamespace(tls=False), environment, lambda backend: app):
                with urlopen(environment["AWS_ENDPOINT_URL"], timeout=5) as response:
                    self.assertEqual(response.read(), b"ok")


class FaultServiceTest(unittest.TestCase):
    def test_abort_failure_requires_an_abort_after_creation(self):
        service = s3_integration.FaultService(Mock())
        client = Mock()
        client.list_multipart_uploads.return_value = {"Uploads": [{"UploadId": "test-upload"}]}
        service.counts["abort-failure", "create"] = 1
        with self.assertRaises(AssertionError):
            service.verify(client, "test-bucket")
        service.counts["abort-failure", "abort"] = 1
        service.verify(client, "test-bucket")
        client.list_multipart_uploads.assert_called_once_with(
            Bucket="test-bucket", Prefix="roundtrip/abort-failure/")

    def test_slot_owner_barrier_holds_response_until_release(self):
        def backend(environment, start_response):
            start_response("200 OK", [("Content-Length", "0")])
            return [b""]

        service = s3_integration.FaultService(backend)
        key = "roundtrip/slot-owner/data"
        statuses = []
        owner = threading.Thread(target=lambda: service(
            {"PATH_INFO": "/bucket/" + key, "QUERY_STRING": "", "REQUEST_METHOD": "PUT"},
            lambda status, headers: statuses.append(status)))
        owner.start()
        with service.changed:
            held = service.changed.wait_for(lambda: key in service.first_parts, timeout=2)
        still_running = owner.is_alive()
        service({"PATH_INFO": "/bucket/roundtrip/release/slot-owner", "QUERY_STRING": "",
                 "REQUEST_METHOD": "HEAD"}, lambda status, headers: None)
        owner.join(timeout=2)
        self.assertTrue(held)
        self.assertTrue(still_running)
        self.assertFalse(owner.is_alive())
        self.assertEqual(statuses, ["200 OK"])

    def test_partial_tree_fault_allows_first_object_and_only_denies_bad_put(self):
        forwarded = []

        def backend(environment, start_response):
            forwarded.append((environment["REQUEST_METHOD"], environment["PATH_INFO"]))
            start_response("200 OK", [("Content-Length", "2")])
            return [b"ok"]

        service = s3_integration.FaultService(backend)
        statuses = []
        prefix = "/bucket/roundtrip/engine-partial/" + "artifacts/a/containers/main" + "/data/"
        for method, name in (("PUT", "data"), ("PUT", "last"), ("HEAD", "data")):
            service({"PATH_INFO": prefix + name,
                     "QUERY_STRING": "", "REQUEST_METHOD": method},
                    lambda status, headers: statuses.append(status))
        self.assertEqual(statuses, ["200 OK", "403 Forbidden", "200 OK"])
        self.assertEqual(forwarded, [("PUT", prefix + "data"), ("HEAD", prefix + "data")])

    def test_retries_are_scoped_to_the_native_test_prefix(self):
        forwarded = []

        def backend(environment, start_response):
            forwarded.append(environment["PATH_INFO"])
            start_response("200 OK", [("Content-Length", "2")])
            return [b"ok"]

        service = s3_integration.FaultService(backend)
        statuses = []
        for key in ("fixture/retry", "roundtrip/retry/data", "roundtrip/retry/data", "roundtrip/retry/data"):
            environment = {"PATH_INFO": "/bucket/" + key, "QUERY_STRING": "", "REQUEST_METHOD": "PUT",
                           "wsgi.input": io.BytesIO(b"")}
            service(environment, lambda status, headers: statuses.append(status))
        self.assertEqual(statuses, ["200 OK", "503 Service Unavailable", "503 Service Unavailable", "200 OK"])
        self.assertEqual(forwarded, ["/bucket/fixture/retry", "/bucket/roundtrip/retry/data"])

    def test_lost_response_happens_after_backend_completion(self):
        accepted = []

        def backend(environment, start_response):
            start_response("200 OK", [("Content-Length", "2")])

            def body():
                yield b"ok"
                accepted.append(True)

            return body()

        service = s3_integration.FaultService(backend)
        statuses = []
        body = service({"PATH_INFO": "/bucket/roundtrip/lost-complete/data", "QUERY_STRING": "uploadId=test-id",
                        "REQUEST_METHOD": "POST"}, lambda status, headers: statuses.append(status))
        self.assertEqual(accepted, [True])
        self.assertEqual(statuses, ["503 Service Unavailable"])
        self.assertIn(b"ServiceUnavailable", b"".join(body))

    def test_expected_cancelled_body_does_not_hide_unrelated_backend_errors(self):
        for during_iteration in (False, True):
            for message in ("Invalid chunk header", "unrelated backend error"):
                with self.subTest(during_iteration=during_iteration, message=message):
                    def body():
                        yield b"partial body"
                        raise OSError(message)

                    def backend(environment, start_response):
                        if during_iteration:
                            start_response("200 OK", [])
                            return body()
                        raise OSError(message)

                    service = s3_integration.FaultService(backend)
                    environment = {"PATH_INFO": "/bucket/roundtrip/source-truncated/data", "QUERY_STRING": "",
                                   "REQUEST_METHOD": "PUT"}
                    statuses = []
                    if message == "Invalid chunk header":
                        service(environment, lambda status, headers: statuses.append(status))
                        self.assertEqual(statuses, ["408 Request Timeout"])
                    else:
                        with self.assertRaises(OSError):
                            service(environment, lambda status, headers: None)
                    environment["PATH_INFO"] = "/bucket/roundtrip/ordinary/data"
                    with self.assertRaises(OSError):
                        service(environment, lambda status, headers: None)


if __name__ == "__main__":
    unittest.main()
