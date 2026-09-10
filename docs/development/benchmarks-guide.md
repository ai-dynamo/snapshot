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
- A Hugging Face token/cache if the model you're benchmarking is gated

## Quick start

Sanity-check the cluster before committing to a multi-minute sweep — this
prints the same environment bundle every run records, without deploying
anything:

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

Each run writes one JSON file per model to `benchmarks/results/<date>-<git-sha>/`
(gitignored — these are local artifacts, not published content). Render a
standalone text summary from a results directory:

```bash
uv run --project benchmarks python -m snapshot_benchmarks report \
  --results-dir benchmarks/results/<date>-<git-sha>
```

`report` never edits [benchmarks.md](benchmarks.md) or any other doc — it
prints/writes a separate summary for you to read or paste from by hand.

## What gets measured

Every run separates two things the published doc reports together as "wake /
remap": **Snapshot's own restore work** and **the engine's own wake and
copy-to-GPU work**. The boundary is the node agent's `nvidia.com/Restored` pod
condition, which fires the moment Snapshot believes the restore is done —
before vLLM's own `wake_up()`/`resume_generation()`/warmup sequence runs. So,
in each run's JSON:

- `restore.snapshot_restore_seconds` — container start → `nvidia.com/Restored`
- `restore.vllm_wake_and_copy_seconds` — `nvidia.com/Restored` → pod `Ready`

These come from Kubernetes condition timestamps, which have one-second
resolution — a large relative error for the smallest models. For the headline
total, `agent_log_phases.duration` is parsed from the node agent's own restore
log, which has sub-second precision and is what actually produced the
published numbers. `agent_log_phases` also includes a best-effort remap of the
agent's own phase names to the published doc's 4-stage vocabulary
(`agent_setup_approx`, `criu_restore_approx`, `cuda_restore_approx`) — these
are approximate by construction; `cuda_restore_approx` is the one exact 1:1
mapping. `wake_remap_approx` is always `null`: that published stage isn't
represented in the agent log at all, use `vllm_wake_and_copy_seconds` instead.

Cold start is timed from pod creation to the source pod's `Ready` condition
(model load + one warmup generation + pause — the readiness gate
[deployment.yaml](../guides/vllm/deployment.yaml) already uses), with
container start (`cold_start.container_start_seconds`) reported separately so
it can be excluded to match the published doc's "Cold start" column.

Checkpoint size (`model.checkpoint_artifact_bytes`) is measured with `du -sb`
on the capture node — neither `PodSnapshot` nor `PodSnapshotContent` status
exposes a size field.

## Interpreting results

The published numbers were measured on a single B200 (driver 595) with a VAST
NFS PVC — see [Storage throughput sets the floor](benchmarks.md#storage-throughput-sets-the-floor)
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
