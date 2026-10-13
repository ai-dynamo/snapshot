# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Verify the independent Kubernetes, client, resource and GPU smoke results."""

from collections import Counter
import json
import math
import re
import tarfile
import xml.etree.ElementTree as ET

from run import validate_report


RESULT_FILES = {"summary.json", "client-report.json", "client.log", "resources.jsonl"}
METRICS = {
    "rssBytes", "pssBytes", "privateBytes", "anonymousRssBytes", "fileRssBytes", "sharedRssBytes",
    "rssHighWaterBytes", "anonymousSwapBytes", "fileDescriptors", "threads",
    "stagingFilesystemUsedBytes", "stagingFilesystemUsedInodes",
}
QWEN_CASES = ("qwen3-0.6b", "qwen3-8b")


def extract_results(archive_path, destination):
    destination.mkdir()
    seen = set()
    with tarfile.open(archive_path) as archive:
        for member in archive:
            if member.name == "results" and member.isdir():
                continue
            name = member.name.removeprefix("results/")
            if member.name != f"results/{name}" or name not in RESULT_FILES or not member.isfile() or name in seen:
                raise ValueError(f"unexpected exported result: {member.name}")
            if member.size > 64 * 1024**2:
                raise ValueError(f"exported result exceeds 64 MiB: {name}")
            seen.add(name)
            with archive.extractfile(member) as source, (destination / name).open("xb") as output:
                while block := source.read(1024**2):
                    output.write(block)
    # Preserve partial evidence; the validators reject missing files on successful Jobs.


def condition(resource, name):
    return any(item["type"] == name and item["status"] == "True"
               for item in resource.get("status", {}).get("conditions", []))


def validate_job(job, pods):
    if not condition(job, "Complete") or condition(job, "Failed") or job["status"].get("failed", 0):
        raise ValueError("Job did not complete successfully without failed attempts")
    if len(pods) != 1:
        raise ValueError("expected exactly one Job Pod")
    pod = pods[0]
    if pod["status"].get("phase") != "Succeeded":
        raise ValueError("Job Pod did not succeed")
    if not any(owner.get("uid") == job["metadata"]["uid"] and owner.get("controller") is True
               for owner in pod["metadata"].get("ownerReferences", [])):
        raise ValueError("Pod is not owned by this Job")
    statuses = pod["status"].get("containerStatuses", [])
    if Counter(item["name"] for item in statuses) != Counter(["client", "pagebroker"]):
        raise ValueError("both container exit statuses are required")
    images = {}
    for item in statuses:
        terminated = item.get("state", {}).get("terminated", {})
        if item.get("restartCount") != 0 or terminated.get("exitCode") != 0 or terminated.get("reason") != "Completed":
            raise ValueError(f"{item['name']} did not exit cleanly without restarts")
        if not item.get("imageID"):
            raise ValueError(f"missing resolved image ID for {item['name']}")
        images[item["name"]] = item["imageID"]
    return {"podUID": pod["metadata"]["uid"], "node": pod["spec"]["nodeName"], "images": images}


