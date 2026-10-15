# Limitations and known issues

Snapshot currently focuses on inference cold-start; further use cases are on the
roadmap.

## Current limitations

- Single-GPU workloads only.
- Linux nodes on x86_64 or arm64. See [Platform support](#platform-support) for
  what is validated on each.
- vGPU is not supported.
- Runs only on NVIDIA GPUs supported by the required CUDA driver.
- The checkpointed workload must not have tools that intercept `libcuda.so`
  calls (e.g. Datadog GPU monitoring) — CUDA calls may fail or hang after
  restore, with undefined results. Disable such interception for workloads
  that will be snapshotted.

Multi-GPU support is on the roadmap.

## Platform support

Single-GPU workloads only.

| Platform | vLLM | SGLang | TensorRT-LLM |
| --- | --- | --- | --- |
| x86_64 | Supported | Supported | Supported |
| arm64, NVIDIA GB300 | Validated | Validated | Validated with `TLLM_WORKER_USE_SINGLE_PROCESS=1` |

arm64 was tested on GB300 nodes (kernel `6.17.0-1019-aws-64k`, 64 KiB pages,
driver 580.173.02). Other arm64 hardware and kernels are untested.

- **Images.** Use framework images that publish a `linux/arm64` variant, pinned by
  the multi-architecture index digest. A single-architecture digest pulls the wrong
  architecture.
- **TensorRT-LLM.** With the guide's default setup, the checkpoint hangs on the
  tested nodes because the MPI worker process does not answer CUDA checkpoint
  requests. Set `TLLM_WORKER_USE_SINGLE_PROCESS=1` to run the worker in the app
  process. x86_64 is unaffected.

## Storage

On FSx for Lustre (tested with client 2.15.6), restore failed with the default
`config.criu.imageIoMode: direct` (`vdso: Invalid ELF magic`). Set it to
`writeback`. `direct` worked on EBS.

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
