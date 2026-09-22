# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

import contextlib
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
            args = SimpleNamespace(tls=False, image=None, binary="unused-test-binary")
            with self.assertRaises(RuntimeError) as raised:
                s3_integration.run(args, environment)
            self.assertIs(raised.exception, failure)
            self.assertFalse(Path(environment["PAGEBROKER_S3_TEST_FIXTURE"]).parent.exists())


if __name__ == "__main__":
    unittest.main()
