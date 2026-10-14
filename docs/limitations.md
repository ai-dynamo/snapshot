# Limitations and known issues

Snapshot currently focuses on inference cold-start; further use cases are on the
roadmap.

## Current limitations

- Single-GPU workloads only.
- x86_64 nodes only.
- vGPU is not supported.
- Runs only on NVIDIA GPUs supported by the required CUDA driver.
- The checkpointed workload must not have tools that intercept `libcuda.so`
  calls (e.g. Datadog GPU monitoring) — CUDA calls may fail or hang after
  restore, with undefined results. Disable such interception for workloads
  that will be snapshotted.

Multi-GPU and Arm support are on the roadmap.

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
