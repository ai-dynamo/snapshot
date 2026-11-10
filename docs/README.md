<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Snapshot Documentation

This page lists every page in the Snapshot documentation, grouped by section.

## Overview

- [What Is Snapshot?](overview/what-is-snapshot.md) - What Snapshot is, why it exists, and where it fits.
- [Core Concepts](overview/core-concepts.md) - Checkpoint and restore, resources, the workload contract, the snapshot store, and multi-GPU.
- [Architecture](overview/architecture-overview.md) - The components, the Kubernetes resources that drive them, and how a checkpoint and a restore proceed.
- [Performance](overview/performance.md) - Cold start and restore times for vLLM, SGLang, and TensorRT-LLM on one to eight B200 GPUs.
- [Limitations and Known Issues](overview/limitations.md) - Current limitations and what is on the roadmap.

## Get Started

- [Prerequisites](get-started/prerequisites.md) - Minimum GPU driver and other dependency versions, per feature.
- [Installation](get-started/installation.md) - Helm install and uninstall.
- [Quickstart: Checkpoint and Restore One Pod](get-started/quickstart.md) - Checkpoint and restore one pod end to end.

## Using Snapshot

- [Using Snapshot](using-snapshot/README.md) - The checkpoint and restore flow, with a recipe for each inference framework.
- [Prepare a vLLM Workload](using-snapshot/supported-frameworks/vllm.md) - Make a vLLM workload snapshot-ready.
- [Prepare an SGLang Workload](using-snapshot/supported-frameworks/sglang.md) - Make an SGLang workload snapshot-ready.
- [Prepare a TensorRT-LLM Workload](using-snapshot/supported-frameworks/tensorrt-llm.md) - Make a TensorRT-LLM workload snapshot-ready.
- [Use Your Own Engine or Image](using-snapshot/supported-frameworks/use-your-own-engine-or-image.md) - Use an engine other than vLLM, SGLang, or TensorRT-LLM.
- [Checkpoint a Pod](using-snapshot/checkpoint.md) - Save a running replica's state as a `PodSnapshot`.
- [Restore a Pod](using-snapshot/restore.md) - Start a new replica from a snapshot.

## Operate

- [Configure Storage](operate/configure-storage.md) - The shared snapshot volume and how to configure it.
- [Tune Storage](operate/tune-storage/README.md) - Storage option tuning and examples.
- [Upgrade Snapshot and the CRDs](operate/upgrade.md) - Upgrade with `helm upgrade`; the operator updates the CRDs.

## Reference

- [API Reference](reference/api.md) - `PodSnapshot`, `PodSnapshotContent`, `SnapshotJob`, and the `restore-from` annotation.
- [Helm Chart Values](reference/helm-values.md) - The Helm values that configure Snapshot, with their defaults.
- [`snapshotctl` CLI](reference/cli.md) - Lower-level checkpoint/restore from a pod manifest.
- [Workload Contract](reference/workload-contract.md) - What a workload must do, and how its pod must be shaped, to be checkpointed and restored; a custom image is one packaging method.
- [Restore Pod Contract](reference/restore-pod-contract.md) - The pod interface for programmatic restore: annotations, control volume, startup gate, and seccomp.

## Troubleshooting

- [Reading a Failure Message](troubleshooting/reading-a-failure-message.md) - Find the step a checkpoint or restore failed at.
- [Troubleshoot Checkpoints](troubleshooting/checkpoints.md) - A checkpoint does not finish.
- [Troubleshoot Restores](troubleshooting/restores.md) - A restore is slow or fails.
- [Troubleshoot Slow Restores](troubleshooting/slow-restores.md) - Check timings, NFS clients, mount options, and fio results.
- [Troubleshoot the Agent](troubleshooting/agent.md) - The node agent or a component it runs fails.

## Development

- [Building from Source](development/build-from-source.md) - Build the images and install locally.
- [Reproducing the Benchmarks](development/benchmarks-guide.md) - Run the cold-start, checkpoint, and restore measurements.
- [Writing Documentation](development/writing-docs.md) - Where a change goes and how to write it.
- [Naming Conventions](development/naming.md) - Terms and naming patterns for the API, chart, and docs.
- [Snapshot Enhancement Proposal (SNEP)](design/README.md) - The design process for significant Snapshot changes.
