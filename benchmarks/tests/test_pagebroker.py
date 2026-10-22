# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

import argparse
import datetime as dt
import io
import json
from pathlib import Path
import subprocess
import sys
from types import SimpleNamespace

import pytest
from botocore.paginate import Paginator
from botocore.session import Session

from snapshot_benchmarks import cli, pagebroker as pb, results
from snapshot_benchmarks import pagebroker_storage as s


def arguments(tmp_path, *extra):
    return cli.build_parser().parse_args([
        "pagebroker", "--backend", "filesystem", "--transfer-engine", "posix-copy",
        "--broker-bin", sys.executable, "--client-bin", sys.executable,
        "--work-dir", str(tmp_path / "work"), "--storage-root", str(tmp_path / "storage"),
        "--output-dir", str(tmp_path / "output"), "--duration", "30m", *extra,
    ])


@pytest.fixture
def campaign(tmp_path):
    args = arguments(tmp_path)
    pb.validate(args)
    instance = pb.Campaign(args)
    instance.paths = dict(output=tmp_path, work=tmp_path, staging=tmp_path, storage=tmp_path, socket=tmp_path / "sock")
    yield instance
    instance.selector.close()


@pytest.mark.parametrize("value,seconds", [("30m", 1800), ("4h", 14400), ("3d", 259200), ("7d", 604800)])
def test_duration(value, seconds):
    assert pb.parse_duration(value) == seconds


@pytest.mark.parametrize("value", ["-1h", "0s", "1.5h", "3", "2w", "9999999999999999d", "366d"])
def test_invalid_duration(value):
    with pytest.raises(argparse.ArgumentTypeError):
        pb.parse_duration(value)


@pytest.mark.parametrize("extra", [["--cooldown", "30s"], ["--cycle-timeout", "30s"],
                                    ["--duration", "1m"], ["--payload-bytes", "0"],
                                    ["--credentials-source", "static-environment"],
                                    ["--transfer-engine", "model-streamer"]])
def test_reject_before_side_effects(tmp_path, extra):
    with pytest.raises(ValueError):
        pb.validate(arguments(tmp_path, *extra))
    assert not list(tmp_path.iterdir())


def test_s3_posix_and_overlaps(tmp_path):
    args = arguments(tmp_path)
    args.backend, args.storage_root = "s3", None
    args.s3_config, args.credentials_source = tmp_path / "s3.json", "static-environment"
    with pytest.raises(ValueError, match="Model Streamer"):
        pb.validate(args)
    args = arguments(tmp_path)
    args.output_dir = args.work_dir / "results"
    with pytest.raises(ValueError, match="overlap"):
        pb.validate(args)


@pytest.mark.parametrize("fixture", ["default", "generated", "corpus"])
def test_effective_configuration_survives_setup_failure(tmp_path, monkeypatch, fixture):
    extra = [] if fixture == "default" else ["--duration", "7d", "--cycle-timeout", "91s", "--cleanup-timeout", "37s"]
    if fixture == "generated":
        extra += ["--payload-bytes", "12345"]
    if fixture == "corpus":
        source = tmp_path / "source"
        source.mkdir()
        extra += ["--fixture-dir", str(source)]
    args = arguments(tmp_path, *extra)
    pb.validate(args)
    assert set(pb.CONFIG_FIELDS) == set(vars(args)) - {"command", "func"}
    for key, value in KEYS.items():
        monkeypatch.setenv(key, value)
    monkeypatch.setattr(pb, "digest", lambda _: "b" * 64)
    monkeypatch.setattr(pb.Campaign, "helper", lambda *_: dict(manifest_sha256="c" * 64, payload_bytes=987))
    monkeypatch.setattr(pb, "launch_broker", lambda *_: (_ for _ in ()).throw(RuntimeError("intentional setup failure")))
    campaign = pb.Campaign(args)
    assert campaign.execute() == 1
    record = json.loads((campaign.paths["output"] / "run.json").read_text())
    assert record["config"] == {k: str(v) if isinstance(v, Path) else v for k, v in vars(args).items()
                                if k not in ("func", "command")}
    expected = {"default": ((128 << 20) + 37, 1800, 600, 120), "generated": (12345, 604800, 91, 37),
                "corpus": (None, 604800, 91, 37)}[fixture]
    assert tuple(record["config"][k] for k in ("payload_bytes", "duration", "cycle_timeout", "cleanup_timeout")) == expected
    if fixture == "corpus":
        assert record["fixture"] == dict(manifest=str(campaign.fixture), manifest_sha256="c" * 64, payload_bytes=987)
    assert not any(value in json.dumps(record) for value in KEYS.values())
    assert not campaign.summary["attempted_cycles"]


