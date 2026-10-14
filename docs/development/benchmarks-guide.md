# Reproducing the benchmarks

This guide runs the tool in [`benchmarks/`](../../benchmarks) that produced the
methodology behind [Performance Benchmarks](benchmarks.md): it deploys a
snapshot-ready vLLM replica, times cold start, checkpoints it, restores it, and
records the result as raw JSON plus per-run environment metadata (GPU, driver,
storage backend, placement, checkpoint size).

Only vLLM is implemented today ("the agreed day-one vLLM comparison" scope).
SGLang and TensorRT-LLM can be added later without redesigning the tool — see
`benchmarks/snapshot_benchmarks/engines/__init__.py`.

## Prerequisites

- [`uv`](https://docs.astral.sh/uv/)
- `kubectl`
- A Snapshot Helm release already [installed](../operations/install.md) and
  reachable via `KUBECONFIG`
- A GPU-schedulable node with `RuntimeClass/nvidia` (see the repo
  [Prerequisites](../../README.md#prerequisites))
- A pre-built snapshot-ready vLLM image — follow
  [Build and deploy a vLLM replica](../guides/vllm.md) steps 1–2 first; the
  benchmark deploys that image, it does not build it

## Quick start

Sanity-check the cluster before committing to a multi-minute sweep — this
prints the same environment bundle every run records (storage backend, cluster
version, etc.), without deploying anything. It omits `gpu_product` and
`gpu_driver_version` (both come back `null`), since those are queried live from
inside a running GPU pod via `nvidia-smi`, and this command deploys none:

```bash
export SNAPSHOT_E2E_TEST_NAMESPACE=<namespace-vllm-runs-in>
export SNAPSHOT_E2E_SNAPSHOT_RELEASE=<helm-release-name>
uv run --project benchmarks python -m snapshot_benchmarks metadata \
  --snapshot-namespace <namespace-snapshot-is-installed-in>
```

Run a single model:

```bash
uv run --project benchmarks python -m snapshot_benchmarks run \
  --snapshot-namespace <namespace-snapshot-is-installed-in> \
  --model-label "Qwen3 0.6B" \
  --image <registry>/vllm-snapshot:<tag>
```

Run the full published model sweep (`benchmarks/models.yaml`, editable — see
the comments in that file about tuning `VLLM_MAX_MODEL_LEN` /
`VLLM_GPU_MEMORY_UTILIZATION` per model for your GPU's VRAM):

```bash
uv run --project benchmarks python -m snapshot_benchmarks sweep \
  --snapshot-namespace <namespace-snapshot-is-installed-in> \
  --image <registry>/vllm-snapshot:<tag>
```

Each run writes one JSON file per model to `benchmarks/results/<date>-<git-sha>-<invocation-id>/`
(gitignored — these are local artifacts, not published content). Render a
standalone text summary from a results directory:

```bash
uv run --project benchmarks python -m snapshot_benchmarks report \
  --results-dir benchmarks/results/<date>-<git-sha>-<invocation-id>
```

`report` never edits [benchmarks.md](benchmarks.md) or any other doc — it
prints/writes a separate summary for you to read or paste from by hand.

## What gets measured

Every run separates two things the published doc reports together as "wake /
remap": **Snapshot's own restore work** and **the engine's own wake and
copy-to-GPU work**. The boundary is the node agent's `nvidia.com/Restored` pod
condition, which fires the moment Snapshot believes the restore is done —
before vLLM's own `wake_up()`/`resume_generation()`/warmup sequence runs. So,
in each run's result JSON (the per-model file written to `benchmarks/results/`,
see [Quick start](#quick-start)):

- `restore.snapshot_restore_seconds` — container start → `nvidia.com/Restored`
- `restore.wake_and_copy_seconds` — `nvidia.com/Restored` → pod `Ready`

These come from Kubernetes condition timestamps, which have one-second
resolution — a large relative error for the smallest models. For the headline
total, `agent_log_phases.duration` is parsed from the node agent's own restore
log, which has sub-second precision and is what actually produced the
published numbers. `agent_log_phases` also includes a best-effort remap of the
agent's own phase names to the published doc's 4-stage vocabulary
(`agent_setup_approx`, `criu_restore_approx`, `cuda_restore_approx`) — these
are approximate by construction; `cuda_restore_approx` is the one exact 1:1
mapping. `wake_remap_approx` is always `null`: that published stage isn't
represented in the agent log at all, so if you're reproducing the published
"wake / remap" number, read `wake_and_copy_seconds` instead.

Cold start is timed from pod creation to the source pod's `Ready` condition
(model load + one warmup generation + pause — the readiness gate
[capture/qwen3-0.6b.yaml](../guides/vllm/capture/qwen3-0.6b.yaml) already uses), with
container start (`cold_start.container_start_seconds`) reported separately so
it can be excluded to match the published doc's "Cold start" column.

Checkpoint size (`model.checkpoint_artifact_bytes`) is measured with `du -sb`
on the capture node — neither `PodSnapshot` nor `PodSnapshotContent` status
exposes a size field.

### Capture breakdown

The published tables in [benchmarks.md](benchmarks.md) cover restore only.
Each run also records capture. `checkpoint.checkpoint_seconds` is the
Kubernetes `PodSnapshot` Ready time, at one-second resolution. The precise
split is `checkpoint_agent_log`, parsed from the capture-node agent's
`Checkpoint timing summary`:

- `duration` — agent wall time for the whole capture
- `phases.cuda_checkpoint` — CUDA lock + device-to-host copy + job file
- `cuda_lock`, `cuda_copy`, `cuda_job_file` — that CUDA total, split
- `phases.criu_dump` — CRIU dump RPC wall time, including the page write
- `criu_freezing`, `criu_memdump`, `criu_memwrite` — CRIU's own timers.
  Memdump is reading the frozen process. Memwrite is writing pages. They
  overlap the CRIU wall and are not added to it; CRIU can count the last
  write interval twice, so use them to see which side dominates
- `criu_pages_written_bytes` and `criu_image_bytes` — page payload versus
  the whole CRIU image, with `memwrite_gigabytes_per_second` and
  `criu_image_gigabytes_per_second`
- `image_growth` — image-directory size sampled every 200ms during the dump.
  `image_growth_max_stall_seconds` is the longest stretch where the size did
  not grow. `image_growth_midpoint_fraction` near 0.5 is a steady write;
  near 0 means the bytes arrived late
- `phases.overlay_capture` and `overlay_diff_bytes` — writable-layer tar

`report` prints this as **Where the capture time goes** and **How the CRIU
image was written**. An agent built before this split still produces phase
totals; the result then warns that the cuda/criu breakdown is absent.

Before the run deletes the artifact, it also lists every file under that
content directory and reads `dump.log`. `checkpoint_image.categories` sums
those files by kind (`pages`, `core`, `mm`, and so on). Each total has an
apparent size (`st_size`, holes included) and an allocated size
(`st_blocks * 512`). `pages_sparse_bytes` is the difference for page images.
`pagemap_holes` is CRIU's own hole count from every `Pagemap generated` line.
A large non-`pages` category is the rest of the image. The finish-path
offsets (`first_dumping_core_seconds`, the thread-core span,
`unfreezing_seconds`, `writing_stats_seconds`, `dumping_finished_seconds`)
are CRIU's own clock, the same clock as `image_growth`. Pagemap and
per-thread lines are summarized so `Dumping finished` is not dropped.

A single-model baseline on the same hardware you care about:

```bash
uv run --project benchmarks python -m snapshot_benchmarks run \
  --snapshot-namespace <namespace-snapshot-is-installed-in> \
  --model-label "Qwen3 0.6B" \
  --image <registry>/vllm-snapshot:<tag>
uv run --project benchmarks python -m snapshot_benchmarks report \
  --results-dir benchmarks/results/<date>-<git-sha>-<invocation-id>
```

## Interpreting results

The published numbers were measured on a single B200 (driver 595) with a VAST
NFS PVC with `direct_io` enabled — see [Storage throughput sets the floor](benchmarks.md#storage-throughput-sets-the-floor)
and [Checkpoint size is what predicts restore time](benchmarks.md#checkpoint-size-is-what-predicts-restore-time).
A run on different hardware produces a **valid, different-baseline** result,
not an invalid comparison — this is exactly why every run self-reports its own
`environment` block (GPU product, driver version, storage class/provisioner,
placement). Compare against the published table only when `gpu_product`,
`gpu_driver_version`, and `storage_class`/`storage_provisioner` genuinely
match; otherwise compare your own cold-start-vs-restore ratio, or checkpoint
size vs. restore time, within your own results.

## Reporting a reproducibility mismatch

Treat it as a mismatch worth reporting when the restore total
(`agent_log_phases.duration`) is more than roughly 2x the published figure for
the same model **on equivalent hardware** (same GPU model, same driver major,
same storage backend/class) — not when hardware differs, per the section
above.

Open a GitHub issue on the repository and attach:

- The raw `RunResult` JSON for the run in question
- The output of `python -m snapshot_benchmarks metadata` for the same context
- The Snapshot chart version and operator/agent image tags installed
- `kubectl get nodes -o wide`
- The node agent's pod logs for the run window
  (`kubectl logs -n <snapshot-namespace> <snapshot-agent-pod>`)
