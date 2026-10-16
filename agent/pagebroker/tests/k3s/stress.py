#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Run bounded batches against one supervised PageBroker process."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
from typing import Protocol
import uuid

from run import exited, validate_report
from stress_config import Clock, RunSpec, SCHEMA_VERSION
from stress_control import Channel, Peer, own_record, start_ticks
from stress_storage import EvidenceLimitError, EvidenceStore


class Observation(Protocol):
    """Collect resource evidence without hiding stalled lifecycle control loops."""

    metrics: dict

    def bind(self, store) -> None: ...
    def attach(self, peer, spec, excluded_pids=()) -> None: ...
    def baseline(self, peer, spec) -> None: ...
    def sample(self, phase, batch, monotonic) -> None: ...
    def idle(self, phase, batch, started, finished) -> None: ...
    def final(self) -> None: ...
    def checkpoint(self) -> None: ...
    def abort(self) -> None: ...
    def close(self) -> None: ...


class Cleanup(Protocol):
    """Coordinate asynchronous cleanup while independent supervision owns shutdown."""

    def ready(self, deadline) -> dict | None: ...
    def request(self, request: dict) -> None: ...
    def poll(self, run_id, batch) -> dict | None: ...
    def check(self) -> None: ...
    def emergency_stop(self) -> None: ...
    def close(self) -> None: ...


# The child cannot issue requests until its identity is acknowledged by supervision.
GATE = "import os,sys; permit=os.read(0,1); os.close(0); os.open('/dev/null',os.O_RDONLY); " \
       "sys.exit(125) if permit!=b'1' else os.execvpe(sys.argv[1],sys.argv[1:],os.environ)"


def staging_usage(path):
    if not Path(path).is_mount():
        raise ValueError("staging must be a dedicated filesystem mount")
    usage = os.statvfs(path)
    return ((usage.f_blocks - usage.f_bfree) * usage.f_frsize,
            usage.f_files - usage.f_ffree)


