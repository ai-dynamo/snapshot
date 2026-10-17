# vLLM 0.28+ compatibility notes

These notes supplement the [existing vLLM guide](vllm.md) for vLLM 0.28+.
When vLLM is used as a Python library, its default worker multiprocessing
method can be [`fork`, rather than `spawn`](https://docs.vllm.ai/en/latest/design/multiprocessing/).
Forked workers can inherit CUDA state and NVIDIA device handles. CUDA
checkpointing must handle that state across the worker process tree. The
options below address this multiprocessing capture issue.

The existing application, deployment manifests, and pinned 0.27.1 image
remain unchanged. Start with
**Snapshot 0.1.0** for the single-GPU options below: they were validated with
**vLLM 0.30.0** and do not require a newer Snapshot release or a shared-memory
annotation. **Snapshot 0.2+** adds the shared-CUDA-memory method for single-node
workloads with one or multiple GPUs, while retaining those first two options
for their single-GPU configurations. Both Snapshot versions are covered below.
The tested runtime was the official Ubuntu 24.04 image:

```text
vllm/vllm-openai:v0.30.0-ubuntu2404@sha256:439c19d48db36401abc914b9842d060fe610a54bc3ac29bb02505f0b69af1baf
```

The previous guide's warning about 0.28 must not be read as a blanket
restriction on every newer version: 0.30.0 checkpoint, restore, and inference
passed in the configurations below.

## Snapshot 0.1.0: validated options

| Workload | Snapshot | Setup | Result |
| --- | --- | --- | --- |
| vLLM 0.30.0, one GPU | 0.1.0 | Application defaults worker multiprocessing to `spawn` before importing vLLM | Checkpoint, restore, and HTTP inference passed |
| vLLM 0.30.0, one GPU | 0.1.0 | Original application launched under `cuda-checkpoint --launch-job` | Checkpoint, restore, and HTTP inference passed |

### Option 1: worker multiprocessing with spawn

A single-GPU vLLM workload can have multiple worker processes. Initialize
the process start method before importing vLLM:

```python
os.environ.setdefault("VLLM_WORKER_MULTIPROC_METHOD", "spawn")
```

The current guide's `app.py` already does this. In our 0.30.0 test, the
unwrapped application without it failed capture on an open `/dev/nvidiactl`
descriptor; the `spawn` variant passed.

### Option 2: CUDA job wrapping

Launching the application with [`cuda-checkpoint --launch-job`](https://github.com/NVIDIA/cuda-checkpoint)
groups its CUDA processes into a job so CUDA checkpointing can coordinate
their state. This resolved the multiprocessing capture failure in the
Snapshot 0.1.0 single-GPU test without changing the application to use
`spawn`. It is an alternative to option 1, not an additional prerequisite.

SnapshotJob does **not** automatically add native CUDA job wrapping.

## Snapshot 0.2+: single-node workloads

### New method: shared CUDA memory with cuInterpose

Snapshot 0.2+ provides the new shared-CUDA-memory method, cuInterpose, for
**single-node workloads with either one GPU or multiple GPUs**. Enable it
with `nvidia.com/cuda-shared-memory-support: enabled` on
`SnapshotJob.spec.podTemplate.metadata.annotations`. SnapshotJob installs
the matching libraries and automatically adds the launcher; this method
does not require manually adding the native CUDA job wrapper described above.

The annotation does not configure vLLM parallelism or GPU resource requests.
The existing guide's application and manifests remain single-GPU examples;
using multiple GPUs also requires configuring vLLM's `tensor_parallel_size`
and matching GPU requests in the source and restore pods.

The application must still quiesce before capture and resume only after
`restore-complete`.

### The Snapshot 0.1.0 options remain available

Snapshot 0.2+ can also use **option 1 (`spawn`)** or **option 2 (native CUDA
job wrapping)** for the single-GPU configurations described above. The new
method is an additional option, not a requirement to replace those paths.
Neither earlier option requires the shared-memory annotation for that
single-GPU setup. Revalidate the specific Snapshot/vLLM pair when upgrading;
the recorded results for those two options were obtained on Snapshot 0.1.0.
