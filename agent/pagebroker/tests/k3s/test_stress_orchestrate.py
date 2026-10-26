# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

import base64
from copy import deepcopy
from dataclasses import replace
from datetime import datetime, timezone
import io
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch
import uuid

import yaml

import stress_orchestrate as cli
from stress_config import RunSpec, Timing
from stress_export import export, extract
from stress_kube import Kubernetes, execute, durable_json, pod_request
from stress_render import CAMPAIGN, CONFIG, JOB, OWNER, RECORD, RESULTS, SECRET, render_stress
from stress_storage import EvidenceStore
from test_stress_images import image_document
from test_stress import FakeClock


def options(root, backend="filesystem"):
    timing = Timing(startup=10, warmup_min=1, warmup_max=2, warmup_batches=1, measured=10,
                    measured_batches=1, cadence=1, idle=1, cooldown=1, batch=2, cleanup=1,
                    drain=3, shutdown=5, margin=1, job=40, broker_term=0.2, kill_wait=0.1, client_term=0.2)
    profile = root / "profile.json"
    profile.write_text(json.dumps({"timing": vars(timing)}))
    identities = root / "identities.json"
    identities.write_text(json.dumps(image_document()))
    return SimpleNamespace(backend=backend, node="test-node", client_image="local/stress:test", pagebroker_image="local/broker:test",
                           storage_class="local-path", context="test-context", profile=profile, ca_bundle=None,
                           harness_revision="1" * 40, broker_revision="2" * 40, bucket="fake-bucket", region="us-east-1",
                           endpoint="https://s3.example.invalid", prefix="test", addressing="virtual",
                           record=root / "run.json", credentials_source="environment", aws_profile=None,
                           image_identities=identities)


def fake_credentials():
    return {"AWS_ACCESS_KEY_ID": "fake-access", "AWS_SECRET_ACCESS_KEY": "fake-secret", "AWS_SESSION_TOKEN": "fake-token",
            "AWS_CREDENTIAL_EXPIRATION": datetime.fromtimestamp(10000, timezone.utc).isoformat()}


def normalize_api(resource):
    resource = deepcopy(resource)
    pod = resource.get("spec", {}).get("template", {}).get("spec", {}) if resource["kind"] == "Job" else resource.get("spec", {})
    for container in pod.get("containers", []):
        for variable in container.get("env", []):
            if variable.get("value") == "":
                del variable["value"]
        if container.get("env") == []:
            del container["env"]
    if resource["kind"] == "Job":
        resource["spec"].update(parallelism=1, completions=1, completionMode="NonIndexed")
    return resource


