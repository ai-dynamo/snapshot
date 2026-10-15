# vLLM 0.28+ compatibility notes

These notes supplement the [existing vLLM guide](vllm.md). They do not change
its application, deployment manifests, or pinned 0.27.1 image. Start with
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
passed in the configurations below. **0.28 and 0.29 themselves were not
rerun**, and the result does not establish compatibility with every later
version or model.

## Snapshot 0.1.0: validated options

| Workload | Snapshot | Setup | Result |
| --- | --- | --- | --- |
| vLLM 0.30.0, one GPU | 0.1.0 | Application defaults worker multiprocessing to `spawn` before importing vLLM | Checkpoint, restore, and HTTP inference passed |
| vLLM 0.30.0, one GPU | 0.1.0 | Original application launched under `cuda-checkpoint --launch-job` | Checkpoint, restore, and HTTP inference passed |

Both options used Qwen/Qwen3-0.6B on one NVIDIA B200 GPU, driver 595.58.03,
and VAST RWX checkpoint storage. Source and restore used the same immutable
runtime image. These are functional results for that configuration, not a
compatibility guarantee for other models, hardware, drivers, or versions.

### Option 1: worker multiprocessing with spawn

A single-GPU vLLM workload can have multiple worker processes. Initialize
the process start method before importing vLLM:

```python
os.environ.setdefault("VLLM_WORKER_MULTIPROC_METHOD", "spawn")
```

The current guide's `app.py` already does this. In our 0.30.0 test, the
unwrapped application without it failed capture on an open `/dev/nvidiactl`
descriptor; the `spawn` variant passed. No CUDA job wrapper or shared-memory
annotation was needed for this tested single-GPU path.

### Option 2: CUDA job wrapping

The original application also passed the Snapshot 0.1.0 single-GPU test when
launched under `cuda-checkpoint --launch-job`, without adding the `spawn`
setting. This is an alternative to option 1, not an additional prerequisite.

SnapshotJob does **not** automatically add native CUDA job wrapping. Launch
the workload under the wrapper before it creates
CUDA state and worker processes, persist the driver-created job file at
`/snapshot-control/cuda-checkpoint-job`, and point `CUDA_CHECKPOINT_JOB_FILE`
to that stable path. The workload image must contain the required binary;
source and restore must provide the required binary layout at the same path.

For example, with the guide's application and control volume mounted, use
this as the source container's startup command:

```sh
cuda-checkpoint --launch-job /bin/sh -c '
  set -eu
  : "${CUDA_CHECKPOINT_JOB_FILE:?CUDA job file was not provided by the driver}"
  umask 077
  job_file=/snapshot-control/cuda-checkpoint-job
  cat "$CUDA_CHECKPOINT_JOB_FILE" > "$job_file"
  export CUDA_CHECKPOINT_JOB_FILE="$job_file"
  exec python3 /snapshot-app/app.py
'
```

Wrapping only the later capture command is too late. The tested source
already ran under the wrapper when it initialized the engine. Revalidate
native wrapper behavior when changing the driver or runtime.

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
`restore-complete`. Validation with vLLM 0.30.0 covered tensor parallelism
across two GPUs (`TP=2`) on two B200 GPUs on one node using Snapshot
0.2.0-rc.2. This does not establish cross-node or cross-Pod support.

### The Snapshot 0.1.0 options remain available

Snapshot 0.2+ can also use **option 1 (`spawn`)** or **option 2 (native CUDA
job wrapping)** for the single-GPU configurations described above. The new
method is an additional option, not a requirement to replace those paths.
Neither earlier option requires the shared-memory annotation for that
single-GPU setup. Revalidate the specific Snapshot/vLLM pair when upgrading;
the recorded results for those two options were obtained on Snapshot 0.1.0.
