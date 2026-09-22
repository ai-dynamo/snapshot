<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
-->

# Changelog

Notable changes to Snapshot, in the format of
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html); see
[RELEASE.md](RELEASE.md) for how releases are cut and which versions receive
security fixes.

Entries describe what changed for someone operating Snapshot — new
capabilities, behavior changes, and anything requiring action on upgrade. CRD
changes and changes to checkpoint or restore semantics are called out
explicitly, because they affect checkpoints that already exist. The full
commit-level history for each release is on its
[GitHub release page](https://github.com/ai-dynamo/snapshot/releases).

## [Unreleased]

### Added

- `PodSnapshotContent.status.source` records what a checkpoint was captured on:
  the source node's name, architecture and kernel version, the captured
  container's image, image digest and CPU and memory limits, and the NVIDIA
  driver version and GPU models the capture could see. Informational only —
  restore compatibility still compares the artifact's manifest. CRD change;
  populated for captures that reach `Ready` after the upgrade, so a content
  already `Ready` before it is not backfilled.

### Changed

- The agent's capture path is driven by a workqueue keyed on the work order,
  replacing the per-capture `coordination.k8s.io` Lease and the in-process
  guard that arbitrated between the validation and dump paths. A failed
  reconcile is now retried with backoff instead of waiting for the next resync,
  and the `LeaseCancelled` failure reason no longer occurs. The agent
  `ClusterRole` no longer requests `leases`; a hand-maintained copy of that role
  can drop the `coordination.k8s.io` rule.

## [0.1.0] - 2026-09-06

First release. Snapshot checkpoints a fully initialized GPU pod — running
process, CPU and GPU memory — and restores that state on another compatible
node, cutting cold-start time for large inference workloads.

### Added

- `PodSnapshot`, `PodSnapshotContent`, and `SnapshotJob` custom resources, and
  the `restore-from` annotation for programmatic restore.
- Cluster operator that reconciles the custom resources, and a node-level agent
  DaemonSet that drives CRIU and `cuda-checkpoint` against live processes.
- `snapshotctl` for lower-level checkpoint and restore from a pod manifest.
- Helm chart published as an OCI artifact to GHCR, with CRDs upgraded from an
  init container so a chart upgrade cannot leave them stale.
- Restore Pod contract covering the control volume, startup gate, seccomp
  profile, and the `SNAPSHOT_CONTROL_DIR` environment variable.
- Framework guides with runnable manifests for vLLM, SGLang, and TensorRT-LLM.
- Corresponding source and third-party attribution shipped inside both images
  under `/legal`.

### Known limitations

Checkpoint and restore require compatible GPU and driver versions between the
source and target node. See [Limitations](docs/limitations.md) for the current
boundaries.

## [0.1.0-rc2] - 2026-09-03

Release candidate for 0.1.0.

## [0.1.0-rc.1] - 2026-09-01

Release candidate for 0.1.0.

## [0.1.0-alpha.1] - 2026-08-16

First tagged build, published to validate the release pipeline.

[Unreleased]: https://github.com/ai-dynamo/snapshot/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/ai-dynamo/snapshot/releases/tag/v0.1.0
[0.1.0-rc2]: https://github.com/ai-dynamo/snapshot/releases/tag/v0.1.0-rc2
[0.1.0-rc.1]: https://github.com/ai-dynamo/snapshot/releases/tag/v0.1.0-rc.1
[0.1.0-alpha.1]: https://github.com/ai-dynamo/snapshot/releases/tag/v0.1.0-alpha.1
