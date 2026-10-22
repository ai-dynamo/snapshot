<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# PageBroker k3s smoke test

The endurance extension is being implemented separately. See
[STRESS_PLAN.md](STRESS_PLAN.md) for its six steps. Steps 1–4 add the persistent
lifecycle, validated configuration, independent supervisor, readiness-only Go
probe, resource evidence, supervised S3 cleanup, container packaging and the four
endurance commands. `stress.py` requires the real resource
observer and, in S3 mode, a successful cleanup-helper readiness acknowledgment.
`run.py` remains the smoke entry point.

Step 5 adds the reproducible native local qualifier, committed short/four-hour
profiles and [review and AWS runbook](STRESS_RUNBOOK.md). `stress_local.py` runs
multiple real batches with the packaged roles, then independently validates
bounded read-only exports, container shutdown, staging and S3 retention. It uses
only loopback TLS S3 with fake credentials; AWS execution waits for review.

Run the lifecycle/evidence tests and existing smoke regressions locally, from the
repository root:

```bash
uv run --locked --project e2e python -B -m unittest discover \
  -s agent/pagebroker/tests/k3s -p 'test_*.py' -v
go test ./agent/internal/pagebroker -count=1
```

The process tests need Linux Unix sockets, pidfds and same-UID signals. They use
local fixture processes, real `/proc` sampling and an explicit staging-filesystem
fixture. The Go readiness tests exercise the actual protocol client. These tests
require no AWS, Kubernetes or GPU and do not establish that the packaged native
daemon passes endurance. Synthetic resource traces use a fake clock for four
measured hours to test analysis; they are not actual four-hour campaigns.

The endurance modules have these responsibilities:

- `stress_config.py` validates a versioned `RunSpec` and its `Timing` budget.
  Persist `created_at`, `startup_deadline` and `expires_at` before starting peers;
  loading the spec never refreshes these UTC Unix timestamps.
- `stress.py` runs warm-up, measurement, optional final-batch drain and final idle
  against one registered broker. Every invocation has a private result directory;
  client UUID claims on disk reject reused IDs without an in-memory history.
- `stress_control.py` shares atomic control messages and pidfd identities.
  Messages carry the run UUID; each process keeps its own exclusive owner marker.
- `stress_supervisor.py` independently registers the runner and socket peer,
  acknowledges client gates and owns broker shutdown. Both control loops keep
  heartbeats while waiting, and neither uses an independent heartbeat thread.
- `stress_evidence.py` samples process memory, descriptors, threads and staging;
  writes bounded resource chunks and idle-window records; and validates reports.
- `stress_analysis.py` independently recomputes idle medians, coverage, aggregates
  and later-hour slopes from saved streams. `stress_cgroup.py` adds optional
  cgroup v2 measurements only after checking broker membership and peer exclusion.
- `stress_storage.py` checks quotas before writes, reserves capacity for external
  client reports, synchronizes files/checkpoints and creates a strict hash manifest.
- `s3_cleanup.py` owns a unique S3 prefix, probes permissions and runs bounded SDK
  workers. The supervisor acknowledges each worker's pidfd gate before SDK work.
- `stress_orchestrate.py` implements `start`, `status`, `collect` and `stop` with
  explicit cluster selection, durable ownership and recoverable collection.
- `stress_render.py` renders the separate endurance profile from the smoke bases;
  `stress_export.py` streams and extracts only bounded, permitted evidence files.
- `stress_images.py` packages the reviewed Docker images as OCI archives and
  verifies configuration and manifest identities separately.

The runner, supervisor and S3 helper consume the same JSON spec through `--spec`.
They require a shared UID, PID namespace and control directory. The helper also
reads verified client reports from the results volume. Cleanup requests/replies
use atomic, synchronized files and monotonic deadlines. The Docker `stress` target
packages these roles; the `runtime` target retains the existing smoke entry point.

A run-scoped `stop.json` control message requests cooperative drain and produces
`INTERRUPTED`. SIGINT/SIGTERM skip drain and enter bounded cleanup. Runner death
or stale heartbeats trigger independent supervision; supervisor failure triggers
runner takeover. Forced kills and missing shutdown acknowledgments fail the run.
Before a broker exposes its socket, or if both observers are lost, prompt peer
cleanup cannot be guaranteed; the Job deadline remains the fallback.

The `evidence` section of the run spec configures sampling and storage. Defaults
are one-second samples, progress every ten seconds, 16 MiB resource chunks and
512 MiB exported capacity. The in-Pod writers use at most 384 MiB, including a
2 MiB reserve for terminal/failure evidence; 128 MiB remains reserved for later
collection diagnostics. Client logs are streamed through a bounded pipe reader
with a 1 MiB per-invocation limit. Exceeding a required-evidence limit fails the
run and preserves a bounded diagnostic tail. File/directory counts are capped
at 4,096; manifests reject links, unexpected paths, changed hashes and extra files.

`progress.json` records current phase, batch, counts, identity and latest metrics.
`batches.jsonl` records admissions/completions; byte totals cover the deterministic
payload, excluding auxiliary fixture files. Resource chunks, `idle-windows.jsonl`,
`resource-metadata.json` and `resource-analysis.json` preserve the analysis inputs
and independently checked results. The fixed staging baseline follows protocol
readiness. Cgroup counters remain explicitly unsupported when attribution cannot
be verified; the runner's cgroup is never substituted for the broker's.

Idle medians use the last ten samples. The initial reference uses three warm-up
windows; sustained-growth analysis uses measured hours 2–4 and a later reference
after measured second 3,600. This accounts for the broker's one-hour terminal
transaction retention and two-minute reap interval. `retention_revision` records
the tested revision's policy. Missing coverage, unknown retention or a short
profile prevents a conclusive resource assessment.

