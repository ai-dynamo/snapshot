# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Validated, nonsecret specifications shared by the endurance processes."""

from dataclasses import asdict, dataclass, field
import json
import math
from pathlib import Path
import time as system_time
import uuid


SCHEMA_VERSION = 1
RESOURCE_METRICS = frozenset({
    "rssBytes", "pssBytes", "privateBytes", "anonymousRssBytes", "fileRssBytes", "sharedRssBytes",
    "rssHighWaterBytes", "anonymousSwapBytes", "fileDescriptors", "threads",
    "stagingFilesystemUsedBytes", "stagingFilesystemUsedInodes",
})


@dataclass(frozen=True)
class EvidenceOptions:
    interval: float = 1
    progress_seconds: float = 10
    chunk_bytes: int = 16 * 1024**2
    total_bytes: int = 512 * 1024**2
    diagnostic_bytes: int = 128 * 1024**2
    terminal_bytes: int = 2 * 1024**2
    report_bytes: int = 1024**2
    client_log_bytes: int = 1024**2
    max_files: int = 4096
    idle_samples: int = 10
    retention_seconds: float = 3600
    reap_seconds: float = 120
    later_reference_seconds: float = 3600
    retention_revision: str = ""
    growth_limits: dict[str, float] = field(default_factory=dict)
    absolute_limits: dict[str, float] = field(default_factory=dict)

    @property
    def ordinary_bytes(self):
        return self.total_bytes - self.diagnostic_bytes - self.terminal_bytes

    def validate(self):
        if isinstance(self.interval, bool) or isinstance(self.progress_seconds, bool):
            raise ValueError("sampling/progress intervals must be numbers")
        if not 0.1 <= self.interval <= 60 or not math.isfinite(self.interval):
            raise ValueError("evidence.interval must be between 0.1 and 60 seconds")
        if not 0 < self.progress_seconds <= 10 or not math.isfinite(self.progress_seconds):
            raise ValueError("progress must be checkpointed at least every ten seconds")
        for name in ("chunk_bytes", "total_bytes", "terminal_bytes", "report_bytes", "client_log_bytes",
                     "max_files", "idle_samples"):
            if type(getattr(self, name)) is not int or getattr(self, name) <= 0:
                raise ValueError(f"evidence.{name} must be a positive integer")
        if type(self.diagnostic_bytes) is not int or self.diagnostic_bytes < 0:
            raise ValueError("evidence.diagnostic_bytes must be a nonnegative integer")
        if self.ordinary_bytes <= 0 or self.chunk_bytes > self.ordinary_bytes:
            raise ValueError("evidence reserves leave insufficient ordinary-writer capacity")
        if self.idle_samples != 10:
            raise ValueError("idle analysis requires the last ten samples")
        if self.max_files > 4096:
            raise ValueError("evidence.max_files exceeds the bounded manifest capacity")
        if not isinstance(self.retention_revision, str) or len(self.retention_revision) > 128:
            raise ValueError("retention_revision must be a bounded revision string")
        for name in ("retention_seconds", "reap_seconds", "later_reference_seconds"):
            value = getattr(self, name)
            if isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(value) or value < 0:
                raise ValueError(f"evidence.{name} must be finite and nonnegative")
        for limits in (self.growth_limits, self.absolute_limits):
            if not isinstance(limits, dict) or set(limits) - RESOURCE_METRICS:
                raise ValueError("resource limits must name supported process/staging metrics")
            for value in limits.values():
                if isinstance(value, bool) or not isinstance(value, (float, int)) or not math.isfinite(value) or value < 0:
                    raise ValueError("resource limits must be finite and nonnegative")


