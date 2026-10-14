# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Turn Datadog GPU monitoring off for an e2e run and back on afterwards.

Datadog's GPU monitoring (system-probe + NVML) attaches to GPU processes on
the node and is a suspected interferer for CRIU/CUDA checkpoint and restore.
The e2e workflow disables it on the host cluster before the framework jobs and
restores it when they are done, but only if it was on to begin with.

The Datadog operator exposes the switch in two places; both are handled:

  DatadogAgentProfile  .spec.config.features.gpu.enabled   (per node profile)
  DatadogAgent         .spec.features.gpu.enabled          (cluster default)

State lives on the resources themselves, so overlapping workflow runs do not
step on each other. `disable` records the original value in an annotation and
adds the run to a holders list; `restore` removes the run from the list and
re-enables only when the list is empty and the original value was on. A
cluster without the Datadog CRDs is a no-op.

Usage (KUBECONFIG must point at the host cluster):

  datadog-gpu-monitoring.py status [--github-output]
  datadog-gpu-monitoring.py disable --holder <run-id>
  datadog-gpu-monitoring.py restore --holder <run-id>
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys

ANNOTATION_PREFIX = "snapshot-e2e.nvidia.com/"
WAS_ENABLED = ANNOTATION_PREFIX + "datadog-gpu-was-enabled"
HOLDERS = ANNOTATION_PREFIX + "datadog-gpu-holders"

# (kubectl resource, path to the gpu feature block)
KINDS = (
    ("datadogagentprofiles.datadoghq.com", ("spec", "config", "features", "gpu")),
    ("datadogagents.datadoghq.com", ("spec", "features", "gpu")),
)


def kubectl(*args: str, check: bool = True) -> subprocess.CompletedProcess[str]:
    return subprocess.run(["kubectl", *args], text=True, capture_output=True, check=check)


def dig(obj: dict, path: tuple[str, ...]) -> dict | None:
    cur: object = obj
    for key in path:
        if not isinstance(cur, dict) or key not in cur:
            return None
        cur = cur[key]
    return cur if isinstance(cur, dict) else None


class Target:
    def __init__(self, resource: str, path: tuple[str, ...], item: dict) -> None:
        self.resource = resource
        self.path = path
        meta = item["metadata"]
        self.namespace = meta["namespace"]
        self.name = meta["name"]
        self.annotations = meta.get("annotations") or {}
        gpu = dig(item, path) or {}
        self.enabled = bool(gpu.get("enabled", False))

    @property
    def ref(self) -> str:
        return f"{self.resource}/{self.name} -n {self.namespace}"

    @property
    def holders(self) -> list[str]:
        raw = self.annotations.get(HOLDERS, "")
        return [h for h in raw.split(",") if h]

    def set_enabled(self, value: bool) -> None:
        patch: dict = {"enabled": value}
        for key in reversed(self.path):
            patch = {key: patch}
        kubectl(
            "patch", self.resource, self.name, "-n", self.namespace,
            "--type=merge", "-p", json.dumps(patch),
        )

    def annotate(self, **values: str | None) -> None:
        args = ["annotate", "--overwrite", self.resource, self.name, "-n", self.namespace]
        for key, value in values.items():
            annotation = ANNOTATION_PREFIX + key.replace("_", "-")
            args.append(f"{annotation}-" if value is None else f"{annotation}={value}")
        kubectl(*args)


def discover() -> list[Target]:
    targets: list[Target] = []
    for resource, path in KINDS:
        result = kubectl("get", resource, "-A", "-o", "json", check=False)
        if result.returncode != 0:
            if "doesn't have a resource type" in result.stderr or "the server could not find" in result.stderr:
                print(f"{resource}: CRD not installed, skipping")
                continue
            sys.exit(f"kubectl get {resource} failed: {result.stderr.strip()}")
        for item in json.loads(result.stdout).get("items", []):
            targets.append(Target(resource, path, item))
    return targets


def report(targets: list[Target]) -> bool:
    enabled_any = False
    if not targets:
        print("no Datadog operator resources found")
    for t in targets:
        holders = ",".join(t.holders) or "-"
        print(f"{t.ref}: gpu.enabled={t.enabled} was-enabled={t.annotations.get(WAS_ENABLED, '-')} holders={holders}")
        enabled_any |= t.enabled
    return enabled_any


def github_output(**values: str) -> None:
    path = os.environ.get("GITHUB_OUTPUT")
    if not path:
        return
    with open(path, "a", encoding="utf-8") as fh:
        for key, value in values.items():
            fh.write(f"{key}={value}\n")


def cmd_status(args: argparse.Namespace) -> None:
    enabled = report(discover())
    if args.github_output:
        github_output(enabled=str(enabled).lower())


def cmd_disable(args: argparse.Namespace) -> None:
    targets = discover()
    report(targets)
    touched = False
    for t in targets:
        holders = t.holders
        if t.enabled:
            print(f"disabling GPU monitoring on {t.ref}")
            t.annotate(datadog_gpu_was_enabled="true")
            t.set_enabled(False)
        elif not holders:
            # Off before we got here and not because of another run: leave it
            # alone and do not claim it, so restore will not turn it on.
            print(f"{t.ref}: already off, not held by an e2e run; leaving as is")
            continue
        if args.holder not in holders:
            holders.append(args.holder)
            t.annotate(datadog_gpu_holders=",".join(holders))
        touched = True
    github_output(disabled=str(touched).lower())


def cmd_restore(args: argparse.Namespace) -> None:
    targets = discover()
    report(targets)
    for t in targets:
        holders = t.holders
        if args.holder not in holders:
            continue
        holders.remove(args.holder)
        if holders:
            print(f"{t.ref}: still held by {','.join(holders)}; leaving GPU monitoring off")
            t.annotate(datadog_gpu_holders=",".join(holders))
            continue
        if t.annotations.get(WAS_ENABLED) == "true":
            print(f"re-enabling GPU monitoring on {t.ref}")
            t.set_enabled(True)
        t.annotate(datadog_gpu_holders=None, datadog_gpu_was_enabled=None)


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="command", required=True)
    status = sub.add_parser("status", help="print the current state")
    status.add_argument("--github-output", action="store_true", help="write enabled=true|false to $GITHUB_OUTPUT")
    status.set_defaults(func=cmd_status)
    for name, func in (("disable", cmd_disable), ("restore", cmd_restore)):
        p = sub.add_parser(name)
        p.add_argument("--holder", required=True, help="identifier of this workflow run")
        p.set_defaults(func=func)
    args = parser.parse_args()
    args.func(args)


if __name__ == "__main__":
    main()
