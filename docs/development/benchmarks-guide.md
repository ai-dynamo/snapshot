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
[deployment.yaml](../guides/vllm/deployment.yaml) already uses), with
container start (`cold_start.container_start_seconds`) reported separately so
it can be excluded to match the published doc's "Cold start" column.

Checkpoint size (`model.checkpoint_artifact_bytes`) is measured with `du -sb`
on the capture node — neither `PodSnapshot` nor `PodSnapshotContent` status
exposes a size field.

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

## Direct PageBroker transfer endurance

`pagebroker` publishes checkpoint files, verifies two restores per cycle, and
deletes its publication. The same broker runs throughout workload and two idle
hours. Choose one case per invocation:

| Backend | Transfer engine |
| --- | --- |
| `filesystem` | `posix-copy` |
| `s3` | `model-streamer` |

This tests file integrity and broker resources. Existing `run`/`sweep` commands
exercise live-model Snapshot operations; transferring a captured Qwen directory
with `--fixture-dir` does not establish that its model process can resume.

Use Linux amd64, Python 3.12+, and permission to read the child broker's
`/proc/PID/{status,smaps_rollup,stat,fd}`. Build with the pinned Go toolchain:

```bash
uv sync --locked --project benchmarks
make -C agent pagebroker-bench
make -C agent/pagebroker daemon CUDA_ROOT=/opt/cuda
```

The native build needs the NIXL, Model Streamer, AWS CRT/C++ SDK, protobuf,
OpenSSL, NVTX and CUDA dependencies from the Dockerfile's `build-dependencies`
stage. Install matching runtime libraries, configure `LD_LIBRARY_PATH` and
`NIXL_PLUGIN_DIR` as needed; copying the executable alone is insufficient. A GPU
or installed Snapshot stack is unnecessary: CUDA initialization is disabled.

Use separate absolute work, storage and output roots. Work must be local;
tmpfs is optional and must have room for staging. Publications use the storage
mount under test; successful cycles delete only their unique owned children.

```bash
uv run --locked --project benchmarks python -m snapshot_benchmarks pagebroker \
  --backend filesystem --transfer-engine posix-copy \
  --broker-bin /opt/snapshot/pagebroker --client-bin /opt/snapshot/pagebroker-bench \
  --work-dir /dev/shm/pagebroker-bench --storage-root /mnt/checkpoints/pagebroker-bench \
  --output-dir /mnt/benchmark-results --duration 30m
```

Use `30m`, `4h`, `3d` or `7d`; 7,200 idle seconds follow **final transfer and
cleanup**. Starts are at least 60s apart without catch-up bursts; three verified
measured cycles are required. Crossing cycles retain their original timeout and
count as drain. Defaults: `--cycle-timeout 600s`, `--cleanup-timeout 120s`,
`--sample-interval 10s`. Transactions close between cycles and before idle.

Generated fixtures have `(128 << 20) + 37` payload bytes plus a manifest, small
binary/empty files, nested/empty directories and varied modes. `--payload-bytes`
changes the large file; `--fixture-dir` instead preserves and copies a captured
source. Verification checks paths/types/modes/sizes/SHA-256. Native limits:
10,000 entries, 512-byte paths, readable/traversable ordinary modes, no links or
special files, 1 TiB total, and a regular root `manifest.yaml` up to 1 MiB.
Captured files use `corpus/` plus an outer manifest copy so reserved `gpu/` files
also use CPU staging. Counts include the wrapper and extra manifest; fixture
preparation is timed separately.

### Native S3

Use the existing nonsecret daemon configuration, for example:

```json
{"storeId":"template","endpoint":"https://s3.us-east-1.amazonaws.com",
 "region":"us-east-1","bucket":"YOUR-TEST-BUCKET","prefix":"pagebroker-benchmarks",
 "addressing":"virtual"}
```

The runner derives a unique store/data prefix. The SDK uses the same endpoint,
region, addressing and optional absolute `caFile`; TLS is verified. HTTP needs
explicit `allowHttp` and a loopback endpoint. The SDK performs probes/deletion;
PageBroker publishes via native upload and restores through Model Streamer.

Set the existing test key pair in `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`:

```bash
uv run --locked --project benchmarks python -m snapshot_benchmarks pagebroker \
  --backend s3 --transfer-engine model-streamer \
  --broker-bin /opt/snapshot/pagebroker --client-bin /opt/snapshot/pagebroker-bench \
  --work-dir /dev/shm/pagebroker-bench --output-dir /mnt/benchmark-results \
  --s3-config /etc/pagebroker-bench/s3.json \
  --credentials-source static-environment --duration 3d
```

