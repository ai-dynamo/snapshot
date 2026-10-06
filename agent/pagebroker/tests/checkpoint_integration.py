#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Exercise the real Go checkpoint client against two packaged native daemons."""

import argparse
from collections import Counter, defaultdict
import contextlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import threading
import time
import uuid
from urllib.parse import parse_qs

from botocore.config import Config
from s3_integration import FaultService, create_session, local_environment, local_service


class CheckpointService:
    def __init__(self, app):
        self.app = app
        self.changed = threading.Condition()
        self.blocked = set()
        self.waiting = set()
        self.counts = Counter()
        self.labels = {}
        self.credentials = set()
        self.payload_writes = Counter()
        self.published_payload_writes = {}
        self.payload_credentials = defaultdict(set)
        self.index_writes = Counter()
        self.restart = None
        self.unconfirmed = set()
        self.confirmable = set()
        self.client = None
        self.bucket = "pagebroker-checkpoint-test"
        self.prefix = ""

    def control(self, action, checkpoint_id, start_response):
        if action.startswith("label-"):
            self.labels[checkpoint_id] = action[len("label-"):]
        uid = self.labels.get(checkpoint_id, checkpoint_id)
        with self.changed:
            if action == "block":
                self.blocked.add(uid)
            elif action == "wait":
                if not self.changed.wait_for(lambda: uid in self.waiting, timeout=15):
                    return FaultService.error(start_response)
            elif action == "confirm":
                self.confirmable.add(uid)
            elif action == "release":
                self.blocked.discard(uid)
                self.changed.notify_all()
        if action == "assert-unpublished":
            assert self.index_writes[uid] == 0, self.index_writes
        if action == "assert-no-reupload":
            assert self.payload_writes[uid] == self.published_payload_writes[uid]
        if action == "assert-no-upload":
            assert self.payload_writes[uid] == self.index_writes[uid] == 0
        if action == "conflicting-index":
            self.confirmable.add(uid)
            key = f"{self.prefix}/{checkpoint_id}/index.json"
            value = json.loads(self.client.get_object(Bucket=self.bucket, Key=key)["Body"].read())
            value["rootMode"] ^= 1
            self.client.put_object(Bucket=self.bucket, Key=key, Body=json.dumps(value).encode())
        if action == "restart":
            self.restart()
        if action in ("missing-empty", "corrupt-data", "corrupt-index"):
            prefix = f"{self.prefix}/{checkpoint_id}/"
            listing = self.client.list_objects_v2(Bucket=self.bucket, Prefix=prefix)
            suffix = {"missing-empty": "/data/empty", "corrupt-data": "/data/nested/data", "corrupt-index": "/index.json"}[action]
            key = next(item["Key"] for item in listing["Contents"] if item["Key"].endswith(suffix))
            if action == "missing-empty":
                self.client.delete_object(Bucket=self.bucket, Key=key)
            elif action == "corrupt-data":
                body = self.client.get_object(Bucket=self.bucket, Key=key)["Body"].read()
                self.client.put_object(Bucket=self.bucket, Key=key, Body=bytes([body[0] ^ 1]) + body[1:])
            else:
                self.client.put_object(Bucket=self.bucket, Key=key, Body=b'{"format":"snapshot.s3-checkpoint/v1"}')
        start_response("200 OK", [("Content-Length", "0")])
        return [b""]

    def __call__(self, environment, start_response):
        path = environment.get("PATH_INFO", "")
        if path.startswith("/__checkpoint_test__/"):
            _, _, action, uid = path.split("/", 3)
            return self.control(action, uid, start_response)
        if f"/{self.prefix}/" not in path:
            return self.app(environment, start_response)
        checkpoint_id = path.split(f"/{self.prefix}/", 1)[1].split("/", 1)[0]
        uid = self.labels.get(checkpoint_id, checkpoint_id)
        method = environment["REQUEST_METHOD"]
        query = parse_qs(environment.get("QUERY_STRING", ""), keep_blank_values=True)
        native = "aws-sdk-cpp" in environment.get("HTTP_USER_AGENT", "")
        with self.changed:
            self.counts[uid, method] += 1
            auth = environment.get("HTTP_AUTHORIZATION", "")
            if "Credential=" in auth:
                key = auth.split("Credential=", 1)[1].split("/", 1)[0]
                self.credentials.add(key)
                if method == "GET" and "/data/" in path:
                    self.payload_credentials[uid].add(key)
            if native:
                if method == "PUT" and "/data/" in path:
                    self.payload_writes[uid] += 1
                if method == "PUT" and path.endswith("/index.json"):
                    self.index_writes[uid] += 1
                    self.published_payload_writes.setdefault(uid, self.payload_writes[uid])
                    assert environment.get("HTTP_IF_NONE_MATCH") == "*", (uid, method, query)
                elif method == "PUT" or (method == "POST" and "uploadId" in query):
                    assert not environment.get("HTTP_IF_NONE_MATCH"), (uid, method, query)
            blocked_payload = method in ("GET", "PUT") and "/data/" in path and uid != "publication-abort"
            blocked_preflight = uid.startswith("blocked-preflight-") and method in ("HEAD", "GET") and path.endswith("/index.json")
            if uid in self.blocked and (blocked_payload or blocked_preflight):
                self.waiting.add(uid)
                self.changed.notify_all()
                if not self.changed.wait_for(lambda: uid not in self.blocked, timeout=20):
                    return FaultService.error(start_response)
        if uid == "denied-access":
            return FaultService.error(start_response, "AccessDenied", "403 Forbidden")
        if uid in self.unconfirmed and uid not in self.confirmable and path.endswith("/index.json") and method in ("HEAD", "GET"):
            return FaultService.error(start_response)
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
            if uid not in ("blocked-upload", "deadline-upload") or str(error) != "Invalid chunk header":
                raise
            return FaultService.error(start_response, "RequestTimeout", "408 Request Timeout")
        if uid == "publication-abort" and native and method == "PUT" and path.endswith("/index.json"):
            with self.changed:
                self.waiting.add(uid)
                self.changed.notify_all()
                if not self.changed.wait_for(lambda: uid not in self.blocked, timeout=20):
                    return FaultService.error(start_response)
        if uid in ("unconfirmed-index", "conflicting-index") and native and method == "PUT" and path.endswith("/index.json"):
            self.unconfirmed.add(uid)
            return FaultService.error(start_response)
        if native and method == "PUT" and (
                (uid == "lost-index" and path.endswith("/index.json")) or
                (uid == "lost-payload" and uid not in self.confirmable and "/data/" in path)):
            return FaultService.error(start_response)
        start_response(response["status"], response["headers"])
        return [body]