class LocalRuntime:
    def __init__(self, spec, client, observation=None, cleanup=None, clock=None):
        self.spec, self.client = spec, list(client)
        self.observation, self.cleanup = observation, cleanup
        self.clock = clock or Clock()
        self.channel = Channel(spec.control, spec.run_id)
        self.broker = self.supervisor = self.process = None
        self.observing = False
        self.stop_requested = False
        self.signal_received = False
        self.phase, self.number = "startup", 0
        self.next_beat = self.next_socket_check = 0
        self.storage = None
        self.evidence = None
        self.progress_frozen = False
        self.next_progress = 0
        self.progress_key = None

    def configure_evidence(self, store, summary):
        self.evidence, self.summary = store, summary
        if self.observation is not None:
            self.observation.bind(store)

    def checkpoint_progress(self, force=False):
        if self.evidence is None or self.progress_frozen:
            return
        now = self.clock.monotonic()
        key = (self.phase, self.number)
        if force or now >= self.next_progress or key != self.progress_key:
            value = {"schemaVersion": 1, "runId": self.spec.run_id, "phase": self.phase, "batch": self.number,
                     "monotonic": now, "updatedAt": self.clock.time(), "executionStatus": self.summary["executionStatus"],
                     "counts": self.summary["counts"], "broker": self.broker.record() if self.broker else None}
            if self.observation is not None and self.observation.metrics:
                value["resourceMetrics"] = {name: stat["last"] for name, stat in self.observation.metrics.items()}
            if self.observing:
                self.observation.checkpoint()
            self.evidence.write("progress.json", value)
            self.next_progress = now + self.spec.evidence.progress_seconds
            self.progress_key = key

    def pulse(self, broker=True, supervisor=True):
        now = self.clock.monotonic()
        if now >= self.next_beat:
            self.channel.beat("runner", self.clock)
            self.next_beat = now + self.spec.timing.heartbeat
        if self.signal_received:
            raise InterruptedError("runner received a termination signal")
        if self.channel.read("stop.json") is not None:
            self.stop_requested = True
        if supervisor and self.supervisor is not None:
            self.supervisor.check()
            self.channel.check_heartbeat("supervisor", self.clock, self.spec.timing.stale)
        if broker and self.broker is not None:
            self.broker.check()
            if now >= self.next_socket_check:
                self.broker.verify_socket(self.spec.socket)
                self.next_socket_check = now + self.spec.timing.heartbeat
        if broker and self.cleanup is not None:
            self.cleanup.check()
        if self.observing:
            phase = self.phase
            if phase == "measured" and now > self.summary.get("measuredStarted", float("inf")) + self.spec.timing.measured:
                phase = "drain"
            self.observation.sample(phase, self.number, now)
        self.checkpoint_progress()

    def event(self, phase, number=0):
        self.phase, self.number = phase, number
        self.pulse()

    def wait_until(self, deadline, phase, number=0):
        self.phase, self.number = phase, number
        started = self.clock.monotonic()
        while self.clock.monotonic() < deadline:
            self.pulse()
            if self.stop_requested:
                raise InterruptedError("cooperative stop requested")
            self.clock.sleep(min(self.spec.timing.poll, max(0, deadline - self.clock.monotonic())))
        self.pulse()
        if self.stop_requested:
            raise InterruptedError("cooperative stop requested")
        if self.observing and phase in ("idle", "warmup-idle", "final-idle"):
            self.observation.idle(phase, number, started, self.clock.monotonic())

    def wait_message(self, name, deadline, predicate=lambda value: True, broker=True):
        while True:
            self.pulse(broker=broker)
            if self.clock.monotonic() >= deadline:
                raise TimeoutError(f"deadline waiting for {name}")
            message = self.channel.read(name)
            if message is not None and predicate(message):
                return message
            self.clock.sleep(self.spec.timing.poll)

    def initialize(self, startup, expiry):
        self.expiry = expiry
        Path(self.spec.control).mkdir(parents=True, exist_ok=True)
        with (Path(self.spec.control) / "runner-owner").open("x"):
            pass
        while self.broker is None:
            self.pulse(supervisor=False)
            if self.clock.monotonic() >= startup:
                raise TimeoutError("PageBroker startup admission expired")
            try:
                self.broker = Peer.discover(self.spec.socket, min(0.1, self.spec.timing.poll))
            except TimeoutError:
                pass
        self.channel.write("runner-registration.json", peer=own_record(), broker=self.broker.record())
        while self.supervisor is None:
            self.pulse(supervisor=False)
            registration = self.channel.read("supervisor-registration.json")
            heartbeat = self.channel.read("supervisor-heartbeat.json")
            if registration is not None and heartbeat is not None:
                self.supervisor = Peer.open(registration["peer"])
            if self.clock.monotonic() >= startup:
                raise TimeoutError("independent supervisor did not register before startup deadline")
            self.clock.sleep(self.spec.timing.poll)
        registered = self.wait_message("registered.json", startup)
        if registered["broker"] != self.broker.record() or registered["peer"] != self.supervisor.record():
            raise RuntimeError("conflicting supervisor registration")
        if self.spec.mode == "s3":
            if self.cleanup is None:
                raise RuntimeError("S3 admission requires the cleanup helper")
            self.spec.s3.validate_expiry(self.spec, self.clock.time())
            while True:
                self.pulse()
                if self.clock.monotonic() >= startup:
                    raise TimeoutError("S3 cleanup readiness exceeded startup deadline")
                ready = self.cleanup.ready(startup)
                if ready is not None:
                    self.evidence.write("cleanup-readiness.json", ready)
                    if (ready.get("status") != "READY" or ready.get("credentialExpiry") != self.spec.s3.credential_expiry
                            or ready.get("credentialProvider") != self.spec.s3.credential_provider):
                        raise RuntimeError("S3 cleanup readiness or credential admission failed")
                    break
                self.clock.sleep(self.spec.timing.poll)
        if self.observation is None:
            raise NotImplementedError("resource observation is required; implement the step 2 facility before running batches")
        excluded = (self.supervisor.pid,) + ((self.cleanup.peer.pid,) if self.cleanup else ())
        self.observation.attach(self.broker, self.spec, excluded_pids=excluded)
        self.observing = True
        environment = self.environment() | {
            "PAGEBROKER_READY_RUN_ID": self.spec.run_id, "PAGEBROKER_READY_SOCKET": self.spec.socket,
            "PAGEBROKER_READY_REPORT": str(Path(self.spec.results) / "readiness.json"),
        }
        path = Path(environment["PAGEBROKER_READY_REPORT"])
        if path.exists():
            raise FileExistsError("readiness report already exists")
        self.invoke([*self.client, "-test.run=^TestPageBrokerK3sReady$", "-test.timeout=5m"],
                    environment, Path(self.spec.results) / "readiness.log", startup, "readiness.json", 64 * 1024)
        if path.stat().st_size > 64 * 1024:
            raise ValueError("readiness report exceeds 64 KiB")
        report = json.loads(path.read_text())
        if (report.get("status"), report.get("runId"), report.get("socket")) != (
                "SUCCESS", self.spec.run_id, self.spec.socket):
            raise ValueError("readiness probe was skipped or returned a stale/mismatched report")
        if report.get("error") or type(report.get("customStorageAvailable")) is not bool:
            raise ValueError("invalid readiness capabilities report")
        self.broker.verify_socket(self.spec.socket)
        self.baseline = staging_usage(self.spec.staging)
        self.observation.baseline(self.broker, self.spec)
        self.observing = True
        if self.clock.monotonic() >= startup:
            raise TimeoutError("protocol readiness and baseline exceeded startup budget")
        self.storage = Path(self.spec.storage) / self.spec.run_id
        self.storage.mkdir(parents=True, exist_ok=False)

    def environment(self):
        return {key: value for key, value in os.environ.items() if not key.startswith("AWS_")} | {
            "PAGEBROKER_SMOKE_MODE": self.spec.mode, "PAGEBROKER_SMOKE_SOCKET": self.spec.socket,
            "PAGEBROKER_SMOKE_STAGING_ROOT": self.spec.staging,
            "PAGEBROKER_SMOKE_STORE_ID": self.spec.store_id,
        }

    def invoke(self, command, environment, log_path, deadline, report_name=None, report_limit=None):
        if self.process is not None:
            raise RuntimeError("another client is still registered")
        self.pulse()
        if self.clock.monotonic() >= deadline:
            raise TimeoutError("client admission deadline expired")
        if self.stop_requested:
            raise InterruptedError("cooperative stop requested before client admission")
        if self.evidence is None:
            raise RuntimeError("bounded evidence store is required before admitting a client")
        token = str(uuid.uuid4())
        log_name = log_path.relative_to(self.spec.results).as_posix()
        self.evidence.write(log_name, b"", exclusive=True)
        report_limit = report_limit or self.spec.evidence.report_bytes
        tail = bytearray()
        def drain():
            while True:
                try:
                    data = os.read(self.process.stdout.fileno(), 16384)
                except BlockingIOError:
                    return
                if not data:
                    return
                tail.extend(data)
                del tail[:-4096]
                if self.evidence.sizes[log_name] + len(data) > self.spec.evidence.client_log_bytes:
                    self.evidence.write("failure-client-tail.log", bytes(tail))
                    raise EvidenceLimitError("required client log exceeded its size limit")
                self.evidence.append(log_name, data)
        with self.evidence.reserve(2 * report_limit):
            self.process = subprocess.Popen([sys.executable, "-c", GATE, *command], env=environment,
                                            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                            start_new_session=True)
            os.set_blocking(self.process.stdout.fileno(), False)
            peer = {"pid": self.process.pid, "startTicks": start_ticks(self.process.pid)}
            self.channel.write("client.json", token=token, active=True, peer=peer)
            self.wait_message("client-ack.json", deadline, lambda value: value["token"] == token)
            if self.clock.monotonic() >= deadline:
                raise TimeoutError("client admission deadline expired")
            if self.stop_requested:
                raise InterruptedError("cooperative stop requested before releasing the client gate")
            self.process.stdin.write(b"1")
            self.process.stdin.close()
            while self.process.poll() is None:
                self.pulse()
                drain()
                if self.clock.monotonic() >= deadline:
                    raise TimeoutError("client execution exceeded its deadline")
                self.clock.sleep(self.spec.timing.poll)
            self.pulse()
            drain()
            self.process.stdout.close()
            if self.clock.monotonic() >= deadline:
                raise TimeoutError("client execution exceeded its deadline")
            code = self.process.returncode
            self.process = None
            self.channel.write("client.json", token=str(uuid.uuid4()), active=False)
            if code:
                raise RuntimeError(f"client exited with code {code}")
        if report_name is not None:
            self.evidence.register(report_name, report_limit)

    def batch(self, number, phase, deadline):
        self.event(phase, number)
        directory = Path(self.spec.results) / "batches" / f"{number:06d}"
        directory.mkdir()
        storage = self.storage / f"{number:06d}"
        storage.mkdir()
        environment = self.environment() | {
            "PAGEBROKER_SMOKE_REPORT": str(directory / "client-report.json"),
            "PAGEBROKER_SMOKE_STORAGE_ROOT": str(storage),
        }
        self.invoke([*self.client, "-test.run=^TestPageBrokerK3sSmoke$", "-test.v", "-test.timeout=25m"],
                    environment, directory / "client.log", deadline, f"batches/{number:06d}/client-report.json")
        report = validate_report(directory / "client-report.json", self.spec.mode, self.spec.store_id)
        if any(report["config"].get(key) != value for key, value in (
                ("socket", self.spec.socket), ("stagingRoot", self.spec.staging), ("storageRoot", str(storage)))):
            raise ValueError("client report configuration differs from the admitted batch")
        storage.rmdir()
        if staging_usage(self.spec.staging) != self.baseline:
            raise RuntimeError("staging bytes/inodes did not return to the protocol-ready baseline")
        if self.clock.monotonic() >= deadline:
            raise TimeoutError("report verification exceeded its batch deadline")
        return report

    def finish_batch(self, number, report, phase_deadline):
        if self.spec.mode != "s3":
            return
        if self.process is not None:
            raise RuntimeError("cleanup cannot start while a client is active")
        deadline = min(self.clock.monotonic() + self.spec.timing.cleanup, phase_deadline)
        self.event("cleanup", number)
        registration = self.channel.read("client.json")
        self.wait_message("client-ack.json", deadline, lambda value: value.get("token") == registration["token"])
        name = f"batches/{number:06d}/client-report.json"
        path = Path(self.spec.results) / name
        # Register/fsync the verified report before allowing destructive work.
        self.evidence.write(name, report)
        request = {"token": str(uuid.uuid4()), "batch": number, "report": name,
                   "sha256": hashlib.sha256(path.read_bytes()).hexdigest(), "clientRunId": report["runId"],
                   "deadline": deadline, "quiescent": True, "broker": self.broker.record()}
        self.evidence.write("cleanup-final.json", {"runId": self.spec.run_id, "event": "REQUEST", "status": "PENDING", **request})
        self.evidence.append("cleanup.jsonl", {"runId": self.spec.run_id, "event": "REQUEST", **request})
        self.cleanup.request(request)
        while True:
            self.pulse()
            if self.clock.monotonic() >= deadline:
                raise TimeoutError("S3 cleanup acknowledgment exceeded its deadline; batch retained or uncertain")
            reply = self.cleanup.poll(self.spec.run_id, number)
            if reply is not None:
                self.evidence.write("cleanup-final.json", {"event": "ACK", **reply})
                self.evidence.append("cleanup.jsonl", {"event": "ACK", **reply})
                if reply.get("status") != "SUCCESS" or reply.get("incompleteUploads") != 0:
                    raise RuntimeError("S3 cleanup failed; retaining the failed batch")
                break
            self.clock.sleep(self.spec.timing.poll)

    def final(self):
        self.phase, self.number = "final-sample", 0
        self.observation.final()
        self.observing = False
        self.pulse()
        from stress_analysis import analyze_resources
        analysis = analyze_resources(self.spec.results, self.spec, self.summary, self.pulse)
        self.evidence.write("resource-analysis.json", analysis)

    def validate_evidence(self, summary):
        from stress_evidence import validate_directory
        self.checkpoint_progress(force=True)
        self.progress_frozen = True
        def pulse():
            if self.supervisor is not None and not exited(self.supervisor.fd):
                self.pulse(broker=False)
            else:
                self.channel.beat("runner", self.clock)
        self.evidence.manifest(self.spec.run_id, pulse)
        return validate_directory(self.spec.results, self.spec, summary, pulse)

    def terminate(self, peer, term_seconds, deadline):
        peer.send(signal.SIGTERM)
        term_deadline = min(deadline, self.clock.monotonic() + term_seconds)
        while not exited(peer.fd) and self.clock.monotonic() < term_deadline:
            self.channel.beat("runner", self.clock)
            self.clock.sleep(self.spec.timing.poll)
        if not exited(peer.fd):
            peer.send(signal.SIGKILL)
            kill_deadline = min(deadline, self.clock.monotonic() + self.spec.timing.kill_wait)
            while not exited(peer.fd) and self.clock.monotonic() < kill_deadline:
                self.clock.sleep(self.spec.timing.poll)
            raise RuntimeError("emergency shutdown required SIGKILL" if exited(peer.fd) else "peer exit unconfirmed")

    def shutdown(self, status):
        self.observing = False
        if self.observation is not None:
            try:
                self.observation.abort()
            except (OSError, ValueError, RuntimeError):
                pass  # Evidence failure must not prevent peer shutdown.
        self.signal_received = False
        deadline = min(getattr(self, "expiry", float("inf")), self.clock.monotonic() + self.spec.timing.shutdown)
        self.shutdown_deadline = deadline
        try:
            if self.process is not None:
                peer = Peer.open({"pid": self.process.pid, "startTicks": start_ticks(self.process.pid)}) if self.process.poll() is None else None
                if peer is not None:
                    try:
                        self.terminate(peer, self.spec.timing.client_term, deadline)
                    finally:
                        peer.close()
                self.process.wait(timeout=max(0.01, deadline - self.clock.monotonic()))
                if self.process.stdin and not self.process.stdin.closed:
                    self.process.stdin.close()
                if self.process.stdout and not self.process.stdout.closed:
                    self.process.stdout.close()
                self.process = None
                self.channel.write("client.json", token=str(uuid.uuid4()), active=False)
            if self.supervisor is None:
                raise RuntimeError("independent supervisor unavailable; emergency shutdown required")
            self.channel.write("shutdown.json", status=status)
            report = self.wait_message("shutdown-complete.json", deadline, broker=False)
            if report["broker"] != self.broker.record():
                raise RuntimeError("shutdown report names a different broker")
            self.completion = report
            return report
        except BaseException:
            # A failed observer cannot retain shutdown ownership while the runner takes over.
            if self.supervisor is not None and not exited(self.supervisor.fd):
                self.supervisor.send(signal.SIGKILL)
                confirm = min(deadline, self.clock.monotonic() + self.spec.timing.kill_wait)
                while not exited(self.supervisor.fd) and self.clock.monotonic() < confirm:
                    self.clock.sleep(self.spec.timing.poll)
            if self.cleanup is not None:
                self.cleanup.emergency_stop()
            if self.broker is not None and not exited(self.broker.fd):
                self.terminate(self.broker, self.spec.timing.broker_term, deadline)
            raise

    def complete(self):
        if hasattr(self, "completion"):
            self.channel.write("completion-ack.json")
            deadline = min(self.shutdown_deadline, self.clock.monotonic() + self.spec.timing.kill_wait)
            while not exited(self.supervisor.fd):
                self.channel.beat("runner", self.clock)
                if self.clock.monotonic() >= deadline:
                    self.supervisor.send(signal.SIGKILL)
                    raise TimeoutError("supervisor did not exit after completion acknowledgment")
                self.clock.sleep(self.spec.timing.poll)

    def close(self):
        if self.cleanup is not None:
            self.cleanup.close()
        if self.observation is not None:
            self.observation.close()
        for peer in (self.supervisor, self.broker):
            if peer is not None:
                peer.close()
        if self.storage is not None and self.storage.exists():
            try:
                self.storage.rmdir()
            except OSError:
                pass  # Failed batches retain their private files for diagnosis.