@dataclass(frozen=True)
class Timing:
    startup: float = 300
    warmup_min: float = 600
    warmup_max: float = 1800
    warmup_batches: int = 5
    measured: float = 14400
    measured_batches: int = 200
    cadence: float = 60
    idle: float = 30
    cooldown: float = 600
    batch: float = 300
    cleanup: float = 120
    drain: float = 420
    shutdown: float = 60
    margin: float = 420
    job: float = 18000
    poll: float = 0.1
    heartbeat: float = 1
    stale: float = 10
    broker_term: float = 20
    kill_wait: float = 5
    client_term: float = 5

    def validate(self):
        for name, value in asdict(self).items():
            if isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(value) or value <= 0:
                raise ValueError(f"timing.{name} must be finite and positive")
        for name in ("warmup_batches", "measured_batches"):
            if type(getattr(self, name)) is not int:
                raise ValueError(f"timing.{name} must be an integer")
        if not self.poll <= self.heartbeat <= 1 or self.stale <= 2 * self.heartbeat:
            raise ValueError("poll <= heartbeat <= 1 and stale > 2 * heartbeat are required")
        if self.warmup_min > self.warmup_max:
            raise ValueError("warm-up minimum exceeds its deadline")
        if self.drain < self.batch + self.cleanup:
            raise ValueError("drain must cover a batch and cleanup")
        shutdown_work = self.broker_term + 2 * self.kill_wait + self.client_term
        if self.shutdown <= shutdown_work:
            raise ValueError("shutdown leaves no time for termination and completion acknowledgments")
        if self.worst_case > self.job:
            raise ValueError("startup, phases, drain, shutdown and margin exceed the Job deadline")
        if (self.measured_batches - 1) * max(self.cadence, self.idle) >= self.measured:
            raise ValueError("minimum measured batch count cannot fit the pacing interval")

    @property
    def worst_case(self):
        return (self.startup + self.warmup_max + self.measured + self.drain
                + self.cooldown + self.shutdown + self.margin)


@dataclass(frozen=True)
class S3Options:
    bucket: str
    prefix: str
    region: str
    endpoint: str
    credential_expiry: float | None = None
    addressing: str = "virtual"
    credential_provider: str = "temporary-environment"
    credential_margin: int = 900
    max_pages: int = 100
    max_items: int = 10000
    page_size: int = 250
    retained_bytes: int = 1024**3

    def validate(self, spec):
        from urllib.parse import urlsplit
        import re
        if (not re.fullmatch(r"[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]", self.bucket)
                or not re.fullmatch(r"[A-Za-z0-9_-]+", self.region)):
            raise ValueError("invalid S3 bucket or region")
        parts = self.prefix.split("/")
        if (parts[-1] != spec.run_id or any(not re.fullmatch(r"[A-Za-z0-9_.-]+", part)
                or part in (".", "..") for part in parts) or len(self.prefix.encode()) > 800):
            raise ValueError("S3 prefix must be normalized and end in this run's UUID")
        if spec.store_id != "stress-" + spec.run_id:
            raise ValueError("S3 requires a unique store_id bound to the run UUID")
        endpoint = urlsplit(self.endpoint)
        if (endpoint.scheme != "https" or not endpoint.hostname or endpoint.username or endpoint.password
                or endpoint.path not in ("", "/") or endpoint.query or endpoint.fragment):
            raise ValueError("S3 requires a credential-free HTTPS endpoint")
        if self.addressing not in ("path", "virtual"):
            raise ValueError("S3 addressing must match the backend's path or virtual configuration")
        for name, maximum in (("max_pages", 100), ("max_items", 10000), ("page_size", 1000),
                              ("retained_bytes", 1024**3)):
            value = getattr(self, name)
            if type(value) is not int or not 0 < value <= maximum:
                raise ValueError(f"S3 {name} exceeds its bounded profile")
        self.validate_expiry(spec, spec.created_at)

    def validate_expiry(self, spec, now):
        # Recheck at launch and helper admission; mounted Secret rotation cannot refresh clients.
        if self.credential_margin != 900:
            raise ValueError("S3 requires the planned 15-minute credential margin")
        if self.credential_provider == "static-environment":
            if self.credential_expiry is not None:
                raise ValueError("static S3 credentials must not declare an expiry")
            return
        if self.credential_provider != "temporary-environment":
            raise ValueError("unsupported S3 credential provider")
        if (isinstance(self.credential_expiry, bool) or not isinstance(self.credential_expiry, (int, float))
                or not math.isfinite(self.credential_expiry)):
            raise ValueError("S3 credential expiry must be known")
        if self.credential_expiry < max(now, spec.created_at) + spec.timing.job + self.credential_margin:
            raise ValueError("S3 credentials do not cover the full Job deadline plus 15 minutes")


