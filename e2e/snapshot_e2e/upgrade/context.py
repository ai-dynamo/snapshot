# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Settings, shared state, and phase timings for one upgrade run."""

from __future__ import annotations

import os
import time
from contextlib import contextmanager
from dataclasses import dataclass, field
from datetime import datetime
from typing import Iterator

from kubernetes import client

from snapshot_e2e import k8s
from snapshot_e2e import lifecycle
from snapshot_e2e.infra import setup
from snapshot_e2e.workloads import TestRun


PROFILES = ("basic", "all")


@dataclass(frozen=True)
class UpgradeSettings:
    from_tag: str
    to_tag: str
    config: str
    profile: str
    scenarios: tuple[str, ...]
    helm_timeout: str
    ready_timeout_seconds: int

    @classmethod
    def from_env(cls) -> "UpgradeSettings":
        from_tag = os.environ.get("SNAPSHOT_E2E_UPGRADE_FROM_TAG", "").strip()
        to_tag = os.environ.get("SNAPSHOT_E2E_SNAPSHOT_TAG", "").strip()
        if not from_tag:
            raise ValueError("SNAPSHOT_E2E_UPGRADE_FROM_TAG is required: the version to upgrade from")
        if not to_tag:
            raise ValueError("SNAPSHOT_E2E_SNAPSHOT_TAG is required: the version to upgrade to")
        if from_tag == to_tag:
            raise ValueError(f"upgrade from and to the same version {from_tag}")
        profile = os.environ.get("SNAPSHOT_E2E_UPGRADE_PROFILE", "").strip() or "basic"
        if profile not in PROFILES:
            raise ValueError(f"SNAPSHOT_E2E_UPGRADE_PROFILE={profile!r}; expected one of {PROFILES}")
        scenarios = tuple(
            name.strip()
            for name in os.environ.get("SNAPSHOT_E2E_UPGRADE_SCENARIOS", "").split(",")
            if name.strip()
        )
        return cls(
            from_tag=from_tag,
            to_tag=to_tag,
            config=os.environ.get("SNAPSHOT_E2E_UPGRADE_CONFIG", "").strip() or "full",
            profile=profile,
            scenarios=scenarios,
            helm_timeout=os.environ.get("SNAPSHOT_E2E_HELM_TIMEOUT", setup.DEFAULT_HELM_TIMEOUT),
            ready_timeout_seconds=int(
                os.environ.get(
                    "SNAPSHOT_E2E_READY_TIMEOUT_SECONDS", setup.DEFAULT_READY_TIMEOUT_SECONDS
                )
            ),
        )


@dataclass(frozen=True)
class RecordedSnapshot:
    name: str
    uid: str
    content_name: str
    content_uid: str
    node: str
    content_finalizers: tuple[str, ...]


@dataclass
class ScenarioState:
    run: TestRun
    node: str = ""
    observations: int = 0
    snapshot: RecordedSnapshot | None = None
    pre_upgrade_error: BaseException | None = None


class Timings:
    def __init__(self) -> None:
        self.entries: list[tuple[str, float, bool]] = []

    @contextmanager
    def phase(self, label: str) -> Iterator[None]:
        start = time.monotonic()
        setup.log(f"Starting {label}")
        ok = False
        try:
            yield
            ok = True
        finally:
            seconds = time.monotonic() - start
            self.entries.append((label, seconds, ok))
            setup.log(f"{'Finished' if ok else 'Failed'} {label} in {seconds:.0f}s")

    def summary(self, title: str) -> str:
        lines = [f"### {title}", "", "| Phase | Duration | Result |", "| --- | --- | --- |"]
        for label, seconds, ok in self.entries:
            lines.append(f"| {label} | {seconds:.0f}s | {'ok' if ok else 'failed'} |")
        total = sum(seconds for _, seconds, _ in self.entries)
        lines.append(f"| **Total** | **{total:.0f}s** | |")
        return "\n".join(lines) + "\n"

    def publish(self, title: str) -> None:
        text = self.summary(title)
        print("\n" + text, flush=True)
        path = os.environ.get("GITHUB_STEP_SUMMARY")
        if path:
            with open(path, "a", encoding="utf-8") as handle:
                handle.write(text + "\n")


@dataclass
class UpgradeContext:
    config: k8s.E2EConfig
    settings: UpgradeSettings
    timings: Timings = field(default_factory=Timings)
    states: dict[str, ScenarioState] = field(default_factory=dict)
    revision_before: int = 0
    upgrade_started: datetime | None = None
    upgrade_error: BaseException | None = None

    def new_run(self, prefix: str) -> TestRun:
        return TestRun.new(prefix[:24])

    @property
    def snapshots(self) -> list[RecordedSnapshot]:
        return [state.snapshot for state in self.states.values() if state.snapshot]

    def record_snapshot(self, name: str, node: str) -> RecordedSnapshot:
        api = client.CustomObjectsApi()
        snapshot = lifecycle.get_custom_object(api, self.config.namespace, name, lifecycle.PODSNAPSHOTS)
        content_name = snapshot["status"]["boundSnapshotContentName"]
        content = lifecycle.get_custom_object(api, None, content_name, lifecycle.PODSNAPSHOTCONTENTS)
        return RecordedSnapshot(
            name=name,
            uid=snapshot["metadata"]["uid"],
            content_name=content_name,
            content_uid=content["metadata"]["uid"],
            node=node,
            content_finalizers=tuple(content["metadata"].get("finalizers") or ()),
        )
