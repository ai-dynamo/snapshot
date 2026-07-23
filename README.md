# Snapshot

> **This project is under construction.**


Snapshot is a Kubernetes-native checkpoint and restore system for NVIDIA GPU workloads.
It enables AI frameworks and platforms to capture a fully initialized GPU worker and restore that state on any compatible node, allowing new pods to become ready in seconds instead of minutes.

Snapshot focuses on one responsibility: reliably capturing and restoring running GPU workloads. Higher-level decisions - such as which workloads to checkpoint, when to create snapshots, or when to restore them - are left to the systems integrating with Snapshot.


## Why Snapshot?

GPU inference workers are expensive to start. Before serving a single request, a worker typically needs to load large model weights into GPU memory, initialize CUDA and other runtime libraries, warm up execution kernels, and compile or optimize computation graphs.

For large models, this initialization can take several minutes. Every new replica, pod restart, reschedule, or scale-up event repeats the entire process, paying that cost from scratch.
Snapshot eliminates most of this overhead by restoring a previously initialized worker instead of starting a new one.

## How it Works

Snapshot exposes checkpoint and restore as Kubernetes resources.

#### Capture

To create a snapshot, a caller identifies the pod to checkpoint. Snapshot pauses the running process and captures its complete execution state, including both CPU memory and GPU memory, into a persistent artifact.
This artifact is not a container image, a filesystem snapshot, or a volume snapshot. Instead, it represents the complete in-memory state of a live, fully initialized GPU worker.

#### Restore

To restore a worker, a new pod references a previously captured snapshot artifact. During pod startup, Snapshot restores the captured process state directly into the container, bypassing model loading, kernel warm-up, and other initialization steps. The restored process resumes execution from the exact point where it was captured.
Snapshots are portable across compatible machines and can be restored on any node with matching GPU hardware and driver versions. They are not tied to the node where they were originally created.

&nbsp;

## APIs
| Resource | Scope | Role |
|----------|-------|------|
| `PodSnapshot` | Namespaced | Created by callers to request a capture or reference an artifact for restore. |
| `PodSnapshotContent` | Cluster-scoped | System-managed record of the physical artifact, bound to a `PodSnapshot`. Created by the Snapshot operator, never by the caller. |
| `nvidia.com/restore-from` | Namespaced | Added as a pod annotation to trigger restore from a named `PodSnapshot` in the same namespace. |

&nbsp;


## Architecture

Snapshot consists of two main components.

#### Operator

The Kubernetes operator manages the control plane.

It is responsible for:

* Orchestrating checkpoint and restore operations.
* Tracking snapshot lifecycle.
* Exposing status through Kubernetes resources.
* Managing cleanup.


#### Node Agent

A privileged node agent runs on every GPU node.

It performs the actual checkpoint and restore operations by invoking CRIU and cuda-checkpoint against live processes.

The node agent is intentionally an implementation detail. Clients never communicate with it directly.

&nbsp;

## Design Principles

Snapshot owns the mechanics of checkpoint and restore—not the policy.

Systems integrating with Snapshot decide:

* Which workloads should be checkpointed.
* When snapshots should be created.
* When they should be restored.
* How failures should be handled.

Snapshot executes those requests and exposes the resulting state.

Everything Snapshot manages is represented as Kubernetes resources. Snapshot metadata, capture progress, restore status, and lifecycle information are all observable through the Kubernetes API using standard Kubernetes tooling.

Clients interact exclusively through the Kubernetes API. No platform-specific APIs, direct node communication, or custom protocols are required.

&nbsp;

## Status

The project is in early development. API types and control plane components are scaffolded but not yet feature-complete. Not ready for production use.

