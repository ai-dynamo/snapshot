#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Run native S3 uploads and restores against a disposable local Moto service.

Requires boto3 and moto[server]. Uses fixed test credentials and an in-memory
restore inventory; no cloud account or artifact index is needed.
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
        if "list-type=" in environment.get("QUERY_STRING", "") and "rclone/" in environment.get("HTTP_USER_AGENT", ""):
            return self.error(start_response, "AccessDenied", "403 Forbidden")
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
                target = "roundtrip/" + key.split("/", 2)[2] + "/data"
                with self.changed:
                    if scenario == "wait":
                        if not self.changed.wait_for(lambda: target in self.first_parts, timeout=10):
                            return self.error(start_response)
                    else:
                        self.released.add(target)
                        self.changed.notify_all()
                start_response("200 OK", [("Content-Length", "0")])
                return [b""]
            if scenario == "metadata-stalled" and operation == "head":
                time.sleep(2)
                return self.error(start_response)
            if scenario in (f"metadata-{operation}-0", f"metadata-{operation}-2"):
                return self.error(start_response)
            if scenario == "stalled" and operation == "put":
                time.sleep(8)
                return self.error(start_response)
            if scenario == "no-retry" and operation == "put":
                return self.error(start_response)
            if scenario == "retry" and operation == "put" and attempt <= 2:
                return self.error(start_response)
            if operation == "put" and (scenario in ("denied", "concurrent-failure")
                                       or (scenario == "bounded" and key.endswith("/data/bad"))):
                return self.error(start_response, "AccessDenied", "403 Forbidden")
            if scenario == "engine-partial" and operation == "put" and key.endswith("/" + "artifacts/a/containers/main" + "/data/last"):
                return self.error(start_response, "AccessDenied", "403 Forbidden")
            if scenario in ("part-failure", "abort-failure", "verify-failure") and operation == "part" and query["partNumber"] != ["1"]:
                with self.changed:
                    if not self.changed.wait_for(lambda: key in self.first_parts, timeout=10):
                        raise RuntimeError("first multipart part did not complete")
                if scenario == "abort-failure":
                    return self.error(start_response, "AccessDenied", "403 Forbidden")
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
            if success and operation == "head" and scenario in ("metadata-grow", "metadata-shrink"):
                # Report the previous size so GET observes a change after HEAD.
                response["headers"] = [(name, str(1 if scenario == "metadata-grow" else 3)
                                        if name.lower() == "content-length" else value)
                                       for name, value in response["headers"]]
            with self.changed:
                if success and scenario == "metadata-cancel-" + operation:
                    self.first_parts.add(key)
                    self.changed.notify_all()
                    if not self.changed.wait_for(lambda: key in self.released, timeout=10):
                        return self.error(start_response)
                if scenario == "slot-owner" and success and operation == "put":
                    self.first_parts.add(key)
                    self.changed.notify_all()
                    if not self.changed.wait_for(lambda: key in self.released, timeout=10):
                        return self.error(start_response)
                if success and operation == "part" and query["partNumber"] == ["1"]:
                    self.first_parts.add(key)
                    self.changed.notify_all()
                    if scenario in ("source-truncated", "engine-overlap", "cancel-upload"):
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
            if scenario in ("lost-put", "lost-metadata-put") and operation == "put":
                return self.error(start_response)
            if (scenario == "corrupt" and operation == "get" and success and body
                    and key.endswith("/" + "artifacts/a/containers/main" + "/data/data")):
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
        assert not any(count for (scenario, _), count in self.counts.items() if scenario == "slot-waiter"), self.counts
        if self.counts["destination-exists", "put"]:
            assert self.counts["destination-exists", "head"] == 4, self.counts
            assert self.counts["destination-exists", "get"] == 0, self.counts
        if self.counts["engine-partial", "put"]:
            prefix = "roundtrip/engine-partial/" + "artifacts/a/containers/main" + "/"
            assert client.head_object(Bucket=bucket, Key=prefix + "data/data")["ContentLength"] > 0
            keys = client.list_objects_v2(Bucket=bucket, Prefix=prefix)
            # A batch may finish other payloads after one file fails, but never publishes its index.
            actual = {item["Key"] for item in keys.get("Contents", [])}
            assert prefix + "data/data" in actual, keys
            assert actual <= {prefix + "data/data", prefix + "data/manifest.yaml"}, keys
        if self.counts["retry", "put"]:
            assert self.counts["retry", "put"] == 3, self.counts
        for operation in ("head", "get", "put"):
            for retries in (0, 2):
                count = self.counts[f"metadata-{operation}-{retries}", operation]
                if count:
                    # The pinned S3 pacer makes two SDK calls for failed reads.
                    # PUT is non-rewindable in the SDK and uses copy-level retries.
                    expected = (retries + 1) * (1 if operation == "put" else 2)
                    assert count == expected, self.counts
        if self.counts["denied", "put"]:
            assert self.counts["denied", "put"] == 1, self.counts
        if self.counts["part-failure", "create"]:
            assert self.counts["part-failure", "create"] <= 3, self.counts
            assert self.counts["part-failure", "abort"] == self.counts["part-failure", "create"], self.counts
            assert self.counts["part-failure", "complete"] == 0, self.counts
            uploads = client.list_multipart_uploads(Bucket=bucket, Prefix="roundtrip/part-failure/")
            assert not uploads.get("Uploads"), uploads
        if self.counts["abort-failure", "create"]:
            # rclone owns retry/abort scheduling; no PageBroker cleanup loop.
            assert self.counts["abort-failure", "abort"] >= 1, self.counts
            assert client.list_multipart_uploads(Bucket=bucket, Prefix="roundtrip/abort-failure/").get("Uploads")
        for scenario in ("multipart-deadline", "cancel-upload"):
            if self.counts[scenario, "create"]:
                assert self.counts[scenario, "abort"] >= 1, self.counts
                assert not client.list_multipart_uploads(Bucket=bucket, Prefix=f"roundtrip/{scenario}/").get("Uploads")
        if self.counts["lost-create", "create"]:
            # Two pacer calls, three SDK attempts and three copy attempts.
            assert self.counts["lost-create", "create"] <= 18, self.counts
            assert client.list_multipart_uploads(Bucket=bucket, Prefix="roundtrip/lost-create/").get("Uploads")
        for scenario in ("lost-complete", "lost-put", "lost-metadata-put"):
            if self.counts[scenario, "complete"] or self.counts[scenario, "put"]:
                metadata = client.head_object(Bucket=bucket, Key=f"roundtrip/{scenario}/data")
                assert metadata["ContentLength"] > 0
        if self.counts["bounded", "create"]:
            assert self.peak_parts > 0, self.peak_parts
            assert 0 < self.peak_uploads <= 2, self.peak_uploads
            assert not self.active_uploads
        if self.counts["no-retry", "put"]:
            assert self.counts["no-retry", "put"] == 1, self.counts
        if self.counts["verify-failure", "create"]:
            assert self.counts["verify-failure", "list_parts"] == 0, self.counts
        if self.counts["tree", "put"]:
            metadata = client.head_object(Bucket=bucket, Key="roundtrip/tree/" + "artifacts/a/containers/main" + "/data/empty")
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


