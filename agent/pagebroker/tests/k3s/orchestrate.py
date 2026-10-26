#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Run filesystem, S3 and both Qwen smoke cases on an already prepared k3s cluster."""

import argparse
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import time
from types import SimpleNamespace
import uuid

import yaml

import evidence
import render
from run import write_json


REPO = Path(__file__).resolve().parents[4]
CASES = ("filesystem", "s3", *evidence.QWEN_CASES)
EXPORT = """
import pathlib,sys,tarfile
with tarfile.open(fileobj=sys.stdout.buffer, mode='w|') as archive:
    for name in ('summary.json','client-report.json','client.log','resources.jsonl'):
        path=pathlib.Path('/results')/name
        if path.exists():
            archive.add(path, arcname='results/'+name, recursive=False)
"""
CLEAN_QWEN = """
import json,sys
from snapshot_e2e import k8s,lifecycle,pagebroker_qwen
from snapshot_e2e.workloads import TestRun
owner=json.load(open(sys.argv[1]))
config=k8s.E2EConfig.from_env()
assert owner['namespace']==config.namespace
run=TestRun(**owner['run'])
assert run.suffix.startswith('pagebroker-qwen-')
k8s.configure(config)
try:
    lifecycle.cleanup(config,run)
finally:
    pagebroker_qwen.delete_app(config,run)
for name in (run.source_pod,run.restore_pod):
    lifecycle.wait_for_pod_deleted(config.namespace,name,timeout=60)
"""


def now():
    return datetime.now(timezone.utc).isoformat()


def execute(command, *, timeout=40, input=None, output=None, env=None, grace=5, sensitive=False):
    process = subprocess.Popen(command, cwd=REPO, env=env, stdin=subprocess.PIPE if input else subprocess.DEVNULL,
                               stdout=output or subprocess.PIPE, stderr=subprocess.PIPE if sensitive else subprocess.STDOUT,
                               start_new_session=True)
    try:
        stdout, _ = process.communicate(input=input, timeout=timeout)
    except BaseException:
        # Give pytest an interrupt first so fixture cleanup can run before forced termination.
        try:
            os.killpg(process.pid, signal.SIGINT if grace > 5 else signal.SIGTERM)
        except ProcessLookupError:
            pass
        try:
            process.wait(timeout=grace)
        except subprocess.TimeoutExpired:
            pass
        finally:
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            process.wait(timeout=5)
        raise
    finally:
        for stream in (process.stdin, process.stdout, process.stderr):
            if stream is not None:
                stream.close()
    if process.returncode:
        detail = "" if sensitive or output else (stdout or b"").decode(errors="replace")[-2000:]
        raise RuntimeError(f"{command[0]} exited {process.returncode}" + (f": {detail}" if detail else ""))
    return stdout or b""


class Kubernetes:
    def __init__(self, args):
        self.command = ["kubectl", "--kubeconfig", str(args.kubeconfig), "--context", args.context,
                        "--request-timeout=20s"]

    def call(self, *args, **kwargs):
        return execute(self.command + list(args), **kwargs)

    def get(self, namespace, *args):
        return json.loads(self.call("-n", namespace, "get", *args, "-o", "json"))

    def create(self, resource):
        return self.call("create", "-f", "-", input=json.dumps(resource).encode())


def wait_job(kube, namespace, directory, startup_seconds, job_seconds):
    started = time.monotonic()
    observed = False
    while True:
        job = kube.get(namespace, "job", "pagebroker-smoke")
        pods = kube.get(namespace, "pods", "-l", "job-name=pagebroker-smoke")["items"]
        write_json(directory / "job.json", job)
        write_json(directory / "pods.json", {"items": pods})
        if len(pods) > 1:
            raise RuntimeError("Job created more than one Pod")
        for pod in pods:
            observed |= len(pod["status"].get("containerStatuses", [])) == 2 and all(
                item.get("state", {}).get("running") or item.get("state", {}).get("terminated")
                for item in pod["status"]["containerStatuses"])
            for item in pod["status"].get("containerStatuses", []):
                end = item.get("state", {}).get("terminated", {})
                if item.get("restartCount", 0) or end.get("exitCode", 0) or end.get("reason") == "OOMKilled":
                    raise RuntimeError(f"{item['name']} failed or restarted")
        if evidence.condition(job, "Failed") or evidence.condition(job, "FailureTarget"):
            raise RuntimeError("Job reported failure")
        if evidence.condition(job, "Complete"):
            return evidence.validate_job(job, pods)
        elapsed = time.monotonic() - started
        if not observed and elapsed >= startup_seconds:
            raise TimeoutError("Job containers did not start before the readiness deadline")
        if elapsed >= job_seconds:
            raise TimeoutError("Job did not complete before the execution deadline")
        time.sleep(2)