`executionStatus`, `evidenceStatus` and `resourceAssessment` are separate fields.
Without predeclared `growth_limits`, resource assessment is
`INCONCLUSIVE`, even after functional success. Limits name the recorded metrics:
growth compares idle medians with the later reference; absolute limits apply to
sampled peaks. `WITHIN_LIMITS` describes only those limits and the observed period.
Observed growth is evidence for investigation rather than proof of a leak.
The analysis also reports `limitsExceeded`, propagated as
`resourceLimitsExceeded` in terminal summaries. Any known absolute or growth
breach makes the command return nonzero, even on a short run or after optional
cgroup collection fails; functional execution remains a separate outcome.
Client exit code 2 identifies a resource-limit-only failure after successful
execution and complete evidence. Collection accepts that specific Job failure
only when independently recomputed limits confirm it; other failed containers,
OOMs and restarts remain execution failures. Collection still returns nonzero.

The in-Pod summary has `evidenceScope: IN_POD` and
`kubernetesCollection: PENDING_COLLECTION`. The host collector independently
validates that evidence plus authoritative Job/Pod/container status and records
`evidenceScope: KUBERNETES_COLLECTED`. No endurance campaign or AWS deployment has
been executed while implementing steps 1–5. Native integration uses the separate
`stress_local.py` qualification command in the runbook.

For S3, the nonsecret `s3` spec contains `bucket`, `prefix`, `region`, an HTTPS
`endpoint`, `addressing` (`virtual` or `path`), `credential_provider`
(`temporary-environment` or `static-environment`) and `credential_expiry` (a UTC
Unix timestamp for temporary credentials, JSON `null` for static keys). The prefix
must end in the backend run UUID; `store_id` must be `stress-<UUID>`. The renderer generates
the broker storage configuration from these same settings; a Capabilities response
cannot prove which bucket/prefix a configured store uses.

Only PageBroker and the helper receive credentials. For temporary credentials,
export `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN` and
`AWS_CREDENTIAL_EXPIRATION` (timezone-qualified ISO 8601). The helper freezes these
values and requires expiry metadata to match, with validity through a full Job
deadline from admission plus 900 seconds. Unknown or insufficient expiry fails.
The host launcher checks again immediately before Job creation and delivers the
same credentials to PageBroker; mounted Secret rotation does not
refresh the current broker clients. No credential values belong in a RunSpec,
generated manifest, result file or log. The Go client and supervisor need none.

Static keys require the explicit `static-environment` provider and only
`AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`. Session tokens (including the
legacy `AWS_SECURITY_TOKEN`) and expiration metadata must be unset or empty.
The helper records a null expiry rather than inventing a future timestamp. Run
deadlines, TLS verification, owned-prefix cleanup and resource budgets still apply.

The helper refuses populated prefixes, then conditionally creates and retains
`.snapshot-stress-owner.json`. Readiness exercises read/write, multipart
completion/abort and verified deletion. Cleanup follows successful client exit,
report validation/persistence and supervisor acknowledgment of client quiescence.
The request binds the batch, client UUID, broker identity and report SHA-256.
Only the latest request/reply is cached; lost replies are replayed without another
deletion. Changing a failed request cannot admit another batch.

Listing is capped by pages, entries and 1 GiB retained bytes, including versions;
multipart parts/bytes have explicit inspection budgets. Deletion checks every
object acknowledgment, removes versions/delete markers explicitly and verifies
emptiness while preserving the owner marker. Unexpected incomplete uploads fail
before any deletion or recovery abort. The failed batch remains for diagnosis;
partial deletion and timed-out requests can leave partial or uncertain state.
Emergency supervision kills SDK workers and the helper without starting cleanup.
`cleanup-readiness.json`, `cleanup.jsonl`, the reserved `cleanup-final.json` and
the supervisor's helper acknowledgment preserve this evidence. A pending final
request is incomplete evidence, never successful cleanup.

`stress-requirements.txt` pins the helper SDK and transitive dependencies with
hashes. To run the additional local TLS tests in a disposable environment:

```bash
uv venv /tmp/snapshot-stress-tests
uv pip install --python /tmp/snapshot-stress-tests/bin/python --require-hashes \
  -r agent/pagebroker/tests/k3s/stress-requirements.txt
uv pip install --python /tmp/snapshot-stress-tests/bin/python \
  -r agent/pagebroker/tests/requirements.txt
/tmp/snapshot-stress-tests/bin/python -B -m unittest discover \
  -s agent/pagebroker/tests/k3s -p 'test_s3_cleanup*.py' -v
```

These tests require OpenSSL and use fake credentials, a loopback TLS emulator,
fixture brokers/clients and real pidfds. They test SDK operations and the harness
protocol; `stress_local.py` adds native PageBroker qualification. Moto 5.1.11 crashes
on suspended `null`-version deletion, so that response path has direct unit
coverage; enabled versioning and delete markers have TLS coverage. Environments
without the emulator dependencies explicitly skip the TLS tests.

## Endurance commands

Run the host controller on Linux with `kubectl`, `uv`, a readable kubeconfig and
an explicit context. The host needs namespace/Job/Pod/PVC/ConfigMap/Secret create,
read and update permissions, Pod exec/log access, and node/all-namespace Pod reads
for preflight. No workload container receives a Kubernetes API token.

Build the harness image locally:

```bash
make docker-build-pagebroker-stress REGISTRY=local VERSION="$HARNESS_TAG" \
  DOCKER_BUILD_ARGS="--load --label org.opencontainers.image.revision=$HARNESS_SHA"
```

