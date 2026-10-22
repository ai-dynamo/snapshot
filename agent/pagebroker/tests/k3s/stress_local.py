#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Qualify packaged endurance roles against native PageBroker and loopback TLS S3."""

import argparse
import contextlib
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import re
import resource
import subprocess
import sys
import time
from types import SimpleNamespace
import uuid

from stress_config import EvidenceOptions, RunSpec, S3Options, Timing
from stress_evidence import validate_directory
from stress_export import extract
from stress_render import render_stress


def command(arguments, **kwargs):
    return subprocess.run(arguments, check=True, timeout=kwargs.pop("timeout", 60), **kwargs)


def image_identity(reference, revision):
    value = json.loads(command(["docker", "image", "inspect", reference], capture_output=True).stdout)[0]
    if value["Config"].get("Labels", {}).get("org.opencontainers.image.revision") != revision:
        raise ValueError("image source-revision label differs from the intended revision")
    if (value["Os"], value["Architecture"]) != ("linux", "amd64"):
        raise ValueError("qualification requires Linux/amd64 images")
    return {"reference": reference, "id": value["Id"], "revision": revision,
            "repoDigests": value.get("RepoDigests", [])}


def local_spec(mode, endpoint=None):
    now, run_id = time.time(), str(uuid.uuid4())
    timing = Timing(startup=90, warmup_min=1, warmup_max=240, warmup_batches=3,
                    measured=60, measured_batches=2, cadence=1, idle=12, cooldown=12,
                    batch=120, cleanup=30, drain=150, shutdown=60, margin=30, job=660)
    s3 = S3Options(bucket="pagebroker-endurance-local", prefix="local/" + run_id,
                   region="us-east-1", endpoint=endpoint, addressing="path",
                   credential_expiry=now + 86400, page_size=2) if mode == "s3" else None
    return RunSpec(run_id, mode, "/shared/pb.sock", "/staging", "/storage", "/results", "/control",
                   now, now + timing.startup, now + timing.job, timing=timing,
                   evidence=EvidenceOptions(interval=1, progress_seconds=1),
                   store_id="stress-" + run_id if s3 else "", s3=s3).validate()


def verify_results(root, spec):
    summary = json.loads((root / "summary.json").read_text())
    verdict = validate_directory(root, spec)
    if summary["executionStatus"] != "SUCCESS" or verdict["evidenceStatus"] != "COMPLETE":
        raise ValueError(f"native execution/evidence failed: {summary}; {verdict}")
    if summary["counts"]["warmup"] < 3 or summary["counts"]["measured"] < 2:
        raise ValueError("qualification requires multiple warm-up and measured batches")
    if verdict["resourceAssessment"] != "INCONCLUSIVE":
        raise ValueError("short functional qualification cannot establish resource stability")
    return summary, verdict