class Clock:
    now = 0.0

    def monotonic(self):
        return self.now


class ScheduledCampaign(pb.Campaign):
    def __init__(self, args, clock, tmp_path, cycle_seconds=1):
        super().__init__(args)
        self.clock, self.cycle_seconds = clock, cycle_seconds
        self.paths = {"output": tmp_path}
        self.events, self.idle_start, self.idle_end = 0, None, None

    def pause(self, deadline):
        if self.phase == "idle":
            self.idle_start = self.clock.now
        self.clock.now = max(self.clock.now, deadline)
        if self.phase == "idle":
            self.idle_end = self.clock.now

    def one_cycle(self):
        self.clock.now += self.cycle_seconds
        self.summary["cleanup_confirmed"] = True
        return dict(report={"payload_bytes": 10, "verified_bytes": 20}, cleanup={"empty_verified": True})

    def record(self, *_):
        self.events += 1

    def tick(self, **_):
        self.summary["resources"]["final"] = {"rss": 1, "pss": 1}

    def check_quiescent(self):
        assert self.summary["cleanup_confirmed"]

    def logs(self):
        pass

    def require_space(self, _):
        pass


@pytest.mark.parametrize("duration", ["30m", "4h", "3d", "7d"])
def test_full_schedules_with_bounded_state(tmp_path, monkeypatch, duration):
    clock = Clock()
    monkeypatch.setattr(pb.time, "monotonic", clock.monotonic)
    args = arguments(tmp_path, "--duration", duration)
    pb.validate(args)
    campaign = ScheduledCampaign(args, clock, tmp_path)
    campaign.workload()
    assert campaign.summary["idle_seconds"] == 7200
    assert campaign.idle_start == args.duration
    assert campaign.idle_end == args.duration + 7200
    assert campaign.summary["measured_cycles"] == args.duration // 60
    assert campaign.events == 2 * campaign.summary["attempted_cycles"]
    assert len(campaign.summary["resources"]) == 2  # no retained per-cycle arrays


def test_crossing_cycle_and_cleanup_get_full_idle(tmp_path, monkeypatch):
    clock = Clock()
    monkeypatch.setattr(pb.time, "monotonic", clock.monotonic)
    args = arguments(tmp_path, "--duration", "200s", "--cycle-interval", "50s")
    pb.validate(args)
    campaign = ScheduledCampaign(args, clock, tmp_path, cycle_seconds=65)
    campaign.workload()
    assert campaign.summary["measured_cycles"] == 3
    assert campaign.summary["drain_cycles"] == 1
    assert campaign.summary["drain_seconds"] == 60
    assert campaign.idle_start == 260
    assert campaign.idle_end == 7460
    assert campaign.summary["published_bytes"] == 30
    assert campaign.summary["drain_published_bytes"] == 10


