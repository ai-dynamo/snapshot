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

All results are for single-GPU workloads.

| Platform | vLLM | SGLang | TensorRT-LLM |
| --- | --- | --- | --- |
| x86_64 | Supported | Supported | Supported |
| arm64, NVIDIA GB300 | Validated | Validated | Not working: the checkpoint does not complete |

The arm64 results come from NVIDIA GB300 nodes (kernel `6.17.0-1019-aws-64k`
with 64 KiB pages, driver 580.173.02, GPU Operator 26.3.3, containerd). Other
arm64 hardware, kernels and page sizes are untested.

### arm64 requirements

- Set `config.criu.imageIoMode` to `writeback`. With the default `direct`,
  checkpoint succeeds but restore fails in CRIU with
  `vdso: Invalid ELF magic` on the tested kernel.
- Use framework images that publish a `linux/arm64` variant. A digest that pins
  a single-architecture manifest (rather than the multi-architecture index)
  pulls the wrong architecture.
- Build the agent and PageBroker images with `AGENT_PLATFORM=linux/arm64`; see
  [Build from source](development/build-from-source.md).

### Known issue: TensorRT-LLM checkpoint hangs on arm64

On the tested GB300 nodes the TensorRT-LLM guide workload does not finish its
checkpoint. The agent logs `Starting checkpoint operation` and the GPU
checkpoint through PageBroker never completes, with no error. Cancelling the
checkpoint makes PageBroker exit and restart. vLLM and SGLang are not affected.

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