class FakeKube:
    def __init__(self):
        self.objects, self.calls = {}, []
        self.lost_create, self.failed_create = set(), set()
        self.lost_delete = False
        self.lost_replace = False
        self.failure_export = False
        self.failure_logs = False
        self.cooperative = True
        self.archive = b""
        self.node = {"metadata": {"uid": "node-uid", "labels": {"kubernetes.io/hostname": "test-node"}},
                     "status": {"conditions": [{"type": "Ready", "status": "True"}],
                     "allocatable": {"cpu": "32", "memory": "128Gi", "ephemeral-storage": "200Gi"},
                     "images": [{"names": [item["reference"], item["tag"]]} for item in image_document()["images"].values()]}}

    def get(self, kind, name=None, namespace=None, selector=None, field_selector=None):
        if kind == "Node":
            return deepcopy(self.node)
        if kind == "Namespace" and name == "kube-system":
            return {"metadata": {"uid": "cluster-uid"}}
        if kind == "pods":
            result = [deepcopy(v) for (ns, k, _), v in self.objects.items() if k == "Pod"
                      and (namespace is None or ns == namespace)
                      and (not selector or v["metadata"].get("labels", {}).get("job-name") == JOB)]
            return {"items": result}
        return deepcopy(self.objects.get((namespace, kind, name)))

    def create(self, resource, sensitive=False):
        resource = normalize_api(resource)
        key = (resource["metadata"].get("namespace"), resource["kind"], resource["metadata"]["name"])
        if resource["kind"] in self.failed_create:
            raise RuntimeError("injected create failure")
        if key in self.objects:
            raise RuntimeError("already exists")
        resource["metadata"].update(uid=str(uuid.uuid4()), resourceVersion="1")
        self.objects[key] = resource
        self.calls.append(("create", resource["kind"], resource["metadata"]["name"], sensitive))
        if resource["kind"] == "Job":
            pod = deepcopy(resource["spec"]["template"])
            pod.update(apiVersion="v1", kind="Pod", status={"phase": "Running"})
            pod["metadata"].update(name="stress-pod", namespace=key[0], uid=str(uuid.uuid4()),
                                   ownerReferences=[{"uid": resource["metadata"]["uid"], "controller": True}])
            pod["metadata"]["labels"]["job-name"] = JOB
            pod["spec"]["nodeName"] = "test-node"
            pod["status"]["containerStatuses"] = [{"name": c["name"], "image": c["image"], "imageID": "containerd://sha256:" + ("b" if c["name"] == "pagebroker" else "a") * 64,
                                                     "containerID": "containerd://" + c["name"], "restartCount": 0,
                                                     "state": {"running": {}}} for c in pod["spec"]["containers"]]
            self.objects[(key[0], "Pod", "stress-pod")] = pod
        elif resource["kind"] == "Pod":
            resource["status"] = {"phase": "Running", "conditions": [{"type": "Ready", "status": "True"}]}
        if resource["kind"] in self.lost_create:
            raise TimeoutError("lost create reply")
        return deepcopy(resource)

    def replace(self, resource):
        key = (resource["metadata"].get("namespace"), resource["kind"], resource["metadata"]["name"])
        previous = self.objects[key]
        result = deepcopy(resource)
        result["metadata"].update(uid=previous["metadata"]["uid"], resourceVersion=str(int(previous["metadata"]["resourceVersion"]) + 1))
        self.objects[key] = result
        if self.lost_replace:
            raise TimeoutError("lost replace reply")
        return deepcopy(result)

    def patch(self, kind, name, namespace, value):
        self.calls.append(("suspend", namespace))
        self.objects[(namespace, kind, name)]["spec"].update(value["spec"])
        for key in list(self.objects):
            if key[0] == namespace and key[1] == "Pod" and key[2] == "stress-pod":
                del self.objects[key]
        return deepcopy(self.objects[(namespace, kind, name)])

    def delete(self, kind, name, namespace, uid):
        self.calls.append(("delete", kind, namespace, name))
        resource = self.objects[(namespace, kind, name)]
        if uid != resource["metadata"]["uid"]:
            raise ValueError("UID precondition")
        del self.objects[(namespace, kind, name)]
        if self.lost_delete:
            raise TimeoutError("lost delete reply")

    def finish(self, namespace, success=True):
        job = self.objects[(namespace, "Job", JOB)]
        job["status"] = {"conditions": [{"type": "Complete" if success else "Failed", "status": "True"}]}
        pod = self.objects[(namespace, "Pod", "stress-pod")]
        pod["status"]["phase"] = "Succeeded" if success else "Failed"
        for item in pod["status"]["containerStatuses"]:
            item["state"] = {"terminated": {"exitCode": 0 if success else 1, "reason": "Completed" if success else "Error"}}

    def call(self, *args, **kwargs):
        self.calls.append(args)
        namespace = args[1]
        if "logs" in args:
            if self.failure_logs:
                raise RuntimeError("missing rotated log")
            kwargs["output"].write(b"bounded diagnostic tail\n")
            return b""
        if "events" in args:
            kwargs["output"].write(b'{"items":[]}')
            return b""
        if "export" in args:
            if self.failure_export:
                kwargs["output"].write(self.archive[:100])
                raise TimeoutError("interrupted transfer")
            kwargs["output"].write(self.archive)
            return b""
        if cli.REQUEST_STOP in args:
            if self.cooperative:
                self.finish(namespace, False)
            return b""
        config = self.objects[(namespace, "ConfigMap", CONFIG)]
        spec = json.loads(config["data"]["run-spec.json"])
        return json.dumps({"ready": {"runId": spec["run_id"], "status": "SUCCESS"},
                           "progress": {"phase": "warmup", "runId": spec["run_id"], "monotonic": 1, "counts": {}}}).encode()


class EnduranceControllerTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.args = options(self.root)
        self.clock = FakeClock()
        self.record = cli.make_record(self.args, now=1000)
        self.kube = FakeKube()
        self.controller = cli.Controller(self.kube, self.record, self.args.record, self.clock)

    def start(self, s3=False):
        if s3:
            self.args.backend = "both"
            self.record = cli.make_record(self.args, now=1000, credential_expiry=10000)
            self.controller = cli.Controller(self.kube, self.record, self.args.record, self.clock)
        with patch("stress_orchestrate.sdk_client", return_value=Mock()), patch("stress_orchestrate.OwnedStore") as store:
            store.return_value.inventory.return_value = ([], [], "Disabled")
            self.controller.start(fake_credentials() if s3 else None)

    def finish(self):
        for entry in self.record["backends"].values():
            self.kube.finish(entry["namespace"])

    def archive(self, mode="filesystem"):
        spec = RunSpec.from_dict(self.record["backends"][mode]["spec"])
        directory = self.root / ("source-" + mode)
        directory.mkdir()
        store = EvidenceStore(directory, spec.evidence)
        store.write("run-spec.json", spec.as_dict())
        store.write("summary.json", {"executionStatus": "SUCCESS"})
        stream = io.BytesIO()
        export(directory, spec.evidence, stream)
        self.kube.archive = stream.getvalue()

    def test_start_persists_ownership_and_returns_after_readiness(self):
        self.start()
        self.assertEqual(self.record["launchStatus"], "READY")
        saved = json.loads(self.args.record.read_text())
        entry = next(iter(saved["backends"].values()))
        remote = self.kube.get("ConfigMap", RECORD, entry["namespace"])
        self.assertEqual(json.loads(remote["data"]["run.json"]), saved)
        self.assertTrue(entry["podUID"])
        self.assertTrue(entry["uids"]["Job/" + JOB])

    def test_static_launch_uses_two_key_secret_and_nonsecret_provider_metadata(self):
        self.args.backend = "s3"
        self.args.credentials_source = "static-environment"
        values = {"AWS_ACCESS_KEY_ID": "fake-static-access", "AWS_SECRET_ACCESS_KEY": "fake-static-secret"}
        with patch.dict(os.environ, values, clear=True):
            admitted, expiry = cli.credentials(self.args)
        self.record = cli.make_record(self.args, now=1000, credential_expiry=expiry)
        controller = cli.Controller(self.kube, self.record, self.args.record, self.clock)
        with patch("stress_orchestrate.sdk_client", return_value=Mock()), patch("stress_orchestrate.OwnedStore") as store:
            store.return_value.inventory.return_value = ([], [], "Disabled")
            controller.start(admitted)
        self.assertEqual(self.record["launchStatus"], "READY")
        entry = self.record["backends"]["s3"]
        self.assertEqual(entry["spec"]["s3"]["credential_provider"], "static-environment")
        self.assertIsNone(entry["spec"]["s3"]["credential_expiry"])
        secret = self.kube.get("Secret", SECRET, entry["namespace"])
        self.assertEqual(set(secret["data"]), set(values) | {"credentials"})
        self.assertEqual(base64.b64decode(secret["data"]["credentials"]).decode(),
                         "[default]\naws_access_key_id=fake-static-access\naws_secret_access_key=fake-static-secret\n")
        for container in entry["job"]["spec"]["template"]["spec"]["containers"]:
            environment = {item["name"]: item for item in container.get("env", [])}
            if container["name"] == "pagebroker":
                self.assertEqual(environment, {"AWS_SHARED_CREDENTIALS_FILE": {
                    "name": "AWS_SHARED_CREDENTIALS_FILE", "value": "/credentials/credentials"}})
                mounts = {mount["name"]: mount for mount in container["volumeMounts"]}
                self.assertTrue(mounts["credentials"]["readOnly"])
            elif container["name"] == "s3-helper":
                self.assertEqual({name for name, item in environment.items() if "valueFrom" in item}, set(values))
                for name in ("AWS_SESSION_TOKEN", "AWS_SECURITY_TOKEN", "AWS_CREDENTIAL_EXPIRATION"):
                    self.assertEqual(environment[name], {"name": name})
            else:
                self.assertFalse(any(name.startswith("AWS_") for name in environment))
        for value in values.values():
            self.assertNotIn(value, self.args.record.read_text())

    def test_static_credentials_require_explicit_source_and_reject_session_metadata(self):
        values = {"AWS_ACCESS_KEY_ID": "fake-static-access", "AWS_SECRET_ACCESS_KEY": "fake-static-secret"}
        with patch.dict(os.environ, values, clear=True), self.assertRaises(ValueError):
            cli.credentials(self.args)
        self.args.credentials_source = "static-environment"
        for name in ("AWS_SESSION_TOKEN", "AWS_SECURITY_TOKEN", "AWS_CREDENTIAL_EXPIRATION"):
            with patch.dict(os.environ, values | {name: "fake-sensitive-value"}, clear=True):
                with self.subTest(name=name), self.assertRaisesRegex(RuntimeError, "must not include") as result:
                    cli.credentials(self.args)
                self.assertNotIn("fake-sensitive-value", str(result.exception))
        for name in values:
            with patch.dict(os.environ, {name: values[name]}, clear=True), self.assertRaisesRegex(RuntimeError, "require"):
                cli.credentials(self.args)

    def test_temporary_sources_still_require_token_and_expiry(self):
        for source in ("environment", "aws-cli"):
            self.args.credentials_source = source
            values = fake_credentials()
            exported = {field: values[name] for name, field in (
                ("AWS_ACCESS_KEY_ID", "AccessKeyId"), ("AWS_SECRET_ACCESS_KEY", "SecretAccessKey"),
                ("AWS_SESSION_TOKEN", "SessionToken"), ("AWS_CREDENTIAL_EXPIRATION", "Expiration"))}
            with self.subTest(source=source), patch.dict(os.environ, values, clear=True), \
                    patch("stress_orchestrate.execute", return_value=json.dumps(exported)):
                self.assertEqual(cli.credentials(self.args), (values, 10000))
            exported.pop("SessionToken")
            values.pop("AWS_SESSION_TOKEN")
            with patch.dict(os.environ, values, clear=True), \
                    patch("stress_orchestrate.execute", return_value=json.dumps(exported)), self.assertRaises(ValueError):
                cli.credentials(self.args)

    def test_lost_create_and_update_replies_are_reconciled(self):
        self.kube.lost_create = {"Namespace", "PersistentVolumeClaim", "ConfigMap", "Job", "Secret"}
        self.kube.lost_replace = True
        self.start(s3=True)
        self.assertEqual(self.record["launchStatus"], "READY")
        self.assertEqual(sum(call[:2] == ("create", "Job") for call in self.kube.calls), 2)
        text = self.args.record.read_text()
        self.assertNotIn("fake-secret", text)
        self.assertNotIn("fake-token", text)

    def test_normalized_job_creation_recovery_and_final_validation(self):
        self.kube.lost_create = {"Job"}
        self.start(s3=True)
        self.finish()
        for mode, entry in self.record["backends"].items():
            job, pods = self.controller.snapshot(entry)
            for container in job["spec"]["template"]["spec"]["containers"]:
                self.assertNotEqual(container.get("env"), [])
            cli.validate_job(job, pods, mode, entry, "test-node")
            for mutation in ("deadline", "security", "arguments"):
                modified = deepcopy(job)
                if mutation == "deadline":
                    modified["spec"]["activeDeadlineSeconds"] += 1
                elif mutation == "security":
                    modified["spec"]["template"]["spec"]["containers"][0]["securityContext"]["readOnlyRootFilesystem"] = False
                else:
                    modified["spec"]["template"]["spec"]["containers"][1]["args"].append("--other")
                with self.subTest(mode=mode, mutation=mutation), self.assertRaises(ValueError):
                    cli.validate_job(modified, pods, mode, entry, "test-node")

    def test_node_with_only_the_same_tag_cannot_pass_image_preflight(self):
        self.kube.node["status"]["images"] = [{"names": [i["tag"]]} for i in image_document()["images"].values()]
        with self.assertRaisesRegex(ValueError, "immutable image"):
            self.controller.preflight()
        self.assertFalse(self.kube.calls)

    def test_same_tag_with_different_runtime_contents_is_rejected(self):
        self.start()
        self.finish()
        entry = self.record["backends"]["filesystem"]
        job, pods = self.controller.snapshot(entry)
        for item in pods[0]["status"]["containerStatuses"]:
            identity = entry["images"]["broker" if item["name"] == "pagebroker" else "harness"]
            item["image"] = identity["tag"]
            item["imageID"] = "docker-pullable://" + identity["reference"]
        cli.validate_job(job, pods, "filesystem", entry, "test-node")
        for name in ("pagebroker", "client", "supervisor"):
            changed = deepcopy(pods)
            next(s for s in changed[0]["status"]["containerStatuses"] if s["name"] == name)["imageID"] = "containerd://sha256:" + "e" * 64
            with self.subTest(name=name), self.assertRaisesRegex(ValueError, "reviewed image"):
                cli.validate_job(job, changed, "filesystem", entry, "test-node")

    def test_namespace_collision_never_touches_foreign_resource(self):
        entry = next(iter(self.record["backends"].values()))
        foreign = deepcopy(entry["resources"][0])
        foreign["metadata"]["annotations"][OWNER] = str(uuid.uuid4())
        self.kube.create(foreign)
        with self.assertRaises(ValueError):
            self.controller.start()
        self.assertFalse(any(call[0] in ("delete", "suspend") for call in self.kube.calls))

    def test_second_backend_failure_stops_first_and_cleans_secret(self):
        self.args.backend = "both"
        self.record = cli.make_record(self.args, now=1000, credential_expiry=10000)
        self.controller = cli.Controller(self.kube, self.record, self.args.record, self.clock)
        original = self.kube.create
        def fail(resource, sensitive=False):
            if resource["kind"] == "Job" and resource["metadata"]["namespace"].endswith("s3"):
                raise RuntimeError("second Job rejected")
            return original(resource, sensitive)
        with patch.object(self.kube, "create", fail), patch("stress_orchestrate.sdk_client", return_value=Mock()), patch("stress_orchestrate.OwnedStore") as store:
            store.return_value.inventory.return_value = ([], [], "Disabled")
            with self.assertRaises(RuntimeError):
                self.controller.start(fake_credentials())
        self.assertEqual(self.record["launchStatus"], "ERROR")
        self.assertTrue(self.record["backends"]["filesystem"]["stopRequested"])
        entry = self.record["backends"]["s3"]
        self.assertEqual(entry["secretCleanup"], "COMPLETE")
        self.assertIsNone(self.kube.get("Secret", SECRET, entry["namespace"]))

    def test_expired_startup_and_insufficient_capacity_create_no_job(self):
        self.clock.now = 11
        with self.assertRaises(TimeoutError):
            self.controller.start()
        self.assertFalse(any(call[:2] == ("create", "Job") for call in self.kube.calls))
        self.clock.now = 0
        self.kube.node["status"]["allocatable"]["memory"] = "1Gi"
        with self.assertRaises(ValueError):
            self.controller.preflight()

    def test_node_name_and_hostname_label_may_differ(self):
        self.kube.node["metadata"]["labels"]["kubernetes.io/hostname"] = "selected-node-hostname"
        self.start()
        entry = self.record["backends"]["filesystem"]
        self.assertEqual(entry["job"]["spec"]["template"]["spec"]["nodeSelector"],
                         {"kubernetes.io/hostname": "selected-node-hostname"})
        self.assertEqual(entry["exporter"]["spec"]["nodeSelector"], {"kubernetes.io/hostname": "selected-node-hostname"})
        self.assertEqual(self.record["node"], "test-node")

    def test_credential_expiry_is_checked_again_immediately_before_s3_job(self):
        self.args.backend = "s3"
        self.record = cli.make_record(self.args, now=1000, credential_expiry=1945)
        self.controller = cli.Controller(self.kube, self.record, self.args.record, self.clock)
        original = self.kube.create
        def delay(resource, sensitive=False):
            result = original(resource, sensitive)
            if resource["kind"] == "Secret":
                self.clock.sleep(6)
            return result
        value = fake_credentials() | {"AWS_CREDENTIAL_EXPIRATION": datetime.fromtimestamp(1945, timezone.utc).isoformat()}
        with patch.object(self.kube, "create", delay), patch("stress_orchestrate.sdk_client", return_value=Mock()), patch("stress_orchestrate.OwnedStore") as store:
            store.return_value.inventory.return_value = ([], [], "Disabled")
            with self.assertRaisesRegex(ValueError, "credentials"):
                self.controller.start(value)
        self.assertFalse(any(call[:2] == ("create", "Job") for call in self.kube.calls))

    def test_status_recovers_from_remote_record_without_local_controller(self):
        self.start()
        entry = next(iter(self.record["backends"].values()))
        args = SimpleNamespace(record=None, namespace=entry["namespace"], context=self.args.context)
        restored = cli.load_record(args, self.kube)
        self.assertEqual(restored.status()["backends"]["filesystem"]["state"], "RUNNING")
        self.assertFalse(any(call[0] == "delete" for call in self.kube.calls))
        args.context = "different-context"
        with self.assertRaises(ValueError):
            cli.load_record(args, self.kube)

    def test_replacement_pod_or_node_is_rejected(self):
        self.start()
        entry = next(iter(self.record["backends"].values()))
        pod = self.kube.objects[(entry["namespace"], "Pod", "stress-pod")]
        pod["metadata"]["uid"] = "replacement"
        with self.assertRaisesRegex(ValueError, "identity"):
            self.controller.status()
        self.kube.node["metadata"]["uid"] = "replacement-node"
        with self.assertRaisesRegex(ValueError, "cluster or node"):
            self.controller.status()

    def test_stale_controller_cannot_erase_recorded_pod_identity(self):
        self.start()
        stale = deepcopy(self.record)
        entry = stale["backends"]["filesystem"]
        entry["podUID"] = None
        entry["uids"].pop("Job/" + JOB)
        pod = self.kube.objects[(entry["namespace"], "Pod", "stress-pod")]
        pod["metadata"]["uid"] = "replacement"
        restored = cli.Controller(self.kube, stale, clock=self.clock)
        with self.assertRaisesRegex(ValueError, "identity"):
            restored.status()

    def test_interrupted_start_stops_partial_campaign(self):
        original = self.kube.call
        interrupted = False
        def call(*args, **kwargs):
            nonlocal interrupted
            if cli.READ_PROGRESS in args and not interrupted:
                interrupted = True
                raise InterruptedError("cancel startup")
            return original(*args, **kwargs)
        with patch.object(self.kube, "call", call), self.assertRaises(InterruptedError):
            self.controller.start()
        self.assertEqual(self.record["launchStatus"], "INTERRUPTED")
        self.assertTrue(self.record["backends"]["filesystem"]["stopRequested"])

    def test_pending_job_and_lost_suspend_response_are_reconciled(self):
        self.start()
        entry = self.record["backends"]["filesystem"]
        del self.kube.objects[(entry["namespace"], "Pod", "stress-pod")]
        original = self.kube.patch
        def lost(*args):
            original(*args)
            raise TimeoutError("lost suspension reply")
        with patch.object(self.kube, "patch", lost):
            self.assertEqual(self.controller.stop(), [])
        self.assertTrue(entry["forcedStop"])

    def test_stop_requests_both_backends_before_waiting_for_drain(self):
        self.start(s3=True)
        self.kube.cooperative = False
        observed = []
        original = self.clock.sleep
        def sleep(seconds):
            requests = [call for call in self.kube.calls if cli.REQUEST_STOP in call]
            if not observed:
                observed.append(len(requests))
            original(seconds)
        self.clock.sleep = sleep
        self.assertEqual(self.controller.stop(), [])
        self.assertEqual(observed[0], 2)

    def test_changed_owned_job_and_secret_uids_are_never_deleted(self):
        self.start(s3=True)
        entry = self.record["backends"]["s3"]
        secret = self.kube.objects[(entry["namespace"], "Secret", SECRET)]
        secret["metadata"]["uid"] = "replacement-secret"
        self.finish()
        errors = self.controller.stop()
        self.assertTrue(errors)
        self.assertEqual(secret["metadata"]["uid"], "replacement-secret")
        self.assertEqual(entry["secretCleanup"], "PENDING")

    def test_terminal_states_require_every_container_and_image_identity(self):
        self.start(s3=True)
        self.finish()
        entry = self.record["backends"]["s3"]
        job, pods = self.controller.snapshot(entry)
        cli.validate_job(job, pods, "s3", entry, "test-node")
        for mutation in ("missing-helper", "restart", "oom", "missing-image", "wrong-image", "missing-container"):
            with self.subTest(mutation=mutation):
                broken = deepcopy(pods)
                statuses = broken[0]["status"]["containerStatuses"]
                if mutation == "missing-helper":
                    statuses.pop()
                elif mutation == "restart":
                    statuses[0]["restartCount"] = 1
                elif mutation == "oom":
                    statuses[0]["state"]["terminated"]["reason"] = "OOMKilled"
                elif mutation == "missing-image":
                    statuses[0]["imageID"] = ""
                elif mutation == "wrong-image":
                    statuses[0]["image"] = "other:image"
                else:
                    statuses[0]["containerID"] = ""
                with self.assertRaises(ValueError):
                    cli.validate_job(job, broken, "s3", entry, "test-node")

    def test_cooperative_stop_preserves_pods_and_forced_stop_prevents_replacement(self):
        self.start()
        self.assertEqual(self.controller.stop(), [])
        self.assertTrue(next(iter(self.record["backends"].values()))["stopRequested"])
        self.assertFalse(any(call[0] == "suspend" for call in self.kube.calls))
        entry = next(iter(self.record["backends"].values()))
        self.kube.objects[(entry["namespace"], "Pod", "stress-pod")]["status"]["phase"] = "Running"
        self.kube.cooperative = False
        self.assertEqual(self.controller.stop(), [])
        self.assertTrue(entry["forcedStop"])
        self.assertTrue(any(call[0] == "suspend" for call in self.kube.calls))

    def test_collect_refuses_live_writers_without_deleting_secrets(self):
        self.start(s3=True)
        result = self.controller.collect(self.root / "collected")
        self.assertFalse(result["success"])
        self.assertFalse(any(call[0] == "delete" for call in self.kube.calls))

    def test_collection_recovery_and_diagnostic_gaps(self):
        self.start()
        self.finish()
        self.archive()
        self.kube.failure_export = True
        output = self.root / "collected"
        first = self.controller.collect(output)
        self.assertFalse(first["success"])
        self.assertFalse((output / "filesystem/results").exists())
        self.kube.failure_export, self.kube.failure_logs = False, True
        evidence = {"evidenceStatus": "COMPLETE", "resourceAssessment": "INCONCLUSIVE"}
        with patch("stress_orchestrate.validate_directory", return_value=evidence):
            second = self.controller.collect(output)
            self.assertTrue(second["success"])
            third = self.controller.collect(output)
            self.assertTrue(third["success"])
        diagnostics = json.loads((output / "filesystem/diagnostics.json").read_text())
        self.assertFalse(diagnostics["logs"]["pagebroker"]["retrieved"])
        self.assertEqual(diagnostics["logs"]["pagebroker"]["coverage"], "TAIL_ONLY")
        self.assertFalse(any(key[1] == "Pod" and key[2] == RESULTS for key in self.kube.objects))

    def test_missing_in_pod_evidence_cannot_pass_collection(self):
        self.start()
        self.finish()
        self.archive()
        result = self.controller.collect(self.root / "collected")
        self.assertFalse(result["success"])
        self.assertEqual(result["evidenceStatus"], "INCOMPLETE")

    def test_failed_required_container_overrides_successful_client_report(self):
        self.start()
        self.finish()
        self.archive()
        entry = self.record["backends"]["filesystem"]
        pod = self.kube.objects[(entry["namespace"], "Pod", "stress-pod")]
        pod["status"]["containerStatuses"][-1]["state"]["terminated"].update(exitCode=137, reason="OOMKilled")
        evidence = {"evidenceStatus": "COMPLETE", "resourceAssessment": "INCONCLUSIVE"}
        with patch("stress_orchestrate.validate_directory", return_value=evidence):
            result = self.controller.collect(self.root / "collected")
        self.assertFalse(result["success"])
        self.assertEqual(result["executionStatus"], "ERROR")

    def test_resource_limit_failure_preserves_functional_and_evidence_outcomes(self):
        self.start()
        self.finish()
        self.archive()
        entry = self.record["backends"]["filesystem"]
        job = self.kube.objects[(entry["namespace"], "Job", JOB)]
        job["status"] = {"failed": 1, "conditions": [{"type": "Failed", "status": "True", "reason": "BackoffLimitExceeded"}]}
        pod = self.kube.objects[(entry["namespace"], "Pod", "stress-pod")]
        pod["status"]["phase"] = "Failed"
        client = next(item for item in pod["status"]["containerStatuses"] if item["name"] == "client")
        client["state"]["terminated"].update(exitCode=2, reason="Error")
        evidence = {"evidenceStatus": "COMPLETE", "resourceAssessment": "INCONCLUSIVE", "limitsExceeded": True}
        with patch("stress_orchestrate.validate_directory", return_value=evidence):
            result = self.controller.collect(self.root / "collected")
        self.assertFalse(result["success"])
        self.assertEqual(result["executionStatus"], "SUCCESS")
        self.assertEqual(result["evidenceStatus"], "COMPLETE")
        self.assertTrue(result["resourceLimitsExceeded"])
        self.assertEqual(result["resourceAssessment"], "INCONCLUSIVE")
        client["state"]["terminated"].update(exitCode=137, reason="OOMKilled")
        with self.assertRaises(ValueError):
            cli.validate_job(job, [pod], "filesystem", entry, "test-node", limit_exit=True)

    def test_lost_secret_deletion_reply_still_confirms_cleanup(self):
        self.start(s3=True)
        self.finish()
        self.kube.lost_delete = True
        self.controller.stop()
        self.assertEqual(self.record["backends"]["s3"]["secretCleanup"], "COMPLETE")

    def test_changed_configuration_and_cancelled_collection_cannot_pass(self):
        self.start()
        self.finish()
        self.archive()
        entry = self.record["backends"]["filesystem"]
        config = self.kube.objects[(entry["namespace"], "ConfigMap", CONFIG)]
        config["data"]["streamer.json"] = "{}"
        result = self.controller.collect(self.root / "changed-config")
        self.assertFalse(result["success"])
        config["data"] = deepcopy(entry["resources"][2]["data"])
        entry["stopRequested"] = True
        evidence = {"evidenceStatus": "COMPLETE", "resourceAssessment": "INCONCLUSIVE"}
        with patch("stress_orchestrate.validate_directory", return_value=evidence):
            result = self.controller.collect(self.root / "cancelled")
        self.assertFalse(result["success"])
        self.assertEqual(result["backends"]["filesystem"]["executionStatus"], "INTERRUPTED")

    def test_collection_directory_ownership_cannot_be_reused_by_another_run(self):
        self.start()
        output = self.root / "foreign"
        output.mkdir()
        (output / "collection.json").write_text(json.dumps({"campaign": "other", "owner": "other"}))
        with self.assertRaises(ValueError):
            self.controller.collect(output)

    def test_foreign_results_pvc_cannot_be_mounted_by_exporter(self):
        self.start()
        self.finish()
        entry = self.record["backends"]["filesystem"]
        claim = self.kube.objects[(entry["namespace"], "PersistentVolumeClaim", RESULTS)]
        claim["metadata"]["uid"] = "replacement-pvc"
        result = self.controller.collect(self.root / "collected")
        self.assertFalse(result["success"])
        self.assertFalse(any(call[:2] == ("create", "Pod") for call in self.kube.calls))

    def test_exporter_lost_create_response_is_recovered_and_removed(self):
        self.start()
        self.finish()
        self.archive()
        self.kube.lost_create = {"Pod"}
        evidence = {"evidenceStatus": "COMPLETE", "resourceAssessment": "INCONCLUSIVE"}
        with patch("stress_orchestrate.validate_directory", return_value=evidence):
            result = self.controller.collect(self.root / "collected")
        self.assertTrue(result["success"])
        self.assertFalse(any(k[1:] == ("Pod", RESULTS) for k in self.kube.objects))


class EndurancePackagingTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)

    def test_manifests_roundtrip_and_container_contracts(self):
        args = options(self.root, "both")
        record = cli.make_record(args, now=1000, credential_expiry=10000)
        for mode, entry in record["backends"].items():
            with self.subTest(mode=mode):
                resources = yaml.safe_load_all(yaml.safe_dump_all([*entry["resources"], entry["job"], entry["exporter"]]))
                self.assertEqual(len(list(resources)), 5)
                spec = entry["job"]["spec"]
                pod = spec["template"]["spec"]
                self.assertEqual(spec["backoffLimit"], 0)
                self.assertEqual(spec["activeDeadlineSeconds"], 40)
                self.assertEqual(pod["terminationGracePeriodSeconds"], 90)
                self.assertFalse(pod["automountServiceAccountToken"])
                self.assertEqual(pod["restartPolicy"], "Never")
                self.assertTrue(pod["shareProcessNamespace"])
                self.assertEqual([c["name"] for c in pod["containers"]], cli.roles(mode))
                for container in pod["containers"]:
                    self.assertEqual(container["imagePullPolicy"], "Never")
                    self.assertTrue(container["securityContext"]["readOnlyRootFilesystem"])
                    self.assertEqual(set(container["resources"]), {"requests", "limits"})
                    secrets = [e for e in container.get("env", []) if "valueFrom" in e]
                    self.assertEqual(bool(secrets), mode == "s3" and container["name"] == "s3-helper")
                exporter = entry["exporter"]["spec"]
                self.assertEqual(exporter["activeDeadlineSeconds"], 600)
                self.assertTrue(all(mount["readOnly"] for mount in exporter["containers"][0]["volumeMounts"]))
                if mode == "s3":
                    storage = json.loads(entry["resources"][2]["data"]["storage.json"])
                    self.assertEqual(storage["storeId"], entry["spec"]["store_id"])
                    self.assertEqual(storage["prefix"], entry["spec"]["s3"]["prefix"])
        self.assertNotIn("fake-secret", json.dumps(record))

    def test_context_is_always_explicit_and_delete_uses_uid_precondition(self):
        path = self.root / "kubeconfig"
        path.touch()
        kube = Kubernetes(path, "explicit-context")
        with patch("stress_kube.execute", return_value=b"{}") as call:
            kube.get("Job", JOB, "owned-namespace")
            kube.delete("Secret", SECRET, "owned-namespace", "expected-uid")
        for item in call.call_args_list:
            command = item.args[0]
            self.assertIn("--kubeconfig", command)
            self.assertEqual(command[command.index("--context") + 1], "explicit-context")
        self.assertEqual(json.loads(call.call_args.kwargs["input"])["preconditions"], {"uid": "expected-uid"})
        with self.assertRaises(ValueError):
            Kubernetes(path, "")

    @unittest.skipUnless(importlib.util.find_spec("kubernetes"), "requires the e2e project's Kubernetes models")
    def test_rendered_resources_deserialize_with_pinned_kubernetes_models(self):
        from kubernetes import client
        record = cli.make_record(options(self.root, "both"), now=1000, credential_expiry=10000)
        api = client.ApiClient()
        self.addCleanup(api.close)
        for entry in record["backends"].values():
            for resource in [*entry["resources"], entry["job"], entry["exporter"]]:
                model = {"Namespace": "V1Namespace", "PersistentVolumeClaim": "V1PersistentVolumeClaim",
                         "ConfigMap": "V1ConfigMap", "Job": "V1Job", "Pod": "V1Pod"}[resource["kind"]]
                decoded = api._ApiClient__deserialize(resource, model)
                serialized = api.sanitize_for_serialization(decoded)
                self.assertEqual(serialized["kind"], resource["kind"])
                self.assertEqual(serialized["metadata"]["name"], resource["metadata"]["name"])
                self.assertEqual(serialized.get("spec"), resource.get("spec"))

    def test_bounded_commands_timeout_and_redact_sensitive_errors(self):
        with self.assertRaisesRegex(ValueError, "output"):
            execute([sys.executable, "-c", "print('x'*4096)"], limit=100)
        with self.assertRaises(TimeoutError):
            execute([sys.executable, "-c", "import time; time.sleep(3)"], timeout=0.1)
        with self.assertRaises(RuntimeError) as error:
            execute([sys.executable, "-c", "import sys; sys.stderr.write('fake-secret'); sys.exit(1)"], sensitive=True)
        self.assertNotIn("fake-secret", str(error.exception))

    def test_capacity_accounts_for_restartable_init_containers(self):
        pod = {"spec": {"containers": [{"resources": {"requests": {"memory": "2Gi"}}}],
                        "initContainers": [{"restartPolicy": "Always", "resources": {"requests": {"memory": "1Gi"}}},
                                           {"resources": {"requests": {"memory": "4Gi"}}}],
                        "overhead": {"memory": "128Mi"}}}
        self.assertEqual(pod_request(pod, "memory"), 5 * 1024**3 + 128 * 1024**2)

    def test_strict_nested_archive_extraction(self):
        args = options(self.root)
        record = cli.make_record(args, now=1000)
        spec = RunSpec.from_dict(record["backends"]["filesystem"]["spec"])
        for kind in ("link", "traversal", "absolute", "duplicate", "unknown", "oversize", "directory", "hardlink"):
            with self.subTest(kind=kind):
                archive = self.root / (kind + ".tar")
                with tarfile.open(archive, "w") as output:
                    name = {"traversal": "results/../outside", "absolute": "/results/summary.json", "unknown": "results/secret.txt"}.get(kind, "results/batches/000001/client-report.json")
                    item = tarfile.TarInfo(name)
                    item.size = 0
                    if kind in ("link", "hardlink", "directory"):
                        item.type = {"link": tarfile.SYMTYPE, "hardlink": tarfile.LNKTYPE, "directory": tarfile.DIRTYPE}[kind]
                        item.linkname = "/tmp/outside"
                    if kind == "oversize":
                        item.size = 1024**2 + 1
                    output.addfile(item, io.BytesIO(b"x" * item.size))
                    if kind == "duplicate":
                        output.addfile(item, io.BytesIO())
                with self.assertRaises((ValueError, RuntimeError)):
                    extract(archive, self.root / kind, spec.evidence)

    def test_docker_copy_inputs_and_make_targets_preserve_smoke(self):
        here = Path(__file__).resolve().parent
        dockerfile = (here / "Dockerfile").read_text()
        ignore = (here / "Dockerfile.dockerignore").read_text()
        self.assertIn("FROM runtime AS stress", dockerfile)
        self.assertIn("--require-hashes", dockerfile)
        for module in ("stress.py", "stress_export.py", "stress_supervisor.py", "s3_cleanup.py", "stress_storage.py"):
            self.assertIn("!agent/pagebroker/tests/k3s/" + module, ignore)
            self.assertIn("agent/pagebroker/tests/k3s/" + module, dockerfile)
        makefile = (here.parents[3] / "Makefile").read_text()
        self.assertIn("Dockerfile --target runtime", makefile)
        self.assertIn("Dockerfile --target stress", makefile)


if __name__ == "__main__":
    unittest.main()
