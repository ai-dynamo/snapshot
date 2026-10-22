# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Direct PageBroker file-transfer campaigns with one continuously observed broker."""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import math
import os
from pathlib import Path
import platform
import re
import selectors
import shutil
import signal
import stat
import subprocess
import sys
import tempfile
import time
import uuid

from snapshot_benchmarks import results
from snapshot_benchmarks.pagebroker_storage import (
    child_environment, read_json, render_s3_config, validate_credentials, write_json,
)

KIND = "snapshot.pagebroker-benchmark/v1"
TAIL_BYTES = 65536
RECORD_BYTES = 16384
SETUP_SECONDS, STARTUP_SECONDS, SHUTDOWN_SECONDS, STOP_MARGIN = 1800, 60, 30, 120
CONFIG_FIELDS = (
    "backend", "transfer_engine", "broker_bin", "client_bin", "work_dir", "output_dir",
    "storage_root", "s3_config", "streamer_config", "duration", "cycle_interval", "sample_interval",
    "cycle_timeout", "cleanup_timeout", "qualification", "cooldown", "payload_bytes", "fixture_dir",
    "credentials_source", "credentials_expire_at",
)


def parse_duration(text: str) -> int:
    match = re.fullmatch(r"([1-9][0-9]*)([smhd])", text)
    if not match or len(match[1]) > 9:
        raise argparse.ArgumentTypeError("duration must be a positive integer followed by s, m, h or d")
    seconds = int(match[1]) * {"s": 1, "m": 60, "h": 3600, "d": 86400}[match[2]]
    if seconds > 365 * 86400:
        raise argparse.ArgumentTypeError("duration exceeds one year")
    return seconds


def add_parser(subparsers) -> None:
    p = subparsers.add_parser("pagebroker", help="Publish/restore checkpoint files through a dedicated native PageBroker.")
    p.add_argument("--backend", choices=("filesystem", "s3"), required=True)
    p.add_argument("--transfer-engine", choices=("posix-copy", "model-streamer"), required=True)
    for name in ("broker-bin", "client-bin", "work-dir"):
        p.add_argument(f"--{name}", type=Path, required=True)
    for name in ("storage-root", "s3-config", "streamer-config"):
        p.add_argument(f"--{name}", type=Path)
    p.add_argument("--output-dir", type=Path, default=results.DEFAULT_RESULTS_ROOT)
    p.add_argument("--duration", type=parse_duration, required=True)
    for name, default in (("cycle-interval", 60), ("sample-interval", 10), ("cycle-timeout", 600), ("cleanup-timeout", 120)):
        p.add_argument(f"--{name}", type=parse_duration, default=default)
    p.add_argument("--qualification", action="store_true")
    p.add_argument("--cooldown", type=parse_duration)
    fixture = p.add_mutually_exclusive_group()
    fixture.add_argument("--payload-bytes", type=int)
    fixture.add_argument("--fixture-dir", type=Path)
    p.add_argument("--credentials-source", choices=("static-environment", "temporary-environment"))
    p.add_argument("--credentials-expire-at")
    p.set_defaults(func=run)


def budget(args) -> int:
    return SETUP_SECONDS + args.duration + args.cycle_timeout + args.cleanup_timeout + args.cooldown + SHUTDOWN_SECONDS + STOP_MARGIN


