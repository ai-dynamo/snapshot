# Snapshot Benchmarks

This directory contains the Python tool that reproduces the numbers published in
[Performance Benchmarks](../docs/development/benchmarks.md): deploy a
snapshot-ready vLLM replica, time cold start, checkpoint it, restore it, time
the restore, and record everything (including per-run environment metadata —
GPU, driver, storage backend, placement, checkpoint size) as raw JSON.

See the [benchmark guide](../docs/development/benchmarks-guide.md) for the full
walkthrough. This README only covers the local dev/CI shape of the directory.

## Requirements

- `uv`
- `kubectl`
- A Snapshot Helm release already installed and reachable via `KUBECONFIG`
- A pre-built snapshot-ready vLLM image (see
  [Build and deploy a vLLM replica](../docs/guides/vllm.md))

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
- `snapshot_benchmarks/engines/` — per-engine pod-spec builders (`vllm` only
  today), each loading and patching the corresponding `docs/guides/<engine>/`
  deployment YAML directly, so the benchmark can't drift from the documented
  manual flow
- `snapshot_benchmarks/logs.py` — parses the node agent's "Restore timing
  summary" log line for sub-second-precision phase timing
- `snapshot_benchmarks/metadata.py` — collects GPU/driver/storage/placement
  metadata for every run
- `snapshot_benchmarks/cli.py` — `run` / `sweep` / `metadata` / `report`
  subcommands
- `models.yaml` — the default model sweep (the 7 models published in
  `benchmarks.md`)
- `results/` — gitignored, per-invocation raw JSON output

## Quick start

```bash
uv run --project benchmarks python -m snapshot_benchmarks metadata
uv run --project benchmarks python -m snapshot_benchmarks run \
  --model-label "Qwen3 0.6B" --image <registry>/vllm-snapshot:<tag>
```
