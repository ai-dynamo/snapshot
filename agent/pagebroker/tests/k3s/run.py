#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Run the smoke client and shut down its peer in a shared process namespace."""

import argparse
from collections import Counter
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import select
import signal
import socket
import struct
import subprocess
import tempfile
import time
import uuid

from monitor import ResourceMonitor, validate_timing


def expected_steps(mode):
    engines = ["model-streamer"] if mode == "s3" else ["posix-copy", "model-streamer"]
    steps = ["ready"]
    for engine in engines:
        for cycle in range(1, 4):
            prefix = f"{engine}/cycle-{cycle}"
            steps.append(prefix + "/publish")
            if mode == "s3":
                steps.append(prefix + "/metadata")
            steps.extend(prefix + f"/restore-{number}" for number in (1, 2))
        steps.append(engine + "/abort-prepared")
    if mode == "s3":
        steps.append("model-streamer/wrong-store")
    return steps


def validate_report(path, mode, store_id):
    if path.stat().st_size > 1024 * 1024:
        raise ValueError("client report exceeds 1 MiB")
    report = json.loads(path.read_text())
    uuid.UUID(report["runId"])
    if report["status"] != "SUCCESS" or report["config"]["mode"] != mode:
        raise ValueError("client did not report success for the requested mode")
    if report["payloadBytes"] != (128 << 20) + 37:
        raise ValueError("client used an unexpected payload size")
    steps = report["steps"]
    if [step["name"] for step in steps] != expected_steps(mode):
        raise ValueError("client skipped, duplicated or reordered required steps")
    if any(step["status"] != "SUCCESS" or step.get("error") for step in steps):
        raise ValueError("client report contains an unsuccessful step")
    publications = report["publications"]
    expected = {"model-streamer": 3} if mode == "s3" else {"posix-copy": 3, "model-streamer": 3}
    if Counter(item["engine"] for item in publications) != expected:
        raise ValueError("client did not complete all publications")
    if mode == "s3":
        artifacts = [item["artifact"] for item in publications]
        if report["config"]["storeId"] != store_id or any(item["store_id"] != store_id for item in artifacts):
            raise ValueError("client report names a different S3 store")
        handles = [item["artifact_handle"] for item in artifacts]
        if any(not handle for handle in handles) or len(set(handles)) != 3:
            raise ValueError("client report contains missing or duplicate artifact handles")
    else:
        destinations = [item["destination"] for item in publications]
        if any(not path for path in destinations) or len(set(destinations)) != 6:
            raise ValueError("client report contains missing or duplicate destinations")
    return report


def write_json(path, value):
    with tempfile.NamedTemporaryFile(mode="w", dir=path.parent, delete=False) as output:
        temporary = Path(output.name)
        try:
            json.dump(value, output, indent=2)
            output.write("\n")
            output.flush()
            os.replace(temporary, path)
        finally:
            temporary.unlink(missing_ok=True)


def exited(pidfd):
    return bool(select.select([pidfd], [], [], 0)[0])


def wait_for_peer(path, timeout):
    deadline = time.monotonic() + timeout
    while True:
        try:
            with socket.socket(socket.AF_UNIX) as connection:
                connection.settimeout(min(1, timeout))
                connection.connect(path)
                pid, uid, _ = struct.unpack("3i", connection.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, 12))
                if pid <= 1 or pid == os.getpid() or uid != os.geteuid():
                    raise RuntimeError("PageBroker must share the client's process namespace and UID")
                pidfd = os.pidfd_open(pid)
                if exited(pidfd):
                    os.close(pidfd)
                    raise RuntimeError("PageBroker exited during startup")
                return pid, pidfd
        except (FileNotFoundError, ConnectionRefusedError, socket.timeout):
            if time.monotonic() >= deadline:
                raise TimeoutError("PageBroker socket did not become available") from None
            time.sleep(0.1)


def stop_peer(pidfd, timeout):
    if exited(pidfd):
        raise RuntimeError("PageBroker exited before the requested shutdown")
    signal.pidfd_send_signal(pidfd, signal.SIGTERM)
    if not select.select([pidfd], [], [], timeout)[0]:
        signal.pidfd_send_signal(pidfd, signal.SIGKILL)
        raise TimeoutError("PageBroker did not exit after SIGTERM; sent SIGKILL")


def stop_client(process):
    if process is None or process.poll() is not None:
        return
    os.killpg(process.pid, signal.SIGTERM)
    try:
        process.wait(timeout=5)
    except subprocess.TimeoutExpired:
        os.killpg(process.pid, signal.SIGKILL)
        process.wait(timeout=5)