def run(spec, runtime):
    clock, timing = runtime.clock, spec.timing
    results = Path(spec.results)
    summary = {"schemaVersion": SCHEMA_VERSION, "runId": spec.run_id, "executionStatus": "RUNNING",
               "evidenceStatus": "INCOMPLETE", "resourceAssessment": "INCONCLUSIVE",
               "counts": {"warmup": 0, "measured": 0, "drain": 0}, "errors": [],
               "verifiedPayloadBytes": {"warmup": 0, "measured": 0, "drain": 0},
               "publishedPayloadBytes": {"warmup": 0, "measured": 0, "drain": 0}}
    reserved, store, number = False, None, 0
    try:
        startup, expiry = spec.deadlines(clock)
        results.mkdir(parents=True, exist_ok=True)
        with (results / "summary.json").open("x") as output:
            json.dump(summary, output)
        reserved = True
        store = EvidenceStore(results, spec.evidence)
        store.register("summary.json", 64 * 1024)
        store.write("run-spec.json", spec.as_dict(), exclusive=True)
        if hasattr(runtime, "configure_evidence"):
            runtime.configure_evidence(store, summary)
        (results / "batches").mkdir()
        (results / "client-ids").mkdir()
        store.write("batches.jsonl", b"", exclusive=True)
        runtime.initialize(startup, expiry)
        work_deadline = expiry - timing.shutdown - timing.margin

        def batch(phase, phase_deadline):
            nonlocal number
            number += 1
            started = clock.monotonic()
            store.append("batches.jsonl", {"runId": spec.run_id, "batch": number, "phase": phase, "event": "START", "monotonic": started})
            report = runtime.batch(number, phase, min(started + timing.batch, phase_deadline, work_deadline))
            run_id = report["runId"]
            if str(uuid.UUID(run_id)) != run_id:
                raise ValueError("client run ID is not a canonical UUID")
            # Disk-backed uniqueness avoids retaining every report or UUID in RAM.
            store.write(f"client-ids/{run_id}", str(number).encode(), exclusive=True)
            if hasattr(runtime, "finish_batch"):
                runtime.finish_batch(number, report, min(phase_deadline, work_deadline))
            finished = clock.monotonic()
            if finished >= phase_deadline:
                raise TimeoutError("batch verification/cleanup exceeded its phase deadline")
            effective_phase = "drain" if phase == "measured" and finished > measured_end else phase
            summary["counts"][effective_phase] += 1
            summary["verifiedPayloadBytes"][effective_phase] += report["payloadBytes"] * (6 if spec.mode == "s3" else 12)
            summary["publishedPayloadBytes"][effective_phase] += report["payloadBytes"] * len(report["publications"])
            store.append("batches.jsonl", {"runId": spec.run_id, "batch": number, "phase": effective_phase, "event": "COMPLETE",
                                      "clientRunId": run_id, "started": started, "finished": finished,
                                      "steps": len(report["steps"]), "publications": len(report["publications"]),
                                      "cleanup": "VERIFIED"})
            store.write("summary.json", summary)
            if hasattr(runtime, "checkpoint_progress"):
                runtime.checkpoint_progress(force=True)
            return started, finished

        warmup_start = clock.monotonic()
        summary["warmupStarted"] = warmup_start
        warmup_end = min(warmup_start + timing.warmup_max, work_deadline)
        next_start = warmup_start
        while clock.monotonic() < warmup_start + timing.warmup_min or summary["counts"]["warmup"] < timing.warmup_batches:
            if summary["counts"]["warmup"] >= timing.warmup_batches:
                runtime.wait_until(min(next_start, warmup_start + timing.warmup_min), "warmup-wait")
                if clock.monotonic() >= warmup_start + timing.warmup_min:
                    break
            if next_start >= warmup_end:
                raise TimeoutError("minimum warm-up work cannot fit its deadline")
            runtime.wait_until(next_start, "warmup-wait")
            if clock.monotonic() >= warmup_start + timing.warmup_min and summary["counts"]["warmup"] >= timing.warmup_batches:
                break
            started, finished = batch("warmup", warmup_end)
            if finished + timing.idle > warmup_end:
                raise TimeoutError("warm-up idle window exceeds its deadline")
            runtime.wait_until(finished + timing.idle, "warmup-idle", number)
            next_start = max(started + timing.cadence, finished + timing.idle)
        runtime.event("initial-reference")
        if clock.monotonic() > warmup_end:
            raise TimeoutError("initial reference exceeded the warm-up deadline")
        measured_start = clock.monotonic()
        measured_end = measured_start + timing.measured
        if measured_end + timing.drain + timing.cooldown > work_deadline:
            raise TimeoutError("remaining run budget cannot cover measurement, drain and cooldown")
        summary["measuredStarted"] = measured_start
        while next_start < measured_end:
            runtime.wait_until(max(next_start, measured_start), "measured-wait")
            if clock.monotonic() >= measured_end:
                break
            started, finished = batch("measured", measured_end + timing.drain)
            runtime.wait_until(max(clock.monotonic(), min(finished + timing.idle, measured_end)), "idle", number)
            next_start = max(started + timing.cadence, finished + timing.idle)
        runtime.wait_until(max(clock.monotonic(), measured_end), "measured-wait")
        if summary["counts"]["measured"] < timing.measured_batches:
            raise RuntimeError("minimum measured batch count was not satisfied")
        runtime.wait_until(clock.monotonic() + timing.cooldown, "final-idle")
        runtime.final()
        if clock.monotonic() > work_deadline:
            raise TimeoutError("final sampling exceeded the remaining work budget")
        summary["executionStatus"] = "SUCCESS"
    except (Exception, KeyboardInterrupt) as error:
        summary["executionStatus"] = "INTERRUPTED" if isinstance(error, (InterruptedError, KeyboardInterrupt)) else "ERROR"
        summary["errors"].append(str(error) or type(error).__name__)
    finally:
        if reserved:
            try:
                shutdown = runtime.shutdown(summary["executionStatus"])
                summary["shutdown"] = shutdown
                if shutdown["status"] != "SUCCESS":
                    raise RuntimeError("supervised shutdown did not complete cleanly")
            except (Exception, KeyboardInterrupt) as error:
                summary["executionStatus"] = "ERROR"
                summary["errors"].append(f"shutdown: {error}")
            try:
                if hasattr(runtime, "validate_evidence"):
                    try:
                        report = runtime.validate_evidence(summary)
                        summary.update(evidenceStatus=report["evidenceStatus"], resourceAssessment=report["resourceAssessment"],
                                       resourceLimitsExceeded=report.get("limitsExceeded", False),
                                       resourceEvidence=report, evidenceScope="IN_POD", kubernetesCollection="PENDING_COLLECTION")
                    except (OSError, ValueError, RuntimeError) as error:
                        summary.update(evidenceStatus="ERROR", evidenceErrors=[str(error)])
                store.write("summary.json", summary)
                runtime.complete()
            except (Exception, KeyboardInterrupt) as error:
                summary["executionStatus"] = "ERROR"
                summary["errors"].append(f"completion: {error}")
                try:
                    store.write("summary.json", summary)
                except OSError:
                    pass  # Nonzero process exit and the supervisor retain independent failure evidence.
        runtime.close()
    if (summary["executionStatus"] != "SUCCESS"
            or (hasattr(runtime, "validate_evidence") and summary["evidenceStatus"] != "COMPLETE")):
        return 1
    return 2 if summary.get("resourceLimitsExceeded", False) or summary["resourceAssessment"] == "GROWTH_DETECTED" else 0


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--spec", type=Path, required=True)
    parser.add_argument("--client", default="/usr/local/bin/pagebroker-smoke.test")
    args = parser.parse_args()
    spec = RunSpec.load(args.spec)
    from stress_evidence import ResourceObservation
    from s3_cleanup import CleanupChannel
    runtime = LocalRuntime(spec, [args.client], ResourceObservation(spec),
                           cleanup=CleanupChannel(spec) if spec.mode == "s3" else None)
    def interrupted(_signum, _frame):
        runtime.signal_received = True
    signal.signal(signal.SIGINT, interrupted)
    signal.signal(signal.SIGTERM, interrupted)
    return run(spec, runtime)


if __name__ == "__main__":
    raise SystemExit(main())