class CheckedService:
    """Propagate unexpected HTTP worker failures to the test runner."""

    def __init__(self, app):
        self.app = app
        self.lock = threading.Lock()
        self.errors = []

    def __call__(self, environment, start_response):
        started = time.monotonic()
        try:
            iterable = self.app(environment, start_response)
            try:
                yield from iterable
            finally:
                if hasattr(iterable, "close"):
                    iterable.close()
        except Exception as error:
            query = parse_qs(environment.get("QUERY_STRING", ""))
            # Only test object identifiers belong in diagnostics, never headers
            # or the complete query string (which could contain credentials).
            request = dict(method=environment["REQUEST_METHOD"], path=environment["PATH_INFO"],
                           upload_id=query.get("uploadId"), part=query.get("partNumber"),
                           seconds=round(time.monotonic() - started, 3), error=type(error).__name__)
            with self.lock:
                self.errors.append(request)
            raise

    def verify(self):
        with self.lock:
            if self.errors:
                raise AssertionError(f"Unexpected fixture exceptions: {self.errors}")


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
        checked = CheckedService(faults)
        server = make_server("127.0.0.1", 0, checked,
                             threaded=True, ssl_context=tls)
        # server_close must join request handlers before inspecting their errors.
        # This also covers late failures after cancellation or a resource probe.
        server.daemon_threads = False
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
        # Preserve an existing test failure; otherwise worker exceptions must
        # fail the run even when every native process returned successfully.
        checked.verify()


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


