#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Owned S3 cleanup with a supervised, disposable SDK worker and bounded replies."""

import argparse
from contextlib import closing
from datetime import datetime
import hashlib
import os
from pathlib import Path
import signal
import subprocess
import sys
import uuid

from run import validate_report
from stress_config import Clock, RunSpec
from stress_control import Channel, Peer, own_record, start_ticks
from stress_storage import encoded


class CleanupError(RuntimeError):
    pass


def static_credentials(environment):
    names = ("AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY")
    if any(environment.get(name) for name in ("AWS_SESSION_TOKEN", "AWS_SECURITY_TOKEN", "AWS_CREDENTIAL_EXPIRATION")):
        raise CleanupError("static credentials must not include a session token or expiration")
    if any(not environment.get(name) for name in names):
        raise CleanupError("static credentials require an access key and secret key")
    return {name: environment[name] for name in names}


def frozen_credentials(spec, environment, now):
    spec.s3.validate_expiry(spec, now)
    if spec.s3.credential_provider == "static-environment":
        return static_credentials(environment)
    names = ("AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_CREDENTIAL_EXPIRATION")
    if any(not environment.get(name) for name in names):
        raise CleanupError("temporary environment credentials and expiry are required")
    try:
        expiry = datetime.fromisoformat(environment[names[-1]].replace("Z", "+00:00"))
        if expiry.tzinfo is None or abs(expiry.timestamp() - spec.s3.credential_expiry) > 0.000001:
            raise ValueError()
    except (ValueError, OverflowError):
        raise CleanupError("credential expiry does not match the admitted metadata") from None
    return {name: environment[name] for name in names}


def sdk_client(spec, credentials, ca_bundle=None):
    import boto3
    from botocore.config import Config
    return boto3.client("s3", region_name=spec.s3.region, endpoint_url=spec.s3.endpoint,
                        verify=ca_bundle or os.environ.get("AWS_CA_BUNDLE") or True,
                        aws_access_key_id=credentials["AWS_ACCESS_KEY_ID"],
                        aws_secret_access_key=credentials["AWS_SECRET_ACCESS_KEY"],
                        aws_session_token=credentials.get("AWS_SESSION_TOKEN"),
                        config=Config(connect_timeout=2, read_timeout=3,
                                      retries={"total_max_attempts": 2, "mode": "standard"},
                                      s3={"addressing_style": spec.s3.addressing}))