def validate_resources(path, summary):
    if summary["status"] != "SUCCESS" or summary.get("error") or not summary["stagingIsMountPoint"]:
        raise ValueError("resource collection did not succeed on a dedicated staging mount")
    phases, aggregates = Counter(), {}
    previous, count = -1, 0
    first_phase = last_phase = None
    with path.open() as source:
        for line in source:
            record = json.loads(line)
            if record["status"] != "SUCCESS" or record.get("error"):
                raise ValueError("resource sample failed")
            if (record["pid"], record["processStartTicks"]) != (summary["pid"], summary["processStartTicks"]):
                raise ValueError("resource samples changed process identity")
            elapsed = record["elapsedSeconds"]
            if not math.isfinite(elapsed) or elapsed < previous:
                raise ValueError("resource timestamps are invalid or reordered")
            previous = elapsed
            count += 1
            first_phase = first_phase or record["phase"]
            last_phase = record["phase"]
            phases[record["phase"]] += 1
            if set(record["metrics"]) != METRICS:
                raise ValueError("resource sample is missing required metrics")
            for key, value in record["metrics"].items():
                if type(value) is not int or value < 0:
                    raise ValueError("resource metric must be a nonnegative integer")
                stat = aggregates.setdefault(key, {"first": value, "last": value, "peak": value, "delta": 0})
                stat.update(last=value, peak=max(stat["peak"], value), delta=value - stat["first"])
    if (first_phase, last_phase) != ("baseline", "settled") or phases["baseline"] != 1 or phases["settled"] != 1:
        raise ValueError("resource samples lack baseline or final idle sample")
    if not phases["workload"] or phases["after-client"] != 1 or set(phases) - {"baseline", "workload", "after-client", "settling", "settled"}:
        raise ValueError("resource samples lack required phases")
    if count != summary["sampleCount"] or phases != summary["phases"] or aggregates != summary["metrics"]:
        raise ValueError("resource samples disagree with aggregates")
    if previous != summary["durationSeconds"]:
        raise ValueError("resource samples disagree with duration")
    return summary


def validate_synthetic(directory, mode, store_id):
    report = validate_report(directory / "client-report.json", mode, store_id)
    summary = json.loads((directory / "summary.json").read_text())
    expected = {"status": "SUCCESS", "clientExitCode": 0, "pagebrokerShutdown": "SIGTERM", "mode": mode,
                "clientRunId": report["runId"], "stepsVerified": len(report["steps"]),
                "publicationsVerified": len(report["publications"])}
    if summary.get("errors") or any(summary.get(key) != value for key, value in expected.items()):
        raise ValueError("launcher summary disagrees with the verified client report")
    if summary["pagebrokerPid"] != summary["resources"]["pid"]:
        raise ValueError("launcher and resource process identities differ")
    return {"clientRunId": report["runId"], "steps": len(report["steps"]),
            "publications": len(report["publications"]),
            "resources": validate_resources(directory / "resources.jsonl", summary["resources"])}


def image_digest(image):
    match = re.search(r"sha256:[a-f0-9]{64}$", image)
    if not match:
        raise ValueError("resolved image ID lacks a SHA-256 digest")
    return match.group()


def validate_qwen(directory, case, run_id, broker_image):
    junit = ET.parse(directory / "junit.xml").getroot()
    tests = list(junit.iter("testcase"))
    expected_name = f"test_pagebroker_qwen_repeated_restore[{case}]"
    if len(tests) != 1 or tests[0].get("name") != expected_name:
        raise ValueError("JUnit did not contain the required Qwen case exactly once")
    if any(list(junit.iter(tag)) for tag in ("failure", "error", "skipped")):
        raise ValueError("Qwen JUnit contains failure, error or skip")
    reports = list((directory / "benchmarks").glob("*.json"))
    if len(reports) != 1:
        raise ValueError("expected exactly one Qwen benchmark report")
    report = json.loads(reports[0].read_text())
    identity = report["identity"]
    if (identity["suite"], identity["case"], identity["runId"]) != ("pagebroker-qwen-smoke", case, run_id):
        raise ValueError("Qwen benchmark identity differs from this run")
    environment = report["environment"]
    required = {"completedRestores": 2, "expectedRestores": 2, "storageType": "pvc",
                "restoreMode": "staged", "transferEngine": "model-streamer"}
    if report["outcome"] != "passed" or report.get("error") or any(environment.get(k) != v for k, v in required.items()):
        raise ValueError("Qwen did not verify two staged Model Streamer restores")
    events = Counter(item["name"] for item in report["events"])
    if any(events[name] != 1 for name in ("checkpoint.ready", "restore.1.inference_verified", "restore.2.inference_verified")):
        raise ValueError("Qwen checkpoint or inference evidence is missing")
    if broker_image and image_digest(environment["pagebrokerImage"]) != image_digest(broker_image):
        raise ValueError("Qwen used a different PageBroker image from the filesystem Job")
    return {"benchmark": str(reports[0].relative_to(directory)), "environment": environment}