def verify_resource_upload(client, bucket, key, size):
    # Resource probes write 'm' bytes. Read them independently after the native
    # process exits, so verification cannot affect its upload memory sample.
    response = client.get_object(Bucket=bucket, Key=key)
    with contextlib.closing(response["Body"]) as body:
        if response["ContentLength"] != size:
            raise AssertionError(f"Resource upload size mismatch: {key}")
        received = 0
        while chunk := body.read(64 * 1024):
            received += len(chunk)
            if received > size or chunk != b"m" * len(chunk):
                raise AssertionError(f"Resource upload contents mismatch: {key}")
        if received != size:
            raise AssertionError(f"Resource upload truncated: {key}")


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
        credentials = Path(directory) / "credentials"
        credentials.write_text("[default]\naws_access_key_id=local-test-access-key\naws_secret_access_key=local-test-secret-key\n")
        environment["AWS_SHARED_CREDENTIALS_FILE"] = str(credentials)
        environment["PAGEBROKER_S3_TEST_FIXTURE"] = str(fixture)
        container_name = None
        if args.image:
            container_name = "pagebroker-s3-test-" + uuid.uuid4().hex
            command = ["docker", "run", "--rm", "--network=host", "--user", f"{os.getuid()}:{os.getgid()}"]
            command += ["--name", container_name]
            command += ["--mount", f"type=bind,source={fixture},target={fixture},readonly"]
            command += ["--mount", f"type=bind,source={credentials},target={credentials},readonly"]
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
            # ru_maxrss is a process high-water mark: never share a process
            # between probes, or upload allocations can hide metadata growth.
            for test, size in (("UploadMemoryProbe", 512), ("ConcurrentUploadMemoryProbe", 256), ("MetadataMemoryProbe", 512)):
                probe = dict(environment)
                probe["GTEST_FILTER"] = "S3RoundTripTest." + test
                probe["PAGEBROKER_S3_TEST_MEMORY_MIB"] = str(size)
                resource_command = list(command)
                if args.image:
                    resource_command[-1:-1] = ["--env", "GTEST_FILTER", "--env", "PAGEBROKER_S3_TEST_MEMORY_MIB"]
                run_native(resource_command, probe, container_name, 120)
                if test == "UploadMemoryProbe":
                    verify_resource_upload(client, bucket, f"roundtrip/memory/{size}", size * 1024 * 1024)
                elif test == "ConcurrentUploadMemoryProbe":
                    for name in ("first", "second"):
                        verify_resource_upload(client, bucket, f"roundtrip/memory/{name}", size * 1024 * 1024)
        if args.tls:
            # A separate process is necessary: the pinned native library
            # caches its AWS configuration for the process lifetime.
            untrusted = dict(environment)
            untrusted["AWS_CA_BUNDLE"] = "/etc/ssl/certs/ca-certificates.crt"
            untrusted["PAGEBROKER_S3_TEST_TLS_REJECT"] = "1"
            rejection = list(command)
            if args.image:
                rejection[-1:-1] = ["--env", "GTEST_FILTER", "--env", "PAGEBROKER_S3_TEST_TLS_REJECT"]
            # Each native client can spend time draining TLS retries during
            # teardown. Give each rejection check its own process and watchdog.
            for test in ("ModelStreamerS3Test.RejectsUntrustedTLS", "S3RoundTripTest.RejectsUntrustedTLS"):
                untrusted["GTEST_FILTER"] = test
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
