# Snapshot

Snapshot is a Kubernetes-native checkpoint and restore system for NVIDIA GPU
workloads. It checkpoints a fully initialized GPU pod — its running process, with
CPU and GPU memory — and restores that state on any compatible node, so a pod
becomes ready in seconds instead of minutes.

Snapshot provides the checkpoint and restore primitives for GPU pods.
Orchestration — which pods to checkpoint, when, and how the checkpoints are
restored — is left to the systems that integrate it.

> [!NOTE]
> Snapshot's APIs may still change, so it is not yet recommended for
> production-critical workloads.

## The Problem

In inference serving, a replica can't answer a single request until it is fully
initialized — model weights loaded into GPU memory, CUDA and runtime libraries
initialized, execution kernels warmed up, and computation graphs compiled. For
large models, this **cold start** takes minutes.

That cost is paid over and over. Every replica added to meet demand, every
scale-up from zero, every restart or reschedule pays the full cold start again
before it can serve traffic:

- New replicas take minutes to become ready, so autoscaling lags behind demand.
- Teams over-provision idle GPUs just to absorb demand spikes.
- Restarts and reschedules stall serving capacity exactly when it is needed.

## The Solution

Snapshot checkpoints a fully initialized pod once and restores it on demand, so a
new replica comes online in seconds instead of minutes.

- **Checkpoint** — pause a running pod and save its complete execution state (CPU
  and GPU memory) as a portable artifact.
- **Restore** — start a new pod from that artifact on any node with matching GPU
  hardware and driver versions, skipping model loading and warm-up; the process
  resumes from where it was checkpointed.

## Benchmarks

<div align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/development/img/cold-start-vs-snapshot-dark.svg">
    <source media="(prefers-color-scheme: light)" srcset="docs/development/img/cold-start-vs-snapshot-light.svg">
    <img width="900" alt="Paired column chart comparing cold start against Snapshot for each model. Cold start ranges from 52 to 102 seconds, Snapshot from 3.5 to 40.9 seconds." src="docs/development/img/cold-start-vs-snapshot-light.svg">
  </picture>
</div>

<p align="center"><i><b>Figure 1.</b> Restoring a captured workload is 2.4 to 14.9 times faster than starting the same workload from scratch on the same hardware.</i></p>

For the experiment setup, the per stage breakdown, and the full results, see [benchmarks](docs/development/benchmarks.md).

## When to use it

- **Autoscaling inference** — scale out from an existing snapshot: bring the N+1
  replica and beyond online in seconds to keep pace with demand.
- **Scale-to-zero** — park idle models at zero replicas and restore them quickly
  when capacity is needed again.
- **Faster restarts and reschedules** — recover a pod's initialized state after a
  restart or a move to another node.

Snapshot currently focuses on inference cold-start; further use cases are on the
roadmap.

## Who it's for

Snapshot is a building block for the teams that build and operate inference
infrastructure:

- **Developers** building Kubernetes controllers, operators, or serving platforms.
- **MLOps and platform engineers** who assemble deployment pipelines declaratively
  with GitOps or workflow tools.

## Prerequisites

Before installing Snapshot, make sure the following are in place:

- A Kubernetes cluster with NVIDIA GPU nodes
- containerd or CRI-O as the container runtime
- [NVIDIA GPU Operator](https://github.com/NVIDIA/gpu-operator) 26.3 or newer, with CUDA driver 580 or newer and MIG disabled
- A `ReadWriteMany` (RWX) storage class
- The [Helm](https://helm.sh/docs/intro/install) CLI
- A cluster that permits privileged pods for the node agent — see [Security](docs/operations/security.md)

## Installation

Snapshot installs as a single per-cluster Helm release — a control-plane operator
plus a privileged node agent (DaemonSet) on GPU nodes. Install it in its own
namespace, and run GPU workloads in separate namespaces.

Snapshot can be installed:

- **From a release** (recommended)
- **From source** (build the images and install locally)

### From a release

Find the latest version on the [releases page](https://github.com/ai-dynamo/snapshot/releases),
then install the published chart, replacing `<VERSION>`:

```bash
helm install snapshot oci://ghcr.io/ai-dynamo/snapshot/snapshot \
  --version <VERSION> \
  --namespace snapshot --create-namespace
```

By default the chart provisions its own RWX checkpoint volume, shared by every
checkpoint. See [Storage](docs/operations/storage.md) for the volume model and options
(including reusing an existing claim), and [Installation](docs/operations/install.md)
for install and uninstall.

### From source

Follow the instructions in [Building from source](docs/development/build-from-source.md).

## How to use it

Snapshot is driven entirely through Kubernetes resources, with standard tooling.
Create a `PodSnapshot` to checkpoint a running pod, and annotate a new pod with
`nvidia.com/restore-from` to restore it. Higher-level systems wire these
primitives into their own control loop.

| Resource | Scope | Role |
|----------|-------|------|
| `PodSnapshot` | Namespaced | Created by callers to request a checkpoint, or to reference an artifact for restore. |
| `PodSnapshotContent` | Cluster-scoped | System-managed record of the physical artifact, bound to a `PodSnapshot`. Created by the operator, never by the caller. |
| `SnapshotJob` | Namespaced | Runs a pod from a template and checkpoints it into a `PodSnapshot` once ready — a self-contained checkpoint job. |
| `nvidia.com/restore-from` | Namespaced | Pod annotation that triggers a restore from a named `PodSnapshot` in the same namespace. |

Under the hood, a control-plane operator and a per-node agent perform the CRIU
and `cuda-checkpoint` work; see [Architecture](docs/reference/architecture.md).
The [API reference](docs/reference/api.md) covers the resources and the
checkpoint/restore lifecycle.

Once Snapshot is installed, follow the **[usage guides](docs/guides/README.md)**
to checkpoint and restore a pod.

## Limitations

Current limitations:

- Single-GPU workloads only.
- x86_64 nodes only.
- vGPU is not supported.
- Runs only on NVIDIA GPUs supported by the required CUDA driver.

Multi-GPU and Arm support are on the roadmap.

## Documentation

**Get started**

- [Usage guides](docs/guides/README.md) — build a snapshot-ready image per inference framework, then checkpoint and restore.

**Reference**

- [API](docs/reference/api.md) — `PodSnapshot`, `PodSnapshotContent`, `SnapshotJob`, and the `restore-from` annotation.
- [Architecture](docs/reference/architecture.md) — operator and node-agent design, and the checkpoint/restore internals.
- [CLI (`snapshotctl`)](docs/reference/cli.md) — lower-level checkpoint/restore from a pod manifest.

**Operations**

- [Installation](docs/operations/install.md) — Helm install and uninstall.
- [Storage](docs/operations/storage.md) — the shared checkpoint volume and how to configure it.
- [Troubleshooting](docs/operations/troubleshooting.md) — common failures and where to look.
- [Security](docs/operations/security.md) — the privileged agent, seccomp, and Pod Security.

**Development**

- [Building from source](docs/development/build-from-source.md) — build the images and install locally.
- [Benchmarks](docs/development/benchmarks.md) — how startup performance is measured.

**More**

- [Limitations & known issues](docs/limitations.md) — current limitations and what's on the roadmap.

## Adopters

[NVIDIA Dynamo](https://github.com/ai-dynamo/dynamo), the open-source
inference-serving stack, integrates Snapshot for GPU cold-start. On Dynamo, Snapshot is available through it directly — see
[Snapshotting GPU Workers](https://docs.nvidia.com/dynamo/latest/kubernetes/operations/cold-start-optimizations/dynamo-snapshot)
in the Dynamo docs.

## Contributing

Contributions are welcome under the project's [Apache 2.0 license](LICENSE). See
[CONTRIBUTING.md](CONTRIBUTING.md) — all commits must be signed off (DCO).

## Security

To report a security vulnerability, follow the process in [SECURITY.md](SECURITY.md).

## Feedback

Feedback and issues are welcome — please [open an issue](https://github.com/ai-dynamo/snapshot/issues).

## License

Snapshot is licensed under the [Apache License 2.0](LICENSE).