Follow [STRESS_RUNBOOK.md](STRESS_RUNBOOK.md) to package both images into the
reviewed `images.oci.tar` and `identities.json` before importing the OCI archive.
Stress uses `imagePullPolicy: Never`; preflight requires both immutable manifest
references in the node inventory. `--image-identities` binds each declared source
SHA to the archived configuration label and manifest/configuration digests.
Jobs and exporters use immutable manifest references. Collection checks actual
runtime IDs using the appropriate digest type; a changed image under the same
tag cannot pass. Campaign records use schema version 2 and reject older records
without this binding. Native image integration and
the implementation review are step 5; AWS qualification/campaigns are step 6.
The launcher's `--node` is the Node name; preflight derives its hostname label
for both the workload and exporter selectors.

Define the command from the repository root. `--kubeconfig` and `--context` precede
the subcommand and are mandatory on every invocation:

```bash
stressctl() {
  uv run --locked --project e2e \
    --with-requirements agent/pagebroker/tests/k3s/stress-requirements.txt \
    python agent/pagebroker/tests/k3s/stress_orchestrate.py \
    --kubeconfig "$KUBECONFIG" --context "$CONTEXT" "$@"
}
```

Launch both backends with a fresh record path, after the required review:

```bash
stressctl start --record "$RUN_DIR/run.json" --backend both \
  --node "$NODE" --client-image "$STRESS_IMAGE" \
  --pagebroker-image "$PAGEBROKER_IMAGE" \
  --harness-revision "$HARNESS_SHA" --broker-revision "$BROKER_SHA" \
  --image-identities "$REVIEW_DIR/images/identities.json" \
  --bucket "$S3_BUCKET" --region "$S3_REGION" --prefix "$S3_PREFIX" \
  --credentials-source aws-cli
```

`--credentials-source aws-cli` calls `aws configure export-credentials --format
process` into memory; `--aws-profile NAME` optionally selects its source. The
default `--credentials-source environment` uses the four AWS variables documented
above. No credential file or Secret manifest is written. Each S3 namespace gets
an immutable temporary Secret, referenced only by the broker and helper. Temporary
credentials require expiry through the full configured Job deadline plus 15 minutes, checked
again before submission. Unknown/short-lived expiry cannot launch the S3 Job.

For a local S3-compatible service or other non-expiring keys, explicitly select
`--credentials-source static-environment`. Load `AWS_ACCESS_KEY_ID` and
`AWS_SECRET_ACCESS_KEY` from your private credential source first; do not put
their values on the command line. Check that these are static keys before clearing
any inherited session metadata:

```bash
unset AWS_SESSION_TOKEN AWS_SECURITY_TOKEN AWS_CREDENTIAL_EXPIRATION
stressctl start --record "$RUN_DIR/run.json" --backend s3 \
  --node "$NODE" --client-image "$STRESS_IMAGE" \
  --pagebroker-image "$PAGEBROKER_IMAGE" \
  --harness-revision "$HARNESS_SHA" --broker-revision "$BROKER_SHA" \
  --image-identities "$REVIEW_DIR/images/identities.json" \
  --profile "$RUN_PROFILE" \
  --bucket "$S3_BUCKET" --region "$S3_REGION" --prefix "$S3_PREFIX" \
  --endpoint "$S3_ENDPOINT" --addressing path --ca-bundle "$S3_CA_BUNDLE" \
  --credentials-source static-environment
```

The endpoint must be reachable from both the launcher and Pods. Use the same
trusted CA for both. Static mode creates a Secret with only the two key fields;
it rejects session tokens or expiration metadata instead of silently treating
temporary credentials as non-expiring. The default `environment` and `aws-cli`
sources continue to require a session token and sufficient expiry. Rebuild and
import the updated harness image so the in-Pod helper supports the same provider
as the launcher; the native PageBroker image does not need a credential change.

`--backend filesystem` requires no S3 arguments or credentials; `--backend s3`
selects S3 alone. The default is `both`, and an unavailable backend fails the
launch. Additional options are `--storage-class` (default `local-path`),
`--endpoint` (HTTPS origin), `--addressing path|virtual` (default `virtual`),
`--ca-bundle` (public PEM certificates only) and `--profile FILE`.

The optional JSON profile contains only `timing` and `evidence` overrides, using
the field names in `stress_config.py`. Missing fields use the four-hour defaults.
Short profiles must still fit all warm-up, batch, cleanup, drain, cooldown,
shutdown and margin budgets; the Job deadline must be an integer. Record resource
limits and `retention_revision` before launch if a gated assessment is intended.
Short runs and runs without declared growth limits remain `INCONCLUSIVE`.

`start` records the plan before mutations, preflights selected-node readiness,
imported images, requests/memory-limit headroom, ephemeral-storage allocation and
S3 prefix access, then submits Jobs and waits for protocol/baseline readiness.
Capacity checks use allocatable resources and existing requests; they do not
measure actual free node disk. Three containers run for filesystem, four for S3.
Each has explicit resource limits; each Job has zero retries, no container restart,
a five-hour default active deadline, 90-second Pod termination grace, dedicated
tmpfs staging and a 2 GiB result PVC. The production Helm release is untouched.

After `start` returns, the local process can exit. Jobs supervise themselves.
The local record and each owned namespace's `pagebroker-stress-run` ConfigMap
persist the same nonsecret campaign plan and observed UIDs. Resource names and
ownership markers allow lost replies to be reconciled; foreign resources, changed
UIDs and replacement Pods are rejected. Startup UTC deadlines never reset.

```bash
stressctl status --record "$RUN_DIR/run.json"
stressctl collect --record "$RUN_DIR/run.json" --output "$RUN_DIR/collected"
stressctl stop --record "$RUN_DIR/run.json" --output "$RUN_DIR/stopped"
```

If the local record is unavailable, substitute `--namespace "$RUN_NAMESPACE"`
for `--record ...` on any of these three commands. Either owned backend namespace
can recover the full campaign. The explicit context must match the persisted
context; the cluster and selected node UIDs must also match.