class OwnedStore:
    def __init__(self, client, spec, deadline, clock=None):
        self.client, self.spec, self.options = client, spec, spec.s3
        self.deadline, self.clock = deadline, clock or Clock()
        self.prefix = self.options.prefix + "/"
        self.marker = self.prefix + ".snapshot-stress-owner.json"
        self.owner = encoded({"schemaVersion": 1, "runId": spec.run_id, "storeId": spec.store_id})
        self.calls = 0
        self.last_operation = "INITIALIZE"

    def call(self, operation, **arguments):
        if self.clock.monotonic() >= self.deadline:
            raise TimeoutError("S3 operation deadline reached")
        self.calls += 1
        self.last_operation = operation
        response = getattr(self.client, operation)(Bucket=self.options.bucket, **arguments)
        if self.clock.monotonic() >= self.deadline:
            raise TimeoutError("S3 operation exceeded its deadline")
        return response

    def key(self, value):
        if (not isinstance(value, str) or not value.startswith(self.prefix)
                or len(value.encode()) > 1024):
            raise CleanupError("S3 listing escaped the owned prefix")
        return value

    def pages(self, operation, arguments, fields, tokens):
        previous = None
        for _ in range(self.options.max_pages):
            response = self.call(operation, **arguments)
            for field in fields:
                values = response.get(field, [])
                if not isinstance(values, list) or len(values) > self.options.page_size:
                    raise CleanupError("S3 listing page exceeds its limit")
                for item in values:
                    self.key(item["Key"])
                yield field, values
            if not response.get("IsTruncated", False):
                return
            cursor = tuple(response.get(source) for source, _ in tokens)
            if not cursor[0] or cursor == previous:
                raise CleanupError("S3 pagination did not advance")
            previous = cursor
            arguments = arguments | {target: value for (_, target), value in zip(tokens, cursor) if value is not None}
        raise CleanupError("S3 listing page limit exceeded")

    def inventory(self, include_marker=False):
        versioning = self.call("get_bucket_versioning").get("Status", "Disabled")
        if versioning not in ("Enabled", "Suspended", "Disabled"):
            raise CleanupError("unknown S3 versioning state")
        objects, uploads, count, size = [], [], 0, 0
        operation = "list_object_versions" if versioning != "Disabled" else "list_objects_v2"
        fields = ("Versions", "DeleteMarkers") if versioning != "Disabled" else ("Contents",)
        tokens = (("NextKeyMarker", "KeyMarker"), ("NextVersionIdMarker", "VersionIdMarker")) if versioning != "Disabled" else (
            ("NextContinuationToken", "ContinuationToken"),)
        args = {"Prefix": self.prefix, "MaxKeys": self.options.page_size}
        for field, items in self.pages(operation, args, fields, tokens):
            for item in items:
                count += 1
                size += item.get("Size", 0)
                if count > self.options.max_items or size > self.options.retained_bytes:
                    raise CleanupError("retained S3 object/version budget exceeded")
                if include_marker or item["Key"] != self.marker:
                    entry = {"Key": item["Key"]}
                    if field != "Contents":
                        entry["VersionId"] = item["VersionId"]
                    objects.append((entry, item.get("Size", 0), field == "DeleteMarkers"))
        args = {"Prefix": self.prefix, "MaxUploads": self.options.page_size}
        for _, items in self.pages("list_multipart_uploads", args, ("Uploads",), (
                ("NextKeyMarker", "KeyMarker"), ("NextUploadIdMarker", "UploadIdMarker"))):
            for item in items:
                count += 1
                if count > self.options.max_items:
                    raise CleanupError("retained S3 upload budget exceeded")
                parts, part_bytes, marker = 0, 0, 0
                for _ in range(self.options.max_pages):
                    page = self.call("list_parts", Key=item["Key"], UploadId=item["UploadId"],
                                     PartNumberMarker=marker, MaxParts=self.options.page_size)
                    values = page.get("Parts", [])
                    parts += len(values)
                    count += len(values)
                    part_bytes += sum(part["Size"] for part in values)
                    if (len(values) > self.options.page_size or count > self.options.max_items
                            or part_bytes + size > self.options.retained_bytes):
                        raise CleanupError("multipart inspection budget exceeded")
                    if not page.get("IsTruncated", False):
                        break
                    next_marker = page.get("NextPartNumberMarker", 0)
                    if next_marker <= marker:
                        raise CleanupError("multipart pagination did not advance")
                    marker = next_marker
                else:
                    raise CleanupError("multipart page limit exceeded")
                uploads.append({"key": item["Key"], "uploadId": item["UploadId"], "parts": parts, "bytes": part_bytes})
                size += part_bytes
        return objects, uploads, versioning

    def verify_owner(self):
        body = self.call("get_object", Key=self.marker)["Body"]
        try:
            if body.read(4097) != self.owner:
                raise CleanupError("S3 ownership marker does not match this run")
        finally:
            body.close()

    def sweep(self):
        self.verify_owner()
        objects, uploads, versioning = self.inventory()
        if uploads:
            # Preserve the failed batch; never hide incomplete uploads with recovery deletion.
            return {"status": "ERROR", "error": "UnexpectedMultipartUploads", "incompleteUploads": len(uploads),
                    "multipartParts": sum(item["parts"] for item in uploads),
                    "multipartBytes": sum(item["bytes"] for item in uploads), "recoveryCleanup": "NOT_STARTED"}
        for offset in range(0, len(objects), self.options.page_size):
            self.verify_owner()
            entries = [item[0] for item in objects[offset:offset + self.options.page_size]]
            reply = self.call("delete_objects", Delete={"Objects": entries, "Quiet": False})
            if reply.get("Errors"):
                raise CleanupError("S3 reported per-object deletion errors")
            requested = {(item["Key"], item.get("VersionId")) for item in entries}
            deleted = {(self.key(item["Key"]), item.get("VersionId")) for item in reply.get("Deleted", [])}
            if deleted != requested or len(reply.get("Deleted", [])) != len(entries):
                raise CleanupError("S3 did not acknowledge every object deletion")
        self.verify_owner()
        remaining, uploads, _ = self.inventory()
        if remaining or uploads:
            raise CleanupError("S3 cleanup verification found retained batch data")
        return {"status": "SUCCESS", "objects": len(objects), "versions": sum("VersionId" in item[0] for item in objects),
                "deleteMarkers": sum(item[2] for item in objects), "bytes": sum(item[1] for item in objects),
                "incompleteUploads": 0, "versioning": versioning}

    def ready(self):
        objects, uploads, _ = self.inventory(include_marker=True)
        # Require ordinary listing access as well as any version-specific inspection.
        existing = self.call("list_objects_v2", Prefix=self.prefix, MaxKeys=1)
        if objects or uploads or existing.get("Contents"):
            raise CleanupError("S3 run prefix is populated or already owned")
        self.call("put_object", Key=self.marker, Body=self.owner, IfNoneMatch="*")
        self.verify_owner()
        key = self.prefix + ".snapshot-stress-probe/" + str(uuid.uuid4())
        data = b"snapshot-stress-readiness"
        self.call("put_object", Key=key, Body=data)
        body = self.call("get_object", Key=key)["Body"]
        try:
            if body.read(len(data) + 1) != data:
                raise CleanupError("S3 readiness readback mismatch")
        finally:
            body.close()
        upload = self.call("create_multipart_upload", Key=key)["UploadId"]
        part = self.call("upload_part", Key=key, UploadId=upload, PartNumber=1, Body=data)
        self.call("list_parts", Key=key, UploadId=upload, MaxParts=1)
        self.call("complete_multipart_upload", Key=key, UploadId=upload,
                  MultipartUpload={"Parts": [{"PartNumber": 1, "ETag": part["ETag"]}]})
        upload = self.call("create_multipart_upload", Key=key)["UploadId"]
        self.call("abort_multipart_upload", Key=key, UploadId=upload)
        if self.call("get_bucket_versioning").get("Status") in ("Enabled", "Suspended"):
            self.call("delete_object", Key=key)
        result = self.sweep()
        if result["status"] != "SUCCESS":
            raise CleanupError("S3 readiness cleanup failed")
        return result | {"status": "READY", "credentialExpiry": self.options.credential_expiry,
                         "credentialProvider": self.options.credential_provider}


