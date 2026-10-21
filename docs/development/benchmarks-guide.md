# Reproducing the benchmarks

Run cold-start, checkpoint and restore measurements for vLLM, SGLang or TensorRT-LLM
with the tool in [`benchmarks/`](../../benchmarks). Each run uses one node, captures
a checkpoint and restores it three times. The runner uses the engine guide workloads
and saves JSON results and logs. Use the `report` command to build the tables.
See the [published results](benchmarks.md).

## Prerequisites

- Python 3.12 or newer, `uv` and `kubectl`.
- An installed Snapshot operator, node agent and PageBroker.
- A node with enough free GPUs and host memory for the selected model.
- Permission to clear that node's page cache and create a privileged root init
  container. The init container must be able to write `/proc/sys/vm/drop_caches`.
  Namespace admission policies must allow this container; baseline and restricted
  Pod Security Admission policies reject it unless an exemption applies.
  Clearing the cache affects all workloads on the node. Use `--no-drop-page-cache`
  to skip cache clearing.
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

These mount options require a compatible VAST client. The runner uses existing mounts;
it does not set their options.

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
The source pod records the engine arguments.

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
| `--keep` | Retain resources for debugging after the run. |
| `--detailed-timing` | Enable engine-source patches and import tracing for the cold-start breakdown. Off by default. |
| `--no-drop-page-cache` | Skip node page-cache eviction and weight residency verification. |
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

For a fully offline run, also prepare the kernel artifacts required by your image.
FlashInfer 0.6.16 uses `FLASHINFER_NO_DOWNLOAD=1` and `FLASHINFER_CUBIN_DIR`. Set these
in the model's `env` settings and keep prebuilt artifacts separate from the JIT cache.

## Measurements and verification

Before each cold start, an unprivileged init container resolves and checks the model files.
A separate helper, using a digest-pinned official Python image, checks write access to
`/proc/sys/vm/drop_caches`, runs `sync`, then writes `1` to clear the node's page cache.
The helper runs as privileged root. This clears caches for the whole node and requires
permission to do so. Admission policies can reject the pod before the helper starts.
The init container also calls `POSIX_FADV_DONTNEED` on the selected model's weight
files and checks residency with `mincore`. The source starts only if all checked
weight pages are nonresident. Permission failures, unsupported weight layouts and
failed eviction stop the run. Cache preparation is outside the cold-start timer.

Weight sizes are measured from the cached model files, including when cache clearing is disabled.
The run saves `cache-control.json`, including file sizes and page counts, with its results.
`resident_pages_after_node_drop` is measured after the node-wide drop and before
per-file eviction. `resident_pages_after` is measured after per-file eviction.
This controls client page cache; it does not clear storage-server caches. Restore
pods do not clear caches. Use `--no-drop-page-cache` to skip this preparation;
the run records whether cache preparation was skipped.

See [Measurement boundaries](benchmarks.md#measurement-boundaries) for the timing
endpoints and stage definitions. Inference checks are outside the measured
cold-start and restore intervals.

Every restore must keep the source process token and answer three factual prompts. The
run fails if wake events are missing. Timing ends at wake/resume completion, before the
inference checks. Each run saves the restore totals, phase timings and inference results.

Each source uses fresh compiler-cache directories. The checkpoint includes TRT-LLM's
FlashInfer workspace in the container rootfs. FlashInfer can load extension files and
then delete them while they remain in use. Each restore needs its own copy of these
files. Sharing them through NFS can break repeated restores.

Total startup and restore timings are always collected. Use `--detailed-timing` to
collect the cold-start breakdown. This patches engine functions and traces imports.

Timing logs use a 2 GiB memory volume. The runner copies them to the local results
before capture. Trace transfer uses gzip to handle large multi-GPU logs. Restore pods
start with `sleep infinity` so a second engine does not start before checkpoint injection.

Before capture, the application pauses requests and asks the engine to release KV
memory. Weights remain on the GPU. SGLang requests KV-only release without a CPU
weight backup. TRT-LLM uses its KV sleep/wakeup RPC. A successful release call does not
prove that the engine freed all memory. See the SGLang notes in the [published
results](benchmarks.md#benchmarking-setup).

## Interpreting results

Compare your settings with the [reference setup](benchmarks.md#benchmarking-setup).
Check the GPU, driver, engine image, model precision, parallelism, constructor
arguments, storage and cache conditions. Read the
[limitations](benchmarks.md#limitations), including retained SGLang KV memory.

## Reporting a reproducibility mismatch

To report unexpected results, include the command and expected and actual results.
Attach the run's JSON, manifests, `startup.log`, restore agent logs, timing files and
`metadata` output.
Include the Snapshot chart version, operator/agent images and engine image. Remove
credentials, private addresses and identifying volume or cluster names before sharing.

## Cleanup

The runner attempts to delete its pods, ConfigMap, PodSnapshot and PodSnapshotContent
after the run. Failed runs keep local evidence. If deletion fails, resources can remain;
the runner saves `cleanup-errors.json`, raises `CleanupError` and stops the sweep. Use `--keep` to keep resources for debugging. The runner
still removes earlier restore pods to free GPUs for the next attempt.

Compiler caches stay on the work PVC under the run ID. Logs and results are saved
locally. The runner does not delete model files or PVCs.
