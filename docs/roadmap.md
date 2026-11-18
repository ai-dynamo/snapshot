<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Snapshot Roadmap

Snapshot checkpoints a running GPU workload and restores it on any compatible node, so a new replica is ready in seconds instead of minutes. This page shows where the project is heading, one quarter at a time. It stays high level on purpose: each item links to the issue or design proposal where the details, discussion, and progress live.

> [!NOTE]
> This roadmap states intent, not commitment. Plans change with community feedback, with the schedules of the projects Snapshot works with (inference engines, NVIDIA drivers), and with what we learn from real deployments. Last updated: 2026-11-18.

## How to Read This Roadmap

- **Quarters, not versions.** Snapshot releases about once a month (see [Release Process](https://github.com/ai-dynamo/snapshot/blob/main/RELEASE.md)), so work is grouped by calendar quarter. Any release in a quarter can carry some of that quarter's items. The [CHANGELOG](https://github.com/ai-dynamo/snapshot/blob/main/CHANGELOG.md) lists what each release contains.
- **Confidence drops with distance.** Q4 2026 is mostly work already under way. Q1 2027 is planned. Q2 2027 is the direction we expect to take. Long-term items are ideas we are exploring, with no date.
- **Details live in the issues.** Follow the links for scope, design, and status. Larger changes are proposed as Snapshot Enhancement Proposals ([SNEPs](https://github.com/ai-dynamo/snapshot/blob/main/docs/design/README.md)).

## Q4 2026 (October to December)

### Delivered

Releases [v0.2.0](https://github.com/ai-dynamo/snapshot/releases/tag/v0.2.0) and [v0.3.0](https://github.com/ai-dynamo/snapshot/releases/tag/v0.3.0) shipped this quarter.

- **Multi-GPU workloads on one node.** Checkpoint and restore a workload that uses several GPUs on a single node ([#627](https://github.com/ai-dynamo/snapshot/issues/627)).
- **Arm64 support.** Snapshot runs on arm64 nodes as well as x86_64 ([#127](https://github.com/ai-dynamo/snapshot/issues/127)).
- **Object storage.** Keep snapshots in any S3-compatible object store, in addition to the snapshot volume ([#562](https://github.com/ai-dynamo/snapshot/issues/562)).
- **Safer restores.** A restore that cannot work on the destination node is refused up front, with a clear reason ([#231](https://github.com/ai-dynamo/snapshot/issues/231)).
- **Lifecycle hooks.** A workload tells Snapshot when it is ready to checkpoint and when a restore has finished, without a custom entrypoint. vLLM has merged its own support, which will be available in vLLM v0.34 ([#411](https://github.com/ai-dynamo/snapshot/issues/411), [vllm-project/vllm#62570](https://github.com/vllm-project/vllm/pull/62570)).

### In Progress

- **Coordinated multi-Pod checkpoint and restore (alpha).** Checkpoint and restore a group of cooperating Pods as one unit. This is the building block for prefill/decode serving and for models that span nodes ([#324](https://github.com/ai-dynamo/snapshot/issues/324)).
- **Prefill/decode serving.** Restore prefill and decode workers independently or together ([#847](https://github.com/ai-dynamo/snapshot/issues/847), [#324](https://github.com/ai-dynamo/snapshot/issues/324)).
- **Multi-node on NVLink.** Restore a model that spans nodes connected by multi-node NVLink. This needs NVIDIA driver support for sharing GPU memory across nodes ([#611](https://github.com/ai-dynamo/snapshot/issues/611)).
- **Retries and fallback.** A failed checkpoint or restore is retried, and a restore that cannot succeed falls back to a normal cold start instead of leaving the workload stuck ([#319](https://github.com/ai-dynamo/snapshot/issues/319), [#244](https://github.com/ai-dynamo/snapshot/issues/244), [#247](https://github.com/ai-dynamo/snapshot/issues/247)).
- **Reuse snapshots.** Restore a workload from a snapshot that already exists, across workloads, namespaces, and clusters, so the same model is not checkpointed over and over ([#604](https://github.com/ai-dynamo/snapshot/issues/604), [#740](https://github.com/ai-dynamo/snapshot/issues/740), [#809](https://github.com/ai-dynamo/snapshot/pull/809)).
- **Stock vLLM out of the box.** Use the upstream vLLM image with no wrapper script or custom image ([#44](https://github.com/ai-dynamo/snapshot/issues/44), [#234](https://github.com/ai-dynamo/snapshot/issues/234); help wanted).
- **Faster restores.** Overlap loading GPU state with the rest of the restore ([#710](https://github.com/ai-dynamo/snapshot/issues/710), [#943](https://github.com/ai-dynamo/snapshot/issues/943)).
- **Faster restores with ModelExpress (vLLM).** Running replicas hand model weights to new replicas over the network, so a new replica restores faster from a smaller snapshot ([#927](https://github.com/ai-dynamo/snapshot/issues/927)).
- **Node-local cache (phase 1).** Optionally keep a copy of a snapshot on a node's fast local storage (such as NVMe), so later restores on that node read from it instead of the snapshot store. Snapshot status shows which nodes hold a copy, and administrators can warm nodes ahead of time with their own tools ([#644](https://github.com/ai-dynamo/snapshot/issues/644), design proposal [#918](https://github.com/ai-dynamo/snapshot/pull/918)).
- **Storage per workload (object storage).** A workload can use its own S3-compatible bucket as its snapshot store instead of the cluster default ([#248](https://github.com/ai-dynamo/snapshot/issues/248), design proposal [#889](https://github.com/ai-dynamo/snapshot/pull/889)).

## Q1 2027 (January to March)

Planned. Scope and order may shift as Q4 work lands.

- **Coordinated multi-Pod checkpoint and restore (beta).** Harden it using feedback from the alpha ([#984](https://github.com/ai-dynamo/snapshot/issues/984)).
- **Multi-node on more networks.** Multi-node restore on InfiniBand and RoCE networks, in addition to NVLink. Builds on coordinated multi-Pod checkpoint and restore ([#324](https://github.com/ai-dynamo/snapshot/issues/324)).
- **SGLang and TensorRT-LLM out of the box.** The same no-wrapper experience as vLLM ([#45](https://github.com/ai-dynamo/snapshot/issues/45), [#236](https://github.com/ai-dynamo/snapshot/issues/236); help wanted).
- **Node-local cache (phase 2).** Pre-warm nodes: copy a snapshot to selected nodes ahead of a scale-up, so even the first restore on those nodes is fast ([#644](https://github.com/ai-dynamo/snapshot/issues/644)).
- **Metrics.** Prometheus metrics for checkpoints and restores, so operators can see durations and failures ([#665](https://github.com/ai-dynamo/snapshot/issues/665), [#879](https://github.com/ai-dynamo/snapshot/issues/879)).
- **More guides.** Walkthroughs for llm-d, Argo CD, vCluster, and DGX Spark ([#990](https://github.com/ai-dynamo/snapshot/issues/990), [#654](https://github.com/ai-dynamo/snapshot/issues/654), [#840](https://github.com/ai-dynamo/snapshot/issues/840), [#624](https://github.com/ai-dynamo/snapshot/issues/624); some are good first issues).

## Q2 2027 (April to June)

Direction. Expect this list to be refined as Q1 progresses.

- **ModelExpress for SGLang and TensorRT-LLM.** The same faster restore as vLLM ([#927](https://github.com/ai-dynamo/snapshot/issues/927)).
- **Snapshot-aware placement.** Prefer nodes that can restore a snapshot and already hold a cached copy of it when a restore is scheduled ([#644](https://github.com/ai-dynamo/snapshot/issues/644)).
- **Smaller, faster snapshots.** Optional compression of snapshots, with hardware-assisted decompression where available ([#573](https://github.com/ai-dynamo/snapshot/issues/573), [#938](https://github.com/ai-dynamo/snapshot/issues/938)).
- **llm-d built in.** Restored replicas work with the llm-d router and autoscaler without custom glue ([#992](https://github.com/ai-dynamo/snapshot/issues/992)).
- **NVIDIA Government-Ready.** FIPS mode, hardened images, and validated publishing, for regulated environments. Signed releases and SBOMs are already in place ([#993](https://github.com/ai-dynamo/snapshot/issues/993)).

## Long Term

Directions we are exploring. They have no date, and most have no issue yet. An issue is opened when the work is scheduled.

- **Automatic checkpoint and restore.** Snapshot decides when to checkpoint a workload and restores new replicas on its own as a deployment scales, with no wiring by the integrator.
- **Recovery.** Come back quickly after an application failure, preemption, or eviction, and fail over to a restored replica (design proposal for application failures: [#884](https://github.com/ai-dynamo/snapshot/pull/884)).
- **More workload types.** GPU instances, interactive notebooks, GPU sandboxes, and training (fast and incremental checkpoints, spot instances, reinforcement learning).
- **Heterogeneous GPUs.** Restore on a different GPU model than the one a snapshot came from.
- **Protected snapshots.** Signing and encryption of snapshots, and confidential computing ([#563](https://github.com/ai-dynamo/snapshot/issues/563)).
- **Sandboxed runtimes.** Run snapshotted workloads under gVisor or Kata Containers.
- **Storage per workload (PVC).** The same for a workload's own PVC ([#248](https://github.com/ai-dynamo/snapshot/issues/248)).
- **More storage options.** Different storage for different parts of a snapshot, direct storage-to-GPU paths, and peer-to-peer checkpoints.

## Shape the Roadmap

- **A use case that is not here?** Start a [Discussion](https://github.com/ai-dynamo/snapshot/discussions). Issues are for work the project intends to do.
- **Want to build one of these?** Pick an item marked help wanted or a good first issue, or open a [feature request](https://github.com/ai-dynamo/snapshot/issues/new/choose). A maintainer labels accepted issues `approved` before significant work starts; see [CONTRIBUTING.md](https://github.com/ai-dynamo/snapshot/blob/main/CONTRIBUTING.md#start-with-an-issue).
- **Talk to the team.** Join the [ai-dynamo Slack](https://ai-dynamo.org/slack) and find us in `#snapshot-dev`.

Snapshot is also used by [NVIDIA Dynamo](https://github.com/ai-dynamo/dynamo). See the [Dynamo roadmap issues](https://github.com/ai-dynamo/dynamo/issues?q=is%3Aissue+label%3Aroadmap) for how it fits.
