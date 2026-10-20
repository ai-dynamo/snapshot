#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Run native S3 reads against a disposable local Moto service.

Requires boto3 and moto[server]. Uses fixed test credentials and an in-memory
restore plan; no cloud account or artifact index is needed.
"""

import argparse
import contextlib
import logging
import os
from pathlib import Path
import subprocess
import tempfile
import threading
import uuid

import boto3
import botocore.session
from botocore.config import Config


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
def local_service(args, environment):
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
        server = make_server("127.0.0.1", 0, DomainDispatcherApplication(create_backend_app),
                             threaded=True, ssl_context=tls)
        worker = threading.Thread(target=server.serve_forever)
        worker.start()
        try:
            scheme = "https" if args.tls else "http"
            environment["AWS_ENDPOINT_URL"] = f"{scheme}://127.0.0.1:{server.server_port}"
            yield
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
    # Crosses the native S3 reader's chunk boundary without a large heap buffer.
    with (root / "nested" / "large").open("wb") as file:
        for _ in range(192):
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


def run(args, environment):
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
        run_native(command, environment, container_name, 180)
        if args.tls:
            # A separate process is necessary: the pinned native library
            # caches its AWS configuration for the process lifetime.
            untrusted = dict(environment)
            untrusted["AWS_CA_BUNDLE"] = "/etc/ssl/certs/ca-certificates.crt"
            untrusted["GTEST_FILTER"] = "ModelStreamerS3Test.RejectsUntrustedTLS"
            untrusted["PAGEBROKER_S3_TEST_TLS_REJECT"] = "1"
            rejection = list(command)
            if args.image:
                rejection[-1:-1] = ["--env", "GTEST_FILTER", "--env", "PAGEBROKER_S3_TEST_TLS_REJECT"]
            run_native(rejection, untrusted, container_name, 60)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    target = parser.add_mutually_exclusive_group(required=True)
    target.add_argument("--binary", help="native model-streamer-s3-test executable")
    target.add_argument("--image", help="PageBroker Docker image built with --target s3-test (Linux)")
    parser.add_argument("--tls", action="store_true", help="use verified HTTPS with a temporary local CA")
    args = parser.parse_args()
    environment = local_environment()
    with local_service(args, environment):
        run(args, environment)


if __name__ == "__main__":
    main()