def run(client, environment, results, startup_timeout=60, client_timeout=1500, shutdown_timeout=20,
        sample_interval=1, settle_seconds=5):
    summary = {"status": "RUNNING", "started": datetime.now(timezone.utc).isoformat()}
    peer = None
    process = None
    monitor = None
    reserved = False
    errors = []
    try:
        results.mkdir(parents=True, exist_ok=True)
        with (results / "summary.json").open("x") as output:
            json.dump(summary, output)
        reserved = True
        validate_timing(sample_interval, settle_seconds)
        pid, peer = wait_for_peer(environment["PAGEBROKER_SMOKE_SOCKET"], startup_timeout)
        summary["pagebrokerPid"] = pid
        mode = environment["PAGEBROKER_SMOKE_MODE"]
        if mode not in ("filesystem", "s3"):
            raise ValueError("PAGEBROKER_SMOKE_MODE must be filesystem or s3")
        report_path = results / "client-report.json"
        if report_path.exists():
            raise ValueError("client report already exists; use a fresh results volume")
        monitor = ResourceMonitor(pid, peer, environment["PAGEBROKER_SMOKE_STAGING_ROOT"],
                                  results / "resources.jsonl", sample_interval)
        monitor.sample("baseline", force=True)
        environment = environment | {"PAGEBROKER_SMOKE_REPORT": str(report_path)}
        print(f"Running {mode} smoke test against PageBroker PID {pid}", flush=True)
        with (results / "client.log").open("x") as log:
            process = subprocess.Popen(client, env=environment, stdout=log, stderr=subprocess.STDOUT,
                                       start_new_session=True)
            deadline = time.monotonic() + client_timeout
            monitor.sample("workload", force=True)
            while process.poll() is None:
                if exited(peer):
                    raise RuntimeError("PageBroker exited while the client was running")
                if time.monotonic() >= deadline:
                    raise TimeoutError("smoke client exceeded its deadline")
                monitor.sample("workload")
                time.sleep(0.1)
            summary["clientExitCode"] = process.returncode
            if process.returncode:
                raise RuntimeError(f"smoke client exited with code {process.returncode}")
        report = validate_report(report_path, mode, environment.get("PAGEBROKER_SMOKE_STORE_ID", ""))
        summary.update(mode=mode, clientRunId=report["runId"], stepsVerified=len(report["steps"]),
                       publicationsVerified=len(report["publications"]))
        monitor.sample("after-client", force=True)
        settle_deadline = time.monotonic() + settle_seconds
        while time.monotonic() < settle_deadline:
            monitor.check_alive()
            monitor.sample("settling")
            time.sleep(min(0.1, max(0, settle_deadline - time.monotonic())))
        monitor.sample("settled", force=True)
    except (Exception, KeyboardInterrupt) as error:
        errors.append(str(error) or type(error).__name__)
    finally:
        try:
            stop_client(process)
        except Exception as error:
            errors.append(f"stop client: {error}")
        if monitor is not None:
            try:
                if errors and not monitor.error:
                    monitor.sample("failure", force=True)
            except Exception as error:
                errors.append(str(error))
            finally:
                summary["resources"] = monitor.summary() | {"settleSeconds": settle_seconds}
                try:
                    monitor.close()
                except OSError as error:
                    errors.append(f"close resource log: {error}")
        else:
            summary["resources"] = {"status": "NOT_STARTED", "leakAssessment": "NOT_ASSESSED"}
        if peer is not None:
            try:
                stop_peer(peer, shutdown_timeout)
                summary["pagebrokerShutdown"] = "SIGTERM"
            except Exception as error:
                errors.append(f"stop PageBroker: {error}")
            finally:
                os.close(peer)
        summary.update(status="ERROR" if errors else "SUCCESS", errors=errors,
                       finished=datetime.now(timezone.utc).isoformat())
        if reserved:
            try:
                write_json(results / "summary.json", summary)
            except OSError as error:
                errors.append(f"save summary: {error}")
        print("ERROR: " + "; ".join(errors) if errors else "SUCCESS: client verified and PageBroker stopped", flush=True)
    return 1 if errors else 0


def interrupted(signum, _frame):
    raise InterruptedError(f"launcher received signal {signum}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--client", default="/usr/local/bin/pagebroker-smoke.test")
    parser.add_argument("--results", type=Path, default=Path("/results"))
    parser.add_argument("--sample-interval", type=float, default=1, help="Resource sampling interval in seconds (0.1–60)")
    parser.add_argument("--settle-seconds", type=float, default=5, help="Idle sampling after a successful client run (0–60)")
    args = parser.parse_args()
    if not args.results.is_absolute():
        parser.error("--results must be an absolute path")
    try:
        validate_timing(args.sample_interval, args.settle_seconds)
    except ValueError as error:
        parser.error(str(error))
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    return run([args.client, "-test.run=^TestPageBrokerK3sSmoke$", "-test.v", "-test.timeout=25m"],
               dict(os.environ), args.results, sample_interval=args.sample_interval, settle_seconds=args.settle_seconds)


if __name__ == "__main__":
    raise SystemExit(main())
