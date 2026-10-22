#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Start, inspect, collect and stop autonomous, owned PageBroker endurance Jobs."""

import argparse
import base64
from collections import Counter
from copy import deepcopy
from datetime import datetime
import json
import os
from pathlib import Path
import re
import shutil
import signal
import sys
import tempfile
import time
import uuid

from evidence import condition
from render import DNS_LABEL
from s3_cleanup import frozen_credentials, sdk_client, static_credentials, OwnedStore
from stress_config import EvidenceOptions, RunSpec, S3Options, Timing
from stress_evidence import validate_directory
from stress_export import extract, inventory
from stress_images import canonical_reference, load_identities, runtime_identity, validate_identities
from stress_kube import Kubernetes, contains, durable_json, execute, owned, pod_request, quantity
from stress_render import CAMPAIGN, CONFIG, JOB, OWNER, RECORD, RESULTS, SECRET, render_stress


READ_PROGRESS = """
import json,pathlib,sys
root=pathlib.Path('/results'); control=pathlib.Path('/control')
result={}
for key,path in [('progress',root/'progress.json'),('ready',root/'readiness.json'),('failure',control/'failure.json')]:
    if path.exists():
        data=path.read_bytes() if path.stat().st_size<=65536 else None
        if data is None: raise ValueError('progress exceeds its limit')
        value=json.loads(data)
        if value.get('runId')!=sys.argv[1]: raise ValueError('foreign progress record')
        result[key]=value
print(json.dumps(result))
"""
REQUEST_STOP = """
import sys
sys.path.insert(0,'/opt/stress')
from stress_control import Channel
Channel('/control',sys.argv[1]).write('stop.json',reason='host-request')
"""


def roles(mode):
    return ["pagebroker", "client", "supervisor"] + (["s3-helper"] if mode == "s3" else [])


def key(resource):
    return resource["kind"] + "/" + resource["metadata"]["name"]


def metadata(record, namespace, name):
    return {"name": name, "namespace": namespace,
            "annotations": {OWNER: record["owner"], CAMPAIGN: record["campaign"]}}


def credentials(args):
    if args.credentials_source == "static-environment":
        return static_credentials(os.environ), None
    if args.credentials_source == "environment":
        value = {name: os.environ.get(name, "") for name in (
            "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_CREDENTIAL_EXPIRATION")}
    else:
        command = ["aws", "configure", "export-credentials", "--format", "process"]
        if args.aws_profile:
            command += ["--profile", args.aws_profile]
        data = json.loads(execute(command, timeout=30, limit=16 * 1024, sensitive=True))
        value = {name: data.get(field, "") for name, field in (
            ("AWS_ACCESS_KEY_ID", "AccessKeyId"), ("AWS_SECRET_ACCESS_KEY", "SecretAccessKey"),
            ("AWS_SESSION_TOKEN", "SessionToken"), ("AWS_CREDENTIAL_EXPIRATION", "Expiration"))}
    try:
        expiry = datetime.fromisoformat(value["AWS_CREDENTIAL_EXPIRATION"].replace("Z", "+00:00"))
        if expiry.tzinfo is None or not all(value.values()):
            raise ValueError()
        return value, expiry.timestamp()
    except (ValueError, TypeError, AttributeError):
        raise ValueError("temporary AWS credentials with a known timezone-qualified expiry are required") from None


def make_record(args, now=None, credential_expiry=None):
    now = time.time() if now is None else now
    profile = {}
    if args.profile:
        if args.profile.stat().st_size > 64 * 1024:
            raise ValueError("profile exceeds 64 KiB")
        profile = json.loads(args.profile.read_text())
        if set(profile) - {"timing", "evidence"}:
            raise ValueError("profile may contain only timing and evidence settings")
    timing, evidence = Timing(**profile.get("timing", {})), EvidenceOptions(**profile.get("evidence", {}))
    timing.validate()
    evidence.validate()
    for revision in (args.harness_revision, args.broker_revision):
        if not re.fullmatch(r"[0-9a-f]{40}", revision):
            raise ValueError("full broker and harness source revisions are required")
    images = load_identities(args.image_identities,
                             {"harness": args.harness_revision, "broker": args.broker_revision},
                             {"harness": args.client_image, "broker": args.pagebroker_image})
    campaign = str(uuid.uuid4())
    record = {"schemaVersion": 2, "campaign": campaign, "owner": str(uuid.uuid4()), "context": args.context,
              "node": args.node, "nodeUID": None, "clusterUID": None, "createdAt": now, "launchStatus": "PLANNED",
              "harnessRevision": args.harness_revision, "brokerRevision": args.broker_revision, "backends": {}}
    if args.backend in ("both", "s3") and (not args.bucket or not args.region or not args.prefix):
        raise ValueError("S3 requires explicit bucket, region and a nonempty prefix")
    if args.ca_bundle and args.ca_bundle.stat().st_size > 64 * 1024:
        raise ValueError("CA bundle exceeds 64 KiB")
    ca_pem = args.ca_bundle.read_text() if args.ca_bundle else None
    for mode in (["filesystem", "s3"] if args.backend == "both" else [args.backend]):
        run_id = str(uuid.uuid4())
        namespace = f"pagebroker-stress-{campaign[:12]}-{mode}"
        s3 = None
        if mode == "s3":
            s3 = S3Options(bucket=args.bucket, prefix=args.prefix + "/" + run_id, region=args.region,
                           endpoint=args.endpoint or f"https://s3.{args.region}.amazonaws.com",
                           addressing=args.addressing, credential_expiry=credential_expiry,
                           credential_provider="static-environment" if args.credentials_source == "static-environment"
                           else "temporary-environment")
        spec = RunSpec(run_id=run_id, mode=mode, socket="/shared/pb.sock", staging="/staging", storage="/storage",
                       results="/results", control="/control", created_at=now, startup_deadline=now + timing.startup,
                       expires_at=now + timing.job, timing=timing, evidence=evidence, s3=s3,
                       store_id="stress-" + run_id if s3 else "").validate()
        resources, job, exporter = render_stress(spec, namespace, campaign, record["owner"], images["harness"]["reference"],
                                                images["broker"]["reference"], args.node, args.storage_class, ca_pem if s3 else None)
        record["backends"][mode] = {"namespace": namespace, "spec": spec.as_dict(), "resources": resources,
                                     "job": job, "exporter": exporter, "images": deepcopy(images), "uids": {}, "podUID": None,
                                     "secretCleanup": "PENDING" if s3 else "NOT_REQUIRED"}
    return record