@dataclass(frozen=True)
class RunSpec:
    run_id: str
    mode: str
    socket: str
    staging: str
    storage: str
    results: str
    control: str
    created_at: float
    startup_deadline: float
    expires_at: float
    timing: Timing = field(default_factory=Timing)
    evidence: EvidenceOptions = field(default_factory=EvidenceOptions)
    store_id: str = ""
    s3: S3Options | None = None
    schema_version: int = SCHEMA_VERSION

    def validate(self):
        if type(self.schema_version) is not int or self.schema_version != SCHEMA_VERSION:
            raise ValueError("unsupported run-spec schema version")
        if str(uuid.UUID(self.run_id)) != self.run_id:
            raise ValueError("run_id must be a canonical UUID")
        if self.mode not in ("filesystem", "s3"):
            raise ValueError("mode must be filesystem or s3")
        if self.mode == "s3" and not self.store_id:
            raise ValueError("S3 requires a store_id")
        if self.mode == "s3":
            if self.s3 is None:
                raise ValueError("S3 requires cleanup and credential configuration")
            self.s3.validate(self)
            if self.timing.shutdown <= self.timing.broker_term + 2 * self.timing.client_term + 5 * self.timing.kill_wait:
                raise ValueError("S3 shutdown must cover client, observers, SDK worker and broker termination")
        elif self.s3 is not None:
            raise ValueError("filesystem runs cannot have S3 configuration")
        paths = []
        for name in ("socket", "staging", "storage", "results", "control"):
            value = getattr(self, name)
            path = Path(value)
            if not path.is_absolute() or str(path) != value or ".." in path.parts or value == "/":
                raise ValueError(f"{name} must be a normalized absolute path other than /")
            paths.append(path)
        for index, first in enumerate(paths):
            for second in paths[index + 1:]:
                if first == second or first in second.parents or second in first.parents:
                    raise ValueError("socket, staging, storage, results and control must not overlap")
        for name in ("created_at", "startup_deadline", "expires_at"):
            value = getattr(self, name)
            if isinstance(value, bool) or not isinstance(value, (float, int)) or not math.isfinite(value):
                raise ValueError(f"{name} must be a finite UTC Unix timestamp")
        self.timing.validate()
        self.evidence.validate()
        if not self.created_at < self.startup_deadline <= self.created_at + self.timing.startup:
            raise ValueError("startup deadline must fit the startup budget")
        if not self.created_at + self.timing.worst_case <= self.expires_at <= self.created_at + self.timing.job:
            raise ValueError("run expiry must cover the full budget and fit the Job deadline")
        return self

    def deadlines(self, clock):
        self.validate()
        wall, monotonic = clock.time(), clock.monotonic()
        if wall < self.created_at or wall >= self.startup_deadline:
            raise TimeoutError("startup admission expired or clock precedes run creation")
        return (monotonic + self.startup_deadline - wall,
                monotonic + self.expires_at - wall)

    def as_dict(self):
        return asdict(self)

    @classmethod
    def load(cls, path):
        if Path(path).stat().st_size > 64 * 1024:
            raise ValueError("run specification exceeds 64 KiB")
        return cls.from_dict(json.loads(Path(path).read_text()))

    @classmethod
    def from_dict(cls, value):
        value = dict(value)
        value["timing"] = Timing(**value.get("timing", {}))
        value["evidence"] = EvidenceOptions(**value.get("evidence", {}))
        if value.get("s3") is not None:
            value["s3"] = S3Options(**value["s3"])
        return cls(**value).validate()


class Clock:
    monotonic = staticmethod(system_time.monotonic)
    time = staticmethod(system_time.time)
    sleep = staticmethod(system_time.sleep)