def attempt(errors, label, action):
    try:
        return action()
    except Exception as error:
        errors.append(f"{label}: {error}")
        return None


def collect_job(kube, namespace, directory, exporter, errors):
    job = attempt(errors, "read final Job", lambda: kube.get(namespace, "job", "pagebroker-smoke"))
    if job is not None:
        write_json(directory / "final-job.json", job)
    pods = attempt(errors, "read final Pods", lambda: kube.get(namespace, "pods", "-l", "job-name=pagebroker-smoke"))
    if pods is not None:
        write_json(directory / "final-pods.json", pods)
        for pod in pods["items"]:
            for container in ("client", "pagebroker"):
                with (directory / f"{pod['metadata']['name']}-{container}.log").open("wb") as output:
                    attempt(errors, f"{container} logs", lambda: kube.call("-n", namespace, "logs", pod["metadata"]["name"],
                            "-c", container, "--limit-bytes=16777216", output=output))
    with (directory / "events.json").open("wb") as output:
        attempt(errors, "events", lambda: kube.call("-n", namespace, "get", "events", "-o", "json", output=output))
    # Stop unfinished work before mounting its RWO results volume in the exporter.
    terminal = job is not None and (evidence.condition(job, "Complete") or evidence.condition(job, "Failed"))
    if not terminal or pods is None or any(pod["status"].get("phase") not in ("Succeeded", "Failed") for pod in pods["items"]):
        kube.call("-n", namespace, "delete", "job", "pagebroker-smoke", "--ignore-not-found",
                  "--cascade=foreground", "--wait=true", "--timeout=120s", timeout=150)
    try:
        kube.create(exporter)
        kube.call("-n", namespace, "wait", "pod/pagebroker-smoke-results", "--for=condition=Ready",
                  "--timeout=180s", timeout=210)
        with (directory / "results.tar").open("xb") as output:
            kube.call("-n", namespace, "exec", "pagebroker-smoke-results", "--", "python3", "-c", EXPORT,
                      output=output, timeout=180, sensitive=True)
        evidence.extract_results(directory / "results.tar", directory / "results")
    finally:
        attempt(errors, "stop exporter", lambda: kube.call("-n", namespace, "delete", "pod", "pagebroker-smoke-results",
                "--ignore-not-found", "--wait=true", "--timeout=60s", timeout=90))


def synthetic(kube, args, mode, run_id, directory, result):
    namespace = f"pagebroker-smoke-{run_id}-{mode}"
    result["namespace"] = namespace
    values = SimpleNamespace(**vars(args), mode=mode, namespace=namespace, credentials_secret="pagebroker-smoke-aws")
    resources, job, exporter = render.render(values)
    for name, docs in (("resources.yaml", resources), ("job.yaml", [job]), ("export.yaml", [exporter])):
        (directory / name).write_text(yaml.safe_dump_all(docs, sort_keys=False))
    if mode == "s3":
        result["s3"] = {key: render.storage_config(values)[key] for key in ("bucket", "prefix", "storeId")}
    owned = False
    try:
        kube.create(resources[0])
        owned = True
        for resource in resources[1:]:
            kube.create(resource)
        if mode == "s3":
            kube.call("-n", namespace, "create", "secret", "generic", values.credentials_secret,
                      f"--from-file=credentials={os.environ['PAGEBROKER_SMOKE_AWS_CREDENTIALS_FILE']}", sensitive=True)
        kube.create(job)
        result.update(wait_job(kube, namespace, directory, args.startup_seconds, args.job_seconds))
    except Exception as error:
        result["errors"].append(str(error))
    finally:
        if owned:
            attempt(result["errors"], "collect Job evidence", lambda: collect_job(kube, namespace, directory, exporter, result["errors"]))
            if mode == "s3":
                attempt(result["errors"], "remove credential Secret", lambda: kube.call("-n", namespace, "delete", "secret",
                        values.credentials_secret, "--ignore-not-found", "--wait=false", sensitive=True))
    details = attempt(result["errors"], "validate exported reports",
                      lambda: evidence.validate_synthetic(directory / "results", mode, namespace))
    if details:
        result.update(details)


