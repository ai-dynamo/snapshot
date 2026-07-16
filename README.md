# Snapshot

> **This project is under construction.**

Snapshot is a Kubernetes operator that checkpoints and restores NVIDIA GPU workload pods.

## What problem it solves

Starting a GPU inference workload from scratch takes time. Loading model weights, warming kernels, and compiling graphs can take minutes, even for moderate-sized models. Snapshot captures a pod at its initialized, ready state and replays that state on any compatible node. Subsequent pods start in seconds rather than starting over.

## How it works

Two components work together. A central operator manages the checkpoint lifecycle: scheduling captures, tracking artifacts, and coordinating restores. A per-node agent DaemonSet runs the actual work on each GPU node, quiescing the target container and using CRIU (Checkpoint/Restore in Userspace) and NVIDIA's `cuda-checkpoint` to dump process and GPU memory state to storage, then replaying that state into a new pod on restore.

## Status

The project is in early development. API types and control plane components are scaffolded but not feature-complete. Not ready for production use.
