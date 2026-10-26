# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Sample one PageBroker process and its staging filesystem without retaining history."""

from collections import Counter
from datetime import datetime, timezone
import json
from itertools import islice
import math
import os
from pathlib import Path
import select
import time


def validate_timing(interval, settle):
    if not math.isfinite(interval) or not 0.1 <= interval <= 60:
        raise ValueError("sample interval must be between 0.1 and 60 seconds")
    if not math.isfinite(settle) or not 0 <= settle <= 60:
        raise ValueError("settling time must be between 0 and 60 seconds")


def fields(text):
    values = {}
    for line in text.splitlines():
        key, separator, value = line.partition(":")
        if separator:
            values[key] = value.split()
    return values


def number(values, key, unit=None):
    tokens = values[key]
    if len(tokens) != (2 if unit else 1) or (unit and tokens[1] != unit):
        raise ValueError(f"unexpected unit for {key}")
    value = int(tokens[0])
    if value < 0:
        raise ValueError(f"negative value for {key}")
    return value * (1024 if unit == "kB" else 1)


class ProcessSampler:
    def __init__(self, pid, pidfd, staging):
        self.pid, self.pidfd = pid, pidfd
        self.staging = Path(staging)
        # Keep reads tied to this process even if Linux later reuses its PID.
        self.proc = os.open(f"/proc/{pid}", os.O_RDONLY | os.O_DIRECTORY)
        try:
            self.check_alive()
            self.start_ticks = int(self.read("stat").rsplit(")", 1)[1].split()[19])
            self.staging_is_mount = self.staging.is_mount()
        except BaseException:
            os.close(self.proc)
            raise

    def read(self, name):
        with os.fdopen(os.open(name, os.O_RDONLY, dir_fd=self.proc)) as file:
            return file.read()

    def check_alive(self):
        if select.select([self.pidfd], [], [], 0)[0]:
            raise RuntimeError("PageBroker exited during resource collection")

    def collect(self):
        self.check_alive()
        status = fields(self.read("status"))
        rollup = fields(self.read("smaps_rollup"))
        descriptors = os.open("fd", os.O_RDONLY | os.O_DIRECTORY, dir_fd=self.proc)
        try:
            with os.scandir(descriptors) as entries:
                fd_count = sum(1 for _ in entries)
        finally:
            os.close(descriptors)
        usage = os.statvfs(self.staging)
        metrics = {
            "rssBytes": number(rollup, "Rss", "kB"),
            "pssBytes": number(rollup, "Pss", "kB"),
            "privateBytes": number(rollup, "Private_Clean", "kB") + number(rollup, "Private_Dirty", "kB"),
            "anonymousRssBytes": number(status, "RssAnon", "kB"),
            "fileRssBytes": number(status, "RssFile", "kB"),
            "sharedRssBytes": number(status, "RssShmem", "kB"),
            "rssHighWaterBytes": number(status, "VmHWM", "kB"),
            "anonymousSwapBytes": number(status, "VmSwap", "kB"),
            "fileDescriptors": fd_count,
            "threads": number(status, "Threads"),
            "stagingFilesystemUsedBytes": (usage.f_blocks - usage.f_bfree) * usage.f_frsize,
            "stagingFilesystemUsedInodes": usage.f_files - usage.f_ffree,
        }
        self.check_alive()
        return metrics

    def diagnostics(self):
        descriptors = os.open("fd", os.O_RDONLY | os.O_DIRECTORY, dir_fd=self.proc)
        types, names = Counter(), Counter()
        try:
            with os.scandir(descriptors) as entries:
                names_to_read = [entry.name for entry in islice(entries, 4096)]
            for name in names_to_read:
                try:
                    target = os.readlink(name, dir_fd=descriptors)
                    kind = next((prefix for prefix in ("socket:", "pipe:", "anon_inode:")
                                 if target.startswith(prefix)), "file")
                    types[kind] += 1
                except FileNotFoundError:
                    types["closed-during-sample"] += 1
        finally:
            os.close(descriptors)
        tasks = os.open("task", os.O_RDONLY | os.O_DIRECTORY, dir_fd=self.proc)
        try:
            with os.scandir(tasks) as entries:
                tids = [entry.name for entry in islice(entries, 4096)]
            for tid in tids:
                try:
                    with os.fdopen(os.open(f"{tid}/comm", os.O_RDONLY, dir_fd=tasks)) as source:
                        name = source.read(64).strip()
                    names[name if name in names or len(names) < 64 else "other"] += 1
                except FileNotFoundError:
                    names["exited-during-sample"] += 1
        finally:
            os.close(tasks)
        return {"descriptorTypes": dict(types), "threadNames": dict(names)}

    def close(self):
        os.close(self.proc)


class ResourceMonitor(ProcessSampler):
    def __init__(self, pid, pidfd, staging, output, interval):
        super().__init__(pid, pidfd, staging)
        self.interval = interval
        self.started = time.monotonic()
        self.next_sample = self.started
        self.samples = 0
        self.phases = Counter()
        self.metrics = {}
        self.error = None
        self.duration = 0
        try:
            self.output = Path(output).open("x", buffering=1)
        except BaseException:
            super().close()
            raise

    def sample(self, phase, force=False):
        now = time.monotonic()
        if not force and now < self.next_sample:
            return
        record = {"timestamp": datetime.now(timezone.utc).isoformat(),
                  "elapsedSeconds": now - self.started, "pid": self.pid,
                  "processStartTicks": self.start_ticks, "phase": phase}
        try:
            metrics = self.collect()
            record.update(status="SUCCESS", metrics=metrics)
            self.output.write(json.dumps(record) + "\n")
            self.output.flush()
        except Exception as error:
            self.error = f"{type(error).__name__}: {error}"
            record.update(status="ERROR", error=self.error)
            try:
                self.output.write(json.dumps(record) + "\n")
                self.output.flush()
            except OSError:
                pass
            raise RuntimeError(f"resource monitoring failed: {self.error}") from error
        self.samples += 1
        self.phases[phase] += 1
        self.duration = record["elapsedSeconds"]
        for name, value in metrics.items():
            stat = self.metrics.setdefault(name, {"first": value, "last": value, "peak": value, "delta": 0})
            stat.update(last=value, peak=max(stat["peak"], value), delta=value - stat["first"])
        self.next_sample = time.monotonic() + self.interval

    def summary(self):
        complete = self.phases["baseline"] == 1 and self.phases["settled"] == 1
        return {"status": "ERROR" if self.error else "SUCCESS" if complete else "INCOMPLETE",
                "samplesFile": "resources.jsonl", "sampleCount": self.samples,
                "intervalSeconds": self.interval, "durationSeconds": self.duration,
                "pid": self.pid, "processStartTicks": self.start_ticks,
                "stagingRoot": str(self.staging), "stagingIsMountPoint": self.staging_is_mount,
                "phases": dict(self.phases), "metrics": self.metrics, "error": self.error,
                "leakAssessment": "NOT_ASSESSED"}

    def close(self):
        try:
            self.output.close()
        finally:
            super().close()
