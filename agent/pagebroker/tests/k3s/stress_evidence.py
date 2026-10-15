# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Phase-aware sampling and independent validation of in-Pod endurance evidence."""

from collections import Counter, deque
from datetime import datetime, timezone
import hashlib
import math
from pathlib import Path
import uuid

from monitor import ProcessSampler
from run import validate_report
from stress_analysis import IDLE_PHASES, IncompleteEvidence, analyze_resources, medians, metrics, records
from stress_cgroup import BrokerCgroup
from stress_config import Clock
from stress_storage import EvidenceLimitError, encoded, read_json, validate_manifest


class ResourceObservation:
    def __init__(self, spec, clock=None, sampler_factory=ProcessSampler):
        self.spec, self.clock, self.sampler_factory = spec, clock or Clock(), sampler_factory
        self.store = self.sampler = self.cgroup = None
        self.started = self.next_sample = 0
        self.last_key = None
        self.tail = deque(maxlen=10)
        self.group_count = 0
        self.chunk = 1
        self.count = 0
        self.phases, self.metrics = Counter(), {}
        self.status, self.error = "RUNNING", None
        self.staging_baseline = None
        self.closed = False

    def bind(self, store):
        self.store = store

    def attach(self, peer, spec, excluded_pids=()):
        self.peer = peer
        self.sampler = self.sampler_factory(peer.pid, peer.fd, spec.staging)
        if self.sampler.start_ticks != peer.ticks:
            raise RuntimeError("resource sampler attached to a different broker identity")
        self.cgroup = BrokerCgroup(self.sampler, excluded_pids)
        self.started = self.clock.monotonic()
        self.next_sample = self.started
        self.sample("cold-start", 0, self.started)

    def checkpoint(self):
        if self.sampler is None:
            return
        self.store.write("resource-metadata.json", {
            "schemaVersion": 1, "runId": self.spec.run_id, "mode": self.spec.mode, "status": self.status,
            "error": self.error, "broker": self.peer.record(), "intervalSeconds": self.spec.evidence.interval,
            "monotonicStarted": self.started,
            "stagingIsMountPoint": self.sampler.staging_is_mount, "stagingBaseline": self.staging_baseline,
            "sampleCount": self.count, "phases": dict(self.phases), "metrics": self.metrics})

    def baseline(self, peer, spec):
        if not self.sampler.staging_is_mount:
            raise ValueError("resource staging baseline requires a dedicated mount")
        values = self.sampler.collect()
        self.staging_baseline = [values["stagingFilesystemUsedBytes"], values["stagingFilesystemUsedInodes"]]
        self.sample("protocol-ready", 0, self.clock.monotonic(), force=True)
        self.checkpoint()

    def sample(self, phase, batch, monotonic, force=False):
        key = (phase, batch)
        if not force and monotonic < self.next_sample and key == self.last_key:
            return
        if self.closed:
            raise RuntimeError("resource sampler is closed")
        record = {"schemaVersion": 1, "runId": self.spec.run_id, "mode": self.spec.mode,
                  "sequence": self.count + 1, "monotonic": monotonic, "elapsedSeconds": monotonic - self.started,
                  "timestamp": datetime.fromtimestamp(self.clock.time(), timezone.utc).isoformat(),
                  "pid": self.peer.pid, "processStartTicks": self.peer.ticks, "phase": phase, "batch": batch}
        try:
            values = self.sampler.collect()
            metrics(values)
            optional = self.cgroup.collect()
            if optional["status"] == "VERIFIED":
                optional.update(brokerPid=self.peer.pid, runnerExcluded=True)
            record.update(status="SUCCESS", metrics=values, cgroup=optional)
            data = encoded(record)
            if len(data) > self.spec.evidence.chunk_bytes:
                raise EvidenceLimitError("one resource sample exceeds its chunk limit")
            name = f"resources/{self.chunk:06d}.jsonl"
            if self.store.sizes.get(name, 0) + len(data) > self.spec.evidence.chunk_bytes:
                self.chunk += 1
                name = f"resources/{self.chunk:06d}.jsonl"
            self.store.append(name, data)
        except Exception as error:
            self.status, self.error = "ERROR", type(error).__name__
            self.checkpoint()
            raise RuntimeError(f"resource sampling failed: {type(error).__name__}") from error
        self.count += 1
        self.phases[phase] += 1
        for name, value in values.items():
            stat = self.metrics.setdefault(name, {"first": value, "last": value, "peak": value, "delta": 0})
            stat.update(last=value, peak=max(stat["peak"], value), delta=value - stat["first"])
        if key != self.last_key:
            self.tail.clear()
            self.group_count = 0
            self.group_first = record
        self.group_count += 1
        self.group_last = record
        self.tail.append(values)
        self.last_key = key
        if monotonic >= self.next_sample:
            self.next_sample = monotonic + self.spec.evidence.interval

    def idle(self, phase, batch, started, finished):
        if self.last_key != (phase, batch):
            raise ValueError("idle observation does not match the current sample window")
        valid = (self.group_count >= self.spec.evidence.idle_samples
                 and finished - started >= (self.spec.evidence.idle_samples - 1) * self.spec.evidence.interval)
        try:
            diagnostics = self.sampler.diagnostics()
        except (OSError, ValueError):
            diagnostics = {"status": "UNAVAILABLE"}
        self.store.append("idle-windows.jsonl", {
            "runId": self.spec.run_id, "phase": phase, "batch": batch, "started": started, "finished": finished,
            "firstSequence": self.group_first["sequence"], "lastSequence": self.group_last["sequence"],
            "samples": self.group_count, "status": "VALID" if valid else "INSUFFICIENT",
            "medians": medians(self.tail) if valid else None, "diagnostics": diagnostics})

    def final(self):
        self.sample("final-sample", 0, self.clock.monotonic(), force=True)
        self.status = "SUCCESS"
        self.checkpoint()

    def abort(self):
        if self.sampler is not None and self.status != "SUCCESS":
            self.status = "ERROR"
            self.error = self.error or "RunDidNotReachFinalSampling"
            self.checkpoint()

    def close(self):
        if not self.closed:
            self.closed = True
            if self.cgroup is not None:
                self.cgroup.close()
            if self.sampler is not None:
                self.sampler.close()


