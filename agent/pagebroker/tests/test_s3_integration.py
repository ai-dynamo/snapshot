# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

import contextlib
import io
import os
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

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


class FaultServiceTest(unittest.TestCase):
    def test_partial_tree_fault_allows_first_object_and_only_denies_bad_put(self):
        forwarded = []

        def backend(environment, start_response):
            forwarded.append((environment["REQUEST_METHOD"], environment["PATH_INFO"]))
            start_response("200 OK", [("Content-Length", "2")])
            return [b"ok"]

        service = s3_integration.FaultService(backend)
        statuses = []
        for method, name in (("PUT", "first"), ("PUT", "bad"), ("HEAD", "first")):
            service({"PATH_INFO": "/bucket/roundtrip/engine-partial/" + name,
                     "QUERY_STRING": "", "REQUEST_METHOD": method},
                    lambda status, headers: statuses.append(status))
        self.assertEqual(statuses, ["200 OK", "403 Forbidden", "200 OK"])
        self.assertEqual(forwarded, [("PUT", "/bucket/roundtrip/engine-partial/first"),
                                     ("HEAD", "/bucket/roundtrip/engine-partial/first")])

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
            accepted.append(True)
            start_response("200 OK", [("Content-Length", "2")])
            return [b"ok"]

        service = s3_integration.FaultService(backend)
        statuses = []
        body = service({"PATH_INFO": "/bucket/roundtrip/lost-complete/data", "QUERY_STRING": "uploadId=test-id",
                        "REQUEST_METHOD": "POST"}, lambda status, headers: statuses.append(status))
        self.assertEqual(accepted, [True])
        self.assertEqual(statuses, ["503 Service Unavailable"])
        self.assertIn(b"ServiceUnavailable", b"".join(body))

    def test_expected_cancelled_body_does_not_hide_unrelated_backend_errors(self):
        def backend(environment, start_response):
            raise OSError("Invalid chunk header")

        service = s3_integration.FaultService(backend)
        environment = {"PATH_INFO": "/bucket/roundtrip/source-truncated/data", "QUERY_STRING": "",
                       "REQUEST_METHOD": "PUT"}
        statuses = []
        service(environment, lambda status, headers: statuses.append(status))
        self.assertEqual(statuses, ["408 Request Timeout"])
        environment["PATH_INFO"] = "/bucket/roundtrip/ordinary/data"
        with self.assertRaises(OSError):
            service(environment, lambda status, headers: None)


if __name__ == "__main__":
    unittest.main()