def safe_error(error):
    # SDK messages can contain request URLs, headers or keys; expose only a fixed category.
    if isinstance(error, CleanupError):
        return str(error)
    return "S3DeadlineExceeded" if isinstance(error, TimeoutError) else "S3RequestFailed"


def worker(spec, operation, token, deadline):
    if sys.stdin.buffer.read(1) != b"1":
        return 125
    channel = Channel(spec.control, spec.run_id)
    store = None
    try:
        credentials = frozen_credentials(spec, os.environ, spec.created_at)
        with closing(sdk_client(spec, credentials)) as client:
            store = OwnedStore(client, spec, deadline)
            reply = store.ready() if operation == "READY" else store.sweep()
    except Exception as error:
        reply = {"status": "ERROR", "error": safe_error(error), "errorType": type(error).__name__,
                 "operation": store.last_operation if store else "INITIALIZE", "recoveryCleanup": "NOT_STARTED"}
    channel.write("cleanup-worker-result.json", token=token, **reply)
    return 0


class Helper:
    def __init__(self, spec, spec_path, clock=None):
        self.spec, self.spec_path, self.clock = spec, spec_path, clock or Clock()
        self.channel = Channel(spec.control, spec.run_id)
        self.peers, self.child = [], None
        self.interrupted = False
        self.next_beat = 0
        self.last_request, self.last_reply = None, None
        self.batch = 0
        self.failed = False

    def pulse(self):
        if self.interrupted:
            raise InterruptedError("cleanup helper interrupted")
        for peer in self.peers:
            peer.check()
        if self.peers:
            for role in ("runner", "supervisor"):
                self.channel.check_heartbeat(role, self.clock, self.spec.timing.stale)
        if self.clock.monotonic() >= self.next_beat:
            self.channel.beat("helper", self.clock)
            self.next_beat = self.clock.monotonic() + self.spec.timing.heartbeat

    def wait(self, name, deadline, predicate=lambda value: True):
        while True:
            self.pulse()
            if self.clock.monotonic() >= deadline:
                raise TimeoutError("cleanup control deadline exceeded")
            value = self.channel.read(name)
            if value is not None and predicate(value):
                return value
            self.clock.sleep(self.spec.timing.poll)

    def operation(self, name, deadline):
        token = str(uuid.uuid4())
        command = [sys.executable, str(Path(__file__).resolve()), "--spec", self.spec_path,
                   "--worker", name, "--token", token, "--deadline", str(deadline)]
        self.child = subprocess.Popen(command, stdin=subprocess.PIPE, stdout=subprocess.DEVNULL,
                                      stderr=subprocess.DEVNULL, env=self.environment)
        peer = {"pid": self.child.pid, "startTicks": start_ticks(self.child.pid)}
        self.channel.write("cleanup-worker.json", token=token, active=True, peer=peer)
        self.wait("cleanup-worker-ack.json", deadline, lambda value: value.get("token") == token)
        self.child.stdin.write(b"1")
        self.child.stdin.close()
        while self.child.poll() is None:
            self.pulse()
            if self.clock.monotonic() >= deadline:
                raise TimeoutError("cleanup SDK worker exceeded its deadline")
            self.clock.sleep(self.spec.timing.poll)
        if self.child.returncode != 0:
            raise CleanupError("cleanup SDK worker exited unexpectedly")
        self.child = None
        self.channel.write("cleanup-worker.json", token=str(uuid.uuid4()), active=False)
        return self.wait("cleanup-worker-result.json", deadline, lambda value: value.get("token") == token)

    def validate_request(self, request):
        if (request.get("batch") != self.batch + 1 or request.get("quiescent") is not True
                or request.get("broker") != self.peers[2].record()):
            raise CleanupError("cleanup request is not the next quiescent batch")
        client = self.channel.read("client.json")
        ack = self.channel.read("client-ack.json")
        if not client or client.get("active") is not False or not ack or ack.get("token") != client["token"]:
            raise CleanupError("cleanup requires a reaped and acknowledged client")
        name = f"batches/{request['batch']:06d}/client-report.json"
        path = Path(self.spec.results) / name
        if request.get("report") != name or path.is_symlink() or path.stat().st_size > self.spec.evidence.report_bytes:
            raise CleanupError("cleanup request has an invalid report path")
        data = path.read_bytes()
        if hashlib.sha256(data).hexdigest() != request.get("sha256"):
            raise CleanupError("cleanup report digest mismatch")
        report = validate_report(path, "s3", self.spec.store_id)
        if (report["runId"] != request.get("clientRunId") or report["config"].get("socket") != self.spec.socket
                or report["config"].get("stagingRoot") != self.spec.staging):
            raise CleanupError("cleanup report belongs to another client or broker")
        deadline = request["deadline"]
        now = self.clock.monotonic()
        if not now < deadline <= min(self.expiry, now + self.spec.timing.cleanup):
            raise CleanupError("cleanup request deadline is invalid")
        return deadline

    def respond(self, request):
        if self.last_request == request:
            # Re-publish a lost acknowledgment without repeating destructive work.
            self.channel.write("cleanup-reply.json", **self.last_reply)
            return
        if self.failed:
            raise CleanupError("cleanup failed; another batch cannot be admitted")
        deadline = self.validate_request(request)
        started = self.clock.monotonic()
        result = self.operation("CLEANUP", deadline)
        self.batch = request["batch"]
        self.last_request = request
        self.last_reply = {key: value for key, value in result.items() if key not in ("runId", "token")}
        self.last_reply.update(token=request["token"], batch=self.batch, sha256=request["sha256"],
                               clientRunId=request["clientRunId"], started=started,
                               finished=self.clock.monotonic(), helper=own_record())
        self.failed = result["status"] != "SUCCESS"
        self.channel.write("cleanup-reply.json", **self.last_reply)

    def run(self):
        try:
            Path(self.spec.control).mkdir(parents=True, exist_ok=True)
            startup, self.expiry = self.spec.deadlines(self.clock)
            credentials = frozen_credentials(self.spec, os.environ, self.clock.time())
            self.environment = {key: value for key, value in os.environ.items() if not key.startswith("AWS_")}
            self.environment.update(credentials, AWS_EC2_METADATA_DISABLED="true",
                                    AWS_CONFIG_FILE=os.devnull, AWS_SHARED_CREDENTIALS_FILE=os.devnull)
            if os.environ.get("AWS_CA_BUNDLE"):
                self.environment["AWS_CA_BUNDLE"] = os.environ["AWS_CA_BUNDLE"]
            with (Path(self.spec.control) / "helper-owner").open("x"):
                pass
            self.channel.write("helper-registration.json", peer=own_record())
            registered = self.wait("registered.json", startup)
            runner = self.channel.read("runner-registration.json")
            for record in (runner["peer"], registered["peer"], registered["broker"]):
                self.peers.append(Peer.open(record))
            self.wait("helper-ack.json", startup, lambda value: value.get("peer") == own_record())
            reply = self.operation("READY", startup)
            self.channel.write("cleanup-ready.json", helper=own_record(), **{k: v for k, v in reply.items() if k not in ("runId", "token")})
            if reply["status"] != "READY":
                self.failed = True
            while True:
                self.pulse()
                stop = self.channel.read("cleanup-stop.json")
                if stop is not None:
                    self.channel.write("cleanup-stopped.json", status="STOPPED", helper=own_record(),
                                       remoteCleanup="VERIFIED" if not self.failed and stop.get("executionStatus") == "SUCCESS"
                                       else "RETAINED")
                    return 0
                if self.clock.monotonic() >= self.expiry - self.spec.timing.shutdown:
                    raise TimeoutError("cleanup helper run deadline reached")
                request = self.channel.read("cleanup-request.json")
                if request is not None:
                    self.respond(request)
                self.clock.sleep(self.spec.timing.poll)
        except Exception as error:
            self.channel.write("cleanup-failure.json", status="ERROR", error=safe_error(error),
                               remoteCleanup="UNCERTAIN" if self.child else "RETAINED")
            return 1
        finally:
            if self.child is not None:
                if self.child.poll() is None:
                    self.child.kill()
                self.child.wait(timeout=self.spec.timing.kill_wait)
                if self.child.stdin and not self.child.stdin.closed:
                    self.child.stdin.close()
            for peer in self.peers:
                peer.close()