def validate_batches(root, spec, summary, pulse=lambda: None):
    counts = {"warmup": 0, "measured": 0, "drain": 0}
    verified, published = dict.fromkeys(counts, 0), dict.fromkeys(counts, 0)
    expected, pending, previous_start = 1, None, None
    previous_finish = None
    identifiers = set()
    # The manifest/file-count limit bounds this set independently of campaign duration.
    for record in records(Path(root) / "batches.jsonl"):
        pulse()
        if record.get("runId") != spec.run_id:
            raise ValueError("batch journal belongs to another run")
        if record["event"] == "START":
            if pending is not None or record["batch"] != expected:
                raise ValueError("batch starts are missing or reordered")
            if previous_start is not None and record["monotonic"] - previous_start < spec.timing.cadence - 1e-6:
                raise ValueError("batch cadence was violated")
            if previous_finish is not None and record["monotonic"] - previous_finish < spec.timing.idle - 1e-6:
                raise ValueError("minimum batch idle time was violated")
            previous_start = record["monotonic"]
            pending = record
        elif record["event"] == "COMPLETE":
            if pending is None or record["batch"] != expected or record["started"] != pending["monotonic"]:
                raise ValueError("batch completion does not match its admission")
            if record["finished"] < record["started"] or record["cleanup"] != "VERIFIED":
                raise ValueError("batch cleanup or time bounds are invalid")
            phase = record["phase"]
            if phase not in counts:
                raise ValueError("unexpected completed batch phase")
            if phase != pending["phase"] and (phase, pending["phase"]) != ("drain", "measured"):
                raise ValueError("completed batch changed its admitted phase")
            name = f"batches/{expected:06d}/client-report.json"
            report_path = Path(root) / name
            if report_path.stat().st_size > spec.evidence.report_bytes:
                raise EvidenceLimitError("client report exceeds its configured size limit")
            report = validate_report(report_path, spec.mode, spec.store_id)
            (report_path.parent / "client.log").stat()
            client_id = report["runId"]
            if (client_id in identifiers or client_id != record["clientRunId"]
                    or str(uuid.UUID(client_id)) != client_id):
                raise ValueError("batch client identity is duplicated or mismatched")
            if len(identifiers) >= spec.evidence.max_files:
                raise EvidenceLimitError("client identity validation capacity exceeded")
            identifiers.add(client_id)
            if (Path(root) / "client-ids" / client_id).read_text() != str(expected):
                raise ValueError("client identity claim names a different batch")
            config = report["config"]
            if config.get("socket") != spec.socket or config.get("stagingRoot") != spec.staging:
                raise ValueError("batch report refers to another broker or staging root")
            storage = Path(spec.storage) / spec.run_id / f"{expected:06d}"
            if spec.mode == "filesystem" and config.get("storageRoot") != str(storage):
                raise ValueError("batch report uses a different private storage root")
            if spec.mode == "filesystem" and any(storage not in Path(item["destination"]).parents
                                                 for item in report["publications"]):
                raise ValueError("publication destination escaped its batch storage root")
            if len(report["steps"]) != record["steps"] or len(report["publications"]) != record["publications"]:
                raise ValueError("batch journal disagrees with its client report")
            if phase == "measured" and record["finished"] > summary["measuredStarted"] + spec.timing.measured:
                raise ValueError("drain work was counted as measured")
            counts[phase] += 1
            verified[phase] += report["payloadBytes"] * (6 if spec.mode == "s3" else 12)
            published[phase] += report["payloadBytes"] * len(report["publications"])
            previous_finish = record["finished"]
            pending, expected = None, expected + 1
        else:
            raise ValueError("unknown batch journal event")
    if pending is not None:
        raise IncompleteEvidence("batch journal ends with an unfinished client")
    if counts != summary["counts"] or any(type(value) is not int for value in summary["counts"].values()):
        raise ValueError("terminal counts disagree with the batch journal")
    if verified != summary.get("verifiedPayloadBytes") or published != summary.get("publishedPayloadBytes"):
        raise ValueError("payload byte totals disagree with verified client reports")
    if summary["executionStatus"] == "SUCCESS" and (
            counts["warmup"] < spec.timing.warmup_batches or counts["measured"] < spec.timing.measured_batches):
        raise IncompleteEvidence("minimum declared work is missing")
    return counts