def digest(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def effective_config(args) -> dict:
    """Only record the command's explicitly nonsecret settings, after validation."""
    config = {key: getattr(args, key) for key in CONFIG_FIELDS}
    return {key: str(value) if isinstance(value, Path) else value for key, value in config.items()}


def validate(args) -> None:
    if platform.system() != "Linux" or platform.machine() not in ("x86_64", "amd64"):
        raise ValueError("PageBroker campaigns require Linux amd64")
    args.cooldown = args.cooldown if args.cooldown is not None else (30 if args.qualification else 7200)
    if not args.qualification and args.cooldown != 7200:
        raise ValueError("normal campaigns require a full two-hour cooldown")
    if args.duration <= 2 * args.cycle_interval or args.cycle_timeout <= 30 or args.cycle_timeout > 7200:
        raise ValueError("duration/cadence must admit three cycles; helper timeout must be 31..7200 seconds")
    args.payload_bytes = None if args.fixture_dir else (args.payload_bytes if args.payload_bytes is not None else (128 << 20) + 37)
    if args.payload_bytes is not None and not 1 <= args.payload_bytes <= (1 << 40) - 4096:
        raise ValueError("payload must fit the native 1 TiB checkpoint limit")
    for key in ("broker_bin", "client_bin", "work_dir", "output_dir", "storage_root", "fixture_dir", "s3_config", "streamer_config"):
        path = getattr(args, key)
        if path is not None:
            setattr(args, key, path.resolve())
    for path in (args.broker_bin, args.client_bin):
        if not path.is_file() or not os.access(path, os.X_OK):
            raise ValueError("broker/client binary must be an executable file")
    if args.fixture_dir and not args.fixture_dir.is_dir():
        raise ValueError("fixture directory does not exist")
    if args.backend == "filesystem":
        if args.transfer_engine != "posix-copy":
            raise ValueError("filesystem requires POSIX; Model Streamer supports S3 only")
        if not args.storage_root or args.s3_config or args.credentials_source or args.credentials_expire_at:
            raise ValueError("filesystem requires --storage-root and no S3 credential/config arguments")
    elif args.transfer_engine != "model-streamer" or not args.s3_config or args.storage_root or not args.credentials_source:
        raise ValueError("S3 requires Model Streamer, --s3-config, explicit credentials and no --storage-root")
    # Reject overlaps before creating any owned/deletion directories.
    roots = [p for p in (args.work_dir, args.storage_root, args.output_dir) if p]
    for i, root in enumerate(roots):
        if any(root.is_relative_to(other) or other.is_relative_to(root) for other in roots[i + 1:]):
            raise ValueError("work, storage and output roots must not overlap")
        if args.fixture_dir and (args.fixture_dir.is_relative_to(root) or root.is_relative_to(args.fixture_dir)):
            raise ValueError("source corpus must not overlap benchmark-owned roots")
    if args.backend == "s3":
        validate_credentials(args.credentials_source, args.credentials_expire_at,
                             dt.datetime.now(dt.timezone.utc) + dt.timedelta(seconds=budget(args)))
    # Explicit config gets priority in the daemon. Inherit no unrecorded tuning:
    # require users to move tuning to the nonsecret JSON file for reproducibility.
    if any(k.startswith("RUNAI_STREAMER_") for k in os.environ):
        raise ValueError("move inherited RUNAI_STREAMER_* tuning into --streamer-config before running")


def proc_start_ticks(text: str) -> int:
    return int(text[text.rfind(")") + 2:].split()[19])


def proc_values(text: str) -> dict[str, int]:
    values = {}
    for line in text.splitlines():
        parts = line.split()
        if len(parts) >= 2 and parts[0].endswith(":") and parts[1].isdigit():
            values[parts[0][:-1]] = int(parts[1]) * (1024 if len(parts) == 3 and parts[2] == "kB" else 1)
    return values


def staging_tree(path: Path) -> dict:
    if not path.is_dir() or path.is_symlink():
        raise ValueError("owned staging root is missing or replaced")
    def walk_error(error):
        if not isinstance(error, FileNotFoundError):
            raise error
    tree = {}
    for root, dirs, files in os.walk(path, followlinks=False, onerror=walk_error):
        for name in dirs + files:
            entry = Path(root) / name
            try:
                info = entry.lstat()
            except FileNotFoundError:  # concurrent transaction completion
                continue
            if not (stat.S_ISDIR(info.st_mode) or stat.S_ISREG(info.st_mode)):
                raise ValueError("unexpected staging type")
            tree[str(entry.relative_to(path))] = [stat.S_IFMT(info.st_mode), stat.S_IMODE(info.st_mode),
                                                info.st_size if stat.S_ISREG(info.st_mode) else 0]
            if len(tree) > 20010:
                raise ValueError("staging exceeds entry observation bound")
    return tree


def sample_broker(identity: dict, staging_path: Path, phase: str, cycle_id: str | None) -> dict:
    proc = Path("/proc") / str(identity["pid"])
    if proc_start_ticks((proc / "stat").read_text()) != identity["start_ticks"]:
        raise RuntimeError("broker process identity changed")
    status = proc_values((proc / "status").read_text())
    smaps = proc_values((proc / "smaps_rollup").read_text())
    tree = staging_tree(staging_path)
    return {"phase": phase, "cycle_id": cycle_id, "rss": status["VmRSS"], "hwm_rss": status["VmHWM"],
            "pss": smaps["Pss"], "private_bytes": smaps["Private_Clean"] + smaps["Private_Dirty"],
            "threads": status["Threads"], "fds": len(list((proc / "fd").iterdir())),
            "staging_entries": len(tree), "staging_bytes": sum(v[2] for v in tree.values())}


def launch_broker(args, paths, env) -> subprocess.Popen:
    command = [str(args.broker_bin), str(paths["socket"]), str(paths["staging"]), str(paths["storage"]),
               "--custom-storage-engine", "off"]
    for key, option in (("s3_config", "--storage-config"), ("streamer_config", "--model-streamer-config")):
        if key in paths:
            command += [option, str(paths[key])]
    return subprocess.Popen(command, shell=False, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE)


class Interrupted(Exception):
    pass


class Campaign:
    """Owns one concrete campaign, its processes, streams and bounded observations."""

    def __init__(self, args):
        self.args = args
        self.run_id = uuid.uuid4().hex
        self.paths = {}
        self.started = time.monotonic()
        self.phase, self.cycle_id = "starting", None
        self.selector = selectors.DefaultSelector()
        self.tails = {}
        self.broker = None
        self.identity = {}
        self.stop_requested = False
        self.last_sample = None
        self.next_sample = self.started
        self.last_space_check = self.started
        self.summary = dict(kind=KIND, run_id=self.run_id, backend=args.backend, transfer_engine=args.transfer_engine,
                            qualification=args.qualification, status="failed", requested_active_seconds=args.duration,
                            actual_active_seconds=0, drain_seconds=0, idle_seconds=0, attempted_cycles=0,
                            measured_cycles=0, drain_cycles=0, failed_cycles=0, published_bytes=0, verified_bytes=0,
                            drain_published_bytes=0, drain_verified_bytes=0, identity_unchanged=False,
                            cleanup_confirmed=False, sampling_complete=False, resources={}, resource_assessment="not_assessed")
        self.credentials = {}
        self.active_end = None
        self.active_start = self.idle_start = None

    def write_run(self) -> None:
        self.run_record["paths"] = {key: str(value) for key, value in self.paths.items()}
        write_json(self.paths["output"] / "run.json", self.run_record)

    def record(self, file: str, value: dict) -> None:
        value = dict(value, kind=KIND, run_id=self.run_id, phase=self.phase, cycle_id=self.cycle_id,
                     utc=dt.datetime.now(dt.timezone.utc).isoformat(), elapsed_seconds=time.monotonic() - self.started)
        data = json.dumps(value, separators=(",", ":"), allow_nan=False).encode() + b"\n"
        if len(data) > RECORD_BYTES:
            raise ValueError("observation record exceeds limit")
        with (self.paths["output"] / file).open("ab") as f:
            f.write(data)

    def watch(self, child) -> None:
        for stream in (child.stdout, child.stderr):
            os.set_blocking(stream.fileno(), False)
            self.selector.register(stream, selectors.EVENT_READ)
            self.tails[stream] = bytearray()

    def drain(self) -> None:
        for key, _ in self.selector.select(timeout=0):
            stream = key.fileobj
            data = os.read(stream.fileno(), 16384)
            if data:
                tail = self.tails[stream]
                tail.extend(data)
                del tail[:-TAIL_BYTES]
            else:
                self.selector.unregister(stream)

    def forget(self, child) -> None:
        for stream in (child.stdout, child.stderr):
            try:
                self.selector.unregister(stream)
            except KeyError:
                pass
            self.tails.pop(stream, None)
            stream.close()

    def logs(self) -> None:
        if self.broker:
            tail = b"Incomplete diagnostic tail; earlier output discarded.\n"
            tail += b"\n".join(bytes(self.tails.get(s, b"")) for s in (self.broker.stdout, self.broker.stderr))
            # SDK/broker diagnostics must never persist credential values.
            for value in self.credentials.values():
                tail = tail.replace(value.encode(), b"[REDACTED]")
            (self.paths["output"] / "broker.log").write_bytes(tail)

    def tick(self, *, force_sample=False) -> None:
        self.drain()
        if self.stop_requested:
            raise Interrupted("interrupted by signal")
        now = time.monotonic()
        if self.phase == "active" and self.active_end is not None and now >= self.active_end:
            self.phase = "drain"
        if self.broker:
            # We exclusively own this child: poll retains its exit status after
            # reaping, so PID reuse cannot hide an exit or authorize replacement.
            if self.broker.poll() is not None:
                raise RuntimeError("owned broker exited")
            if now >= self.next_sample or force_sample:
                if self.last_sample is not None and now - self.last_sample > 3 * self.args.sample_interval:
                    raise RuntimeError("resource sampling gap exceeded three intervals")
                sample = sample_broker(self.identity, self.paths["staging"], self.phase, self.cycle_id)
                self.record("resources.jsonl", sample)
                resources = self.summary["resources"]
                resources.setdefault("first", sample)
                resources["final"] = sample
                for metric in ("rss", "pss"):
                    key = f"peak_{metric}"
                    resources[key] = max(resources.get(key, 0), sample[metric])
                self.last_sample, self.next_sample = now, now + self.args.sample_interval
        if now - self.last_space_check >= 60:
            self.require_space(64 << 20)
            self.last_space_check = now

    def pause(self, deadline: float) -> None:
        while time.monotonic() < deadline:
            self.tick()
            time.sleep(min(0.1, max(0, deadline - time.monotonic())))

    def wait_child(self, child, deadline: float) -> int:
        self.watch(child)
        try:
            while child.poll() is None:
                self.tick()
                if time.monotonic() >= deadline:
                    raise TimeoutError("benchmark helper exceeded its deadline")
                time.sleep(0.05)
            self.tick()
            return child.returncode
        finally:
            if child.poll() is None:
                child.terminate()
                end = time.monotonic() + 35
                while child.poll() is None and time.monotonic() < end:
                    self.drain()
                    # Preserve observation during cooperative cancellation; an
                    # existing health failure must not skip forced child cleanup.
                    try:
                        self.tick()
                    except Exception:
                        pass
                    time.sleep(0.05)
                if child.poll() is None:
                    child.kill()
                child.wait(timeout=5)
            if child.returncode:
                diagnostic = b"\n".join(bytes(self.tails.get(s, b"")) for s in (child.stdout, child.stderr))
                for value in self.credentials.values():
                    diagnostic = diagnostic.replace(value.encode(), b"[REDACTED]")
                (self.paths["output"] / "helper.log").write_bytes(diagnostic)
            self.forget(child)

    def helper(self, request: dict, seconds: float, *, worker=False) -> dict:
        if seconds <= 0:
            raise TimeoutError("no helper deadline remains")
        input_path, output_path = self.paths["work"] / "request.json", self.paths["work"] / "report.json"
        output_path.unlink(missing_ok=True)
        write_json(input_path, request)
        command = ([sys.executable, "-m", "snapshot_benchmarks.pagebroker_storage"] if worker else [str(self.args.client_bin)])
        command += ["--request", str(input_path), "--report", str(output_path)]
        child = subprocess.Popen(command, shell=False, env=child_environment(self.credentials if worker else None),
                                 stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        status = self.wait_child(child, time.monotonic() + seconds)
        out = read_json(output_path, RECORD_BYTES)
        for key in ("run_id", "cycle_id", "backend", "action"):
            if out.get(key) != request.get(key):
                raise ValueError("helper report identity mismatch")
        if not worker and (type(out.get("schema_version")) is not int or out["schema_version"] != 1
                           or out.get("transfer_engine") != request.get("transfer_engine")):
            raise ValueError("helper report schema/engine mismatch")
        if status or out.get("status") != "success":
            if request["action"] == "probe" and out.get("retryable"):
                return out
            raise RuntimeError("helper failed: " + str(out.get("error_message", out.get("error", "unspecified")))[:2048])
        return out

    def request(self, action: str, seconds: int) -> dict:
        return dict(schema_version=1, action=action, run_id=self.run_id, cycle_id=self.cycle_id or "",
                    backend=self.args.backend, transfer_engine=self.args.transfer_engine,
                    socket=str(self.paths["socket"]), staging_root=str(self.paths["staging"]), timeout_seconds=seconds)

    def worker(self, action: str, seconds: float, publication: Path | None = None) -> dict:
        request = dict(action=action, run_id=self.run_id, cycle_id=self.cycle_id or "", backend=self.args.backend)
        if self.args.backend == "s3":
            request["config"] = str(self.paths["s3_config"])
        else:
            request.update(publication=str(publication), owned_root=str(self.paths["storage"]))
        return self.helper(request, seconds, worker=True)

    def require_space(self, required: int) -> None:
        if shutil.disk_usage(self.paths["output"]).free < required:
            raise RuntimeError("insufficient output space for bounded campaign records")

    def setup(self) -> None:
        args = self.args
        for root in (args.work_dir, args.output_dir):
            root.mkdir(parents=True, exist_ok=True)
        work = args.work_dir / self.run_id
        work.mkdir(mode=0o700)
        self.paths = dict(work=work, staging=work / "staging", storage=work / "unused-storage",
                          output=results.invocation_dir(args.output_dir / "pagebroker", git_sha=None))
        for name in ("staging", "storage"):
            self.paths[name].mkdir(mode=0o700)
        if args.storage_root:
            args.storage_root.mkdir(parents=True, exist_ok=True)
            self.paths["storage"] = args.storage_root / self.run_id
            self.paths["storage"].mkdir(mode=0o700)
        socket_dir = Path(tempfile.mkdtemp(prefix="pbbench-", dir="/tmp"))
        self.paths.update(socket_dir=socket_dir, socket=socket_dir / "broker.sock")
        rows = math.ceil(budget(args) / args.sample_interval) + 3 * math.ceil(args.duration / args.cycle_interval) + 32
        self.require_space(rows * RECORD_BYTES + (4 << 20) + (64 << 20))
        self.run_record = dict(kind=KIND, run_id=self.run_id, qualification=args.qualification,
                               backend=args.backend, transfer_engine=args.transfer_engine,
                               config=effective_config(args), budget_seconds=budget(args))
        self.write_run()
        setup_end = time.monotonic() + SETUP_SECONDS
        self.run_record["binary_sha256"] = {"broker": digest(args.broker_bin), "client": digest(args.client_bin)}
        revision = subprocess.run(["git", "rev-parse", "HEAD"], capture_output=True, text=True, timeout=5, check=False)
        self.run_record["checkout_sha"] = revision.stdout.strip() if revision.returncode == 0 else None
        self.run_record["work_filesystem"] = filesystem_type(work)
        self.fixture = None
        if args.fixture_dir:
            self.fixture = self.paths["output"] / "fixture.json"
            request = self.request("inspect", max(1, int(setup_end - time.monotonic())))
            request.update(fixture_dir=str(args.fixture_dir), fixture_manifest=str(self.fixture))
            info = self.helper(request, setup_end - time.monotonic())
            self.fixture_hash, self.fixture_bytes = info["manifest_sha256"], info["payload_bytes"]
            self.run_record["fixture"] = dict(manifest=str(self.fixture), manifest_sha256=self.fixture_hash,
                                              payload_bytes=self.fixture_bytes)
            self.write_run()
        else:
            self.fixture_bytes = args.payload_bytes + 4096
        for path in (self.paths["staging"], self.paths["storage"]):
            if shutil.disk_usage(path).free < 2 * self.fixture_bytes + (16 << 20):
                raise ValueError("insufficient staging/storage capacity")
        if args.streamer_config:
            self.paths["streamer_config"] = work / "streamer.json"
            write_json(self.paths["streamer_config"], read_json(args.streamer_config))
        if args.backend == "s3":
            self.paths["s3_config"] = work / "s3.json"
            self.s3_config = render_s3_config(args.s3_config, self.run_id, self.paths["s3_config"])
            if args.cycle_timeout >= self.s3_config.get("limits", {}).get("transactionSeconds", 7200):
                raise ValueError("helper deadline must fit the native transaction lifetime")
            self.credentials = validate_credentials(args.credentials_source, args.credentials_expire_at,
                                                    dt.datetime.now(dt.timezone.utc) + dt.timedelta(seconds=budget(args)))
            self.run_record["s3_config"] = self.s3_config
            self.summary["retained_artifact_location"] = f"s3://{self.s3_config['bucket']}/{self.s3_config['prefix']}/"
            self.write_run()
            self.worker("preflight", min(args.cleanup_timeout, setup_end - time.monotonic()))
        self.run_record["config_sha256"] = {k: digest(v) for k, v in self.paths.items() if k.endswith("config")}
        if time.monotonic() >= setup_end:
            raise TimeoutError("setup deadline exceeded")
        if self.credentials:
            validate_credentials(args.credentials_source, args.credentials_expire_at,
                                 dt.datetime.now(dt.timezone.utc) + dt.timedelta(seconds=budget(args)), self.credentials)
        self.broker = launch_broker(args, self.paths, child_environment(self.credentials))
        self.watch(self.broker)
        proc = Path("/proc") / str(self.broker.pid)
        self.identity = dict(pid=self.broker.pid, start_ticks=proc_start_ticks((proc / "stat").read_text()),
                             boot_id=Path("/proc/sys/kernel/random/boot_id").read_text().strip())
        self.summary["broker_identity"] = self.identity
        self.run_record["broker_identity"] = self.identity
        self.write_run()
        startup_end = min(setup_end, time.monotonic() + STARTUP_SECONDS)
        while True:
            info = self.helper(self.request("probe", max(1, int(startup_end - time.monotonic()))), max(0.1, startup_end - time.monotonic()))
            if info["status"] == "success":
                break
            if time.monotonic() >= startup_end:
                raise TimeoutError("broker readiness deadline exceeded")
            self.pause(min(startup_end, time.monotonic() + 0.1))
        if (proc / "exe").resolve() != args.broker_bin:
            raise RuntimeError("broker binary must be the direct child, without a launcher wrapper")
        self.baseline = staging_tree(self.paths["staging"])
        self.run_record["staging_baseline"] = self.baseline
        self.write_run()
        self.tick(force_sample=True)
        self.summary["resources"]["first"] = self.summary["resources"]["final"]

    def check_quiescent(self) -> None:
        self.tick()
        if staging_tree(self.paths["staging"]) != self.baseline:
            raise RuntimeError("staging did not return to its exact readiness baseline")

    def one_cycle(self) -> dict:
        args = self.args
        publication = self.paths["storage"] / self.cycle_id
        request = self.request("cycle", args.cycle_timeout)
        request.update(destination=str(publication), store_id=getattr(self, "s3_config", {}).get("storeId", ""))
        if self.fixture:
            request.update(fixture_dir=str(args.fixture_dir), fixture_manifest=str(self.fixture), manifest_sha256=self.fixture_hash)
            expected = self.fixture_bytes
        else:
            request["payload_bytes"] = args.payload_bytes
            expected = args.payload_bytes + 7 + len(f"version: 1\nrun: {self.run_id}\ncycle: {self.cycle_id}\n".encode())
        self.summary["cleanup_confirmed"] = False
        self.summary["retained_artifact_location"] = (f"s3://{self.s3_config['bucket']}/{self.s3_config['prefix']}/"
                                                       if args.backend == "s3" else str(publication))
        out = self.helper(request, args.cycle_timeout)
        required = dict(publications_completed=1, restores_completed=2, transactions_closed=True,
                        payload_bytes=expected, verified_bytes=2 * expected)
        if any(type(out.get(k)) is not type(v) or out[k] != v for k, v in required.items()):
            raise ValueError("cycle report counts/quiescence mismatch")
        if args.backend == "s3":
            artifact = out.get("published_artifact", {})
            if artifact.get("store_id") != self.s3_config["storeId"] or not artifact.get("artifact_handle"):
                raise ValueError("S3 descriptor missing or names another store")
        cleanup = self.worker("cleanup", args.cleanup_timeout, publication)
        if cleanup.get("empty_verified") is not True:
            raise ValueError("artifact cleanup was not verified")
        self.check_quiescent()
        self.summary["cleanup_confirmed"] = True
        self.summary.pop("retained_artifact_location", None)
        return dict(report=out, cleanup=cleanup)

    def workload(self) -> None:
        start = time.monotonic()
        self.active_start = start
        end = start + self.args.duration
        self.active_end = end
        self.phase = "active"
        next_cycle = start
        while time.monotonic() < end:
            self.pause(min(next_cycle, end))
            if time.monotonic() >= end:
                break
            cycle_start = time.monotonic()
            self.cycle_id = uuid.uuid4().hex
            self.summary["attempted_cycles"] += 1
            self.record("cycles.jsonl", {"event": "start"})
            try:
                outcome = self.one_cycle()
            except Exception:
                self.summary["failed_cycles"] += 1
                self.record("cycles.jsonl", {"event": "outcome", "status": "failed"})
                raise
            measured = time.monotonic() <= end
            self.phase = "active" if measured else "drain"
            counter = "measured_cycles" if measured else "drain_cycles"
            self.summary[counter] += 1
            for key, value in (("published_bytes", outcome["report"]["payload_bytes"]), ("verified_bytes", outcome["report"]["verified_bytes"])):
                self.summary[("" if measured else "drain_") + key] += value
            self.record("cycles.jsonl", dict(event="outcome", status="success", measured=measured, **outcome))
            self.tick(force_sample=True)
            if self.summary["attempted_cycles"] == 1:
                self.summary["resources"]["after_first_cycle"] = self.summary["resources"]["final"]
            self.logs()
            self.require_space(64 << 20)
            next_cycle = cycle_start + self.args.cycle_interval
        drained = time.monotonic()
        self.summary.update(actual_active_seconds=min(drained - start, self.args.duration), drain_seconds=max(0, drained - end))
        if self.summary["measured_cycles"] < 3:
            raise RuntimeError("fewer than three complete measured cycles")
        self.check_quiescent()
        self.phase, self.cycle_id = "idle", None
        self.tick(force_sample=True)
        idle_start = time.monotonic()
        self.idle_start = idle_start
        self.pause(idle_start + self.args.cooldown)
        self.summary["idle_seconds"] = time.monotonic() - idle_start
        self.check_quiescent()
        self.tick(force_sample=True)
        self.summary.update(identity_unchanged=True, sampling_complete=True)

    def shutdown(self) -> None:
        self.phase = "stopping"
        if not self.broker:
            return
        alive = self.broker.poll() is None
        if alive:
            self.broker.terminate()
            end = time.monotonic() + SHUTDOWN_SECONDS
            while self.broker.poll() is None and time.monotonic() < end:
                self.drain()
                time.sleep(0.05)
        forced = self.broker.poll() is None
        if forced:
            self.broker.kill()
        self.broker.wait(timeout=5)
        self.drain()
        self.logs()
        if forced or not alive or self.broker.returncode != 0:
            raise RuntimeError("broker shutdown was not graceful")

    def execute(self) -> int:
        old = {sig: signal.signal(sig, lambda *_: setattr(self, "stop_requested", True)) for sig in (signal.SIGINT, signal.SIGTERM)}
        error = None
        try:
            self.setup()
            self.workload()
            self.summary["status"] = "success"
        except Exception as exc:
            error = str(exc)[:2048]
            self.summary["status"] = "interrupted" if isinstance(exc, Interrupted) or self.stop_requested else "failed"
            now = time.monotonic()
            if self.active_start is not None:
                self.summary["actual_active_seconds"] = min(now - self.active_start, self.args.duration)
                self.summary["drain_seconds"] = max(0, (self.idle_start if self.idle_start is not None else now) - self.active_end)
            if self.idle_start is not None:
                self.summary["idle_seconds"] = now - self.idle_start
        finally:
            try:
                self.shutdown()
            except Exception as exc:
                error = (error + "; " if error else "") + str(exc)[:2048]
                if self.summary["status"] == "success":
                    self.summary["status"] = "failed"
            for sig, handler in old.items():
                signal.signal(sig, handler)
            if self.broker:
                self.forget(self.broker)
            self.selector.close()
            if "socket_dir" in self.paths:
                shutil.rmtree(self.paths["socket_dir"])
        if self.stop_requested:
            self.summary["status"] = "interrupted"
        if error:
            for value in self.credentials.values():
                error = error.replace(value, "[REDACTED]")
            self.summary["error"] = error
        if "output" in self.paths:
            write_json(self.paths["output"] / "summary.json", self.summary)
            print(f"{self.summary['status']}: {self.paths['output']}")
        if error:
            print(error, file=sys.stderr)
        return {"success": 0, "failed": 1, "interrupted": 130}[self.summary["status"]]


def filesystem_type(path: Path) -> str:
    # mountinfo escapes spaces and backslashes; pick the deepest matching mount.
    matches = []
    for line in Path("/proc/self/mountinfo").read_text().splitlines():
        before, after = line.split(" - ", 1)
        mount = re.sub(r"\\([0-7]{3})", lambda m: chr(int(m[1], 8)), before.split()[4])
        if path.is_relative_to(mount):
            matches.append((len(mount), after.split()[0]))
    return max(matches)[1]


def run(args) -> int:
    try:
        validate(args)
        return Campaign(args).execute()
    except Exception as exc:
        print(f"pagebroker benchmark failed: {exc}", file=sys.stderr)
        return 1
