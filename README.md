# Snapshot

> **This project is under construction.**

Snapshot is a Kubernetes operator that checkpoints and restores NVIDIA GPU workload pods.

## The cold-start problem

GPU inference workers are expensive to start. Before a worker can serve a single request, it must load large model weights onto GPU memory, warm up execution kernels, and compile computation graphs. On large models this can take several minutes. Every new replica, every pod restart, and every scale-up event pays that initialization cost from scratch.

## How Snapshot addresses it

Snapshot captures a running GPU pod at its initialized, ready state, saving both CPU process memory and GPU memory to persistent storage. When a new pod is needed, the Snapshot agent replays that artifact directly, bypassing initialization entirely. What took minutes takes seconds. The checkpoint can be restored on any node with a matching GPU architecture and CUDA environment, so replicas are not bound to the node where the original pod ran.

## How it works

Snapshot deploys as two components. A cluster-wide operator manages the checkpoint lifecycle: it coordinates when to capture a pod, tracks the artifact as it is written, and marks the result ready for restore. A per-node agent DaemonSet performs the actual capture and restore work on each GPU node, using CRIU (Checkpoint/Restore in Userspace) and NVIDIA's `cuda-checkpoint` to dump and replay both CPU process state and GPU memory.

## Status

The project is in early development. API types and control plane components are scaffolded but not yet feature-complete. Not ready for production use.
