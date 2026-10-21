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

Benchmarks use real engine images and take minutes per model, so they run
manually rather than in the e2e CI suite. The runner reuses `snapshot_e2e.k8s`
and `snapshot_e2e.lifecycle` as a local `uv` dependency, plus the engine guide
applications and deployment templates.

- `snapshot_benchmarks/schema.py` — the `RunResult` data model written to disk
- `snapshot_benchmarks/run.py` — runs one model and engine configuration
- `snapshot_benchmarks/engines/` — vLLM, SGLang and TRT-LLM adapters that load
  the corresponding guide deployment templates
- `snapshot_benchmarks/measurement.py` — collects clocks, artifact sizes and inference checks
- `snapshot_benchmarks/timing.py` — computes restore phases from agent and application clocks
- `snapshot_benchmarks/analysis/` — classifies startup spans and accounts for overlapping work
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

## Running a benchmark

Follow the [quick start](../docs/development/benchmarks-guide.md#quick-start)
for cluster settings, a dry run and a measured run. Each run selects one engine,
model and TP/EP configuration.

Results from new runs are saved as JSON and logs under `results/`. The published
tables are in [Performance Benchmarks](../docs/development/benchmarks.md).

## Local checks

```bash
uv run --project benchmarks python -m unittest discover -s benchmarks/tests
```

These checks exercise guide-based manifests, simulated lifecycle calls, timing
accounting and reports. They do not start GPU workloads.
