# Limitations and known issues

Snapshot currently focuses on inference cold-start; further use cases are on the
roadmap.

## Current limitations

- Linux nodes on x86_64 or arm64.
- Workloads, including multi-GPU workloads, must run on a single node. Multi-node is on the roadmap.
- vGPU is not supported.
- Runs only on NVIDIA GPUs supported by the required CUDA driver.
- The checkpointed workload must not have tools that intercept `libcuda.so`
  calls (e.g. Datadog GPU monitoring) — CUDA calls may fail or hang after
  restore, with undefined results. Disable such interception for workloads
  that will be snapshotted.
- The restore proxy needs a container runtime with NRI v0.10 or newer, enabled:
  containerd 2.2 or newer. The node agent uses NRI to make the proxy PID 1 in
  restore containers. On a node without NRI, or with an older runtime, the
  agent restores without the proxy: restore Pods need an inert command there,
  and the container does not follow the restored workload. See
  [Nodes without the restore proxy](reference/restore-pod-contract.md#nodes-without-the-restore-proxy).
- A Pod gets one restore. If a restored container starts again, for example
  under `restartPolicy: Always` after the restored process exits, it ends in
  `CrashLoopBackOff`. Delete the Pod to restore again. Retrying a restore in
  the same Pod is on the roadmap.
- Several Snapshot installations whose clusters share nodes, such as vCluster
  tenants, work only when each sets `daemonset.nriHostNamespace` to its own host
  namespace. An installation with it empty acts on every restore Pod on its
  nodes.

## Driver version compatibility

- On UMD 580 (verified on 580.126.09), restore fails for any checkpointed
  process holding multicast (NCCL NVLS / symmetric-memory) groups: restore
  fails in `RestoreMulticastDevices` with `CUDA_ERROR_INVALID_DEVICE`. On
  multicast-capable systems this affects default multi-GPU vLLM TP>=2
  deployments (multi-GPU support is on the roadmap; recorded here because
  the defect is in the CUDA driver, not in Snapshot), even with NVLS and
  symmetric-memory allreduce disabled, because vLLM's custom-allreduce path
  always creates a multicast group. NVIDIA reports this as an r580-UMD
  defect that should be fixed in r595 and above; the 580 KMD can be kept.
  Tested on 4xH20 (KMD 580.126.09), same UMD on the target processes and the
  host CLI: 610.57.04 passes vLLM TP2 and TP4 checkpoint/restore 20/20 rounds
  with CUDA graphs preserved; 615.71.09 passes TP2 20/20 (TP4 not tested).
- The host-side `cuda-checkpoint` CLI must not remain on UMD 580 while target
  processes use a newer UMD: with the CLI on 580.126.09 and targets on
  615.71.09, freeze hangs; the same UMD on both sides passes. Mixed
  combinations newer than 580 (e.g. 610 CLI + 615 target) have not been
  tested.

## GPU device selection and restore paths

Explicit `NVIDIA_VISIBLE_DEVICES` index or full-UUID lists take precedence over
allocation discovery. Indices refer to the host inventory, not CUDA ordinals.
Absent values and `all` retain normal discovery. Empty, `none`, and `void` disable
legacy injection; Snapshot checks only actual container GPU visibility in these
cases, so independently injected CDI devices can still be discovered. It does
not fall back to host or allocation UUIDs for disabled legacy selections.
`CUDA_VISIBLE_DEVICES` is not rewritten.

Restoring onto a different physical `/dev/nvidiaN` path requires a fresh
checkpoint containing UUID-to-device-path metadata. Older checkpoints retain
their existing behavior; Snapshot does not infer missing device mappings.
This path remapping does not relax hardware, driver, or CUDA-state compatibility
requirements, nor establish multi-GPU workload support.
