# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

import copy
import json
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
from types import SimpleNamespace as NS
import unittest
from unittest.mock import patch

import evidence
import orchestrate as runner
import run
from test_run import report


IMAGE = "sha256:" + "a" * 64


def completed_job():
    job = {"metadata": {"uid": "job-uid"}, "status": {"conditions": [{"type": "Complete", "status": "True"}]}}
    pod = {"metadata": {"uid": "pod-uid", "name": "pagebroker-smoke-fixture",
                       "ownerReferences": [{"uid": "job-uid", "controller": True}]},
           "spec": {"nodeName": "node"}, "status": {"phase": "Succeeded", "containerStatuses": [
               {"name": name, "restartCount": 0, "imageID": IMAGE,
                "state": {"terminated": {"exitCode": 0, "reason": "Completed"}}} for name in ("client", "pagebroker")]}}
    return job, [pod]


def synthetic_files(directory, mode="filesystem", store="fixture"):
    directory.mkdir(parents=True)
    client = report(mode)
    client["config"]["storeId"] = store
    for item in client["publications"]:
        item["artifact"]["store_id"] = store
    run.write_json(directory / "client-report.json", client)
    phases = ["baseline", "workload", "after-client", "settled"]
    records = [{"status": "SUCCESS", "pid": 42, "processStartTicks": 99, "elapsedSeconds": i,
                "phase": phase, "metrics": {key: 10 + i for key in evidence.METRICS}} for i, phase in enumerate(phases)]
    (directory / "resources.jsonl").write_text("".join(json.dumps(row) + "\n" for row in records))
    resources = {"status": "SUCCESS", "error": None, "pid": 42, "processStartTicks": 99, "sampleCount": 4,
                 "stagingIsMountPoint": True, "durationSeconds": 3, "phases": dict.fromkeys(phases, 1),
                 "metrics": {key: {"first": 10, "last": 13, "peak": 13, "delta": 3} for key in evidence.METRICS}}
    run.write_json(directory / "summary.json", {"status": "SUCCESS", "errors": [], "clientExitCode": 0,
                   "pagebrokerShutdown": "SIGTERM", "mode": mode, "clientRunId": client["runId"],
                   "pagebrokerPid": 42, "stepsVerified": len(client["steps"]),
                   "publicationsVerified": len(client["publications"]), "resources": resources})
    (directory / "client.log").write_text("PASS\n")
    return resources, records


def qwen_files(directory, case, run_id):
    benchmarks = directory / "benchmarks"
    benchmarks.mkdir()
    value = {"identity": {"suite": "pagebroker-qwen-smoke", "case": case, "runId": run_id}, "outcome": "passed",
             "environment": {"completedRestores": 2, "expectedRestores": 2, "storageType": "pvc",
                             "restoreMode": "staged", "transferEngine": "posix-copy", "pagebrokerImage": IMAGE},
             "events": [{"name": name} for name in ("checkpoint.ready", "restore.1.inference_verified", "restore.2.inference_verified")]}
    run.write_json(benchmarks / "case.json", value)
    (directory / "junit.xml").write_text(
        f'<testsuites><testsuite><testcase name="test_pagebroker_qwen_repeated_restore[{case}]"/></testsuite></testsuites>')
    return value


class EvidenceTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)

    def test_job_requires_owned_single_pod_and_clean_both_containers(self):
        job, pods = completed_job()
        self.assertEqual(evidence.validate_job(job, pods)["images"]["pagebroker"], IMAGE)
        for mutation in ("incomplete", "failed", "retry", "missing", "restarted", "oom", "unowned", "image"):
            with self.subTest(mutation=mutation), self.assertRaises(ValueError):
                broken, items = copy.deepcopy((job, pods))
                if mutation == "incomplete":
                    broken["status"]["conditions"] = []
                elif mutation == "failed":
                    broken["status"]["failed"] = 1
                elif mutation == "retry":
                    items.append(items[0])
                elif mutation == "missing":
                    items[0]["status"]["containerStatuses"].pop()
                elif mutation == "restarted":
                    items[0]["status"]["containerStatuses"][0]["restartCount"] = 1
                elif mutation == "oom":
                    items[0]["status"]["containerStatuses"][0]["state"]["terminated"]["reason"] = "OOMKilled"
                elif mutation == "unowned":
                    items[0]["metadata"]["ownerReferences"] = []
                else:
                    items[0]["status"]["containerStatuses"][0]["imageID"] = ""
                evidence.validate_job(broken, items)

    def test_samples_must_match_process_phases_metrics_and_aggregates(self):
        directory = self.root / "results"
        resources, records = synthetic_files(directory)
        self.assertEqual(evidence.validate_synthetic(directory, "filesystem", "")["steps"], 11)
        path = directory / "resources.jsonl"
        for mutation in ("pid", "missing-metric", "no-idle", "nonfinite", "reordered", "aggregate", "failed"):
            with self.subTest(mutation=mutation), self.assertRaises(ValueError):
                broken = copy.deepcopy(records)
                if mutation == "pid":
                    broken[-1]["pid"] += 1
                elif mutation == "missing-metric":
                    broken[0]["metrics"].pop("rssBytes")
                elif mutation == "no-idle":
                    broken.pop()
                elif mutation == "nonfinite":
                    broken[-1]["elapsedSeconds"] = float("nan")
                elif mutation == "reordered":
                    broken.reverse()
                elif mutation == "aggregate":
                    broken[0]["metrics"]["rssBytes"] += 1
                else:
                    broken[-1]["status"] = "ERROR"
                path.write_text("".join(json.dumps(row) + "\n" for row in broken))
                evidence.validate_resources(path, resources)

    def test_export_rejects_links_traversal_duplicates_and_oversized_members(self):
        for mutation in ("traversal", "symlink", "duplicate", "large"):
            with self.subTest(mutation=mutation):
                archive = self.root / f"{mutation}.tar"
                with tarfile.open(archive, "w") as output:
                    member = tarfile.TarInfo("../escaped" if mutation == "traversal" else "results/summary.json")
                    member.size = 0
                    if mutation == "symlink":
                        member.type, member.linkname = tarfile.SYMTYPE, "/etc/passwd"
                    if mutation == "large":
                        member.size = 64 * 1024**2 + 1
                    output.addfile(member)
                    if mutation == "duplicate":
                        output.addfile(member)
                with self.assertRaises(ValueError):
                    evidence.extract_results(archive, self.root / mutation)
        self.assertFalse((self.root / "escaped").exists())

    def test_qwen_requires_exact_case_junit_and_benchmark(self):
        case = evidence.QWEN_CASES[0]
        report_value = qwen_files(self.root, case, "run")
        self.assertEqual(evidence.validate_qwen(self.root, case, "run", IMAGE)["environment"]["completedRestores"], 2)
        for mutation in ("restore", "stale", "image", "skipped", "events"):
            with self.subTest(mutation=mutation), self.assertRaises(ValueError):
                broken = copy.deepcopy(report_value)
                if mutation == "restore":
                    broken["environment"]["completedRestores"] = 1
                elif mutation == "stale":
                    broken["identity"]["runId"] = "old"
                elif mutation == "image":
                    broken["environment"]["pagebrokerImage"] = "sha256:" + "b" * 64
                elif mutation == "skipped":
                    broken["outcome"] = "skipped"
                else:
                    broken["events"].pop()
                run.write_json(self.root / "benchmarks/case.json", broken)
                evidence.validate_qwen(self.root, case, "run", IMAGE)
        run.write_json(self.root / "benchmarks/case.json", report_value)
        for xml in ('<testsuites/>', f'<testsuites><testcase name="test_pagebroker_qwen_repeated_restore[{case}]"><skipped/></testcase></testsuites>'):
            (self.root / "junit.xml").write_text(xml)
            with self.assertRaises(ValueError):
                evidence.validate_qwen(self.root, case, "run", IMAGE)


class FakeKubernetes:
    def __init__(self, root, failure=None):
        self.root, self.failure = root, failure
        self.actions = []
        self.created = []
        self.namespaces = {}

    def create(self, resource):
        self.actions.append(("create", resource["kind"]))
        if resource["kind"] == "Namespace":
            if self.failure == "namespace-conflict":
                raise RuntimeError("AlreadyExists")
            self.namespaces[resource["metadata"]["name"]] = resource
        self.created.append(resource)

    def get(self, namespace, *args):
        if args[0] == "daemonset":
            return {"metadata": {"generation": 1},
                    "status": {"desiredNumberScheduled": 1, "numberReady": 1, "updatedNumberScheduled": 1, "observedGeneration": 1},
                    "spec": {"template": {"spec": {"containers": [
                        {"name": name, "image": f"local/{name}:fixture"} for name in ("agent", "pagebroker")]}}}}
        if args[0] == "pvc":
            return {"status": {"phase": "Bound"}}
        job, pods = completed_job()
        if self.failure == "job-failed" and namespace.endswith("s3"):
            job["status"]["conditions"] = [{"type": "Failed", "status": "True"}]
        if self.failure == "timeout":
            job["status"]["conditions"] = []
            pods[0]["status"] = {"phase": "Pending", "containerStatuses": []}
        return job if args[0] == "job" else {"items": pods}

    def call(self, *args, output=None, **kwargs):
        self.actions.append(args)
        if "exec" in args:
            if self.failure == "export":
                raise RuntimeError("export failed")
            namespace = args[1]
            mode = "s3" if namespace.endswith("s3") else "filesystem"
            directory = self.root / namespace
            synthetic_files(directory, mode, namespace)
            with tarfile.open(fileobj=output, mode="w") as archive:
                for path in directory.iterdir():
                    archive.add(path, arcname="results/" + path.name)
        elif output:
            output.write(b"{}\n")
        return b""


class OrchestrationTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.args = NS(output=self.root / "output", kubeconfig=self.root / "kubeconfig", context="fixture-context",
                       client_image="local/client:fixture", pagebroker_image="local/pagebroker:fixture",
                       agent_image="local/agent:fixture", bucket="fake-bucket", region="us-east-2", endpoint=None,
                       prefix="test", storage_class="local-path", image_pull_policy="Never", node=None,
                       sample_interval=1, settle_seconds=5, snapshot_namespace="snapshot", snapshot_release="snapshot",
                       snapshot_pvc="snapshot-pvc", startup_seconds=300, job_seconds=1920, qwen_seconds=3600)
        self.commands = []

    def execute(self, command, env=None, **kwargs):
        self.commands.append((command, env))
        if "pytest" in command:
            directory = Path(env["SNAPSHOT_E2E_BENCHMARK_DIR"]).parent
            case = env["SNAPSHOT_E2E_REQUIRED_TESTS"].split("[")[1][:-1]
            qwen_files(directory, case, env["GITHUB_RUN_ID"])
            (directory / "ownership.json").write_text('{}')
        return b"fixture-revision"

    def suite(self, kube, execute=None):
        with patch.object(runner, "Kubernetes", return_value=kube), patch.object(runner, "execute", side_effect=execute or self.execute), \
                patch.dict(os.environ, {"PAGEBROKER_SMOKE_AWS_CREDENTIALS_FILE": "/external/fake-credentials"}):
            code = runner.run_suite(self.args)
        return code, json.loads((self.args.output / "summary.json").read_text())

    def test_full_workflow_exports_validates_and_cleans_only_owned_resources(self):
        kube = FakeKubernetes(self.root)
        code, summary = self.suite(kube)
        self.assertEqual(code, 0)
        self.assertEqual(summary["status"], "SUCCESS")
        self.assertEqual(list(summary["cases"]), list(runner.CASES))
        self.assertEqual(len(kube.namespaces), 2)
        self.assertTrue(all(case["status"] == "SUCCESS" for case in summary["cases"].values()))
        self.assertEqual(sum("secret" in action and "delete" in action for action in kube.actions), 1)
        self.assertEqual(sum("pagebroker-smoke-results" in action and "delete" in action for action in kube.actions), 2)
        self.assertFalse(any("namespace" in action and "delete" in action for action in kube.actions))
        self.assertTrue((self.args.output / "s3/results/resources.jsonl").is_file())
        qwen_commands = [command for command, _ in self.commands if "pytest" in command]
        self.assertEqual(len(qwen_commands), 2)
        self.assertIn(evidence.QWEN_CASES[1], qwen_commands[1][3])

    def test_failures_cannot_pass_even_with_successful_client_files(self):
        for failure in ("job-failed", "export"):
            with self.subTest(failure=failure):
                self.args.output = self.root / failure
                kube = FakeKubernetes(self.root / failure, failure)
                code, summary = self.suite(kube)
                self.assertEqual(code, 1)
                self.assertEqual(summary["cases"]["s3"]["status"], "ERROR")
                self.assertEqual(summary["cases"][evidence.QWEN_CASES[1]]["status"], "SUCCESS")

    def test_pytest_failure_overrides_successful_benchmark(self):
        def execute(command, **kwargs):
            value = self.execute(command, **kwargs)
            if "pytest" in command:
                raise RuntimeError("pytest exited 1 during teardown")
            return value
        code, summary = self.suite(FakeKubernetes(self.root), execute)
        self.assertEqual(code, 1)
        self.assertEqual(summary["cases"][evidence.QWEN_CASES[0]]["status"], "ERROR")
        self.assertTrue(any(runner.CLEAN_QWEN in command for command, _ in self.commands))

    def test_interrupt_is_error_and_remaining_cases_are_not_run(self):
        def execute(command, **kwargs):
            if "pytest" in command:
                raise KeyboardInterrupt()
            return self.execute(command, **kwargs)
        code, summary = self.suite(FakeKubernetes(self.root), execute)
        self.assertEqual(code, 1)
        self.assertEqual(summary["cases"][evidence.QWEN_CASES[0]]["status"], "ERROR")
        self.assertEqual(summary["cases"][evidence.QWEN_CASES[1]]["status"], "NOT_RUN")

    def test_namespace_collision_is_never_collected_or_deleted(self):
        kube = FakeKubernetes(self.root, "namespace-conflict")
        code, summary = self.suite(kube)
        self.assertEqual(code, 1)
        self.assertEqual(summary["cases"]["filesystem"]["status"], "ERROR")
        self.assertFalse(any("delete" in action or "exec" in action for action in kube.actions))

    def test_timeout_stops_job_before_export_and_preserves_error(self):
        kube = FakeKubernetes(self.root, "timeout")
        self.args.startup_seconds = 1
        with patch.object(runner.time, "monotonic", side_effect=range(100)):
            code, summary = self.suite(kube)
        self.assertEqual(code, 1)
        deletion = next(i for i, action in enumerate(kube.actions) if "delete" in action and "job" in action)
        exporter = kube.actions.index(("create", "Pod"))
        self.assertLess(deletion, exporter)
        self.assertIn("readiness deadline", summary["cases"]["filesystem"]["errors"][0])

    def test_existing_output_is_not_overwritten(self):
        self.args.output.mkdir()
        marker = self.args.output / "summary.json"
        marker.write_text("old-run")
        with self.assertRaises(FileExistsError):
            runner.run_suite(self.args)
        self.assertEqual(marker.read_text(), "old-run")

    def test_pending_job_without_pods_is_stopped_before_export(self):
        kube = FakeKubernetes(self.root)
        directory = self.root / "evidence"
        directory.mkdir()
        with patch.object(kube, "get", side_effect=[{"status": {}}, {"items": []}]):
            runner.collect_job(kube, "pagebroker-smoke-fixture-filesystem", directory,
                               {"kind": "Pod"}, [])
        deletion = next(i for i, action in enumerate(kube.actions) if "delete" in action and "job" in action)
        self.assertLess(deletion, kube.actions.index(("create", "Pod")))

    def test_ambiguous_exporter_creation_still_attempts_cleanup(self):
        kube = FakeKubernetes(self.root)
        with patch.object(kube, "create", side_effect=TimeoutError("API response lost")), self.assertRaises(TimeoutError):
            runner.collect_job(kube, "pagebroker-smoke-fixture-filesystem", self.root, {"kind": "Pod"}, [])
        self.assertTrue(any("delete" in action and "pagebroker-smoke-results" in action for action in kube.actions))

    def test_wrong_installed_images_fail_before_deployment(self):
        kube = FakeKubernetes(self.root)
        self.args.pagebroker_image = "local/pagebroker:another-version"
        code, summary = self.suite(kube)
        self.assertEqual(code, 1)
        self.assertTrue(all(case["status"] == "NOT_RUN" for case in summary["cases"].values()))
        self.assertIn("image differs", summary["errors"][0])
        self.assertFalse(kube.created)

    def test_context_and_required_case_override_inherited_pytest_settings(self):
        with patch.dict(os.environ, {"PYTEST_ADDOPTS": "--collect-only -k other", "PYTEST_PLUGINS": "fixture",
                                     "SNAPSHOT_E2E_TARGET_KUBECONFIG": "/wrong", "SNAPSHOT_E2E_MODE": "vcluster"}):
            env = runner.qwen_environment(self.args, self.root, "run", evidence.QWEN_CASES[0])
        self.assertEqual(env["SNAPSHOT_E2E_KUBE_CONTEXT"], self.args.context)
        self.assertEqual(env["KUBECONFIG"], str(self.args.kubeconfig))
        self.assertNotIn("PYTEST_ADDOPTS", env)
        self.assertNotIn("PYTEST_PLUGINS", env)
        self.assertNotIn("SNAPSHOT_E2E_TARGET_KUBECONFIG", env)
        self.assertEqual(env["SNAPSHOT_E2E_MODE"], "direct")
        self.assertIn(self.args.context, runner.Kubernetes(self.args).command)

    def test_child_deadline_interrupts_and_allows_cleanup(self):
        marker = self.root / "interrupted"
        script = "import pathlib,signal,sys,time; signal.signal(signal.SIGINT, lambda *_: (pathlib.Path(sys.argv[1]).write_text('cleaned'),sys.exit(0))); time.sleep(60)"
        with self.assertRaises(subprocess.TimeoutExpired):
            runner.execute([sys.executable, "-c", script, str(marker)], timeout=0.3, grace=6)
        self.assertEqual(marker.read_text(), "cleaned")

    def test_sensitive_errors_do_not_echo_credentials(self):
        with self.assertRaises(RuntimeError) as raised:
            runner.execute([sys.executable, "-c", "import sys; print('fake-secret'); sys.exit(1)"], sensitive=True)
        self.assertNotIn("fake-secret", str(raised.exception))


if __name__ == "__main__":
    unittest.main()
