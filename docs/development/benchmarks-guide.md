# Reproducing the benchmarks

The tool in [`benchmarks/`](../../benchmarks) measures startup, checkpoints the
initialized engine and restores it three times. It supports vLLM, SGLang and
TensorRT-LLM on one node, using the engine guide applications and templates.

Each run writes a report. The [published results](benchmarks.md) are maintained
separately.

## Prerequisites

- Python 3.12 or newer, `uv` and `kubectl`.
- An installed Snapshot operator, node agent and PageBroker.
- A node with enough free GPUs and host memory for the selected model.
- An engine image that supports the selected guide application's APIs.
- A read-only model PVC, populated before the run, mounted at `/hf-cache`.
- A writable work PVC for fresh compiler caches and timing files.
- The Snapshot installation's filesystem checkpoint PVC, passed with `--pvc-name`.
  The artifact inventory reads this mounted filesystem; bound or remote stores
  are outside this runner's scope.

The guide templates require the NVIDIA runtime class, `/dev/net/tun` and the
node's `profiles/block-iouring.json` seccomp profile. The runner does not install
infrastructure, change node reservations or stop other users' workloads.

The published setup used B200 GPUs, driver 615.71.09 and Snapshot v0.2.0-rc.2.
Engine versions were vLLM 0.31.0, SGLang 0.5.21 and TensorRT-LLM 1.3.0rc24.
Pass the image you intend to measure with `--image`; the guide's default image
may differ. Each result records the image and queries its installed version.
The agent log format must include checkpoint and restore timing summaries and
the external-restore start marker. Missing timing evidence fails collection.

For the published VAST setup, use NFSv3 with 1 MiB read/write sizes,
`nconnect=32`, `spread_reads`, `nosharecache`, `nosharetransport` and
`remoteports=<your four storage addresses>`. Those options require a compatible
VAST client. Inspect the actual `/proc/mounts` records saved in `startup.log`;
a PV declaration alone does not prove the client uses them. The runner can
measure other storage too; it does not force VAST mount options.

## Quick start

Inspect the installed Snapshot release without creating a workload:

```bash
uv run --project benchmarks python -m snapshot_benchmarks metadata \
  --context YOUR_CONTEXT --namespace YOUR_NAMESPACE \
  --snapshot-namespace SNAPSHOT_NAMESPACE --pvc-name CHECKPOINT_PVC
```

Generate a workload for review without making Kubernetes calls:

```bash
uv run --project benchmarks python -m snapshot_benchmarks run \
  --context YOUR_CONTEXT --namespace YOUR_NAMESPACE \
  --snapshot-namespace SNAPSHOT_NAMESPACE --release SNAPSHOT_RELEASE \
  --pvc-name CHECKPOINT_PVC --node YOUR_NODE \
  --model-pvc MODEL_PVC --work-pvc WORK_PVC \
  --engine vllm --model /hf-cache/Qwen3-0.6B --model-label Qwen3-0.6B \
  --image YOUR_ENGINE_IMAGE --tp 1 --dry-run
```

Remove `--dry-run` to run it. Use `--engine sglang` or `--engine trtllm` for the
other engines. `--tp` is the GPU count; `--ep` sets expert parallelism within
those GPUs and must divide TP. vLLM supports EP=1 or EP=TP in this workload.
The source and restore pods are pinned to `--node`. Multi-GPU captures reuse
the guide's launcher installer, using the agent image deployed on that node.
Dry-run manifests retain the guide's `${SNAPSHOT_AGENT_IMAGE}` placeholder;
the runner resolves it before creating a pod. Use a separate run to measure
a different topology.

Model-specific constructor arguments are JSON, using the engine's Python API
names. For example, add this for vLLM Qwen3-14B:

```bash
--tp 2 --engine-args '{"max_model_len":2048,"gpu_memory_utilization":0.85}'
```

Set the model and topology through their dedicated flags. The tool rejects
conflicting model or parallelism keys in `--engine-args`. For a local model,
`--weights-bytes` records the measured logical weight-file size. If omitted,
the report leaves that cell blank. Model files are not downloaded during a run;
the model PVC is read-only and Hugging Face offline mode is enabled.

FlashInfer may still download kernel artifacts when its cache is empty. For an
offline run, pre-stage compatible artifacts separately from the fresh JIT cache
and set the engine's download controls in the model's `env` settings. FlashInfer
0.6.16 uses `FLASHINFER_NO_DOWNLOAD=1` and `FLASHINFER_CUBIN_DIR`. Verify those
controls for the image you use; offline model loading does not disable every
library's artifact downloads.

Use `--model-label` to select an entry from `models.yaml`, or `sweep` to run
the list. Entries can set `tp`, `ep`, `engine_args` and `env`.
Use the same cluster, image and PVC flags with `sweep`, plus `--models FILE` for
your own list. `--tp` and `--ep` override the list's values. Add node
tolerations with the repeatable `--toleration key=value:effect` flag.

Each invocation writes under `benchmarks/results/<date>-<git-sha>-<id>/`.
The top-level JSON files contain complete results. Subdirectories retain
expanded manifests, admitted pods, logs, timing records, inference responses
and failure details. The report reads only complete top-level results:

```bash
uv run --project benchmarks python -m snapshot_benchmarks report \
  --results-dir benchmarks/results/YOUR_INVOCATION --out tables.md
```