`status` reads progress through the existing client container and reports phase,
batch counts, sample metrics, progress age, container states and pending Secret
cleanup. It does not mount another result writer. API/progress gaps remain visible.

`stop` sends drain requests to every running backend before waiting. Each has its
remaining batch/cleanup budget and shutdown allowance. If drain fails, the host
suspends only the owned Job, preventing further Pods, and waits up to 120 seconds
for its writers to disappear. Pod termination then uses the 90-second grace.
Forced cancellation can lose final Pod status and remains incomplete evidence.
The command collects available results and reports interruption with nonzero exit.
Pod SIGTERM skips cooperative drain. Before socket registration, or after loss of
both observers/node, the active Job deadline remains the shutdown fallback.

`collect` refuses active writers. It checks ownership of the result PVC and live
configuration, then uses a temporary exporter with a read-only PVC mount, no API
token and a ten-minute lifetime. Transfer/extraction reject unexpected paths,
links, duplicates, sparse files and size/count overruns. A resumed collection
replaces only unfinished temporary downloads and reuses completed immutable
exports, validating them again. Use the same output directory to resume; a
different campaign cannot adopt it. Allow local disk headroom for an additional
bounded archive during transfer.

Each backend directory contains `results/`, `final-job.json`, `final-pods.json`,
bounded container log tails, `events.json` and `diagnostics.json`. The root has
`run.json` and `summary.json`. Missing log tails/events are diagnostic gaps; missing
or failed required container/evidence status cannot pass. Logs are `TAIL_ONLY`,
never proof of complete history. Diagnostic downloads respect the configured
reserve, with at most 16 MiB per container and 8 MiB for events.

After writers stop, `collect` and `stop` remove only the owned temporary exporter
and S3 Secret, even when export/validation fails. Interrupted or lost deletion
replies are reconciled on the next collection. Secrets remain `PENDING` until a
host invocation confirms deletion; no in-Pod role has permission to remove them.
Namespaces, result PVCs and S3 ownership markers/failed-batch data are retained.
Deleting those retained resources is a separate operator action.

Collection returns nonzero for execution/interruption, incomplete evidence,
temporary-resource cleanup failure or any confirmed resource-limit breach. Functional success with
resource assessment `INCONCLUSIVE` is a completed diagnostic run, not proof of
absence of leaks. The fast harness tests run in CI without a cluster, AWS or GPU;
the optional TLS suite remains the separate local emulator gate.

For manifest-only inspection, `stress_render.py --spec FILE --namespace NAME
--campaign UUID --owner UUID --client-image IMAGE --broker-image IMAGE --node NODE
--output NEW_DIRECTORY` renders workload resources without contacting Kubernetes.
Optional `--storage-class` and `--ca-bundle` match the launcher. It does not create
the controller's ownership ConfigMap or credential Secret.
This offline renderer's `--node` supplies the hostname label directly.

The existing smoke suite runs a dedicated PageBroker and a direct Go client in one
Kubernetes Job. Filesystem and S3 runs use separate Jobs and namespaces. These
Jobs do not change the installed Snapshot release or require the Snapshot agent,
operator or a GPU. The S3 client talks to PageBroker; only PageBroker receives AWS
credentials. Separate opt-in Qwen cases below use the installed Snapshot release
and a GPU to exercise real filesystem checkpoint/restore.

## Coverage and results

The fixture contains a deterministic 128 MiB + 37 byte payload, a manifest,
small and empty files, and empty directories. Every restore verifies all paths,
types, sizes, SHA-256 digests and permissions. Successful Commit and Abort must
remove their staging directories; every transaction has a bounded Abort fallback.

| Mode | Operations | Required result |
| --- | --- | --- |
| Filesystem | Three publish/two-restore cycles and a prepared abort with `posix-copy` | 11 steps, 3 publications |
| S3 | Three publish/metadata/two-restore cycles, a prepared abort, and wrong-store rejection for prepare, metadata and restore, using `model-streamer` | 15 steps, 3 publications |

The launcher waits for the Unix socket, starts the Go client, validates its report,
and sends SIGTERM to the PageBroker process identified through that socket. Both
containers share a process namespace and UID 65532. A Linux pidfd tracks the peer
throughout the run. A client failure, missing or incomplete report, daemon exit,
or shutdown timeout makes the launcher fail. Job retries and container restarts
are disabled. The Job has a 30-minute deadline.

A successful run requires **Job Complete, both containers exiting zero without
restarts, and fresh SUCCESS reports**. The launcher cannot read the exit code of
the peer container; Kubernetes supplies that part of the verdict. A skipped or
unmatched Go test can exit zero but cannot pass report validation.

The results PVC retains `summary.json`, `client-report.json`, `client.log` and
`resources.jsonl`. Resource collection failures also fail the smoke run.
Reports can remain `RUNNING` after a forced termination; that is a failed or
incomplete run. No automatic Job/PVC deletion hides failure evidence. PageBroker
stdout and Kubernetes status must also be exported before deleting the namespace.

The synthetic smoke test checks correctness, cleanup and resources during one
short daemon lifetime. Neither it nor the Qwen cases establish memory-leak freedom.
Multi-day soak tests, fault campaigns and full-agent S3 checkpoint/restore remain
subsequent work.

## Resource monitoring

The launcher samples PageBroker every second, starting before the client runs.
After a successful client run, it samples through five seconds of idle time and
takes a final sample before terminating PageBroker. Set `--sample-interval`
(0.1–60 seconds) and `--settle-seconds` (0–60 seconds) on `render.py` to change
these settings. Zero settling time takes the final sample immediately.

Each JSON line in `resources.jsonl` includes UTC time, monotonic elapsed time,
PageBroker PID/start time, phase and the following metrics:

