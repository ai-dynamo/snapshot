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
