# Snapshot Benchmarks

This directory contains the Python tool that reproduces the numbers published in
[Performance Benchmarks](../docs/development/benchmarks.md): deploy a
replica on vLLM, SGLang or TensorRT-LLM, measure initialization, checkpoint it,
and restore it three times, and record everything (including per-run environment metadata —
GPU, driver, storage backend, placement, checkpoint size) as raw JSON.

See the [benchmark guide](../docs/development/benchmarks-guide.md) for the full
walkthrough. This README only covers the local dev/CI shape of the directory.

## Requirements

- `uv`
- `kubectl`
- A Snapshot Helm release already installed and reachable via `KUBECONFIG`,
  including its checkpoint PVC (provisioned by the chart by default — see
  [Installation](../docs/operations/install.md))
- An engine image compatible with the selected [framework guide](../docs/guides/)
- Pre-populated model and writable work PVCs, plus a node with enough free GPUs

## Why this isn't part of `e2e/`

[`e2e/`](../e2e) is a CI-oriented suite that deploys synthetic CPU/GPU
state-holder workloads and asserts pass/fail. Benchmarking needs a real,
multi-GB inference engine image, takes minutes per model, and is a
human-driven, ad-hoc tool for measuring and reporting timing — not something
that belongs in a CI test suite. This package reuses `e2e/`'s Kubernetes
plumbing (`snapshot_e2e.k8s`, `snapshot_e2e.lifecycle`) as a `uv` path
dependency rather than duplicating it.

## Layout

- `snapshot_benchmarks/schema.py` — the `RunResult` data model written to disk
- `snapshot_benchmarks/run.py` — orchestrates one model+engine run
- `snapshot_benchmarks/engines/` — vLLM, SGLang and TRT-LLM adapters that load
  the corresponding guide deployment templates
- `snapshot_benchmarks/workload.py` — benchmark mounts and fresh caches added
  to those guide pods
- `instrumentation/` — timers projected alongside the guide application;
  the benchmark uses the same `docs/guides/<engine>/app.py`, without app copies
- `snapshot_benchmarks/logs.py` — parses the node agent's "Restore timing
  summary" log line for sub-second-precision phase timing
- `snapshot_benchmarks/metadata.py` — collects GPU/driver/storage/placement
  metadata for every run
- `snapshot_benchmarks/cli.py` — `run` / `sweep` / `metadata` / `report`
  subcommands
- `models.yaml` — an editable example sweep, not the published results matrix
- `results/` — gitignored, per-invocation raw JSON output

## Quick start

```bash
uv run --project benchmarks python -m snapshot_benchmarks metadata --context YOUR_CONTEXT
uv run --project benchmarks python -m snapshot_benchmarks run \
  --context YOUR_CONTEXT --namespace YOUR_NAMESPACE \
  --snapshot-namespace SNAPSHOT_NAMESPACE --node YOUR_NODE \
  --model-pvc YOUR_MODEL_PVC --work-pvc YOUR_WORK_PVC \
  --engine vllm --model /hf-cache/Qwen3-0.6B \
  --image YOUR_ENGINE_IMAGE --tp 1 --dry-run
```

Remove `--dry-run` to run it. Use `--tp 2` for two GPUs, or `--tp 8 --ep 8`
for eight-way tensor and expert parallelism. See the guide for engine arguments,
storage requirements and timing boundaries.

Published results are maintained directly as Markdown tables. The chart images
were imported from the benchmark article; no plotting tool or frozen result JSON
is required. New runs save their own JSON and logs under `results/`.

## Local checks

```bash
uv run --project benchmarks python -m unittest discover -s benchmarks/tests
```

These checks exercise guide-based manifests, simulated lifecycle calls, timing
accounting and reports. They do not start GPU workloads.
