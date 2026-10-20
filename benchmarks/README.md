# Snapshot Benchmarks

Measure cold start, checkpoint time and three restores for vLLM, SGLang or
TensorRT-LLM. Each run saves timings, checkpoint size, environment metadata and
logs under `results/`.

See the [benchmark guide](../docs/development/benchmarks-guide.md) for setup and
commands, and [Performance Benchmarks](../docs/development/benchmarks.md) for
published results.

## Requirements

- `uv`
- `kubectl`
- A Snapshot Helm release already installed and reachable via `KUBECONFIG`,
  including its checkpoint PVC (provisioned by the chart by default — see
  [Installation](../docs/operations/install.md))
- An engine image compatible with the selected [framework guide](../docs/guides/)
- Pre-populated model and writable work PVCs, plus a node with enough free GPUs

## Layout

The runner uses `snapshot_e2e.k8s` and `snapshot_e2e.lifecycle` from
[`e2e/`](../e2e), plus the engine guide applications and deployment templates.

- `snapshot_benchmarks/schema.py` — the `RunResult` data model written to disk
- `snapshot_benchmarks/run.py` — runs one model and engine configuration
- `snapshot_benchmarks/engines/` — vLLM, SGLang and TRT-LLM adapters that load
  the corresponding guide deployment templates
- `snapshot_benchmarks/workload.py` — benchmark mounts and fresh caches added
  to those guide pods
- `instrumentation/` — timers projected alongside the guide application;
  used with `docs/guides/<engine>/app.py`
- `snapshot_benchmarks/logs.py` — parses the node agent's "Restore timing
  summary" log line for sub-second-precision phase timing
- `snapshot_benchmarks/metadata.py` — collects GPU/driver/storage/placement
  metadata for every run
- `snapshot_benchmarks/cli.py` — `run` / `sweep` / `metadata` / `report`
  subcommands
- `models.yaml` — an example model list for sweeps
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

Published results are Markdown tables. New runs save JSON and logs under `results/`.

## Local checks

```bash
uv run --project benchmarks python -m unittest discover -s benchmarks/tests
```

These checks exercise guide-based manifests, simulated lifecycle calls, timing
accounting and reports. They do not start GPU workloads.