def validate_directory(root, spec, summary=None, pulse=lambda: None):
    result = {"scope": "IN_POD", "evidenceStatus": "INCOMPLETE", "resourceAssessment": "INCONCLUSIVE",
              "limitsExceeded": False, "errors": []}
    try:
        summary = summary if summary is not None else read_json(root, "summary.json")
        if read_json(root, "run-spec.json") != spec.as_dict():
            raise ValueError("validation configuration differs from the persisted prelaunch specification")
        if summary.get("runId") != spec.run_id or summary.get("executionStatus") not in ("SUCCESS", "ERROR", "INTERRUPTED"):
            raise IncompleteEvidence("matching terminal summary is missing")
        result["storage"] = validate_manifest(root, spec, pulse)
        progress = read_json(root, "progress.json")
        if (progress.get("runId"), progress.get("executionStatus"), progress.get("counts")) != (
                spec.run_id, summary["executionStatus"], summary["counts"]):
            raise IncompleteEvidence("durable progress does not match the terminal work report")
        (Path(root) / "readiness.log").stat()
        readiness = read_json(root, "readiness.json")
        supervisor = read_json(root, "supervisor.json")
        if (readiness.get("runId"), readiness.get("socket"), readiness.get("status")) != (spec.run_id, spec.socket, "SUCCESS"):
            raise IncompleteEvidence("matching successful readiness report is missing")
        if supervisor.get("runId") != spec.run_id or supervisor.get("status") != "SUCCESS":
            raise IncompleteEvidence("clean supervised shutdown evidence is missing")
        metadata = read_json(root, "resource-metadata.json")
        if metadata.get("broker") != supervisor.get("broker"):
            raise ValueError("shutdown and resource reports identify different brokers")
        result["counts"] = validate_batches(root, spec, summary, pulse)
        if spec.mode == "s3":
            validate_cleanup(root, spec, supervisor, pulse)
        analysis = analyze_resources(root, spec, summary, pulse)
        if read_json(root, "resource-analysis.json", 128 * 1024) != analysis:
            raise ValueError("resource analysis disagrees with independently recomputed evidence")
        result.update(evidenceStatus=analysis["evidenceStatus"], resourceAssessment=analysis["resourceAssessment"],
                      limitsExceeded=analysis["limitsExceeded"], analysis=analysis)
        if summary["executionStatus"] != "SUCCESS":
            result.update(evidenceStatus="INCOMPLETE", resourceAssessment="INCONCLUSIVE")
    except (FileNotFoundError, IncompleteEvidence) as error:
        result["errors"].append(str(error))
    except (OSError, ValueError, KeyError, TypeError, RuntimeError) as error:
        result["evidenceStatus"] = "ERROR"
        result["errors"].append(str(error))
    return result