## What gets measured

**Cold start** begins at the workload's process-start marker and ends when the
engine constructor returns. Image pull, scheduling, container setup and model
download are outside that interval. Inference validation and memory release
follow initialization and are excluded. Timers cover model loading, kernel
compilation, tuning, graph capture and initialization inside the engine.

The report has four cold-start columns: Startup, Model load, Initialization,
and Compilation, autotuning & graph capture. Overlapping work across workers
counts once. Some JIT occurs during Startup and stays there. Small unmeasured
intervals are distributed proportionally for display; those additions are
estimates. Raw intervals and gaps remain in the local evidence.

**Checkpoint** uses the agent's measured capture duration. The application
pauses requests and releases KV memory before capture. vLLM retains weights
on the GPU; SGLang requests KV-only release without a CPU weight backup;
TRT-LLM uses its KV sleep/wakeup RPC. A release call does not prove that every
engine allocation was freed. In particular, the published SGLang GPT-OSS runs
retained KV allocations, which increased their checkpoint sizes.

Checkpoint size sums the logical bytes of regular files under
`containers/main`. It includes CPU/GPU images and rootfs changes. The runner
requires per-process `gpu/<pid>/manifest.txt` files as evidence of PageBroker
CustomStorage. The source pod must use `cuinterpose-launch` for multi-GPU runs;
the runner saves the admitted command and checks it before capture.

**Restore** begins at the agent's external-restore start marker and ends at the
application's wake/resume completion. Its columns are Agent setup / rootfs,
CRIU restore, CUDA restore and Wake / handoff. The CRIU/CUDA values come from
the agent summary; setup is the rest of the agent duration, and handoff is the
interval from agent completion to application wake/resume completion. Multi-GPU
reconstruction is included in the CUDA/CRIU phases and has no separate timer.

Every restore must retain the pre-checkpoint process token and answer three
factual prompts. Inference occurs after wake and is excluded from the measured
restore time. The report uses attempt 1 for both total and phase values;
attempts 2 and 3 remain in the local results. Missing wake events fail the run;
readiness and first generation are not substituted as timing endpoints.

Each source uses fresh compiler-cache directories. TRT-LLM's FlashInfer workspace
stays in the container rootfs and is included in the checkpoint. Its loaded,
deleted extension files must be restored independently; sharing them through
NFS can break repeated restores. The runner does not clear
Linux page cache or storage-server caches. Metadata includes capture/restore
GPU and driver identities, engine version, storage class and placement.
Schema-1 results remain readable, under their original timing definitions;
the report separates them from the new startup-to-ready measurements.

## Known measurement limits

The published TRT-LLM cold starts allowed FlashInfer kernel artifact downloads.
A follow-up used the same pinned engine image with 11 pre-staged, checksum-verified
artifacts, `FLASHINFER_NO_DOWNLOAD=1` and a separate read-only
`FLASHINFER_CUBIN_DIR`. Generated JIT/autotune caches started empty; OS and storage
caches were not cleared.

| Model | Parallelism | Published cold start (s) | Pre-staged artifacts (s) | Difference (s) |
| --- | --- | --- | --- | --- |
| Qwen3-0.6B | TP1 | 121.26 | 117.94 | -3.32 |
| Qwen3-14B | TP1 | 127.02 | 124.14 | -2.89 |
| Qwen3-14B | TP2 | 158.66 | 130.22 | -28.44 |
| gpt-oss-120b | TP1 | 200.55 | 198.10 | -2.45 |
| gpt-oss-120b | TP4 | 223.96 | 191.33 | -32.63 |

The multi-GPU differences were mainly in attention warmup. Logged fetch windows
covered only 2.81 seconds on Qwen3-14B TP2, so the full difference cannot be
attributed to downloads. Original-download controls took 158.39 seconds for that
configuration and 221.88 seconds for GPT-OSS TP4. The first offline GPT-OSS TP4
attempt failed output validation and is excluded; the table uses its successful
repeat. All five selected configurations passed output checks, capture and three
restores. The published tables retain the original measurements.

Published cold starts include instrumentation overhead. The runner now limits
constructor timers to startup modules and disables unused import profiling.
The published measurements have not been rerun with these fixes.

## Cleanup and verification

Cleanup removes this run's pods, ConfigMap, PodSnapshot and
PodSnapshotContent. Failed runs retain local evidence and still clean owned
cluster resources. Cleanup errors stop a sweep. `--keep` retains resources for
debugging; earlier restore pods are removed to free GPUs between repetitions.
Work-PVC caches and logs remain under the run ID. Remove that directory only
after copying the evidence. Model files and PVCs are never deleted.

Local tests cover guide contracts, explicit topology, cache isolation,
failed-run cleanup, precise timing and reports.

Live validation at `13e4935f` used Qwen3-0.6B at TP2 on B200 GPUs with
Snapshot v0.2.0-rc.2, vLLM 0.31.0, SGLang 0.5.21 and TensorRT-LLM 1.3.0rc24.
Each engine passed
one capture and three restores from that checkpoint. Each restore preserved
the source process token and passed three inference checks. Checkpoint payloads
were deleted afterward. Model weights were cached; FlashInfer artifact downloads
were permitted. The published configurations were measured separately. GPU runs
were not repeated after the timing and isolation fixes in `b40e8c73`.