Static mode rejects session tokens/expiry. Temporary mode requires a session
in `AWS_SESSION_TOKEN`, `--credentials-source temporary-environment`, and RFC3339
`--credentials-expire-at` covering the runtime budget below plus 15 minutes.
Credentials resolve once; there is no refresh. Use static test credentials for
multi-day runs. Profiles/instance fallback are disabled; credential values never
enter requests, configuration, results or the Go helper environment.

The identity needs bucket versioning status, object/version/multipart listing,
read/write, and exact object/version deletion permissions in the namespace.
Cleanup verifies emptiness, including versions/delete markers in Enabled or
Suspended buckets. Partial deletion, incomplete uploads or uncertain transactions
fail the run and retain the artifact location for investigation. The small owner
marker remains outside data. Inspect failed results before manually deleting
only that run's data; there is no automatic recovery command.

### Long execution and results

Use a service manager to survive SSH disconnect. Adapt paths/user and run the
prepared venv's absolute Python executable directly:

```bash
sudo systemd-run --unit=pagebroker-bench-filesystem \
  --property=Type=exec --property=User=YOUR-USER --property=UMask=0077 \
  --property=Restart=no --property=KillMode=control-group \
  --property=SendSIGKILL=yes --property=TimeoutStopSec=120 --property=RuntimeMaxSec=11700 \
  --working-directory=/opt/snapshot \
  /opt/snapshot/benchmarks/.venv/bin/python -m snapshot_benchmarks pagebroker \
  --backend filesystem --transfer-engine posix-copy \
  --broker-bin /opt/snapshot/pagebroker --client-bin /opt/snapshot/pagebroker-bench \
  --work-dir /dev/shm/pagebroker-bench --storage-root /mnt/checkpoints/pagebroker-bench \
  --output-dir /mnt/benchmark-results --duration 30m
sudo systemctl status pagebroker-bench-filesystem
sudo journalctl -u pagebroker-bench-filesystem
sudo systemctl stop pagebroker-bench-filesystem
```

Pass loader/plugin settings through the service environment. For S3, use an
existing protected file outside the repository, e.g.
`--property=EnvironmentFile=/run/pagebroker-bench/aws.env`. Keep credential values
out of command arguments and committed files. User services need existing
lingering. Set the runtime cap to at least this budget, plus startup allowance:
`1800 + active + cycle-timeout + cleanup-timeout + cooldown + 30 + 120` seconds.
Defaults give 11,670s for 30m. Children stay in the cgroup; runner death stops
them. Reboot/replacement cannot resume a campaign; forced termination may retain
artifacts. Foreground execution depends on the shell/session; there is no
independent heartbeat supervisor.

Results use `OUTPUT/pagebroker/DATE-nogit-ID/`. `run.json.config` records all 20
effective CLI settings (seconds, resolved paths, generated payload or null,
credential mode/declared expiry), alongside owned paths, identities/hashes and
admitted corpus manifest/hash/wrapped bytes. `cycles.jsonl` journals starts and
outcomes/cleanup; `resources.jsonl` samples RSS/HWM, PSS/private bytes, threads,
FDs and staging counts/bytes. `summary.json` records status, timing/counts and
samples/peaks. Optional `fixture.json` is the source manifest; `broker.log` and
failure-only `helper.log` are bounded, incomplete diagnostic tails.

Broker exit/replacement, missing metrics, sampling gaps over three intervals,
leftovers, failed verification and forced shutdown fail the campaign. Idle sends
no protocol requests. Output/memory remain bounded; missing terminal summary
means incomplete evidence. SIGINT/SIGTERM requests bounded shutdown and exit 130.
A successful result proves recorded transfer/continuity checks; resource values
retain `resource_assessment: not_assessed`. RSS alone does not prove a leak;
caches/allocator retention can matter. Heaptrack remains future work.

For labeled short qualification, append `--qualification --duration 120s
--cycle-interval 1s --sample-interval 1s --payload-bytes 1048613 --cooldown 30s`.
Fake clocks test scheduling; record actual native durations separately. Run
qualification, then 30m plus full two-hour idle before longer campaigns. Fast checks:

```bash
uv run --locked --project benchmarks --group test pytest benchmarks/tests -q
go test ./agent/cmd/pagebroker-bench ./agent/internal/pagebroker
make -C agent pagebroker-bench
```