def test_cycle_failure_and_minimum_count(tmp_path, monkeypatch):
    clock = Clock()
    monkeypatch.setattr(pb.time, "monotonic", clock.monotonic)
    args = arguments(tmp_path)
    pb.validate(args)
    campaign = ScheduledCampaign(args, clock, tmp_path, cycle_seconds=1000)
    with pytest.raises(RuntimeError, match="three"):
        campaign.workload()
    assert campaign.idle_start is None
    campaign.one_cycle = lambda: (_ for _ in ()).throw(RuntimeError("cleanup failed"))
    with pytest.raises(RuntimeError, match="cleanup failed"):
        campaign.workload()
    assert campaign.summary["failed_cycles"] == 1


def test_sampling_error_death_and_gap(tmp_path, monkeypatch, campaign):
    campaign.broker = SimpleNamespace(poll=lambda: None)
    monkeypatch.setattr(pb, "sample_broker", lambda *_: {"rss": 100, "pss": 50})
    campaign.tick(force_sample=True)
    campaign.last_sample -= 31
    with pytest.raises(RuntimeError, match="gap"):
        campaign.tick(force_sample=True)
    campaign.last_sample = None
    monkeypatch.setattr(pb, "sample_broker", lambda *_: (_ for _ in ()).throw(PermissionError("proc denied")))
    with pytest.raises(PermissionError):
        campaign.tick(force_sample=True)
    campaign.broker.poll = lambda: 1
    monkeypatch.setattr(pb, "launch_broker", lambda *_: pytest.fail("broker restarted"))
    with pytest.raises(RuntimeError, match="exited"):
        campaign.tick()


