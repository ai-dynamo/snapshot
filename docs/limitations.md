# Limitations and known issues

Snapshot currently focuses on inference cold-start; further use cases are on the
roadmap.

## Current limitations

- Single-GPU workloads only.
- x86_64 nodes only.
- vGPU is not supported.
- Runs only on NVIDIA GPUs supported by the required CUDA driver.
- Not compatible with tools that intercept `libcuda.so` calls (e.g. Datadog
  GPU monitoring) — CUDA calls hang after restore. Disable such interception
  for workloads that will be snapshotted.

Multi-GPU and Arm support are on the roadmap.
