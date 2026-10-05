#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Run native S3 uploads and restores against a disposable local Moto service.

Requires boto3 and moto[server]. Uses fixed test credentials and an in-memory
restore plan; no cloud account or artifact index is needed.
"""

import argparse
import contextlib
from collections import Counter
import logging
import os
from pathlib import Path
import subprocess
import tempfile
import threading
import time
from urllib.parse import parse_qs
import uuid

import boto3
import botocore.session
from botocore.config import Config


class FaultService:
    """Script S3 failures only for native-test keys under roundtrip/.

    Successful round-trip payloads always come from the C++ uploader. Counters
    let the Python side independently check retries, cleanup and resource caps.
    """

    def __init__(self, app):
        self.app = app
        self.changed = threading.Condition()
        self.counts = Counter()
        self.first_parts = set()
        self.released = set()
        self.active_parts = 0
        self.peak_parts = 0
        self.active_uploads = set()
        self.peak_uploads = 0

    @staticmethod
    def error(start_response, code="ServiceUnavailable", status="503 Service Unavailable"):
        body = f"<Error><Code>{code}</Code><Message>injected local test failure</Message></Error>".encode()
        start_response(status, [("Content-Type", "application/xml"), ("Content-Length", str(len(body)))])
        return [body]

    def __call__(self, environment, start_response):
        key = environment.get("PATH_INFO", "").split("/", 2)[-1]
        if not key.startswith("roundtrip/"):
            return self.app(environment, start_response)
        scenario = key.split("/")[1]
        query = parse_qs(environment.get("QUERY_STRING", ""), keep_blank_values=True)
        method = environment["REQUEST_METHOD"]
        operation = {"PUT": "put", "GET": "get", "HEAD": "head"}.get(method, method)
        if "uploads" in query:
            operation = "create" if method == "POST" else "list_uploads"
        if "uploadId" in query:
            operation = {"PUT": "part", "POST": "complete", "DELETE": "abort", "GET": "list_parts"}[method]
        with self.changed:
            self.counts[scenario, operation] += 1
            attempt = self.counts[scenario, operation]
            if scenario == "bounded" and operation == "part":
                self.active_parts += 1
                self.peak_parts = max(self.peak_parts, self.active_parts)
        try:
            if scenario in ("wait", "release"):
                target = "roundtrip/" + key.split("/")[2] + "/data"
                with self.changed:
                    if scenario == "wait":
                        if not self.changed.wait_for(lambda: target in self.first_parts, timeout=10):
                            return self.error(start_response)
                    else:
                        self.released.add(target)
                        self.changed.notify_all()
                start_response("200 OK", [("Content-Length", "0")])
                return [b""]
            if scenario == "retry" and operation == "put" and attempt <= 2:
                return self.error(start_response)
            if scenario in ("denied", "concurrent-failure") and operation == "put":
                return self.error(start_response, "AccessDenied", "403 Forbidden")
            if scenario == "engine-partial" and operation == "put" and key.endswith("/bad"):
                return self.error(start_response, "AccessDenied", "403 Forbidden")
            if scenario in ("part-failure", "abort-failure", "verify-failure") and operation == "part" and query["partNumber"] != ["1"]:
                with self.changed:
                    if not self.changed.wait_for(lambda: key in self.first_parts, timeout=10):
                        raise RuntimeError("first multipart part did not complete")
                return self.error(start_response)
            if scenario == "abort-failure" and operation == "abort":
                return self.error(start_response)
            if scenario == "verify-failure" and operation == "list_parts":
                return self.error(start_response)
            if scenario == "complete-error" and operation == "complete":
                return self.error(start_response, "InternalError", "200 OK")
            if scenario in ("deadline", "multipart-deadline") and operation in ("put", "part"):
                time.sleep(1)
                return self.error(start_response)
            if scenario == "source-truncated" and operation == "part" and query["partNumber"] != ["1"]:
                with self.changed:
                    if not self.changed.wait_for(lambda: key in self.released, timeout=10):
                        return self.error(start_response)

            response = {}

            def capture(status, headers, exc_info=None):
                response.update(status=status, headers=headers)

            try:
                iterable = self.app(environment, capture)
                try:
                    body = b"".join(iterable)
                finally:
                    if hasattr(iterable, "close"):
                        iterable.close()
            except OSError as error:
                # Source truncation cancels outstanding chunked HTTP bodies.
                # Werkzeug reports their expected early EOF as an OSError.
                if scenario != "source-truncated" or str(error) != "Invalid chunk header":
                    raise
                return self.error(start_response, "RequestTimeout", "408 Request Timeout")
            success = response["status"].startswith("2")
            with self.changed:
                if success and operation == "part" and query["partNumber"] == ["1"]:
                    self.first_parts.add(key)
                    self.changed.notify_all()
                    if scenario in ("source-truncated", "engine-overlap"):
                        if not self.changed.wait_for(lambda: key in self.released, timeout=10):
                            return self.error(start_response)
                if scenario == "bounded" and success:
                    if operation == "create":
                        self.active_uploads.add(key)
                        self.peak_uploads = max(self.peak_uploads, len(self.active_uploads))
                    elif operation in ("complete", "abort"):
                        self.active_uploads.discard(key)
            if scenario == "lost-create" and operation == "create" and success:
                return self.error(start_response)
            if scenario == "lost-complete" and operation == "complete":
                return self.error(start_response)
            if scenario == "lost-put" and operation == "put":
                return self.error(start_response)
            if scenario == "corrupt" and operation == "get" and success and body:
                body = bytes([body[0] ^ 1]) + body[1:]
            start_response(response["status"], response["headers"])
            return [body]
        finally:
            if scenario == "bounded" and operation == "part":
                with self.changed:
                    self.active_parts -= 1

    def verify(self, client, bucket):
        # Run only assertions for cases actually selected by the native filter.
        assert not any(count for (_, operation), count in self.counts.items() if operation == "DELETE")
        assert not any(count for (scenario, _), count in self.counts.items() if scenario == "preflight"), self.counts
        if self.counts["engine-partial", "put"]:
            assert self.counts["engine-partial", "put"] == 2, self.counts
            assert client.head_object(Bucket=bucket, Key="roundtrip/engine-partial/first")["ContentLength"] > 0
            keys = client.list_objects_v2(Bucket=bucket, Prefix="roundtrip/engine-partial/")
            assert [item["Key"] for item in keys.get("Contents", [])] == ["roundtrip/engine-partial/first"], keys
        if self.counts["retry", "put"]:
            assert self.counts["retry", "put"] == 3, self.counts
        if self.counts["denied", "put"]:
            assert self.counts["denied", "put"] == 1, self.counts
        if self.counts["part-failure", "create"]:
            assert self.counts["part-failure", "abort"] == 1, self.counts
            assert self.counts["part-failure", "complete"] == 0, self.counts
            uploads = client.list_multipart_uploads(Bucket=bucket, Prefix="roundtrip/part-failure/")
            assert not uploads.get("Uploads"), uploads
        if self.counts["abort-failure", "abort"]:
            # Two cleanup rounds, three attempts per request in the native test.
            assert self.counts["abort-failure", "abort"] == 6, self.counts
        for scenario in ("multipart-deadline", "source-truncated"):
            if self.counts[scenario, "create"]:
                assert self.counts[scenario, "abort"] >= 1, self.counts
                assert not client.list_multipart_uploads(Bucket=bucket, Prefix=f"roundtrip/{scenario}/").get("Uploads")
        if self.counts["lost-create", "create"]:
            assert self.counts["lost-create", "create"] == 1, self.counts
        for scenario in ("lost-complete", "lost-put"):
            if self.counts[scenario, "complete"] or self.counts[scenario, "put"]:
                metadata = client.head_object(Bucket=bucket, Key=f"roundtrip/{scenario}/data")
                assert metadata["ContentLength"] > 0
        if self.counts["bounded", "create"]:
            assert 0 < self.peak_parts <= 2, self.peak_parts
            assert 0 < self.peak_uploads <= 2, self.peak_uploads
            assert not self.active_uploads
        if self.counts["tree", "put"]:
            metadata = client.head_object(Bucket=bucket, Key="roundtrip/tree/empty")
            assert metadata["ContentLength"] == 0


def local_environment():
    environment = {name: value for name, value in os.environ.items()
                   if not name.startswith(("AWS_", "PAGEBROKER_S3_TEST_"))}
    environment.update(
        AWS_ACCESS_KEY_ID="local-test-access-key",
        AWS_SECRET_ACCESS_KEY="local-test-secret-key",
        AWS_DEFAULT_REGION="us-east-1",
        RUNAI_STREAMER_S3_USE_VIRTUAL_ADDRESSING="0",
        AWS_CONFIG_FILE=os.devnull,
        AWS_SHARED_CREDENTIALS_FILE=os.devnull,
        AWS_EC2_METADATA_DISABLED="true",
    )
    return environment


@contextlib.contextmanager
def local_service(args, environment, service_factory=FaultService):
    from moto.server import DomainDispatcherApplication, create_backend_app
    from werkzeug.serving import make_server

    logging.getLogger("werkzeug").setLevel(logging.ERROR)
    with tempfile.TemporaryDirectory(prefix="pagebroker-s3-tls-") as directory:
        tls = None
        if args.tls:
            certificate = Path(directory) / "ca.pem"
            key = Path(directory) / "key.pem"
            subprocess.run(
                ["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
                 "-subj", "/CN=localhost", "-addext", "subjectAltName=IP:127.0.0.1",
                 "-keyout", str(key), "-out", str(certificate)],
                check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
            )
            environment["AWS_CA_BUNDLE"] = str(certificate)
            tls = (str(certificate), str(key))
        faults = service_factory(DomainDispatcherApplication(create_backend_app))
        server = make_server("127.0.0.1", 0, faults,
                             threaded=True, ssl_context=tls)
        worker = threading.Thread(target=server.serve_forever)
        worker.start()
        try:
            scheme = "https" if args.tls else "http"
            environment["AWS_ENDPOINT_URL"] = f"{scheme}://127.0.0.1:{server.server_port}"
            yield faults
        finally:
            server.shutdown()
            worker.join()
            server.server_close()


def make_fixture(root):
    (root / "nested" / "empty-directory").mkdir(parents=True)
    (root / "empty").touch()
    (root / "small").write_bytes(b"checkpoint\x00binary\xff\n")
    (root / "nested" / "space + percent% question? hash# unicode-é").write_bytes(
        bytes(range(256)) * 17
    )
    # Crosses both the native reader chunk size and two default upload parts.
    with (root / "nested" / "large").open("wb") as file:
        for _ in range(512):
            file.write(bytes(range(256)) * 256)
        file.write(b"tail")
    root.chmod(0o750)
    (root / "nested").chmod(0o750)
    (root / "nested" / "empty-directory").chmod(0o500)
    for path in root.rglob("*"):
        if path.is_file():
            path.chmod(0o400 if path.name == "small" else 0o640)


def run_native(command, environment, container_name, timeout):
    try:
        result = subprocess.run(command, env=environment, check=False, timeout=timeout)
        if result.returncode:
            raise RuntimeError(f"native S3 integration tests exited with status {result.returncode}")
    finally:
        if container_name:
            # Killing the Docker client on timeout does not stop its container.
            subprocess.run(["docker", "rm", "--force", container_name],
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False)


def create_session(environment):
    # A copied environment does not isolate boto3: profile/config providers
    # consult os.environ themselves. Override those providers for the fixture.
    core = botocore.session.Session(session_vars={
        "profile": (None, None, None, None),
        "config_file": (None, None, os.devnull, None),
        "credentials_file": (None, None, os.devnull, None),
    })
    return boto3.Session(
        botocore_session=core,
        aws_access_key_id=environment["AWS_ACCESS_KEY_ID"],
        aws_secret_access_key=environment["AWS_SECRET_ACCESS_KEY"],
        region_name=environment["AWS_DEFAULT_REGION"],
    )


def run(args, environment, faults=None):
    session = create_session(environment)
    client = session.client(
        "s3",
        endpoint_url=environment["AWS_ENDPOINT_URL"],
        verify=environment.get("AWS_CA_BUNDLE", True),
        config=Config(s3={"addressing_style": "path"}, retries={"max_attempts": 2}),
    )
    bucket = "pagebroker-local-test"
    prefix = "fixture/"
    environment.update(PAGEBROKER_S3_TEST_BUCKET=bucket, PAGEBROKER_S3_TEST_PREFIX=prefix)
    environment.setdefault("RUNAI_STREAMER_S3_MAX_RETRIES", "1")
    environment.setdefault("RUNAI_STREAMER_S3_TIMEOUT", "2")
    environment.setdefault("RUNAI_STREAMER_S3_REQUEST_TIMEOUT_MS", "1000")
    environment.setdefault("RUNAI_STREAMER_LOG_LEVEL", "ERROR")
    # Moto keeps the bucket and objects in memory for this test process only.
    with contextlib.closing(client), tempfile.TemporaryDirectory(prefix="pagebroker-s3-") as directory:
        client.create_bucket(Bucket=bucket)
        fixture = Path(directory) / "fixture"
        fixture.mkdir()
        make_fixture(fixture)
        for path in sorted(fixture.rglob("*")):
            if not path.is_file():
                continue
            key = prefix + path.relative_to(fixture).as_posix()
            with path.open("rb") as body:
                client.put_object(Bucket=bucket, Key=key, Body=body)
            # Model Streamer skips zero-length remote reads. Independently
            # confirm object existence and length, including the empty object.
            metadata = client.head_object(Bucket=bucket, Key=key)
            if metadata["ContentLength"] != path.stat().st_size:
                raise RuntimeError("S3 fixture size mismatch")
        environment["PAGEBROKER_S3_TEST_FIXTURE"] = str(fixture)
        container_name = None
        if args.image:
            container_name = "pagebroker-s3-test-" + uuid.uuid4().hex
            command = ["docker", "run", "--rm", "--network=host", "--user", f"{os.getuid()}:{os.getgid()}"]
            command += ["--name", container_name]
            command += ["--mount", f"type=bind,source={fixture},target={fixture},readonly"]
            if environment.get("AWS_CA_BUNDLE"):
                ca = Path(environment["AWS_CA_BUNDLE"]).resolve()
                environment["AWS_CA_BUNDLE"] = str(ca)
                command += ["--mount", f"type=bind,source={ca},target={ca},readonly"]
            for name in sorted(environment):
                if name.startswith(("AWS_", "RUNAI_STREAMER_", "PAGEBROKER_S3_TEST_", "GTEST_")):
                    command += ["--env", name]
            command.append(args.image)
        else:
            command = [str(Path(args.binary).resolve())]
        run_native(command, environment, container_name, 300)
        if faults:
            faults.verify(client, bucket)
        if args.resources:
            for size in (64, 128):
                probe = dict(environment)
                probe["GTEST_FILTER"] = "S3RoundTripTest.*MemoryProbe"
                probe["PAGEBROKER_S3_TEST_MEMORY_MIB"] = str(size)
                resource_command = list(command)
                if args.image:
                    resource_command[-1:-1] = ["--env", "GTEST_FILTER", "--env", "PAGEBROKER_S3_TEST_MEMORY_MIB"]
                run_native(resource_command, probe, container_name, 120)
        if args.tls:
            # A separate process is necessary: the pinned native library
            # caches its AWS configuration for the process lifetime.
            untrusted = dict(environment)
            untrusted["AWS_CA_BUNDLE"] = "/etc/ssl/certs/ca-certificates.crt"
            untrusted["GTEST_FILTER"] = "ModelStreamerS3Test.RejectsUntrustedTLS:S3RoundTripTest.RejectsUntrustedTLS"
            untrusted["PAGEBROKER_S3_TEST_TLS_REJECT"] = "1"
            rejection = list(command)
            if args.image:
                rejection[-1:-1] = ["--env", "GTEST_FILTER", "--env", "PAGEBROKER_S3_TEST_TLS_REJECT"]
            # Both the standalone reader and engine reader reject this CA.
            # Native teardown drains each session's outstanding TLS retries,
            # so allow both lifetimes to finish before the outer watchdog.
            run_native(rejection, untrusted, container_name, 120)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    target = parser.add_mutually_exclusive_group(required=True)
    target.add_argument("--binary", help="native model-streamer-s3-test executable")
    target.add_argument("--image", help="PageBroker Docker image built with --target s3-test (Linux)")
    parser.add_argument("--tls", action="store_true", help="use verified HTTPS with a temporary local CA")
    parser.add_argument("--resources", action="store_true", help="also check upload memory in isolated native processes")
    args = parser.parse_args()
    environment = local_environment()
    with local_service(args, environment) as faults:
        run(args, environment, faults)


if __name__ == "__main__":
    main()
