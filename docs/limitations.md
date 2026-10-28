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
- [Lifecycle hooks](reference/workload-contract.md#lifecycle-hooks) support a
  file or plain HTTP, without authentication. HTTP hooks don't work in
  host-network Pods, and hook files must be on the container's own filesystem or
  an `emptyDir`.

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
