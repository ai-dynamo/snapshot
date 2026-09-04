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

## Shared CUDA memory (cuinterpose)

Pods that opt in with the `nvidia.com/cuinterpose: enabled` annotation can be
checkpointed while their processes share GPU memory through the CUDA virtual
memory API and CUDA multicast (tensor-parallel workers, NCCL, FlashInfer,
PyTorch symmetric memory). The design and its reasoning are in
[the cuinterpose reference](reference/cuinterpose.md). What it does not cover:

- **Memory shared with a process that does not carry the shim.** An import of a
  descriptor that did not come from the shim (a raw `cuMemImportFromShareableHandle`)
  cannot be repaired after restore, so the checkpoint is refused while such an
  import is alive. A descriptor duplicated or passed around outside the shim's
  view is the same case.
- **Fabric handles.** Allocations and multicast objects created with the
  `CU_MEM_HANDLE_TYPE_FABRIC` handle type (or any handle type other than exactly
  the POSIX file descriptor) pass through untracked; their sharing does not
  survive a checkpoint. PyTorch, vLLM, and SGLang allocators prefer fabric
  handles on nodes where the driver offers them; configure them for POSIX
  descriptors (for example FlashInfer's `trtllm` allreduce backend rather than
  `mnnvl`).
- **Legacy CUDA IPC** (`cudaIpcGetMemHandle`), multi-node (IMEX) multicast, and
  another `dlsym` interposer in the same process.
- **Forking a process after it holds tracked memory.** The child gets a fresh
  identity and no records; the parent's sharing is unaffected.
- **A checkpoint while communicators are being set up.** The workload must be
  quiescent from the moment the checkpoint starts until the restore-complete
  sentinel: no CUDA calls, no new shares, no new processes.
- **Topology changes between checkpoint and restore:** a different GPU count,
  multicast group, or per-rank `CUDA_VISIBLE_DEVICES` (the coordinator identifies
  devices by their process-local ordinal).
- **Rollback.** Once the shim has taken the sharing apart for a checkpoint there
  is no way back; a checkpoint that fails afterwards ends the source.
- **Limits:** at most 32 access descriptors per mapping and 4096 records per
  process; `cuMemUnmap` and `cuMemSetAccess` over part of a tracked mapping are
  refused.
- **State files from earlier builds** are not read; source and restore nodes
  must run the same agent image.