class Controller:
    def __init__(self, kube, record, path=None, clock=time):
        self.kube, self.record, self.path, self.clock = kube, record, Path(path) if path else None, clock
        self.ca_bundle = None

    def merge_observations(self, previous):
        for name in ("campaign", "owner", "context", "node", "nodeUID", "clusterUID", "createdAt"):
            if previous[name] != self.record[name]:
                raise ValueError("persisted campaign identity changed")
        if set(previous["backends"]) != set(self.record["backends"]):
            raise ValueError("persisted backend selection changed")
        for mode, observed in previous["backends"].items():
            entry = self.record["backends"][mode]
            for name in ("namespace", "spec", "resources", "job", "exporter", "images"):
                if observed[name] != entry[name]:
                    raise ValueError("persisted launch configuration changed")
            for name, uid in observed["uids"].items():
                if name == "Pod/" + RESULTS:
                    continue  # Exporters are disposable and use UID-conditioned deletion.
                if name in entry["uids"] and entry["uids"][name] != uid:
                    raise ValueError("persisted resource UID changed")
                entry["uids"][name] = uid
            if observed["podUID"] is not None:
                if entry["podUID"] is not None and entry["podUID"] != observed["podUID"]:
                    raise ValueError("persisted Job Pod identity changed")
                entry["podUID"] = observed["podUID"]
            for name in ("stopRequested", "forcedStop"):
                if observed.get(name):
                    entry[name] = True
            if observed["secretCleanup"] == "COMPLETE":
                entry["secretCleanup"] = "COMPLETE"
        order = {"PLANNED": 0, "STARTING": 1, "READY": 2, "ERROR": 3, "INTERRUPTED": 3}
        if order[previous["launchStatus"]] > order[self.record["launchStatus"]]:
            self.record["launchStatus"] = previous["launchStatus"]

    def persist(self):
        if self.path:
            durable_json(self.path, self.record)
        if len(json.dumps(self.record).encode()) > 256 * 1024:
            raise ValueError("campaign record exceeds 256 KiB")
        for entry in self.record["backends"].values():
            if not entry["uids"].get("ConfigMap/" + RECORD):
                continue
            current = self.kube.get("ConfigMap", RECORD, entry["namespace"])
            owned(current, self.record, entry["uids"]["ConfigMap/" + RECORD])
            self.merge_observations(json.loads(current["data"]["run.json"]))
            desired = {"apiVersion": "v1", "kind": "ConfigMap", "metadata": metadata(self.record, entry["namespace"], RECORD),
                       "data": {"run.json": json.dumps(self.record, allow_nan=False)}}
            desired["metadata"]["resourceVersion"] = current["metadata"]["resourceVersion"]
            try:
                self.kube.replace(desired)
            except Exception:
                actual = self.kube.get("ConfigMap", RECORD, entry["namespace"])
                owned(actual, self.record, current["metadata"]["uid"])
                if actual.get("data") != desired["data"]:
                    raise
        if self.path:
            durable_json(self.path, self.record)

    def ensure(self, entry, desired, sensitive=False):
        namespace = None if desired["kind"] == "Namespace" else entry["namespace"]
        name = desired["metadata"]["name"]
        actual = self.kube.get(desired["kind"], name, namespace)
        if actual is None:
            try:
                self.kube.create(desired, sensitive=sensitive)
            except Exception:
                actual = self.kube.get(desired["kind"], name, namespace)
                if actual is None:
                    raise
            else:
                actual = self.kube.get(desired["kind"], name, namespace)
        owned(actual, self.record, entry["uids"].get(key(desired)))
        if not contains(actual, desired):
            raise ValueError("owned resource differs from its persisted specification")
        entry["uids"][key(desired)] = actual["metadata"]["uid"]
        self.persist()
        return actual

    def check_cluster(self):
        cluster = self.kube.get("Namespace", "kube-system")
        node = self.kube.get("Node", self.record["node"])
        if cluster["metadata"]["uid"] != self.record["clusterUID"] or node["metadata"]["uid"] != self.record["nodeUID"]:
            raise ValueError("context identifies a different cluster or node")
        return node

    def preflight(self):
        node = self.kube.get("Node", self.record["node"])
        self.record["clusterUID"] = self.kube.get("Namespace", "kube-system")["metadata"]["uid"]
        self.record["nodeUID"] = node["metadata"]["uid"]
        if (node.get("spec", {}).get("unschedulable") or not condition(node, "Ready")
                or any(condition(node, name) for name in ("MemoryPressure", "DiskPressure", "PIDPressure"))
                or any(item["effect"] in ("NoSchedule", "NoExecute") for item in node.get("spec", {}).get("taints", []))):
            raise ValueError("selected node must be Ready, schedulable and require no missing toleration")
        available = {name: quantity(node["status"]["allocatable"][name]) for name in ("cpu", "memory", "ephemeral-storage")}
        pods = self.kube.get("pods", field_selector="spec.nodeName=" + self.record["node"])["items"]
        for pod in pods:
            if pod.get("status", {}).get("phase") in ("Succeeded", "Failed"):
                continue
            for name in available:
                available[name] -= pod_request(pod, name)
        needed = dict.fromkeys(available, 0)
        imported = {canonical_reference(image) for item in node["status"].get("images", []) for image in item.get("names", [])}
        for entry in self.record["backends"].values():
            for container in entry["job"]["spec"]["template"]["spec"]["containers"]:
                if container["image"] not in imported:
                    raise ValueError("reviewed immutable image is not imported on the selected node")
                for name in ("cpu", "memory"):
                    # Reserve memory limits as headroom, and scheduler CPU requests.
                    field = "limits" if name == "memory" else "requests"
                    needed[name] += quantity(container["resources"][field][name])
            needed["ephemeral-storage"] += 4 * 1024**3
        if any(needed[name] > available[name] for name in needed):
            raise ValueError("selected node lacks capacity for all requested endurance Jobs")
        hostname = node["metadata"].get("labels", {}).get("kubernetes.io/hostname")
        if not hostname:
            raise ValueError("selected node lacks its hostname label")
        self.record["nodeHostname"] = hostname
        for entry in self.record["backends"].values():
            entry["job"]["spec"]["template"]["spec"]["nodeSelector"] = {"kubernetes.io/hostname": hostname}
            entry["exporter"]["spec"]["nodeSelector"] = {"kubernetes.io/hostname": hostname}
        self.persist()

    def snapshot(self, entry):
        namespace = self.kube.get("Namespace", entry["namespace"])
        if namespace is None:
            return None, []
        owned(namespace, self.record, entry["uids"].get("Namespace/" + entry["namespace"]))
        changed = False
        for resource in [namespace, self.kube.get("ConfigMap", RECORD, entry["namespace"])]:
            if resource is not None and key(resource) not in entry["uids"]:
                owned(resource, self.record)
                entry["uids"][key(resource)] = resource["metadata"]["uid"]
                changed = True
        job = self.kube.get("Job", JOB, entry["namespace"])
        if job:
            owned(job, self.record, entry["uids"].get("Job/" + JOB))
            if "Job/" + JOB not in entry["uids"]:
                entry["uids"]["Job/" + JOB] = job["metadata"]["uid"]
                changed = True
        if changed:
            self.persist()
        pods = self.kube.get("pods", namespace=entry["namespace"], selector="job-name=" + JOB)["items"]
        if len(pods) > 1:
            raise ValueError("endurance Job created a replacement or extra Pod")
        for pod in pods:
            owned(pod, self.record)
            job_uid = job["metadata"]["uid"] if job else entry["uids"].get("Job/" + JOB)
            if not any(owner.get("uid") == job_uid and owner.get("controller") is True for owner in pod["metadata"].get("ownerReferences", [])):
                raise ValueError("Pod is not controlled by the recorded Job")
            if entry["podUID"] is not None and pod["metadata"]["uid"] != entry["podUID"]:
                raise ValueError("Job Pod identity changed")
            if entry["podUID"] is None:
                entry["podUID"] = pod["metadata"]["uid"]
                self.persist()
        return job, pods

    def progress(self, entry, pod):
        status = next((s for s in pod.get("status", {}).get("containerStatuses", []) if s["name"] == "client"), {})
        if "running" not in status.get("state", {}):
            return None
        data = self.kube.call("-n", entry["namespace"], "exec", pod["metadata"]["name"], "-c", "client", "--",
                              "python3", "-c", READ_PROGRESS, entry["spec"]["run_id"], limit=200 * 1024)
        return json.loads(data)

    def status(self):
        self.check_cluster()
        result = {"campaign": self.record["campaign"], "launchStatus": self.record["launchStatus"], "backends": {}}
        for mode, entry in self.record["backends"].items():
            job, pods = self.snapshot(entry)
            state = "MISSING" if job is None else "TERMINAL" if terminal(job, pods, mode) else "STARTING"
            progress, progress_error = None, None
            try:
                progress = self.progress(entry, pods[0]) if pods else None
            except Exception as error:
                progress_error = type(error).__name__
            if progress and progress.get("ready") and state != "TERMINAL":
                state = "RUNNING"
            statuses = pods[0].get("status", {}).get("containerStatuses", []) if pods else []
            if job and (condition(job, "Failed") or condition(job, "FailureTarget") or any(
                    s.get("restartCount", 0) or s.get("state", {}).get("terminated", {}).get("exitCode", 0) for s in statuses)):
                state = "FAILED"
            updated = (progress or {}).get("progress", {}).get("updatedAt")
            result["backends"][mode] = {"namespace": entry["namespace"], "state": state, "podUID": entry["podUID"],
                                         "progress": progress, "progressError": progress_error, "containers": statuses,
                                         "progressAgeSeconds": self.clock.time() - updated if updated is not None else None,
                                         "secretCleanup": entry["secretCleanup"]}
        return result

    def start(self, credential_values=None):
        self.preflight()
        self.record["launchStatus"] = "STARTING"
        self.persist()
        try:
            for mode, entry in self.record["backends"].items():
                spec = RunSpec.from_dict(entry["spec"])
                if self.clock.time() >= spec.startup_deadline:
                    raise TimeoutError("persisted startup admission expired before Job creation")
                if spec.s3:
                    frozen_credentials(spec, credential_values or {}, self.clock.time())
                    try:
                        client = sdk_client(spec, credential_values, ca_bundle=self.ca_bundle)
                        try:
                            store = OwnedStore(client, spec, self.clock.monotonic() + min(30, spec.timing.startup))
                            objects, uploads, _ = store.inventory(include_marker=True)
                            if objects or uploads:
                                raise ValueError("S3 run prefix is already populated")
                        finally:
                            client.close()
                    except (InterruptedError, KeyboardInterrupt):
                        raise
                    except Exception as error:
                        raise RuntimeError("S3 preflight failed: " + type(error).__name__) from None
                self.ensure(entry, entry["resources"][0])
                record_map = {"apiVersion": "v1", "kind": "ConfigMap", "metadata": metadata(self.record, entry["namespace"], RECORD),
                              "data": {"run.json": json.dumps(self.record, allow_nan=False)}}
                self.ensure(entry, record_map)
                for resource in entry["resources"][1:]:
                    self.ensure(entry, resource)
                if spec.s3:
                    secret = {"apiVersion": "v1", "kind": "Secret", "metadata": metadata(self.record, entry["namespace"], SECRET),
                              "type": "Opaque", "immutable": True, "data": {name: base64.b64encode(value.encode()).decode()
                                                           for name, value in credential_values.items()}}
                    self.ensure(entry, secret, sensitive=True)
                    frozen_credentials(spec, credential_values, self.clock.time())
                if self.clock.time() >= spec.startup_deadline:
                    raise TimeoutError("startup admission expired before Job submission")
                self.ensure(entry, entry["job"])
            pending = set(self.record["backends"])
            while pending:
                for mode in list(pending):
                    entry = self.record["backends"][mode]
                    job, pods = self.snapshot(entry)
                    if self.clock.time() >= entry["spec"]["startup_deadline"]:
                        raise TimeoutError("endurance startup readiness deadline expired")
                    if condition(job, "Failed") or condition(job, "FailureTarget"):
                        raise RuntimeError("endurance Job failed during startup")
                    if pods:
                        for status in pods[0].get("status", {}).get("containerStatuses", []):
                            if status.get("restartCount", 0) or "terminated" in status.get("state", {}):
                                raise RuntimeError("a container exited or restarted during startup")
                        progress = self.progress(entry, pods[0])
                        if (progress and progress.get("ready", {}).get("status") == "SUCCESS" and not progress.get("failure")
                                and progress.get("progress", {}).get("phase") not in (None, "startup")):
                            pending.remove(mode)
                if pending:
                    self.clock.sleep(1)
            self.record["launchStatus"] = "READY"
            self.persist()
            return self.status()
        except BaseException as error:
            self.record["launchStatus"] = "INTERRUPTED" if isinstance(error, (KeyboardInterrupt, InterruptedError)) else "ERROR"
            self.record["launchError"] = type(error).__name__
            try:
                self.persist()
            except Exception:
                pass
            try:
                self.record["startCleanupErrors"] = self.stop()
            except Exception as cleanup_error:
                self.record["startCleanupErrors"] = [{"error": type(cleanup_error).__name__}]
            try:
                self.persist()
            except Exception:
                pass
            raise

    def remove(self, entry, kind, name):
        actual = self.kube.get(kind, name, entry["namespace"])
        if actual is None:
            if kind == "Pod" and name == RESULTS:
                entry["uids"].pop(kind + "/" + name, None)
            return
        uid = entry["uids"].get(kind + "/" + name)
        if uid is None:
            owned(actual, self.record)
            uid = actual["metadata"]["uid"]
            entry["uids"][kind + "/" + name] = uid
            self.persist()
        owned(actual, self.record, uid)
        try:
            self.kube.delete(kind, name, entry["namespace"], uid)
        except Exception:
            if self.kube.get(kind, name, entry["namespace"]) is not None:
                raise
        deadline = self.clock.monotonic() + 30
        while self.kube.get(kind, name, entry["namespace"]) is not None:
            if self.clock.monotonic() >= deadline:
                raise TimeoutError("owned temporary resource deletion was not confirmed")
            self.clock.sleep(1)
        if kind == "Pod" and name == RESULTS:
            entry["uids"].pop(kind + "/" + name, None)
            self.persist()

    def clean_secret(self, entry):
        if entry["spec"]["mode"] == "s3":
            self.remove(entry, "Secret", SECRET)
            entry["secretCleanup"] = "COMPLETE"
            self.persist()

    def stop(self):
        self.check_cluster()
        errors, pending = [], {}
        for mode, entry in self.record["backends"].items():
            try:
                job, pods = self.snapshot(entry)
                if terminal(job, pods, mode):
                    self.clean_secret(entry)
                    continue
                if job is None:
                    raise RuntimeError("orphaned writers remain without the recorded Job")
                entry["stopRequested"] = True
                self.persist()
                if pods:
                    try:
                        self.kube.call("-n", entry["namespace"], "exec", pods[0]["metadata"]["name"], "-c", "client", "--",
                                       "python3", "-c", REQUEST_STOP, entry["spec"]["run_id"])
                    except Exception:
                        pass  # Independent supervision may already be handling a failed client.
                timing = RunSpec.from_dict(entry["spec"]).timing
                deadline = self.clock.monotonic() + (timing.drain + timing.shutdown if pods else 0)
                pending[mode] = {"deadline": deadline, "forced": False}
            except Exception as error:
                errors.append({"backend": mode, "error": type(error).__name__})
        # Request every backend's drain before waiting for any one of them.
        while pending:
            for mode, state in list(pending.items()):
                entry = self.record["backends"][mode]
                try:
                    job, pods = self.snapshot(entry)
                    if terminal(job, pods, mode):
                        self.clean_secret(entry)
                        del pending[mode]
                        continue
                    if self.clock.monotonic() >= state["deadline"]:
                        if state["forced"]:
                            raise TimeoutError("suspended Job writers did not terminate within the cancellation bound")
                        entry["forcedStop"] = True
                        self.persist()
                        job = owned(self.kube.get("Job", JOB, entry["namespace"]), self.record, entry["uids"]["Job/" + JOB])
                        try:
                            self.kube.patch("Job", JOB, entry["namespace"], {"metadata": {"resourceVersion": job["metadata"]["resourceVersion"]},
                                                                            "spec": {"suspend": True}})
                        except Exception:
                            actual = owned(self.kube.get("Job", JOB, entry["namespace"]), self.record, job["metadata"]["uid"])
                            if not actual.get("spec", {}).get("suspend"):
                                raise
                        state.update(forced=True, deadline=self.clock.monotonic() + 120)
                except Exception as error:
                    errors.append({"backend": mode, "error": type(error).__name__})
                    del pending[mode]
            if pending:
                self.clock.sleep(1)
        self.persist()
        return errors

    def diagnostics(self, entry, pods, directory, spec):
        diagnostics = {"logs": {}, "events": {"coverage": "SNAPSHOT"}}
        required = sum((directory / name).stat().st_size for name in ("final-job.json", "final-pods.json"))
        remaining = spec.evidence.diagnostic_bytes - required - 64 * 1024
        if remaining < 0:
            raise ValueError("diagnostic budget cannot hold required Kubernetes status and collection metadata")
        event_limit = min(8 * 1024**2, remaining // 4)
        log_limit = min(16 * 1024**2, (remaining - event_limit) // max(1, len(roles(spec.mode))))
        def capture(name, arguments, limit):
            with tempfile.NamedTemporaryFile(dir=directory, delete=False) as output:
                temporary = Path(output.name)
                try:
                    self.kube.call(*arguments, output=output, limit=limit)
                    output.flush()
                    os.fsync(output.fileno())
                    os.replace(temporary, directory / name)
                finally:
                    temporary.unlink(missing_ok=True)
        for pod in pods:
            for name in roles(spec.mode):
                relative = f"{name}.log"
                try:
                    if log_limit <= 0:
                        raise ValueError("no diagnostic log capacity remains")
                    capture(relative, ["-n", entry["namespace"], "logs", pod["metadata"]["name"], "-c", name,
                                       "--limit-bytes=" + str(log_limit)], log_limit)
                    diagnostics["logs"][name] = {"coverage": "TAIL_ONLY", "requestedBytes": log_limit,
                                                  "retrieved": True, "completeHistory": "UNKNOWN"}
                except Exception as error:
                    diagnostics["logs"][name] = {"coverage": "TAIL_ONLY", "retrieved": False, "error": type(error).__name__}
        try:
            capture("events.json", ["-n", entry["namespace"], "get", "events", "-o", "json"], event_limit)
            diagnostics["events"]["retrieved"] = True
        except Exception as error:
            diagnostics["events"].update(retrieved=False, error=type(error).__name__)
        durable_json(directory / "diagnostics.json", diagnostics)
        return diagnostics

    def export(self, entry, directory, spec):
        destination = directory / "results"
        if destination.is_symlink():
            raise ValueError("collected results cannot be a link")
        if destination.exists():
            inventory(destination, spec.evidence)
            return destination
        claim = self.kube.get("PersistentVolumeClaim", RESULTS, entry["namespace"])
        if claim is None:
            raise ValueError("results PVC was not created; only Kubernetes evidence is available")
        owned(claim, self.record, entry["uids"].get("PersistentVolumeClaim/" + RESULTS))
        # Remove only this collection's unfinished local artifacts before a resumed transfer.
        partial = directory / ".results.partial"
        if partial.is_symlink():
            raise ValueError("collection temporary directory is a link")
        if partial.exists():
            shutil.rmtree(partial)
        current = self.kube.get("Pod", RESULTS, entry["namespace"])
        if current is not None:
            owned(current, self.record, entry["uids"].get("Pod/" + RESULTS))
            if current.get("status", {}).get("phase") in ("Failed", "Succeeded"):
                self.remove(entry, "Pod", RESULTS)
        else:
            entry["uids"].pop("Pod/" + RESULTS, None)
        self.ensure(entry, entry["exporter"])
        deadline = self.clock.monotonic() + 180
        while True:
            pod = owned(self.kube.get("Pod", RESULTS, entry["namespace"]), self.record, entry["uids"]["Pod/" + RESULTS])
            if pod.get("status", {}).get("phase") in ("Failed", "Succeeded"):
                raise RuntimeError("read-only exporter exited before transfer")
            if condition(pod, "Ready"):
                break
            if self.clock.monotonic() >= deadline:
                raise TimeoutError("read-only exporter did not become Ready")
            self.clock.sleep(1)
        archive = directory / ".results.tar.partial"
        if archive.is_symlink():
            raise ValueError("collection archive cannot be a link")
        limit = spec.evidence.ordinary_bytes + spec.evidence.terminal_bytes + spec.evidence.max_files * 2048 + 10240
        with archive.open("wb") as output:
            self.kube.call("-n", entry["namespace"], "exec", RESULTS, "-c", "results", "--", "python3",
                           "/opt/stress/stress_export.py", "--spec", "/config/run-spec.json", "export",
                           output=output, limit=limit, timeout=300, sensitive=True)
            output.flush()
            os.fsync(output.fileno())
        extract(archive, partial, spec.evidence)
        os.replace(partial, destination)
        archive.unlink()
        return destination

    def collect(self, output):
        self.check_cluster()
        output = Path(output)
        output.mkdir(parents=True, exist_ok=True)
        if output.is_symlink():
            raise ValueError("collection output cannot be a link")
        identity = output / "collection.json"
        if identity.exists():
            if json.loads(identity.read_text()) != {"campaign": self.record["campaign"], "owner": self.record["owner"]}:
                raise ValueError("collection directory belongs to a different campaign")
        elif any(output.iterdir()):
            raise ValueError("refusing to adopt a nonempty collection directory")
        else:
            durable_json(identity, {"campaign": self.record["campaign"], "owner": self.record["owner"]})
        report = {"campaign": self.record["campaign"], "executionStatus": "SUCCESS", "evidenceStatus": "COMPLETE",
                  "resourceAssessment": "INCONCLUSIVE", "backends": {}, "harnessRevision": self.record["harnessRevision"],
                  "brokerRevision": self.record["brokerRevision"], "node": self.record["node"], "nodeUID": self.record["nodeUID"],
                  "launchStatus": self.record["launchStatus"], "evidenceScope": "KUBERNETES_COLLECTED"}
        for mode, entry in self.record["backends"].items():
            result = {"executionStatus": "ERROR", "evidenceStatus": "INCOMPLETE", "resourceAssessment": "INCONCLUSIVE",
                      "kubernetesCollection": "INCOMPLETE", "errors": []}
            report["backends"][mode] = result
            directory = output / mode
            directory.mkdir(exist_ok=True)
            if directory.is_symlink():
                raise ValueError("backend collection directory cannot be a link")
            stopped = False
            try:
                spec = RunSpec.from_dict(entry["spec"])
                job, pods = self.snapshot(entry)
                if not terminal(job, pods, mode):
                    raise RuntimeError("writers are still active; use status or stop before collection")
                stopped = True
                required_bytes = sum(len(json.dumps(value, allow_nan=False, indent=2).encode()) + 1
                                     for value in (job, {"items": pods}))
                if required_bytes + 64 * 1024 > spec.evidence.diagnostic_bytes:
                    raise ValueError("diagnostic budget cannot hold required Kubernetes status")
                durable_json(directory / "final-job.json", job)
                durable_json(directory / "final-pods.json", {"items": pods})
                self.diagnostics(entry, pods, directory, spec)
                config = owned(self.kube.get("ConfigMap", CONFIG, entry["namespace"]), self.record,
                               entry["uids"].get("ConfigMap/" + CONFIG))
                if not contains(config, entry["resources"][2]):
                    raise ValueError("live configuration changed from the persisted launch plan")
                source = self.export(entry, directory, spec)
                evidence = validate_directory(source, spec)
                result.update(evidenceStatus=evidence["evidenceStatus"], resourceAssessment=evidence["resourceAssessment"],
                              resourceLimitsExceeded=evidence.get("limitsExceeded", False),
                              evidence=evidence)
                summary = json.loads((source / "summary.json").read_text())
                result["executionStatus"] = "INTERRUPTED" if entry.get("stopRequested") else summary["executionStatus"]
                limit_exit = (result["resourceLimitsExceeded"] and summary["executionStatus"] == "SUCCESS"
                              and evidence["evidenceStatus"] == "COMPLETE")
                result["kubernetes"] = validate_job(job, pods, mode, entry, self.record["node"], limit_exit=limit_exit)
                result["kubernetesCollection"] = "COMPLETE"
            except Exception as error:
                result["errors"].append(str(error))
                result["executionStatus"] = "INTERRUPTED" if entry.get("stopRequested") else "ERROR"
                result["evidenceStatus"] = "INCOMPLETE"
            finally:
                if stopped:
                    for label, action in (("exporter", lambda: self.remove(entry, "Pod", RESULTS)),
                                          ("Secret", lambda: self.clean_secret(entry))):
                        try:
                            action()
                        except Exception as error:
                            result["errors"].append(label + " cleanup: " + type(error).__name__)
                            result["evidenceStatus"] = "INCOMPLETE"
            result["secretCleanup"] = entry["secretCleanup"]
            size = sum(path.stat().st_size for path in directory.rglob("*") if path.is_file())
            if size > spec.evidence.total_bytes:
                result["evidenceStatus"] = "ERROR"
                result["errors"].append("collected evidence exceeds its total budget")
            if result["executionStatus"] != "SUCCESS":
                report["executionStatus"] = "ERROR"
            if result["errors"] or result["evidenceStatus"] != "COMPLETE":
                report["evidenceStatus"] = "INCOMPLETE"
        assessments = {value["resourceAssessment"] for value in report["backends"].values()}
        executions = {value["executionStatus"] for value in report["backends"].values()}
        report["executionStatus"] = "ERROR" if "ERROR" in executions else "INTERRUPTED" if "INTERRUPTED" in executions else "SUCCESS"
        report["resourceAssessment"] = "GROWTH_DETECTED" if "GROWTH_DETECTED" in assessments else (
            "WITHIN_LIMITS" if assessments == {"WITHIN_LIMITS"} else "INCONCLUSIVE")
        report["resourceLimitsExceeded"] = any(value.get("resourceLimitsExceeded", False) for value in report["backends"].values())
        report["success"] = (report["executionStatus"] == "SUCCESS" and report["evidenceStatus"] == "COMPLETE"
                             and not report["resourceLimitsExceeded"]
                             and report["resourceAssessment"] != "GROWTH_DETECTED")
        self.persist()
        durable_json(output / "run.json", self.record)
        durable_json(output / "summary.json", report)
        return report


def terminal(job, pods, mode):
    if job is None:
        return not pods
    if not pods:
        return bool(job.get("spec", {}).get("suspend"))
    statuses = pods[0].get("status", {}).get("containerStatuses", [])
    return (pods[0].get("status", {}).get("phase") in ("Succeeded", "Failed")
            and Counter(item["name"] for item in statuses) == Counter(roles(mode))
            and all("terminated" in item.get("state", {}) for item in statuses))


def validate_job(job, pods, mode, entry, node, limit_exit=False):
    completed = (job is not None and condition(job, "Complete") and not condition(job, "Failed")
                 and not condition(job, "FailureTarget") and not job.get("status", {}).get("failed", 0))
    limited = (job is not None and not condition(job, "Complete") and condition(job, "Failed")
               and job.get("status", {}).get("failed") == 1
               and all(c.get("reason") == "BackoffLimitExceeded" for c in job.get("status", {}).get("conditions", [])
                       if c["type"] in ("Failed", "FailureTarget") and c["status"] == "True"))
    if (job is None or not terminal(job, pods, mode) or not (limited if limit_exit else completed)
            or entry.get("stopRequested") or entry.get("forcedStop") or len(pods) != 1):
        raise ValueError("Job did not finish the declared campaign without cancellation or failed attempts")
    if not contains(job["spec"], entry["job"]["spec"]):
        raise ValueError("Job configuration changed from its launch plan")
    pod = pods[0]
    if (pod["metadata"]["uid"] != entry["podUID"] or job["metadata"]["uid"] != entry["uids"].get("Job/" + JOB)
            or not any(ref.get("uid") == job["metadata"]["uid"] and ref.get("controller") is True
                       for ref in pod["metadata"].get("ownerReferences", []))):
        raise ValueError("terminal Job or Pod does not match the recorded ownership")
    if pod["status"]["phase"] != ("Failed" if limit_exit else "Succeeded") or pod["spec"]["nodeName"] != node:
        raise ValueError("Job Pod did not succeed on the selected node")
    expected = {c["name"]: c["image"] for c in entry["job"]["spec"]["template"]["spec"]["containers"]}
    if {c["name"]: c["image"] for c in pod["spec"]["containers"]} != expected:
        raise ValueError("Pod images differ from the reviewed immutable references")
    images = {}
    for item in pod["status"]["containerStatuses"]:
        end = item["state"]["terminated"]
        identity = entry["images"]["broker" if item["name"] == "pagebroker" else "harness"]
        code = 2 if limit_exit and item["name"] == "client" else 0
        if (item.get("restartCount") != 0 or end.get("exitCode") != code or end.get("reason") != ("Error" if code else "Completed")
                or not item.get("containerID") or not re.search(r"sha256:[a-f0-9]{64}$", item.get("imageID", ""))
                or canonical_reference(item.get("image", "")) not in (identity["reference"], identity["tag"])):
            raise ValueError("a required container failed, restarted or lacks its requested image identity")
        kind = runtime_identity(item["imageID"], identity)
        images[item["name"]] = {"imageID": item["imageID"], "containerID": item["containerID"],
                                "digestKind": kind, "reviewed": identity}
    return {"podUID": pod["metadata"]["uid"], "node": node, "images": images}


def load_record(args, kube):
    if args.record:
        if args.record.stat().st_size > 256 * 1024:
            raise ValueError("campaign record exceeds its limit")
        record = json.loads(args.record.read_text())
    else:
        resource = kube.get("ConfigMap", RECORD, args.namespace)
        if resource is None or len(resource.get("data", {}).get("run.json", "").encode()) > 256 * 1024:
            raise ValueError("bounded persisted campaign record is missing")
        record = json.loads(resource["data"]["run.json"])
        owned(resource, record)
        if args.namespace not in [entry["namespace"] for entry in record["backends"].values()]:
            raise ValueError("namespace is not part of the persisted campaign")
    if record.get("schemaVersion") != 2 or record.get("context") != args.context:
        raise ValueError("campaign schema or explicit context does not match")
    for value in (record["campaign"], record["owner"]):
        if str(uuid.UUID(value)) != value:
            raise ValueError("invalid campaign ownership")
    if not record["backends"] or set(record["backends"]) - {"filesystem", "s3"}:
        raise ValueError("invalid persisted backend selection")
    for mode, entry in record["backends"].items():
        identities = validate_identities({"schemaVersion": 1, "images": entry["images"]},
            {"harness": record["harnessRevision"], "broker": record["brokerRevision"]})
        spec = RunSpec.from_dict(entry["spec"])
        if spec.mode != mode or not DNS_LABEL.fullmatch(entry["namespace"]) or not entry["namespace"].startswith("pagebroker-stress-"):
            raise ValueError("persisted backend identity is inconsistent")
        for resource in [*entry["resources"], entry["job"], entry["exporter"]]:
            owned(resource, record)
            if resource["kind"] == "Namespace":
                if resource["metadata"]["name"] != entry["namespace"]:
                    raise ValueError("persisted namespace plan is inconsistent")
            elif resource["metadata"].get("namespace") != entry["namespace"]:
                raise ValueError("persisted resource escaped the backend namespace")
        for container in entry["job"]["spec"]["template"]["spec"]["containers"]:
            role = "broker" if container["name"] == "pagebroker" else "harness"
            if container["image"] != identities[role]["reference"]:
                raise ValueError("persisted container differs from its reviewed immutable image")
        if entry["exporter"]["spec"]["containers"][0]["image"] != identities["harness"]["reference"]:
            raise ValueError("persisted exporter differs from the reviewed harness image")
        if json.loads(entry["resources"][2]["data"]["run-spec.json"]) != spec.as_dict():
            raise ValueError("persisted manifest and run specification disagree")
    controller = Controller(kube, record, args.record)
    controller.check_cluster()
    return controller


def parser():
    root = argparse.ArgumentParser(description=__doc__)
    root.add_argument("--kubeconfig", type=Path, required=True)
    root.add_argument("--context", required=True)
    commands = root.add_subparsers(dest="command", required=True)
    start = commands.add_parser("start")
    start.add_argument("--record", type=Path, required=True)
    start.add_argument("--backend", choices=("both", "filesystem", "s3"), default="both")
    for name in ("node", "client-image", "pagebroker-image", "harness-revision", "broker-revision"):
        start.add_argument("--" + name, required=True)
    start.add_argument("--storage-class", default="local-path")
    start.add_argument("--profile", type=Path)
    start.add_argument("--image-identities", type=Path, required=True)
    start.add_argument("--bucket")
    start.add_argument("--region")
    start.add_argument("--endpoint")
    start.add_argument("--prefix", default="pagebroker-stress")
    start.add_argument("--addressing", choices=("virtual", "path"), default="virtual")
    start.add_argument("--credentials-source", choices=("environment", "aws-cli", "static-environment"), default="environment",
                       help="temporary credentials from environment/aws-cli, or explicit static keys without a session token")
    start.add_argument("--aws-profile")
    start.add_argument("--ca-bundle", type=Path)
    for name in ("status", "collect", "stop"):
        command = commands.add_parser(name)
        source = command.add_mutually_exclusive_group(required=True)
        source.add_argument("--record", type=Path)
        source.add_argument("--namespace")
        if name in ("collect", "stop"):
            command.add_argument("--output", type=Path, required=True)
    return root


def main(argv=None):
    args = parser().parse_args(argv)
    def interrupted(_signum, _frame):
        raise InterruptedError("controller interrupted")
    signal.signal(signal.SIGINT, interrupted)
    signal.signal(signal.SIGTERM, interrupted)
    try:
        kube = Kubernetes(args.kubeconfig, args.context)
        if args.command == "start":
            values, expiry = credentials(args) if args.backend in ("both", "s3") else (None, None)
            record = make_record(args, credential_expiry=expiry)
            args.record.parent.mkdir(parents=True, exist_ok=True)
            with args.record.open("x"):
                pass
            controller = Controller(kube, record, args.record)
            controller.ca_bundle = str(args.ca_bundle) if args.ca_bundle else None
            controller.persist()
            result = controller.start(values)
        else:
            controller = load_record(args, kube)
            if args.command == "status":
                result = controller.status()
            else:
                errors = controller.stop() if args.command == "stop" else []
                result = controller.collect(args.output)
                if errors:
                    result.update(success=False, stopErrors=errors)
        print(json.dumps(result, allow_nan=False, indent=2))
        return 1 if result.get("success") is False else 0
    except (Exception, KeyboardInterrupt) as error:
        print(f"ERROR: {type(error).__name__}: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