def validate_cleanup(root, spec, supervisor, pulse=lambda: None):
    ready = read_json(root, "cleanup-readiness.json")
    if (ready.get("runId") != spec.run_id or ready.get("status") != "READY"
            or ready.get("credentialExpiry") != spec.s3.credential_expiry
            or ready.get("credentialProvider") != spec.s3.credential_provider
            or ready.get("incompleteUploads") != 0):
        raise IncompleteEvidence("successful S3 cleanup readiness/credential evidence is missing")
    helper = ready["helper"]
    stopped = supervisor.get("helper", {})
    if (stopped.get("helper") != helper or stopped.get("status") != "STOPPED"
            or stopped.get("remoteCleanup") != "VERIFIED"):
        raise IncompleteEvidence("successful cleanup helper shutdown evidence is missing")
    completions = (item for item in records(Path(root) / "batches.jsonl") if item["event"] == "COMPLETE")
    pending, expected, last_ack = None, 1, None
    for item in records(Path(root) / "cleanup.jsonl"):
        pulse()
        if item.get("runId") != spec.run_id or item.get("batch") != expected:
            raise ValueError("cleanup journal belongs to another run or batch")
        if item["event"] == "REQUEST":
            if pending is not None or item.get("quiescent") is not True or item.get("broker") != supervisor["broker"]:
                raise ValueError("cleanup request is not a quiescent batch boundary")
            if str(uuid.UUID(item["token"])) != item["token"]:
                raise ValueError("cleanup token is not a canonical UUID")
            name = f"batches/{expected:06d}/client-report.json"
            data = (Path(root) / name).read_bytes()
            report = read_json(root, name, spec.evidence.report_bytes)
            if (item.get("report") != name or hashlib.sha256(data).hexdigest() != item.get("sha256")
                    or report["runId"] != item.get("clientRunId")):
                raise ValueError("cleanup request is not bound to its saved client report")
            pending = item
        elif item["event"] == "ACK":
            if pending is None or any(item.get(key) != pending[key] for key in ("token", "sha256", "clientRunId")):
                raise ValueError("cleanup acknowledgment does not match its request")
            if item.get("status") != "SUCCESS" or item.get("incompleteUploads") != 0 or item.get("helper") != helper:
                raise IncompleteEvidence("batch S3 cleanup was not verified")
            if (not all(isinstance(item.get(key), (int, float)) and math.isfinite(item[key])
                        for key in ("started", "finished")) or not item["started"] <= item["finished"] < pending["deadline"]
                    or pending["deadline"] - item["started"] > spec.timing.cleanup):
                raise ValueError("cleanup acknowledgment exceeds its time budget")
            for key in ("objects", "versions", "deleteMarkers", "bytes"):
                if type(item.get(key)) is not int or item[key] < 0:
                    raise ValueError("cleanup acknowledgment has invalid counters")
            complete = next(completions, None)
            if (complete is None or complete["batch"] != expected or complete["clientRunId"] != item["clientRunId"]
                    or not complete["started"] <= item["started"] <= item["finished"] <= complete["finished"]):
                raise ValueError("batch completion precedes cleanup acknowledgment")
            expected, pending = expected + 1, None
            last_ack = item
        else:
            raise ValueError("unknown cleanup journal event")
    if pending is not None or next(completions, None) is not None:
        raise IncompleteEvidence("batch cleanup acknowledgment is missing")
    if last_ack is None or read_json(root, "cleanup-final.json") != last_ack:
        raise IncompleteEvidence("terminal cleanup acknowledgment disagrees with its journal")