class LocalRun:
    def __init__(self, root, spec, harness, broker):
        self.root, self.spec = root, spec
        self.images = {"harness": harness, "broker": broker}
        self.name = "snapshot-endurance-" + spec.run_id
        self.containers = []
        self.volume = self.name + "-staging"
        self.volume_created = False

    def launch(self, role, image, arguments, mounts, environment=None, memory="1g", cpus="1"):
        name = self.name + "-" + role
        args = ["docker", "run", "--detach", "--name", name, "--label", "snapshot.endurance=" + self.spec.run_id,
                "--user", "65532:65532", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
                "--memory", memory, "--cpus", cpus, "--network", "host" if self.spec.mode == "s3" else "none"]
        if role != "anchor":
            args += ["--pid", "container:" + self.name + "-anchor"]
        for mount in mounts:
            args += ["--mount", mount]
        for key in environment or {}:
            args += ["--env", key]
        if role != "broker":
            args += ["--entrypoint", "python3"]
        # Register before creation so a lost CLI reply still leaves an owned cleanup target.
        self.containers.append(name)
        command(args + [image, *arguments], env=os.environ | (environment or {}), capture_output=True)

    def mount(self, name, readonly=False):
        return f"type=bind,source={self.root / name},target=/{name}" + (",readonly" if readonly else "")

    def execute(self, environment=None, ca=None, s3_client=None):
        self.root.mkdir()
        for name in ("config", "shared", "storage", "control", "results"):
            path = self.root / name
            path.mkdir(mode=0o777)
            path.chmod(0o777)
        resources, job, _ = render_stress(self.spec, "pagebroker-stress-" + self.spec.run_id[:8],
            str(uuid.uuid4()), str(uuid.uuid4()), self.images["harness"]["id"], self.images["broker"]["id"],
            "local", ca_pem=Path(ca).read_text() if ca else None)
        for name, data in resources[2]["data"].items():
            (self.root / "config" / name).write_text(data)
        command(["docker", "volume", "create", "--label", "snapshot.endurance=" + self.spec.run_id,
                 "--driver", "local", "--opt", "type=tmpfs", "--opt", "device=tmpfs", "--opt",
                 "o=size=1g,uid=65532,gid=65532,mode=0770", self.volume], capture_output=True)
        self.volume_created = True
        try:
            self.launch("anchor", self.images["harness"]["id"], ["-c", f"import time; time.sleep({self.spec.timing.job + 120})"], [])
            roles = job["spec"]["template"]["spec"]["containers"]
            for role in roles:
                mounts = []
                for mount in role["volumeMounts"]:
                    target = mount["mountPath"].lstrip("/")
                    if target == "staging":
                        mounts.append(f"type=volume,source={self.volume},target=/staging")
                    else:
                        mounts.append(self.mount(target, mount.get("readOnly", False)))
                env = {item["name"]: item.get("value", (environment or {}).get(item["name"])) for item in role.get("env", [])}
                if any(value is None for value in env.values()):
                    raise ValueError("local fixture is missing a required credential environment variable")
                image = self.images["broker" if role["name"] == "pagebroker" else "harness"]["id"]
                name = "broker" if role["name"] == "pagebroker" else role["name"]
                limits = role["resources"]["limits"]
                memory = {"3Gi": "3g", "1Gi": "1g", "256Mi": "256m", "512Mi": "512m"}[limits["memory"]]
                cpu = limits["cpu"]
                self.launch(name, image, ([] if name == "broker" else role["command"][1:]) + role["args"],
                            mounts, env, memory, str(float(cpu[:-1]) / 1000) if cpu.endswith("m") else cpu)
            self.await_exit()
            states = self.states()
            if any(state["Running"] or state["ExitCode"] != 0 or state["OOMKilled"] for state in states.values()):
                raise ValueError(f"containers did not all shut down cleanly: {states}")
            exported = self.export_results()
            summary, verdict = verify_results(exported, self.spec)
            staging = self.inspect_staging()
            if staging["files"] or staging["usedBytes"] != 0:
                raise ValueError(f"staging retained checkpoint contents: {staging}")
            remote = None
            if s3_client:
                prefix = self.spec.s3.prefix + "/"
                inventory = s3_client.list_object_versions(Bucket=self.spec.s3.bucket, Prefix=prefix, MaxKeys=10)
                uploads = s3_client.list_multipart_uploads(Bucket=self.spec.s3.bucket, Prefix=prefix, MaxUploads=10)
                versions = inventory.get("Versions", [])
                if (inventory.get("IsTruncated") or inventory.get("DeleteMarkers") or len(versions) != 1
                        or versions[0]["Key"] != prefix + ".snapshot-stress-owner.json"
                        or uploads.get("Uploads") or uploads.get("IsTruncated")):
                    raise ValueError("independent S3 inspection found retained artifacts/versions/uploads")
                remote = {"objects": 1, "artifacts": 0, "multipartUploads": 0, "ownerMarkerRetained": True}
            result = {"executionStatus": summary["executionStatus"], "independentEvidence": verdict,
                      "images": self.images, "runId": self.spec.run_id, "mode": self.spec.mode,
                      "counts": summary["counts"], "states": states, "staging": staging, "s3": remote}
            (self.root / "qualification.json").write_text(json.dumps(result, indent=2) + "\n")
            return result
        finally:
            self.close()

    def states(self):
        return {name.rsplit("-", 1)[-1]: json.loads(command(
            ["docker", "inspect", "--format", "{{json .State}}", name], capture_output=True).stdout)
                for name in self.containers if not name.endswith("-anchor")}

    def await_exit(self):
        deadline = time.monotonic() + self.spec.timing.job + 30
        while time.monotonic() < deadline:
            states = self.states()
            if not any(state["Running"] for state in states.values()):
                return
            if any(not state["Running"] and state["ExitCode"] != 0 for state in states.values()):
                raise RuntimeError("an endurance role failed; see captured role logs and evidence")
            time.sleep(1)
        raise TimeoutError("local qualification exceeded its hard deadline")

    def inspect_staging(self):
        script = ("import json,os; from pathlib import Path; s=os.statvfs('/staging'); "
                  "print(json.dumps({'files':[str(p) for p in Path('/staging').rglob('*') if p.is_file()],"
                  "'usedBytes':(s.f_blocks-s.f_bfree)*s.f_frsize}))")
        return json.loads(command(["docker", "run", "--rm", "--network", "none", "--read-only", "--cap-drop", "ALL",
            "--user", "65532:65532", "--mount", f"type=volume,source={self.volume},target=/staging,readonly",
            "--entrypoint", "python3", self.images["harness"]["id"], "-c", script], capture_output=True).stdout)

    def export_results(self):
        options = self.spec.evidence
        limit = options.ordinary_bytes + options.terminal_bytes + options.max_files * 2048 + 10240
        def bound_archive():
            resource.setrlimit(resource.RLIMIT_FSIZE, (limit, limit))
        name = self.name + "-exporter"
        self.containers.append(name)
        archive = self.root / "results.tar"
        with archive.open("xb") as output:
            command(["docker", "run", "--rm", "--name", name, "--label", "snapshot.endurance=" + self.spec.run_id,
                "--network", "none", "--read-only", "--cap-drop", "ALL", "--user", "65532:65532",
                "--mount", self.mount("results", True), "--mount", self.mount("config", True),
                "--entrypoint", "python3", self.images["harness"]["id"], "/opt/stress/stress_export.py",
                "--spec", "/config/run-spec.json", "export"], stdout=output, stderr=subprocess.PIPE, preexec_fn=bound_archive)
        destination = self.root / "exported"
        extract(archive, destination, options)
        return destination

    def close(self):
        for name in reversed(self.containers):
            with (self.root / (name.removeprefix(self.name + "-") + ".log")).open("wb") as output:
                subprocess.run(["docker", "logs", "--tail", "200", name], stdout=output, stderr=subprocess.STDOUT, timeout=15)
            # Only exact, randomly named containers bearing this run's ownership label are removable.
            owner = subprocess.run(["docker", "inspect", "--format", '{{index .Config.Labels "snapshot.endurance"}}', name],
                                   capture_output=True, timeout=15)
            if owner.returncode == 0 and owner.stdout.decode().strip() == self.spec.run_id:
                subprocess.run(["docker", "rm", "--force", name], capture_output=True, timeout=30, check=True)
        if self.volume_created:
            command(["docker", "volume", "rm", self.volume], capture_output=True)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("client-image", "pagebroker-image", "harness-revision", "broker-revision", "output"):
        parser.add_argument("--" + name, required=True)
    parser.add_argument("--backend", choices=("both", "filesystem", "s3"), default="both")
    args = parser.parse_args(argv)
    for revision in (args.harness_revision, args.broker_revision):
        if not re.fullmatch("[a-f0-9]{40}", revision):
            parser.error("source revisions must be full Git commit IDs")
    root = Path(args.output).resolve()
    root.mkdir(exist_ok=False)
    harness = image_identity(args.client_image, args.harness_revision)
    broker = image_identity(args.pagebroker_image, args.broker_revision)
    results = {}
    if args.backend in ("both", "filesystem"):
        print("RUNNING native filesystem qualification", flush=True)
        results["filesystem"] = LocalRun(root / "filesystem", local_spec("filesystem"), harness, broker).execute()
    if args.backend in ("both", "s3"):
        sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
        from s3_integration import create_session, local_environment, local_service
        environment = local_environment()
        with local_service(SimpleNamespace(tls=True), environment, service_factory=lambda app: app):
            spec = local_spec("s3", environment["AWS_ENDPOINT_URL"])
            environment.update(AWS_SESSION_TOKEN="local-test-session-token", AWS_CREDENTIAL_EXPIRATION=
                               datetime.fromtimestamp(spec.s3.credential_expiry, timezone.utc).isoformat())
            with contextlib.closing(create_session(environment).client("s3", endpoint_url=spec.s3.endpoint,
                    verify=environment["AWS_CA_BUNDLE"])) as client:
                client.create_bucket(Bucket=spec.s3.bucket)
                print("RUNNING native TLS-emulated S3 qualification", flush=True)
                results["s3"] = LocalRun(root / "s3", spec, harness, broker).execute(
                    environment, environment["AWS_CA_BUNDLE"], client)
    (root / "qualification.json").write_text(json.dumps(results, indent=2) + "\n")
    print(f"SUCCESS: {root / 'qualification.json'}", flush=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())
