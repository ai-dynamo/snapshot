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
- An installed Snapshot Helm release and its checkpoint PVC. Use `KUBECONFIG`
  to connect to the cluster. See [Installation](../docs/operations/install.md).
- An engine image compatible with the selected [framework guide](../docs/guides/)
- A model PVC with the model files, a writable work PVC and enough free GPUs

## Layout

The runner uses the engine guide workloads and the `snapshot_e2e.k8s` and
`snapshot_e2e.lifecycle` helpers. Benchmarks need large engine images and take minutes
per model, so they run manually, outside the e2e CI suite.

- `snapshot_benchmarks/schema.py` — the `RunResult` data model written to disk
- `snapshot_benchmarks/run.py` — runs one model and engine configuration
- `snapshot_benchmarks/engines/` — vLLM, SGLang and TRT-LLM adapters that load
  the corresponding guide deployment templates
- `snapshot_benchmarks/measurement.py` — collects clocks, artifact sizes and inference checks
- `snapshot_benchmarks/timing.py` — computes restore phases from agent and application clocks
- `snapshot_benchmarks/analysis/` — classifies startup spans
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
model and TP/EP configuration. SGLang runs enable checkpoint prefetch by default;
`--engine-args '{"weight_loader_prefetch_checkpoints":false}'` disables it.

New runs save JSON and logs under `results/`. See [Performance
Benchmarks](../docs/development/benchmarks.md) for the published tables.

Cold starts clear the node's page cache and verify weight-file residency before
timing begins. This requires permission to create a privileged init container and
clear the node cache. `--no-drop-page-cache` skips this step. Each run records the
cache policy and saves the probe results.

## Local checks

```bash
uv run --project benchmarks python -m unittest discover -s benchmarks/tests
```

These checks test guide manifests, simulated lifecycle calls, timing calculations and
reports. They do not start GPU workloads.

Use `--detailed-timing` to collect cold-start phase breakdowns. Normal runs do not
patch engine functions or trace imports.
