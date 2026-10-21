# Reproducing the benchmarks

The tool in [`benchmarks/`](../../benchmarks) measures engine startup. It then captures
a checkpoint and restores it three times. It supports vLLM, SGLang and TensorRT-LLM on
one node. It uses the engine guide applications and templates. Each run writes a report.
The [published tables](benchmarks.md) are separate.

## Prerequisites

- Python 3.12 or newer, `uv` and `kubectl`.
- An installed Snapshot operator, node agent and PageBroker.
- A node with enough free GPUs and host memory for the selected model.
- An engine image that supports the selected guide application's APIs.
- A populated model PVC, mounted read-only at `/hf-cache`.
- A writable work PVC for compiler caches.
- A filesystem checkpoint PVC, passed with `--pvc-name`. Artifact inventory does
  not support bound or remote stores.
- The NVIDIA runtime class, `/dev/net/tun` and the node's
  `profiles/block-iouring.json` seccomp profile, as required by the guide templates.

The agent must log checkpoint and restore timing summaries and the external restore
start marker. The run fails if these records are missing.

> [!NOTE]
> The runner does not install infrastructure, change node reservations or stop
> other users' workloads.

## Reference setup

The [published setup](benchmarks.md#benchmarking-setup) used B200 GPUs and driver
615.71.09. It used Snapshot v0.2.0-rc.2, vLLM 0.31.0, SGLang 0.5.21 and TensorRT-LLM
1.3.0rc24. Set `--image` to the image you want to measure. The guide's default image can
differ. The runner records the image and reads its installed engine version.

The VAST client used these NFS mount options:

| Option | Value | Purpose |
| --- | --- | --- |
| `vers`, `proto` | `3`, `tcp` | NFSv3 over TCP |
| `rsize`, `wsize` | `1048576` | Read/write requests up to 1 MiB |
| `nconnect` | `32` | Up to 32 transport connections |
| `spread_reads` | enabled | Spread reads across connections |
| `remoteports` | four storage addresses | Use multiple server addresses |
| `nosharecache`, `nosharetransport` | enabled | Separate cache and transports for this mount |

These options require a compatible VAST client. You can measure other storage. The
runner does not set mount options.

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

SGLang benchmarks enable `weight_loader_prefetch_checkpoints` by default, as in the
published results. SGLang 0.5.21 uses four prefetch threads by default. To measure loading
without prefetch, pass `--engine-args '{"weight_loader_prefetch_checkpoints":false}'`.
The admitted source pod records the resolved constructor arguments.

### 5. Generate a report

Each run writes to `benchmarks/results/<date>-<git-sha>-<id>/`. JSON files at the top
level contain complete results. Subdirectories contain manifests, admitted pods, logs,
timing records, inference responses and failure details. The report reads complete
results from the top level:

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

Use the model and parallelism flags to set those values. The runner rejects conflicting
keys in `--engine-args`. For example, add these arguments for vLLM Qwen3-14B:

```bash
--tp 2 --engine-args '{"max_model_len":2048,"gpu_memory_utilization":0.85}'
```

Multi-GPU captures use the guide's launcher installer and the agent image from the
selected node. Dry-run manifests keep the `${SNAPSHOT_AGENT_IMAGE}` placeholder. The
runner replaces it before it creates a pod. Before capture, the runner saves the
admitted command and checks for `cuinterpose-launch`. It also requires per-process
`gpu/<pid>/manifest.txt` files to confirm use of PageBroker CustomStorage.

Use `--model-label` to select an entry from `models.yaml`, or `sweep` to run
the list. Entries can set `tp`, `ep`, `engine_args` and `env`. Use the same
cluster, image and PVC flags with `sweep`, plus `--models FILE` for a custom
list. `--tp` and `--ep` override the list's values.

## Offline runs

Copy the model files to the model PVC before the run. The runner mounts it read-only and
enables Hugging Face offline mode.

FlashInfer can still download kernel artifacts when its cache is empty. For an offline
run, prepare compatible artifacts in advance. Keep them separate from the fresh JIT
cache. Set the engine's download controls in the model's `env` settings. FlashInfer
0.6.16 uses `FLASHINFER_NO_DOWNLOAD=1` and `FLASHINFER_CUBIN_DIR`. Check these controls
for your image. Offline model loading does not stop every library from downloading
artifacts.

The [FlashInfer follow-up](benchmarks.md#flashinfer-artifact-downloads) reports
how pre-staged artifacts affected five published configurations.

## Measurements and verification

See [Measurement boundaries](benchmarks.md#measurement-boundaries) for the timing
endpoints and stage definitions. Inference checks are outside the measured
cold-start and restore intervals.

Every restore must keep the source process token and answer three factual prompts. The
run fails if wake events are missing. Timing ends at wake/resume completion, before the
inference checks. The report uses the first restore for the total and phase times. It
saves all three attempts.

Each source uses fresh compiler-cache directories. The checkpoint includes TRT-LLM's
FlashInfer workspace in the container rootfs. FlashInfer can load extension files and
then delete them while they remain in use. Each restore needs its own copy of these
files. Sharing them through NFS can break repeated restores.

Timing logs use a 2 GiB memory volume. The runner copies them to the local results
before capture. Trace transfer uses gzip to handle large multi-GPU logs. Restore pods
start with `sleep infinity` so a second engine does not start before checkpoint injection.

Before capture, the application pauses requests and asks the engine to release KV
memory. vLLM keeps weights on the GPU. SGLang requests KV-only release without a CPU
weight backup. TRT-LLM uses its KV sleep/wakeup RPC. A successful release call does not
prove that the engine freed all memory. See the SGLang notes in the [published
results](benchmarks.md#benchmarking-setup).

Earlier tool versions use different timing definitions. The report shows those results
separately.

## Interpreting results

Compare your settings with the [reference setup](benchmarks.md#benchmarking-setup).
Check the GPU, driver, engine image, model precision, parallelism, constructor
arguments, storage and cache conditions. If these differ, compare cold start with
restore within your own run. Read the [limitations](benchmarks.md#limitations),
including retained SGLang KV memory and changes to the timing code.

## Reporting a reproducibility mismatch

If repeated runs give very different results with the same settings, open an issue.
Include the command, expected result and actual result. Attach the run's JSON, expanded
manifests, `startup.log`, restore agent logs, timing files and `metadata` output.
Include the Snapshot chart version, operator/agent images and engine image. Remove
credentials, private addresses and identifying volume or cluster names before sharing.

## Cleanup

The runner deletes its pods, ConfigMap, PodSnapshot and PodSnapshotContent after the
run. If the run fails, it keeps local evidence and still deletes its cluster resources.
A cleanup error stops a sweep. Use `--keep` to keep resources for debugging. The runner
still removes earlier restore pods to free GPUs for the next attempt.

Caches and logs stay on the work PVC under the run ID. Copy the evidence before you
remove that directory. The runner does not delete model files or PVCs.