| Metrics | Source and interpretation |
| --- | --- |
| `rssBytes`, `pssBytes`, `privateBytes` | `/proc/PID/smaps_rollup`: resident memory, proportionally attributed memory and private clean + dirty pages |
| `anonymousRssBytes`, `fileRssBytes`, `sharedRssBytes` | `/proc/PID/status`: approximate resident anonymous, file-backed and shared-memory components |
| `rssHighWaterBytes`, `anonymousSwapBytes` | `/proc/PID/status`: resident high-water mark and private anonymous swap |
| `fileDescriptors`, `threads` | Actual `/proc/PID/fd` entry count and the process's reported thread count |
| `stagingFilesystemUsedBytes`, `stagingFilesystemUsedInodes` | `statvfs` usage for the shared staging filesystem, including allocated space still held by unlinked open files |

The [Linux proc documentation](https://docs.kernel.org/filesystems/proc.html)
describes the memory counters and the approximate accounting in `status`.
Process RSS is distinct from total container memory and filesystem cache. The
sampler does not read cgroup memory or aggregate child processes. Staging usage
covers the whole filesystem: the supplied Jobs give staging its own tmpfs mount;
using a subdirectory of a larger filesystem measures that larger filesystem.
`summary.json` records `stagingIsMountPoint` to make that distinction visible.

Phases are `baseline`, `workload`, `after-client`, `settling` and `settled`.
Failure cleanup attempts a final `failure` sample while the daemon is still
alive. Each record is flushed immediately. Missing fields, permission errors,
daemon exit and output failures cannot silently become zero-valued metrics or
a successful run. The monitor holds only aggregate statistics in memory; the
sample history streams to the PVC. No additional capabilities are required in
the supplied containers; the peer must permit same-UID `/proc` reads.

`summary.json.resources` contains the collection status, sample/phase counts,
elapsed duration and each metric's `first`, `last`, `peak` and `delta`. For a
successful run, `delta` compares the final idle sample with the pre-client sample.
Peaks are sampled peaks and can miss shorter spikes; the separate RSS high-water
counter also reflects activity before sampling began. Reports retain all
completed samples if a later collection fails. A forced kill can leave only
the raw stream and an unfinished launcher summary.

Inspect retained private/RSS memory, descriptor and thread counts after activity
stops, and whether tmpfs usage returns to its baseline. The initial sample is a
cold baseline: allocator caches and lazy initialization can cause legitimate
growth. The report always sets `leakAssessment: NOT_ASSESSED`; resource collection
`SUCCESS` means measurements completed, not that the process is leak-free.
Persistent growth requires comparison across warmed-up cycles in a longer run.

## Build and import images

Run from the repository root with Docker and the pinned Go toolchain. Build the
client image using the new target:

```bash
export SMOKE_TAG="smoke-$(git rev-parse --short HEAD)-$(date -u +%Y%m%d%H%M%S)"
export SMOKE_CLIENT_IMAGE="local/pagebroker-smoke:$SMOKE_TAG"

make docker-build-pagebroker-smoke \
  REGISTRY=local VERSION="$SMOKE_TAG" DOCKER_BUILD_ARGS="--load"
```

Use a PageBroker runtime image built from this stack, including #469's JSON
configuration support. The new client image does not contain PageBroker. To
build the runtime image with the internally downloaded and checksummed SDK:

```bash
make docker-build-pagebroker REGISTRY=local VERSION="$SMOKE_TAG" \
  DOCKER_BUILD_ARGS="--load --target runtime"
export SMOKE_PAGEBROKER_IMAGE="local/pagebroker:$SMOKE_TAG"
```

Alternatively, set `SMOKE_PAGEBROKER_IMAGE` to an existing matching runtime image.
Import both images into the target k3s node's containerd. With your existing
`SNAPSHOT_REMOTE` and `SNAPSHOT_SSH_KEY` variables:

```bash
set -o pipefail

docker image save "$SMOKE_PAGEBROKER_IMAGE" "$SMOKE_CLIENT_IMAGE" |
  gzip -1 |
  ssh -i "$SNAPSHOT_SSH_KEY" "$SNAPSHOT_REMOTE" \
    'bash -o pipefail -c "gzip -d | sudo -n k3s ctr --namespace k8s.io images import -"'
```

The templates default to `imagePullPolicy: Never`. Import on every eligible node
or pass `--node` when rendering to select the imported node. For registry images,
use full image references and `--image-pull-policy IfNotPresent` instead.

## Run the complete smoke suite

After building/importing the images and deploying the Snapshot release as described
in the Qwen section, `orchestrate.py` runs all four required cases in order:
filesystem, S3, Qwen3-0.6B and Qwen3-8B. Run it from the repository root using the
pinned e2e Python environment. There is no additional runner image to build.

The runner requires `kubectl`, one existing kubeconfig file, an explicit context,
and an external environment file containing `AWS_ACCESS_KEY_ID` and
`AWS_SECRET_ACCESS_KEY` as `KEY=value` lines.
Temporary credentials must include `AWS_SESSION_TOKEN` and remain valid through
the S3 case. The AWS file is read by `kubectl` when creating the temporary Secret;
it is not copied into the result directory. Select the same images imported into
k3s and deployed in the Snapshot release. The runner checks the agent rollout,
agent/PageBroker image references and checkpoint PVC before starting work.
Replace the example paths and environment settings below with local values;
keep those values outside the repository.

```bash
export KUBECONFIG="/path/to/kubeconfig"
export CONTEXT="your-kubernetes-context"
export PAGEBROKER_SMOKE_AWS_CREDENTIALS_FILE="/path/to/aws-credentials"
export S3_BUCKET="your-test-bucket"
export S3_REGION="your-aws-region"
export S3_PREFIX="pagebroker-smoke"
export SNAPSHOT_SMOKE_RESULTS="/tmp/pagebroker-smoke-$(date -u +%Y%m%dT%H%M%SZ)"

uv run --locked --project e2e python agent/pagebroker/tests/k3s/orchestrate.py \
  --kubeconfig "$KUBECONFIG" --context "$CONTEXT" \
  --pagebroker-image "docker.io/local/pagebroker:$SNAPSHOT_VALIDATION_TAG" \
  --client-image "docker.io/local/pagebroker-smoke:$SNAPSHOT_VALIDATION_TAG" \
  --agent-image "docker.io/local/agent:$SNAPSHOT_VALIDATION_TAG" \
  --bucket "$S3_BUCKET" --region "$S3_REGION" --prefix "$S3_PREFIX" \
  --snapshot-namespace snapshot --snapshot-release snapshot \
  --snapshot-pvc snapshot-pvc \
  --output "$SNAPSHOT_SMOKE_RESULTS"
```

The output directory must not exist. The default image pull policy is `Never`;
use `--image-pull-policy IfNotPresent` for images hosted in a reachable registry.
`--node` selects a hostname label for the synthetic Jobs and exporters. Qwen
placement follows the existing GPU harness. The model-cache, framework-image and
GPU-memory environment overrides described below still apply.

For each synthetic case, the runner creates a unique namespace, renders and saves
the manifests, starts the Job, waits for both containers to start and for the Job
to finish, then exports the PVC results. It collects Pod/Job status, events and
container logs even after failure. Startup is bounded by `--startup-seconds`
(default 300) and execution by `--job-seconds` (default 1920); the Job itself also
has its fixed 1800-second deadline. API calls and evidence collection have their
own bounded timeouts. The runner stops unfinished Jobs before exporting their
RWO results PVCs. Exporter Pods and the temporary credential Secret are removed.
Namespace/PVC results and completed Job Pods remain for inspection. Failed
cleanup is reported in `summary.json`; if cluster access is lost, resources may
need manual cleanup after access returns.

Each Qwen model runs in a separate required pytest invocation with a default
3600-second deadline (`--qwen-seconds`). Inherited pytest selection/plugin options
are cleared so they cannot deselect a required case. On timeout or interruption,
pytest gets up to 120 seconds for fixture cleanup before its process group is
killed. A separate bounded cleanup uses the recorded names of that case's pods,
snapshot and app ConfigMap. It does not delete unrelated workloads or shared model
cache volumes. Both kubectl and the GPU harness use the selected kubeconfig and
context without modifying the user's current context.

The runner prints each case's start/result and the final summary path. Detailed
Qwen progress is in `qwen3-0.6b/pytest.log` and `qwen3-8b/pytest.log`. Independent
cases continue after a failure; an interruption leaves remaining cases `NOT_RUN`.

| Local evidence | Contents |
| --- | --- |
| `summary.json` | Overall and per-case status, errors, source revision, image identities, namespaces, S3 prefix and resource aggregates |
| `filesystem/`, `s3/` | Rendered manifests, Kubernetes status/events/logs, exported PVC archive and `results/` files |
| `qwen3-0.6b/`, `qwen3-8b/` | pytest log, JUnit, benchmark JSON, ownership/cleanup records and installed Snapshot logs |

Exit zero and overall `SUCCESS` require **all four cases**. Synthetic results must
agree with Kubernetes completion/exit statuses and the complete resource sample
stream. Each Qwen case must have a successful pytest exit, exactly one unskipped
JUnit case, and a matching fresh benchmark containing one checkpoint and two
verified restores. The resolved PageBroker image digest must agree across the
synthetic and Qwen cases. Missing evidence and cleanup failures fail the suite.
The runner never treats skipped tests or an empty report as success.

After inspecting the saved results, delete the two namespaces named in
`summary.json` to remove their Jobs and PVCs. S3 objects are retained under the
reported unique prefix; namespace deletion does not delete those objects.

This runner does not build/import images, upgrade Helm, assess memory leaks or
run a soak/fault campaign. It uses the installed Snapshot release only for Qwen.
The synthetic Jobs include resource samples; Qwen still checks daemon continuity
without collecting the installed daemon's memory time series.

The following sections retain the individual render/deploy/export commands for
running or investigating one synthetic Job independently.

## Render an isolated run

Use a new namespace and output directory for every run. The default results
StorageClass is k3s `local-path`; select another with `--storage-class` if needed.
The node needs capacity for container limits of 3 GiB for PageBroker and 1 GiB for
the client. Staging is a shared 1 GiB memory-backed `emptyDir`; filesystem storage
is a separate 2 GiB disk-backed `emptyDir`. Results use a 1 GiB PVC. Memory-backed
staging also consumes container memory.

Filesystem example:

```bash
export SMOKE_NAMESPACE="pagebroker-smoke-fs-$(date -u +%Y%m%d%H%M%S)"
export SMOKE_MANIFESTS="/tmp/$SMOKE_NAMESPACE"

uv run --locked --project e2e python agent/pagebroker/tests/k3s/render.py \
  --mode filesystem --namespace "$SMOKE_NAMESPACE" \
  --client-image "$SMOKE_CLIENT_IMAGE" \
  --pagebroker-image "$SMOKE_PAGEBROKER_IMAGE" \
  --output "$SMOKE_MANIFESTS"
```

For S3, render a **separate** run using your bucket and region:

```bash
export SMOKE_NAMESPACE="pagebroker-smoke-s3-$(date -u +%Y%m%d%H%M%S)"
export SMOKE_MANIFESTS="/tmp/$SMOKE_NAMESPACE"

uv run --locked --project e2e python agent/pagebroker/tests/k3s/render.py \
  --mode s3 --namespace "$SMOKE_NAMESPACE" \
  --client-image "$SMOKE_CLIENT_IMAGE" \
  --pagebroker-image "$SMOKE_PAGEBROKER_IMAGE" \
  --bucket "$SMOKE_S3_BUCKET" --region "$SMOKE_S3_REGION" \
  --prefix pagebroker-smoke \
  --output "$SMOKE_MANIFESTS"
```

The renderer creates `resources.yaml` (namespace, PVC and ConfigMap), `job.yaml`
and `export.yaml`. It performs no cluster operations. S3 defaults to the region's
HTTPS AWS endpoint and virtual addressing. `--endpoint` accepts an alternative
HTTPS origin; this template assumes a publicly trusted certificate and virtual
addressing. Local path-addressed emulators need a different configuration.

The S3 store ID is the run namespace. The physical prefix is
`<prefix>/<namespace>`, isolating each run. The ConfigMap supplies both JSON files,
8 MiB multipart parts, a 1 GiB upload buffer budget, and a ten-minute transaction
limit. Filesystem transfers use POSIX; Model Streamer uses bounded S3 reader
concurrency. This smoke configuration does not test `io_uring` or peak throughput.

## Create and run the Job

Check that `kubectl` selects the intended k3s cluster, then create the resources:

```bash
kubectl config current-context
kubectl get nodes -o wide
kubectl create -f "$SMOKE_MANIFESTS/resources.yaml"
```

**For S3 only**, create the credentials Secret before creating the Job. Set
`SMOKE_AWS_CREDENTIALS_FILE` to a dedicated file outside the repository containing
`AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` as `KEY=value` lines. Temporary credentials must include
`AWS_SESSION_TOKEN` and remain valid for the run. Do not put credentials into the
rendered ConfigMap. The launcher and Go client do not receive this Secret.

```bash
kubectl -n "$SMOKE_NAMESPACE" create secret generic pagebroker-smoke-aws \
  --from-env-file="$SMOKE_AWS_CREDENTIALS_FILE"
```

The credentials need upload, read, metadata and multipart access under the run's
S3 prefix. No live S3 objects are deleted automatically.

Create the Job and wait. Preserve failure status and continue to collect evidence
if the wait fails:

```bash
kubectl create -f "$SMOKE_MANIFESTS/job.yaml"
smoke_wait_rc=0
kubectl -n "$SMOKE_NAMESPACE" wait --for=condition=Complete \
  job/pagebroker-smoke --timeout=31m || smoke_wait_rc=$?
printf 'Job wait exit code: %s\n' "$smoke_wait_rc"
```

A failed Job can make this wait reach its timeout. Inspect logs in another
terminal for earlier diagnosis. A PVC remaining Pending before the Job exists
can be normal for a `WaitForFirstConsumer` StorageClass.

## Collect evidence

Collect these even after failure. Check Job conditions, both container exit codes,
termination reasons and restart counts in `pods.json`:

```bash
mkdir -p "$SMOKE_MANIFESTS/evidence"
kubectl -n "$SMOKE_NAMESPACE" get job pagebroker-smoke -o json \
  > "$SMOKE_MANIFESTS/evidence/job.json"
kubectl -n "$SMOKE_NAMESPACE" get pods -l job-name=pagebroker-smoke -o json \
  > "$SMOKE_MANIFESTS/evidence/pods.json"
kubectl -n "$SMOKE_NAMESPACE" logs job/pagebroker-smoke -c pagebroker \
  > "$SMOKE_MANIFESTS/evidence/pagebroker.log"
kubectl -n "$SMOKE_NAMESPACE" logs job/pagebroker-smoke -c client \
  > "$SMOKE_MANIFESTS/evidence/launcher.log"
kubectl -n "$SMOKE_NAMESPACE" get events --sort-by=.metadata.creationTimestamp \
  > "$SMOKE_MANIFESTS/evidence/events.txt"
```

After the Job Pod terminates, create the read-only results exporter. It mounts
the same PVC and contains no AWS credentials. If the Job never started, the PVC
may be empty; absent reports do not count as success.

```bash
kubectl create -f "$SMOKE_MANIFESTS/export.yaml"
kubectl -n "$SMOKE_NAMESPACE" wait --for=condition=Ready \
  pod/pagebroker-smoke-results --timeout=120s
kubectl -n "$SMOKE_NAMESPACE" exec pagebroker-smoke-results -- \
  python3 -c 'import sys,tarfile; archive=tarfile.open(fileobj=sys.stdout.buffer,mode="w|"); archive.add("/results",arcname="results"); archive.close()' \
  > "$SMOKE_MANIFESTS/evidence/results.tar"
tar -xf "$SMOKE_MANIFESTS/evidence/results.tar" -C "$SMOKE_MANIFESTS/evidence"
cat "$SMOKE_MANIFESTS/evidence/results/summary.json"
```

Verify `summary.json`, its `resources.status` and `client-report.json` all say
`SUCCESS`, with the expected operation counts and baseline/final resource samples.
Keep the evidence directory. Delete the namespace only when results
are saved; its PVC may be deleted with it. The exporter exits after 24 hours.
Filesystem artifacts are disposable. S3 artifacts remain under the unique run
prefix; keep the returned descriptors for explicit cleanup.

## Qwen GPU checkpoint and repeated restore

`e2e/tests/test_pagebroker_qwen.py` adds two opt-in cases, using the existing vLLM
guide's pinned image, program and manifest builders. Run Qwen3-0.6B first, then
Qwen3-8B, without pytest parallelization. Each case:

1. Starts a source pod with the selected model and waits for its warm-up inference
   and checkpoint-ready sentinel.
2. Checks the source node's installed agent/PageBroker and requires PVC storage,
   `pageBroker.restoreMode=staged`. Filesystem transfers use POSIX.
3. Creates one PodSnapshot, waits for its content to become ready, and deletes the
   source pod before restoring, freeing its GPU allocation.
4. Restores twice from that same PodSnapshot, deleting and waiting for each restore
   pod before creating the next. Pod UIDs must differ. Each restored process must
   report success, become ready, and serve two nonempty inference responses.
5. Rejects a cold-starting placeholder, changed workload image, workload restart,
   or replacement/restart of the agent or PageBroker during the case.

Restore containers follow the framework helper's proxy selection: the restore
proxy replaces the application command, or the container idles when the proxy is
disabled. The source receives the model through the guide's JSON application
arguments. Each case gets its own app ConfigMap, cleaned
up after the pods and snapshot, so it does not replace another vLLM workload's
ConfigMap. The generic framework tests and guides retain their existing defaults.

These GPU tests require the complete Snapshot agent/operator/PageBroker release,
checkpoint PVC, NVIDIA runtime and guide seccomp profile already installed. They
use the namespace containing that release and PVC, not the standalone synthetic
Job namespace. Select the desired cluster with `KUBECONFIG`. During deployment,
set these Helm values and wait for the agent rollout:

```yaml
storage:
  type: pvc
pageBroker:
  restoreMode: staged
config:
  checkpoint:
    checkpointTimeoutSeconds: 600
  restore:
    restoreTimeoutSeconds: 600
```

The tests read the mounted agent configuration and reject a file modified after
the agent started. Changing a ConfigMap without rolling out the agent is therefore
insufficient. They do not modify Helm values or restart the installed release.

Allow enough GPU memory, node RAM, PageBroker tmpfs and checkpoint-PVC capacity for
Qwen3-8B. The vLLM GPU-memory fractions default to 0.30 for 0.6B and 0.80 for 8B;
`SNAPSHOT_E2E_QWEN_GPU_MEMORY_UTILIZATION` can override both. Model loading can
download weights, or use the existing `SNAPSHOT_E2E_MODEL_CACHE_SERVER` / `_PATH`
NFS cache settings; an offline cache must contain both models. The existing
`SNAPSHOT_E2E_FRAMEWORK_IMAGE` override also applies. No new workload image build
is required when using the guide image.

After deployment, run from the repository root (set the workload-image variable
to the agent image already deployed):

```bash
set -o pipefail
export SNAPSHOT_E2E_MODE=direct
export SNAPSHOT_E2E_TEST_NAMESPACE=snapshot
export SNAPSHOT_E2E_SNAPSHOT_RELEASE=snapshot
export SNAPSHOT_E2E_PVC_NAME=snapshot-pvc
export SNAPSHOT_E2E_WORKLOAD_IMAGE="local/agent:$SNAPSHOT_VALIDATION_TAG"
export SNAPSHOT_E2E_PAGEBROKER_QWEN=1
export SNAPSHOT_E2E_REQUIRED_TESTS=test_pagebroker_qwen_repeated_restore
export SNAPSHOT_E2E_BENCHMARK_DIR="$(mktemp -d /tmp/pagebroker-qwen.XXXXXX)"

uv run --locked --project e2e pytest e2e/tests/test_pagebroker_qwen.py -vv -s \
  --junitxml="$SNAPSHOT_E2E_BENCHMARK_DIR/junit.xml" \
  2>&1 | tee "$SNAPSHOT_E2E_BENCHMARK_DIR/pytest.log"
```

Expect **2 passed**, one JSON benchmark report per model with `outcome: passed`,
`completedRestores: 2`, and both `restore.N.inference_verified` events. The required
test guard rejects skipped or deselected model cases. The operation budgets are
15 minutes for source readiness, ten minutes for checkpoint, and ten minutes per
restore, plus readiness, inference and deletion waits. Failures print the existing
framework diagnostics before fixture cleanup.

The Qwen reports use the existing benchmark format, separate from the synthetic
Job's `summary.json`. The in-Job resource sampler monitors only that Job's daemon;
it does not attach to the installed PageBroker during Qwen runs. Qwen checks daemon
continuity, but does not yet collect its resource time series. The complete runner
above automates these cases; their first execution on AWS k3s remains pending.

## Development checks and standalone client

Run the launcher and manifest tests from the repository root:

```bash
uv run --locked --project e2e python -m unittest discover \
  -s agent/pagebroker/tests/k3s -p 'test_*.py' -v

uv run --locked --project e2e pytest e2e/tests/test_pagebroker_qwen_helpers.py -q
```

The Go client can also be compiled independently:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -c ./agent/internal/pagebroker \
  -o /tmp/pagebroker-smoke.test
```

The daemon and client must see the socket and staging volumes at identical
absolute paths and use the same UID. Use a dedicated daemon with
`--custom-storage-engine off` and invoke the test with
`-test.run='^TestPageBrokerK3sSmoke$' -test.v -test.timeout=25m`.

| Environment variable | Meaning |
| --- | --- |
| `PAGEBROKER_SMOKE_MODE` | `filesystem` or `s3`; unset skips the opt-in test |
| `PAGEBROKER_SMOKE_SOCKET` | Absolute path to the daemon's Unix socket |
| `PAGEBROKER_SMOKE_STAGING_ROOT` | Shared staging root passed to the daemon |
| `PAGEBROKER_SMOKE_STORAGE_ROOT` | Existing shared filesystem root; required for filesystem mode |
| `PAGEBROKER_SMOKE_STORE_ID` | Store ID matching the daemon's S3 configuration; required for S3 mode |
| `PAGEBROKER_SMOKE_REPORT` | New absolute report path in an existing results directory |

The standalone Go client does not shut down PageBroker; the packaged Python
launcher owns that lifecycle. Its image requires Linux/amd64 with pidfd support.