def run(args):
    environment = local_environment()
    with local_service(args, environment, CheckpointService) as service, \
            tempfile.TemporaryDirectory(prefix="pagebroker-checkpoint-") as temporary:
        root = Path(temporary)
        session = create_session(environment)
        service.client = session.client("s3", endpoint_url=environment["AWS_ENDPOINT_URL"],
                                        verify=environment.get("AWS_CA_BUNDLE", True),
                                        config=Config(s3={"addressing_style": "path"}))
        with contextlib.closing(service.client):
            service.client.create_bucket(Bucket=service.bucket)
            service.prefix = "snapshots"
            credentials = root / "credentials"
            credentials.write_text("[default]\naws_access_key_id=local-test-access-key\naws_secret_access_key=local-test-secret-key\n")
            config = dict(storeId="test-store", endpoint=environment["AWS_ENDPOINT_URL"], region="us-east-1", bucket=service.bucket,
                          prefix=service.prefix, addressing="path", allowHttp=not args.tls,
                          caFile=environment.get("AWS_CA_BUNDLE", ""),
                          limits=dict(transactionSeconds=120, activeTransactions=8, uploadPartBytes=5*1024*1024,
                                      uploadBufferBytes=20*1024*1024, requestSeconds=2,
                                      uploadConnectSeconds=1, uploadRequestRetries=2))
            config_path = root / "storage.json"
            config_path.write_text(json.dumps(config))
            streamer_path = root / "streamer.json"
            streamer_path.write_text(json.dumps(dict(
                filesystem=dict(strategies=["sync_buffered"]), readChunkBytes=8*1024*1024,
                s3Reader=dict(concurrency=2, maxConnections=2, maxInflightMiB=16,
                              maxRetries=1, retryWindowSeconds=0, lowSpeedTimeoutMs=2000),
                logging=dict(level="ERROR", toStderr=True))))
            missing_command = [
                "docker", "run", "--rm", "--network=host", "--user", f"{os.getuid()}:{os.getgid()}",
                "--mount", f"type=bind,source={root},target={root}",
                "--env", "AWS_SHARED_CREDENTIALS_FILE=/dev/null", "--env", "AWS_CONFIG_FILE=/dev/null",
                "--env", "AWS_EC2_METADATA_DISABLED=true", "--entrypoint", "/usr/local/bin/pagebroker", args.image,
                str(root / "missing.sock"), str(root / "missing-staging"), "/unused",
                "--max-concurrent-requests", "16", "--storage-config", str(config_path),
            ]
            missing = subprocess.run(missing_command, capture_output=True, text=True, timeout=15)
            assert missing.returncode == 1 and "PageBroker startup failed" in missing.stderr, missing.stderr
            invalid_path = root / "invalid-streamer.json"
            invalid_path.write_text('{"s3Reader":{"concurrency":0}}')
            invalid = subprocess.run(missing_command + ["--model-streamer-config", str(invalid_path)],
                                     capture_output=True, text=True, timeout=15)
            assert invalid.returncode == 2 and "s3Reader.concurrency" in invalid.stderr, invalid.stderr
            assert not (root / "missing.sock").exists()
            containers = []
            try:
                for number in range(3):
                    name = "pagebroker-checkpoint-" + uuid.uuid4().hex
                    command = ["docker", "run", "--rm", "--detach", "--network=host", "--name", name,
                               "--user", f"{os.getuid()}:{os.getgid()}", "--mount", f"type=bind,source={root},target={root}"]
                    command += ["--env", f"AWS_SHARED_CREDENTIALS_FILE={credentials}",
                                "--env", "AWS_PROFILE=default", "--env", "AWS_EC2_METADATA_DISABLED=true"]
                    if number == 2:
                        command += ["--env", "AWS_ACCESS_KEY_ID=local-test-access-key",
                                    "--env", "AWS_SECRET_ACCESS_KEY=local-test-secret-key"]
                    if args.tls:
                        ca = environment["AWS_CA_BUNDLE"]
                        command += ["--mount", f"type=bind,source={ca},target={ca},readonly"]
                    selected_config = config_path
                    if number == 2:
                        short_config = json.loads(json.dumps(config))
                        short_config["limits"]["transactionSeconds"] = 1
                        short_config["limits"]["stagingBytes"] = 1024 * 1024
                        selected_config = root / "short.json"
                        selected_config.write_text(json.dumps(short_config))
                    command += ["--entrypoint", "/usr/local/bin/pagebroker", args.image,
                                str(root / f"{number}.sock"), str(root / f"staging-{number}"), "/unused",
                                "--max-concurrent-requests", "16"]
                    # Exercise both argument orders plus the existing no-tuning
                    # invocation in independent daemon processes.
                    storage_args = ["--storage-config", str(selected_config)]
                    streamer_args = ["--model-streamer-config", str(streamer_path)]
                    command += (streamer_args + storage_args if number == 0 else
                                storage_args + streamer_args if number == 1 else storage_args)
                    subprocess.run(command, check=True, stdout=subprocess.DEVNULL)
                    containers.append(name)
                deadline = time.monotonic() + 15
                while not all((root / f"{number}.sock").exists() for number in range(3)):
                    if time.monotonic() > deadline:
                        raise RuntimeError("native daemons did not create sockets")
                    time.sleep(0.05)
                service.restart = lambda: subprocess.run(["docker", "restart", containers[1]], check=True, stdout=subprocess.DEVNULL)
                environment.update(PAGEBROKER_CHECKPOINT_SHORT_SOCKET=str(root / "2.sock"), PAGEBROKER_CHECKPOINT_SOCKET=str(root / "0.sock"),
                                   PAGEBROKER_CHECKPOINT_SECOND_SOCKET=str(root / "1.sock"),
                                   PAGEBROKER_CHECKPOINT_CREDENTIALS=str(credentials),
                                   PAGEBROKER_CHECKPOINT_CA=environment.get("AWS_CA_BUNDLE", ""),
                                   PAGEBROKER_CHECKPOINT_CONTROL=environment["AWS_ENDPOINT_URL"] + "/__checkpoint_test__")
                subprocess.run([str(Path(args.client).resolve()), "-test.run=^TestS3CheckpointDaemon$", "-test.v", "-test.timeout=180s"],
                               env=environment, check=True, timeout=190)
                assert service.credentials == {"local-test-access-key", "rotated-test-key"}, service.credentials
                assert service.payload_credentials["credentials-fixed"] == {"local-test-access-key"}, service.payload_credentials
                assert service.payload_credentials["credentials-restarted"] == {"rotated-test-key"}, service.payload_credentials
                assert not service.client.list_multipart_uploads(Bucket=service.bucket).get("Uploads")
                assert not service.client.list_objects_v2(Bucket=service.bucket, Prefix=service.prefix + "/probes/").get("Contents")
            finally:
                for name in containers:
                    subprocess.run(["docker", "logs", name], check=False)
                    subprocess.run(["docker", "stop", "--time", "10", name], check=False, stdout=subprocess.DEVNULL)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", required=True, help="PageBroker runtime image with checkpoint support")
    parser.add_argument("--client", required=True, help="go test -c output for agent/internal/pagebroker")
    parser.add_argument("--tls", action="store_true")
    run(parser.parse_args())
