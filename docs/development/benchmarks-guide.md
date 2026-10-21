# Reproducing the benchmarks

The tool in [`benchmarks/`](../../benchmarks) measures startup, checkpoints the
initialized engine and restores it three times. It supports vLLM, SGLang and
TensorRT-LLM on one node, using the engine guide applications and templates.
Each run writes a report; the [published tables](benchmarks.md) are maintained
separately.

## Prerequisites

- Python 3.12 or newer, `uv` and `kubectl`.
- An installed Snapshot operator, node agent and PageBroker.
- A node with enough free GPUs and host memory for the selected model.
- An engine image that supports the selected guide application's APIs.
- A populated model PVC, mounted read-only at `/hf-cache`.
- A writable work PVC for compiler caches and timing files.
- A filesystem checkpoint PVC, passed with `--pvc-name`. Artifact inventory does
  not support bound or remote stores.
- The NVIDIA runtime class, `/dev/net/tun` and the node's
  `profiles/block-iouring.json` seccomp profile, as required by the guide templates.

The agent must log checkpoint and restore timing summaries and the external
restore start marker. Missing timing evidence fails collection.

> [!NOTE]
> The runner does not install infrastructure, change node reservations or stop
> other users' workloads.

## Reference setup

The [published setup](benchmarks.md#benchmarking-setup) used B200 GPUs, driver
615.71.09, Snapshot v0.2.0-rc.2, vLLM 0.31.0, SGLang 0.5.21 and
TensorRT-LLM 1.3.0rc24. Pass the image to measure with `--image`; the guide's
default image may differ. Each result records the image and queries its installed
version.

The VAST client used these NFS mount options:

| Option | Value | Purpose |
| --- | --- | --- |
| `vers`, `proto` | `3`, `tcp` | NFSv3 over TCP |
| `rsize`, `wsize` | `1048576` | Read/write requests up to 1 MiB |
| `nconnect` | `32` | Up to 32 transport connections |
| `spread_reads` | enabled | Spread reads across connections |
| `remoteports` | four storage addresses | Use multiple server addresses |
| `nosharecache`, `nosharetransport` | enabled | Separate cache and transports for this mount |

These options require a compatible VAST client. Other storage can be measured;
the runner does not set mount options.

> [!NOTE]
> Check the actual `/proc/mounts` records saved in `startup.log`. A PV declaration
> alone does not prove the client uses the requested mount options.

## Quick start

Run these commands from the repository root.

### 1. Set the cluster and workload values

```bash
export BENCHMARK_CONTEXT=<context>
export WORKLOAD_NAMESPACE=<workload-namespace>
export SNAPSHOT_NAMESPACE=<snapshot-namespace>
export SNAPSHOT_RELEASE=<helm-release>
export CHECKPOINT_PVC=<checkpoint-pvc>
export MODEL_PVC=<model-pvc>
export WORK_PVC=<work-pvc>
export BENCHMARK_NODE=<node>
export ENGINE_IMAGE=<engine-image>
```

### 2. Check the installation

```bash
uv run --project benchmarks python -m snapshot_benchmarks metadata \
  --context "$BENCHMARK_CONTEXT" --namespace "$WORKLOAD_NAMESPACE" \
  --snapshot-namespace "$SNAPSHOT_NAMESPACE" --release "$SNAPSHOT_RELEASE" \
  --pvc-name "$CHECKPOINT_PVC"
```

### 3. Check the workload manifest

This dry run writes manifests without making Kubernetes calls.

```bash
uv run --project benchmarks python -m snapshot_benchmarks run \
  --context "$BENCHMARK_CONTEXT" --namespace "$WORKLOAD_NAMESPACE" \
  --snapshot-namespace "$SNAPSHOT_NAMESPACE" --release "$SNAPSHOT_RELEASE" \
  --pvc-name "$CHECKPOINT_PVC" --node "$BENCHMARK_NODE" \
  --model-pvc "$MODEL_PVC" --work-pvc "$WORK_PVC" \
  --engine vllm --model /hf-cache/Qwen3-0.6B --model-label Qwen3-0.6B \
  --image "$ENGINE_IMAGE" --tp 1 --dry-run
```

### 4. Run the benchmark

Repeat step 3 without `--dry-run`. Use `--engine sglang` or `--engine trtllm`
with a compatible image for those engines. Run each topology separately.

### 5. Generate a report

Each invocation writes under `benchmarks/results/<date>-<git-sha>-<id>/`.
Top-level JSON files contain complete results. Subdirectories contain manifests,
admitted pods, logs, timing records, inference responses and failure details.
The report reads complete top-level results:

```bash
uv run --project benchmarks python -m snapshot_benchmarks report \
  --results-dir benchmarks/results/<invocation> --out tables.md
```

## Workload options

| Flag | Meaning |
| --- | --- |
| `--tp` | GPU count for tensor parallelism; for example, `--tp 2` uses two GPUs. |
| `--ep` | Expert parallelism within those GPUs; must divide TP. vLLM supports EP=1 or EP=TP here. |
| `--node` | Node for both source and restore pods. |
| `--engine-args` | JSON constructor arguments using the engine's Python API names. |
| `--weights-bytes` | Measured logical weight-file size; omitted sizes stay blank in the report. |
| `--keep` | Retain resources for debugging after the run. |
| `--toleration` | Repeatable node toleration in `key=value:effect` form. |

Use dedicated flags for model and topology. Conflicting keys in `--engine-args`
are rejected. For example, add these arguments for vLLM Qwen3-14B:

```bash
--tp 2 --engine-args '{"max_model_len":2048,"gpu_memory_utilization":0.85}'
```

Multi-GPU captures use the guide's launcher installer and the agent image
from the selected node. Dry-run manifests retain the guide's
`${SNAPSHOT_AGENT_IMAGE}` placeholder; the runner resolves it before creating a
pod. The admitted command is saved and checked for `cuinterpose-launch` before
capture. Per-process `gpu/<pid>/manifest.txt` files are required as evidence of
PageBroker CustomStorage.

Use `--model-label` to select an entry from `models.yaml`, or `sweep` to run
the list. Entries can set `tp`, `ep`, `engine_args` and `env`. Use the same
cluster, image and PVC flags with `sweep`, plus `--models FILE` for a custom
list. `--tp` and `--ep` override the list's values.

## Offline runs

Populate the model PVC before the run. It is mounted read-only, and Hugging Face
offline mode is enabled.

FlashInfer may still download kernel artifacts when its cache is empty. For an
offline run, pre-stage compatible artifacts separately from the fresh JIT cache
and set the engine's download controls in the model's `env` settings. FlashInfer
0.6.16 uses `FLASHINFER_NO_DOWNLOAD=1` and `FLASHINFER_CUBIN_DIR`. Verify these
controls for the image in use; offline model loading does not disable every
library's artifact downloads.

The [FlashInfer follow-up](benchmarks.md#flashinfer-artifact-downloads) reports
how pre-staged artifacts affected five published configurations.

## Measurements and verification

See [Measurement boundaries](benchmarks.md#measurement-boundaries) for the timing
endpoints and stage definitions. Inference checks are outside the measured
cold-start and restore intervals.

Every restore must retain the source process token and answer three factual
prompts. Missing wake events fail the run. Readiness and first generation are
not used as timing endpoints. The first restore supplies the report's total and
phase values; all three attempts are saved.

Each source uses fresh compiler-cache directories. TRT-LLM's FlashInfer workspace
stays in the container rootfs and is included in the checkpoint. Its loaded,
deleted extension files must be restored independently; sharing them through
NFS can break repeated restores. OS and storage cache isolation is limited on
shared infrastructure; the first restore may also benefit from cached data.

Before capture, the application pauses requests and asks the engine to release
KV memory. vLLM retains GPU weights; SGLang requests KV-only release without a
CPU weight backup; TRT-LLM uses its KV sleep/wakeup RPC. A successful release call
does not prove that all allocations were freed. See the SGLang notes in the
[published results](benchmarks.md#benchmarking-setup).

Results from earlier tool versions use older timing definitions and are reported
separately.

## Interpreting results

Compare the recorded GPU, driver, engine image, model precision, parallelism,
constructor arguments, storage and cache conditions with the
[reference setup](benchmarks.md#benchmarking-setup). Different settings give a
different baseline. Use the ratio within a run when comparing against a published
row would mix those settings. Read the [limitations](benchmarks.md#limitations),
including retained SGLang KV memory and instrumentation changes.

## Reporting a reproducibility mismatch

If repeated runs differ substantially under equivalent settings, open an issue
with the command, expected result and observed result. Attach the run's JSON,
expanded manifests, `startup.log`, restore agent logs and timing files, plus the
`metadata` output. Include the Snapshot chart version, operator/agent images and
engine image. Remove credentials, private addresses and identifying volume or
cluster names before sharing.

## Cleanup

Cleanup removes the run's pods, ConfigMap, PodSnapshot and PodSnapshotContent.
Failed runs retain local evidence and still clean owned cluster resources.
Cleanup errors stop a sweep. `--keep` retains resources for debugging; earlier
restore pods are removed to free GPUs between repetitions.

Work-PVC caches and logs remain under the run ID. Copy the evidence before
removing that directory. Model files and PVCs are never deleted.