def test_wait_samples_and_bounds_output(tmp_path, campaign):
    ticks = []
    original = campaign.tick
    def tick():
        ticks.append(1)
        original()
        assert all(len(t) <= pb.TAIL_BYTES for t in campaign.tails.values())
    campaign.tick = tick
    child = subprocess.Popen([sys.executable, "-c", "import sys,time;sys.stderr.write('x'*1000000);time.sleep(.2)"],
                             stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    assert campaign.wait_child(child, pb.time.monotonic() + 10) == 0
    assert len(ticks) > 2
    assert not campaign.tails


@pytest.mark.parametrize("kind", ["missing", "stale", "malformed", "oversized", "failed"])
def test_invalid_reports_never_pass(tmp_path, monkeypatch, kind, campaign):
    request = dict(schema_version=1, run_id="run", cycle_id="cycle", backend="filesystem",
                   transfer_engine="posix-copy", action="cycle")
    def wait(*_):
        report = dict(request, status="success")
        path = tmp_path / "report.json"
        if kind == "missing":
            return 0
        if kind == "stale":
            report["cycle_id"] = "wrong"
        if kind == "failed":
            report["status"] = "failed"
        path.write_text("{" if kind == "malformed" else "x" * 16385 if kind == "oversized" else json.dumps(report))
        return 0
    monkeypatch.setattr(pb.subprocess, "Popen", lambda *a, **k: object())
    campaign.wait_child = wait
    with pytest.raises((ValueError, RuntimeError, FileNotFoundError)):
        campaign.helper(request, 1)


def test_proc_and_exact_staging(tmp_path):
    stat_text = "123 (broker ) odd name) S " + " ".join(str(i) for i in range(1, 50))
    assert pb.proc_start_ticks(stat_text) == 19
    assert pb.proc_values("VmRSS: 12 kB\nThreads: 3\n")["VmRSS"] == 12288
    baseline = pb.staging_tree(tmp_path)
    (tmp_path / "leftover").write_text("")
    assert pb.staging_tree(tmp_path) != baseline
    (tmp_path / "link").symlink_to("leftover")
    with pytest.raises(ValueError):
        pb.staging_tree(tmp_path)


def test_qualification_and_no_kubernetes(tmp_path, monkeypatch):
    args = arguments(tmp_path, "--qualification")
    pb.validate(args)
    assert args.cooldown == 30
    monkeypatch.setattr(cli, "_setup", lambda *_: pytest.fail("cluster setup invoked"))
    monkeypatch.setattr(pb.Campaign, "execute", lambda self: 42)
    monkeypatch.delattr(pb.os, "pidfd_open", raising=False)
    assert args.func(args) == 42


def test_existing_run_and_sweep(tmp_path, monkeypatch):
    parser = cli.build_parser()
    run_args = parser.parse_args(["run", "--model-label", "Qwen3 0.6B", "--image", "example.invalid/model"])
    assert run_args.mode == "both" and run_args.engine == "vllm"
    args = parser.parse_args(["sweep", "--image", "example.invalid/model", "--output-dir", str(tmp_path)])
    monkeypatch.setattr(cli, "_setup", lambda _: (None, None))
    models = [SimpleNamespace(label="first"), SimpleNamespace(label="second")]
    monkeypatch.setattr(cli, "_load_models", lambda _: models)
    calls = []
    def fail(cfg, engine, model, args):
        calls.append(model.label)
        raise RuntimeError("ordinary error")
    monkeypatch.setattr(cli, "_run_one", fail)
    assert cli.cmd_sweep(args) == 1
    assert calls == ["first", "second"]
    calls.clear()
    def cleanup_failure(*_):
        calls.append(1)
        raise cli.CleanupError("cleanup failed")
    monkeypatch.setattr(cli, "_run_one", cleanup_failure)
    assert cli.cmd_sweep(args) == 1
    assert calls == [1]
    # PageBroker's files sit beneath a different results family.
    other = results.invocation_dir(tmp_path / "pagebroker", git_sha="abc")
    (other / "summary.json").write_text('{"kind":"snapshot.pagebroker-benchmark/v1"}')
    assert results.load_results(tmp_path) == []


@pytest.mark.parametrize("phase", ["active", "idle"])
def test_interruption_never_succeeds(tmp_path, phase, campaign):
    campaign.setup = lambda: None
    def workload():
        campaign.phase = phase
        campaign.stop_requested = True
        campaign.tick()
    campaign.workload = workload
    campaign.shutdown = lambda: None
    assert campaign.execute() == 130
    assert json.loads((tmp_path / "summary.json").read_text())["status"] == "interrupted"


def test_forced_shutdown_cannot_pass(tmp_path, campaign):
    campaign.setup = campaign.workload = lambda: None
    campaign.shutdown = lambda: (_ for _ in ()).throw(RuntimeError("broker shutdown was not graceful"))
    assert campaign.execute() == 1
    assert json.loads((tmp_path / "summary.json").read_text())["status"] == "failed"


def test_partial_multiday_durations_survive_failure(tmp_path, monkeypatch):
    args = arguments(tmp_path, "--duration", "7d")
    pb.validate(args)
    clock = Clock()
    monkeypatch.setattr(pb.time, "monotonic", clock.monotonic)
    campaign = pb.Campaign(args)
    campaign.paths = {"output": tmp_path}
    campaign.setup = campaign.shutdown = lambda: None
    def fail_after_days():
        campaign.active_start, campaign.active_end = 0, args.duration
        clock.now = 6 * 86400
        raise RuntimeError("late failure")
    campaign.workload = fail_after_days
    assert campaign.execute() == 1
    assert campaign.summary["actual_active_seconds"] == 6 * 86400
    assert campaign.summary["idle_seconds"] == 0


def test_timeout_reaps_helper(tmp_path, campaign):
    child = subprocess.Popen([sys.executable, "-c", "import time;time.sleep(60)"], stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    with pytest.raises(TimeoutError):
        campaign.wait_child(child, pb.time.monotonic() + 0.05)
    assert child.poll() is not None
    assert not campaign.tails


def test_unknown_cycle_never_starts_cleanup(tmp_path, campaign):
    campaign.cycle_id, campaign.fixture = "cycle", None
    campaign.helper = lambda *_: (_ for _ in ()).throw(RuntimeError("unknown Commit"))
    campaign.worker = lambda *_: pytest.fail("cleanup started before quiescence")
    with pytest.raises(RuntimeError):
        campaign.one_cycle()
    assert not campaign.summary["cleanup_confirmed"]
    assert campaign.summary["retained_artifact_location"].endswith("/cycle")


def test_process_identity_and_output_admission(tmp_path, monkeypatch, campaign):
    import os
    ticks = pb.proc_start_ticks(Path(f"/proc/{os.getpid()}/stat").read_text())
    with pytest.raises(RuntimeError, match="identity"):
        pb.sample_broker(dict(pid=os.getpid(), start_ticks=ticks + 1), tmp_path, "idle", None)
    monkeypatch.setattr(pb.shutil, "disk_usage", lambda _: SimpleNamespace(free=0))
    with pytest.raises(RuntimeError, match="space"):
        campaign.require_space(1)


RUN = "a" * 32
CONFIG = dict(storeId=f"bench-{RUN}", prefix=f"tests/bench-{RUN}/data", bucket="fake-bucket",
              endpoint="https://s3.example.invalid", region="us-east-1")
KEYS = dict(AWS_ACCESS_KEY_ID="obviously-fake-test-key", AWS_SECRET_ACCESS_KEY="obviously-fake-test-secret")
SDK_SESSION = Session()


def test_credentials(monkeypatch):
    now = dt.datetime(2026, 10, 18, tzinfo=dt.timezone.utc)
    until = now + dt.timedelta(days=7, hours=3)
    assert s.validate_credentials("static-environment", None, until, KEYS) == KEYS
    temporary = dict(KEYS, AWS_SESSION_TOKEN="obviously-fake-session")
    with pytest.raises(ValueError):
        s.validate_credentials("static-environment", None, until, temporary)
    with pytest.raises(ValueError):
        s.validate_credentials("temporary-environment", (now + dt.timedelta(hours=6)).isoformat(), until, temporary)
    for expiry in (None, "not-a-date", "2026-10-29T00:00:00", "2026-10-29 00:00:00+00:00"):
        with pytest.raises(ValueError):
            s.validate_credentials("temporary-environment", expiry, until, temporary)
    assert s.validate_credentials("temporary-environment", (until + dt.timedelta(hours=1)).isoformat(), until, temporary) == temporary
    monkeypatch.setenv("AWS_SECRET_ACCESS_KEY", KEYS["AWS_SECRET_ACCESS_KEY"])
    monkeypatch.setenv("AWS_PROFILE", "unexpected-profile")
    monkeypatch.setenv("AWS_ENDPOINT_URL", "https://unexpected.invalid")
    clean = s.child_environment()
    assert not any(k in clean for k in KEYS)
    assert "AWS_PROFILE" not in clean and "AWS_ENDPOINT_URL" not in clean
    assert s.child_environment(KEYS)["AWS_SECRET_ACCESS_KEY"] == KEYS["AWS_SECRET_ACCESS_KEY"]


def test_render_config_and_no_secrets(tmp_path):
    template = tmp_path / "template.json"
    original = dict(CONFIG, prefix="tests", storeId="original")
    template.write_text(json.dumps(original))
    out = tmp_path / "effective.json"
    effective = s.render_s3_config(template, RUN, out)
    assert effective == CONFIG
    assert json.loads(template.read_text()) == original
    for key, value in (("credentials", KEYS), ("endpoint", "https://user:password@s3.example.invalid"),
                       ("prefix", "../bad"), ("endpoint", "http://remote.example.invalid")):
        template.write_text(json.dumps(dict(original, **{key: value})))
        with pytest.raises(ValueError):
            s.render_s3_config(template, RUN, out)


class S3:
    def __init__(self, versioning="Disabled", count=0):
        self.versioning = versioning
        self.objects = [{"Key": CONFIG["prefix"] + f"/{i}", "VersionId": "null" if i == 0 else str(i)} for i in range(count)]
        self.owner = s.ownership(CONFIG, RUN)[1]
        self.uploads = []
        self.errors = False
        self.stuck = False
        self.deleted = []

    def get_paginator(self, name):
        operation = {"list_objects_v2": "ListObjectsV2", "list_object_versions": "ListObjectVersions"}[name]
        return Paginator(getattr(self, name), SDK_SESSION.get_paginator_model("s3").get_paginator(operation),
                         SDK_SESSION.get_service_model("s3").operation_model(operation))

    def get_object(self, **_):
        return {"Body": io.BytesIO(self.owner)}

    def get_bucket_versioning(self, **_):
        return {"Status": self.versioning}

    def list_multipart_uploads(self, **_):
        return {"Uploads": self.uploads}

    def list_objects_v2(self, **kwargs):
        assert kwargs["MaxKeys"] == 1000
        start = int(kwargs.get("ContinuationToken", 0))
        selected = self.objects[start:start + 1000]
        truncated = start + 1000 < len(self.objects)
        return dict(Contents=selected, IsTruncated=truncated, NextContinuationToken=str(start if self.stuck else start + 1000))

    def list_object_versions(self, **kwargs):
        start = int(kwargs.get("KeyMarker", 0))
        assert kwargs["MaxKeys"] == 1000
        if start:
            assert kwargs["VersionIdMarker"] == "null"
        selected = self.objects[start:start + 1000]
        truncated = start + 1000 < len(self.objects)
        return dict(Versions=selected[::2], DeleteMarkers=selected[1::2], IsTruncated=truncated,
                    NextKeyMarker=str(start if self.stuck else start + 1000), NextVersionIdMarker="null")

    def delete_objects(self, **kwargs):
        batch = kwargs["Delete"]["Objects"]
        assert len(batch) <= 1000
        self.deleted.extend(batch)
        if self.errors:
            return {"Errors": [{"Code": "AccessDenied"}]}
        for item in batch:
            self.objects = [o for o in self.objects if not (o["Key"] == item["Key"] and ("VersionId" not in item or o["VersionId"] == item["VersionId"]))]
        return {}


@pytest.mark.parametrize("versioning", ["Disabled", "Enabled", "Suspended"])
def test_paginated_cleanup(versioning):
    client = S3(versioning, 2301)
    result = s.s3_cleanup(CONFIG, RUN, client)
    assert result["empty_verified"] and result["deleted_entries"] == 2301
    assert not client.objects
    if versioning != "Disabled":
        assert any(o["VersionId"] == "null" for o in client.deleted)
    assert client.owner == s.ownership(CONFIG, RUN)[1]


@pytest.mark.parametrize("versioned", [False, True])
def test_truncated_listing_without_token_never_deletes(versioned):
    client = S3("Enabled" if versioned else "Disabled", 1100)
    operation = "list_object_versions" if versioned else "list_objects_v2"
    original = getattr(client, operation)
    def missing_token(**kwargs):
        page = original(**kwargs)
        page.pop("NextKeyMarker" if versioned else "NextContinuationToken")
        return page
    setattr(client, operation, missing_token)
    with pytest.raises(ValueError, match="pagination"):
        s.s3_cleanup(CONFIG, RUN, client)
    assert not client.deleted


def test_marker_only_pages_preserve_version_tokens():
    client = S3("Suspended", 1100)
    original = client.list_object_versions
    def markers_only(**kwargs):
        page = original(**kwargs)
        page["DeleteMarkers"] += page.pop("Versions")
        return page
    client.list_object_versions = markers_only
    assert s.s3_cleanup(CONFIG, RUN, client)["deleted_entries"] == 1100
    assert any(item["VersionId"] == "null" for item in client.deleted)


@pytest.mark.parametrize("failure", ["owner", "uploads", "partial", "pagination", "forbidden", "leftover", "foreign"])
def test_cleanup_failures(failure):
    client = S3("Enabled", 1100)
    if failure == "owner":
        client.owner = b"foreign"
    elif failure == "uploads":
        client.uploads = [{"Key": CONFIG["prefix"] + "/upload"}]
    elif failure == "partial":
        client.errors = True
    elif failure == "pagination":
        client.stuck = True
    elif failure == "forbidden":
        client.list_object_versions = lambda **_: (_ for _ in ()).throw(PermissionError())
    elif failure == "leftover":
        client.delete_objects = lambda **_: {}
    elif failure == "foreign":
        client.objects[0]["Key"] = "foreign-prefix/file"
    with pytest.raises((ValueError, PermissionError)):
        s.s3_cleanup(CONFIG, RUN, client)


def test_exact_ownership():
    for prefix in ("", "tests", "data", f"bench-{RUN}/data/", f"other-{RUN}/data"):
        with pytest.raises(ValueError):
            s.ownership(dict(CONFIG, prefix=prefix), RUN)
    marker, _ = s.ownership(CONFIG, RUN)
    assert not marker.startswith(CONFIG["prefix"] + "/")


def test_filesystem_cleanup_scope(tmp_path):
    root = tmp_path / "owned"
    root.mkdir()
    publication = root / "cycle"
    publication.mkdir()
    (publication / "nested").mkdir()
    (publication / "nested/file").write_bytes(b"test")
    (publication / "nested").chmod(0o500)
    source = tmp_path / "source"
    source.mkdir()
    (source / "preserve").write_bytes(b"original")
    (publication / "source-link").symlink_to(source, target_is_directory=True)
    assert s.filesystem_cleanup(publication, root)["empty_verified"]
    assert (source / "preserve").read_bytes() == b"original"
    for path in (root, source, root / "../source"):
        with pytest.raises(ValueError):
            s.filesystem_cleanup(path, root)


def test_worker_does_not_persist_sdk_exception_body(tmp_path, monkeypatch):
    request, report = tmp_path / "request.json", tmp_path / "report.json"
    config = tmp_path / "s3.json"
    config.write_text(json.dumps(CONFIG))
    request.write_text(json.dumps(dict(run_id=RUN, cycle_id="cycle", backend="s3", action="cleanup", config=str(config))))
    monkeypatch.setattr("sys.argv", ["worker", "--request", str(request), "--report", str(report)])
    monkeypatch.setattr(s, "s3_cleanup", lambda *_: (_ for _ in ()).throw(RuntimeError(KEYS["AWS_SECRET_ACCESS_KEY"])))
    assert s.main() == 1
    assert KEYS["AWS_SECRET_ACCESS_KEY"] not in report.read_text()
    assert json.loads(report.read_text())["error"] == "RuntimeError"


@pytest.mark.parametrize("lost_marker_reply", [False, True])
def test_preflight_access_probe_and_marker(lost_marker_reply):
    client = S3()
    writes = []
    marker, body = s.ownership(CONFIG, RUN)
    def put(**kwargs):
        writes.append(kwargs)
        if kwargs["Key"] == marker:
            assert kwargs["IfNoneMatch"] == "*" and kwargs["Body"] == body
            if lost_marker_reply:
                raise ConnectionError("reply lost after write")
        else:
            client.objects.append({"Key": kwargs["Key"], "VersionId": "null"})
    client.put_object = put
    assert s.s3_preflight(CONFIG, RUN, client)["empty_verified"]
    assert len(writes) == 2
    assert not client.objects


def test_preflight_never_adopts_existing_prefix():
    client = S3()
    marker, _ = s.ownership(CONFIG, RUN)
    client.objects = [{"Key": marker, "VersionId": "null"}]
    client.put_object = lambda **_: pytest.fail("overwrote existing prefix")
    with pytest.raises(ValueError, match="already"):
        s.s3_preflight(CONFIG, RUN, client)