def qwen_environment(args, directory, run_id, case):
    env = dict(os.environ)
    for name in ("PYTEST_ADDOPTS", "PYTEST_PLUGINS", "SNAPSHOT_E2E_TARGET_KUBECONFIG", "GITHUB_REPOSITORY"):
        env.pop(name, None)
    env.update(KUBECONFIG=str(args.kubeconfig), SNAPSHOT_E2E_KUBE_CONTEXT=args.context,
               SNAPSHOT_E2E_MODE="direct", SNAPSHOT_E2E_TEST_NAMESPACE=args.snapshot_namespace,
               SNAPSHOT_E2E_SNAPSHOT_RELEASE=args.snapshot_release, SNAPSHOT_E2E_PVC_NAME=args.snapshot_pvc,
               SNAPSHOT_E2E_WORKLOAD_IMAGE=args.agent_image, SNAPSHOT_E2E_PAGEBROKER_QWEN="1",
               SNAPSHOT_E2E_REQUIRED_TESTS=f"test_pagebroker_qwen_repeated_restore[{case}]",
               SNAPSHOT_E2E_BENCHMARK_DIR=str(directory / "benchmarks"),
               SNAPSHOT_E2E_PAGEBROKER_QWEN_OWNERSHIP=str(directory / "ownership.json"),
               PYTEST_DISABLE_PLUGIN_AUTOLOAD="1", GITHUB_RUN_ID=run_id, GITHUB_RUN_ATTEMPT="1")
    return env


def preflight(kube, args):
    daemonset = kube.get(args.snapshot_namespace, "daemonset", f"{args.snapshot_release}-agent")
    status = daemonset["status"]
    desired = status.get("desiredNumberScheduled", 0)
    if (not desired or status.get("numberReady") != desired or status.get("updatedNumberScheduled") != desired
            or status.get("observedGeneration", 0) < daemonset["metadata"]["generation"]):
        raise ValueError("Snapshot agent DaemonSet is not fully rolled out and ready")
    containers = daemonset["spec"]["template"]["spec"]["containers"]
    images = {item["name"]: item["image"].removeprefix("docker.io/") for item in containers}
    for name, expected in (("agent", args.agent_image), ("pagebroker", args.pagebroker_image)):
        if images.get(name) != expected.removeprefix("docker.io/"):
            raise ValueError(f"installed {name} image differs from the requested image")
    if kube.get(args.snapshot_namespace, "pvc", args.snapshot_pvc)["status"].get("phase") != "Bound":
        raise ValueError("Snapshot checkpoint PVC is not Bound")
    return images


def qwen(kube, args, case, run_id, directory, result, broker_image):
    env = qwen_environment(args, directory, run_id, case)
    command = [sys.executable, "-m", "pytest", f"e2e/tests/test_pagebroker_qwen.py::test_pagebroker_qwen_repeated_restore[{case}]",
               "-vv", "-s", "-o", "addopts=", f"--junitxml={directory / 'junit.xml'}"]
    try:
        with (directory / "pytest.log").open("wb") as output:
            execute(command, env=env, output=output, timeout=args.qwen_seconds, grace=120)
    except Exception as error:
        result["errors"].append(f"Qwen execution: {error}")
    finally:
        if (directory / "ownership.json").exists():
            with (directory / "cleanup.log").open("wb") as output:
                attempt(result["errors"], "Qwen cleanup", lambda: execute(
                    [sys.executable, "-c", CLEAN_QWEN, str(directory / "ownership.json")],
                    env=env, output=output, timeout=300))
        else:
            result["errors"].append("Qwen did not record resource ownership")
        for resource in ("pods", "events"):
            with (directory / f"{resource}.json").open("wb") as output:
                attempt(result["errors"], f"Qwen {resource}", lambda: kube.call(
                    "-n", args.snapshot_namespace, "get", resource, "-o", "json", output=output))
        with (directory / "snapshot.log").open("wb") as output:
            attempt(result["errors"], "Snapshot logs", lambda: kube.call(
                "-n", args.snapshot_namespace, "logs", "-l", f"app.kubernetes.io/instance={args.snapshot_release}",
                "--all-containers=true", "--prefix=true", "--tail=-1", f"--since-time={result['started']}",
                "--limit-bytes=16777216", output=output, timeout=60))
    details = attempt(result["errors"], "validate Qwen evidence",
                      lambda: evidence.validate_qwen(directory, case, run_id, broker_image))
    if details:
        result.update(details)