class CleanupChannel:
    """Runner-side channel; this process never loads an AWS SDK or credentials."""

    def __init__(self, spec):
        self.spec, self.channel, self.peer = spec, Channel(spec.control, spec.run_id), None
        self.pending = None

    def check(self):
        if self.peer is not None:
            self.peer.check()
            self.channel.check_heartbeat("helper", Clock(), self.spec.timing.stale)
        failure = self.channel.read("cleanup-failure.json")
        if failure is not None:
            raise CleanupError("cleanup helper failed; remote data retained or uncertain")

    def ready(self, deadline):
        registration = self.channel.read("helper-registration.json")
        if registration is not None and self.peer is None:
            self.peer = Peer.open(registration["peer"])
        self.check()
        reply = self.channel.read("cleanup-ready.json")
        if reply is not None and (self.peer is None or reply.get("helper") != self.peer.record()):
            raise CleanupError("cleanup readiness has a conflicting helper identity")
        return reply

    def request(self, request):
        if self.pending is not None:
            raise CleanupError("a cleanup request is already outstanding")
        self.pending = dict(request)
        self.channel.write("cleanup-request.json", **request)

    def poll(self, run_id, batch):
        if run_id != self.spec.run_id or self.pending is None or batch != self.pending["batch"]:
            raise CleanupError("cleanup poll belongs to a different run or batch")
        self.check()
        reply = self.channel.read("cleanup-reply.json")
        if reply is None or reply.get("token") != self.pending["token"]:
            return None
        for key in ("batch", "sha256", "clientRunId"):
            if reply.get(key) != self.pending[key]:
                raise CleanupError("cleanup acknowledgment does not match its request")
        if reply.get("helper") != self.peer.record():
            raise CleanupError("cleanup acknowledgment changed helper identity")
        self.pending = None
        return reply

    def emergency_stop(self):
        # Terminate a registered worker first; an in-flight request can remain uncertain.
        for name in ("cleanup-worker.json", "helper-registration.json"):
            record = self.channel.read(name)
            if record and record.get("active", True):
                try:
                    peer = Peer.open(record["peer"])
                    peer.send(signal.SIGKILL)
                    peer.close()
                except (ProcessLookupError, FileNotFoundError, RuntimeError):
                    pass

    def close(self):
        if self.peer is not None:
            self.peer.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--spec", required=True)
    parser.add_argument("--worker", choices=("READY", "CLEANUP"))
    parser.add_argument("--token")
    parser.add_argument("--deadline", type=float)
    args = parser.parse_args()
    spec = RunSpec.load(args.spec)
    if spec.mode != "s3":
        parser.error("the cleanup helper requires S3 mode")
    if args.worker:
        return worker(spec, args.worker, args.token, args.deadline)
    helper = Helper(spec, args.spec)
    def interrupt(_signum, _frame):
        helper.interrupted = True
    signal.signal(signal.SIGTERM, interrupt)
    signal.signal(signal.SIGINT, interrupt)
    return helper.run()


if __name__ == "__main__":
    sys.exit(main())