def run_suite(args):
    args.output.mkdir(parents=True, exist_ok=False)
    run_id = datetime.now(timezone.utc).strftime("%Y%m%d%H%M%S") + "-" + uuid.uuid4().hex[:8]
    summary = {"schemaVersion": 1, "runId": run_id, "status": "RUNNING", "started": now(), "context": args.context,
               "requiredCases": list(CASES), "leakAssessment": "NOT_ASSESSED", "errors": [],
               "cases": {name: {"status": "NOT_RUN", "errors": []} for name in CASES}}
    path = args.output / "summary.json"
    write_json(path, summary)
    try:
        kube = Kubernetes(args)
        summary["sourceRevision"] = execute(["git", "rev-parse", "HEAD"]).decode().strip()
        with (args.output / "nodes.json").open("wb") as output:
            kube.call("get", "nodes", "-o", "json", output=output)
        summary["installedImages"] = preflight(kube, args)
        broker_image = None
        for case in CASES:
            result = summary["cases"][case]
            result.update(status="RUNNING", started=now())
            directory = args.output / case
            directory.mkdir()
            write_json(path, summary)
            print(f"RUNNING {case}: {directory}", flush=True)
            try:
                if case in ("filesystem", "s3"):
                    synthetic(kube, args, case, run_id, directory, result)
                    if case == "filesystem":
                        broker_image = result.get("images", {}).get("pagebroker")
                    elif broker_image and result.get("images"):
                        if evidence.image_digest(result["images"]["pagebroker"]) != evidence.image_digest(broker_image):
                            raise ValueError("S3 used a different PageBroker image from the filesystem Job")
                else:
                    qwen(kube, args, case, run_id, directory, result, broker_image)
            except (Exception, KeyboardInterrupt) as error:
                result["errors"].append(str(error) or type(error).__name__)
                if isinstance(error, KeyboardInterrupt):
                    raise
            finally:
                result.update(status="ERROR" if result["errors"] else "SUCCESS", finished=now())
                write_json(path, summary)
            print(f"{result['status']} {case}", flush=True)
    except (Exception, KeyboardInterrupt) as error:
        summary["errors"].append(str(error) or type(error).__name__)
        for result in summary["cases"].values():
            if result["status"] == "RUNNING":
                result.update(status="ERROR", errors=result["errors"] + ["runner interrupted"])
    finally:
        success = not summary["errors"] and all(case["status"] == "SUCCESS" for case in summary["cases"].values())
        summary.update(status="SUCCESS" if success else "ERROR", finished=now())
        write_json(path, summary)
        print(f"{summary['status']}: {path}", flush=True)
    return 0 if summary["status"] == "SUCCESS" else 1


def interrupted(signum, _frame):
    raise KeyboardInterrupt(f"runner received signal {signum}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--kubeconfig", type=Path, required=True, help="One existing kubeconfig file; never copied into results")
    parser.add_argument("--context", required=True)
    parser.add_argument("--output", type=Path, required=True, help="New local results directory")
    parser.add_argument("--pagebroker-image", required=True)
    parser.add_argument("--client-image", required=True)
    parser.add_argument("--agent-image", required=True, help="Agent image already deployed with the Snapshot release")
    parser.add_argument("--bucket", required=True)
    parser.add_argument("--region", required=True)
    parser.add_argument("--endpoint")
    parser.add_argument("--prefix", default="pagebroker-smoke")
    parser.add_argument("--storage-class", default="local-path")
    parser.add_argument("--image-pull-policy", choices=("Never", "IfNotPresent", "Always"), default="Never")
    parser.add_argument("--node", help="Hostname label for synthetic Jobs; Qwen uses the installed release")
    parser.add_argument("--snapshot-namespace", default="snapshot")
    parser.add_argument("--snapshot-release", default="snapshot")
    parser.add_argument("--snapshot-pvc", default="snapshot-pvc")
    parser.add_argument("--sample-interval", type=float, default=1)
    parser.add_argument("--settle-seconds", type=float, default=5)
    parser.add_argument("--startup-seconds", type=int, default=300)
    parser.add_argument("--job-seconds", type=int, default=1920)
    parser.add_argument("--qwen-seconds", type=int, default=3600, help="Deadline per Qwen model, excluding bounded cleanup")
    args = parser.parse_args()
    try:
        args.kubeconfig = args.kubeconfig.expanduser().resolve(strict=True)
        args.output = args.output.expanduser().absolute()
        credentials = Path(os.environ["PAGEBROKER_SMOKE_AWS_CREDENTIALS_FILE"]).expanduser().resolve(strict=True)
        os.environ["PAGEBROKER_SMOKE_AWS_CREDENTIALS_FILE"] = str(credentials)
        if not credentials.is_file() or not args.kubeconfig.is_file():
            raise ValueError("kubeconfig and credentials must be existing files")
        if min(args.startup_seconds, args.job_seconds, args.qwen_seconds) <= 0:
            raise ValueError("timeouts must be positive")
        render.render(SimpleNamespace(**vars(args), mode="s3", namespace="pagebroker-smoke-preflight",
                                      credentials_secret="pagebroker-smoke-aws"))
    except (ValueError, OSError, KeyError) as error:
        parser.error(str(error))
    signal.signal(signal.SIGTERM, interrupted)
    try:
        return run_suite(args)
    except OSError as error:
        parser.error(str(error))


if __name__ == "__main__":
    sys.exit(main())
