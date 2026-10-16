<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# PageBroker four hour endurance implementation plan

Implement a repository-owned runner that repeatedly checkpoints and restores
against the same PageBroker process for four measured hours. Run filesystem and
live S3 cases in separate k3s Jobs. Each Job owns its workload, monitoring,
cleanup and results, allowing execution to continue after the WSL terminal closes.

This is an implementation plan. The stress entry points and options below are
proposed interfaces; the current smoke command does not implement them.

Steps 1–4 are implemented on `dyn-4108/pagebroker-k3s-endurance`: configuration,
persistent lifecycle, readiness-only Go probe, independent supervision and bounded
resource evidence with independent validation, plus the owned-prefix S3 cleanup
helper and credential-expiry admission, durable host commands, stress rendering,
Docker packaging and CI harness checks. Fake-clock traces, local socket/pidfd
regressions and a local TLS emulator exercise the implementation. Suspended
`null` versions use direct response tests because Moto 5.1.11 cannot delete them.
In-Pod evidence remains separate from the host's Kubernetes collection verdict.
Step 5 adds `stress_local.py`, regression coverage, qualification/campaign
profiles and `STRESS_RUNBOOK.md`. The test PR's validation record identifies the
images, revisions and actual local results. Step 6 remains unimplemented; no
endurance AWS deployment or four-hour campaign has run.

## PR placement and deliverables

Keep [#622](https://github.com/ai-dynamo/snapshot/pull/622) above
[#564](https://github.com/ai-dynamo/snapshot/pull/564), whose branch is
`dyn-4108/pagebroker-k3s-smoke`, and retain the complete PageBroker stack through
#469. Keep exactly one signed-off endurance commit above that parent on
`dyn-4108/pagebroker-k3s-endurance`. Record the verified parent and #264 revisions
with each qualification; repeat affected validation when the tested source changes.

Commit the runner, supervisor, cleanup helper, configuration, image packaging,
automated tests and operator instructions together. A local wrapper may supply personal
cluster settings, but no required code may live only under `/tmp`. Keep image
references, cluster context, bucket and credentials outside committed defaults.

The existing smoke workflow remains the short correctness gate. The endurance
PR reuses its Go client and fixture without changing the PageBroker protocol,
production Helm chart, agent or operator. Record the harness revision and broker
image identity separately, since a new harness can test the already built broker.
If production changes are needed, submit them separately and test their new image.

Before opening the new PR, verify its approved issue reference. If the parent
changes during implementation, rebase this child, rerun the affected checks and
update the recorded revisions. The new test PR alone does not require rewriting
the lower stack. Preserve unrelated local changes by using an isolated worktree.

## Campaign profile

| Setting | Initial four hour profile |
| --- | --- |
| Backends | Filesystem and S3, in separate Jobs launched together |
| PageBroker instances | One per Job, retained from warm-up through cooldown |
| Clients | One active client per Job; a fresh client process per batch |
| Supervision | One independent supervisor container per Job |
| Startup limit | 300 seconds, including scheduling, readiness and helper handshakes |
| Warm-up | At least 600 seconds and five completed batches with idle windows; maximum 1,800 seconds |
| Measured interval | 14,400 seconds after warm-up and initial resource-reference collection |
| Batch cadence | At least 60 seconds between starts, with no catch-up bursts |
| Minimum idle | 30 seconds after verification and cleanup before another batch |
| Minimum measured work | 200 complete batches per backend |
| Final idle observation | 600 seconds, followed by final resource samples |
| Resource sampling | Every second, including warm-up, workload and idle periods |
| Per-batch execution limit | 300 seconds for the client and report verification; cleanup is separate |
| S3 cleanup limit | 120 seconds per batch, with bounded individual requests |
| Final batch drain limit | 420 seconds, including client completion, verification and cleanup |
| Failure detection | Pidfds polled at least once per second; runner heartbeat every second, stale after 10 seconds |
| Shutdown limit | 60 seconds after normal shutdown begins or a failure is detected |
| Kubernetes Job active deadline | 18,000 seconds |
| Result storage | 2 GiB PVC per Job; 512 MiB total exported evidence budget |
| Retry policy | No replacement Job attempt or container restart |

With batches finishing inside the cadence, the measured interval produces about
240 batches per backend. Each filesystem batch publishes three checkpoints per
engine and restores each twice: six publications and twelve restores. Each S3
batch publishes three checkpoints and restores each twice, plus metadata, Abort
and wrong-store checks. Reuse the existing 128 MiB plus 37 byte fixture and its
full path, type, permission, size and SHA-256 verification.

At 240 measured S3 batches, payload traffic is approximately 90 GiB uploaded and
180 GiB downloaded, excluding warm-up, metadata and retries. Bounded artifact
retention does not bound cumulative traffic. Record actual completed batches and
verified payload bytes; do not report these estimates as observed throughput.

Compute the next start as the later of the nominal cadence and the end of the
minimum idle interval. Stop admitting measured batches at the interval deadline,
allow one admitted batch to finish within its timeout, and report that drain
separately. Do not compensate for slow execution with extra concurrent clients.
Fewer than 200 measured batches means the requested profile was not satisfied.
Count a batch as measured only when its verification and cleanup finish inside
the measured interval. Record an overlapping final batch as drain work instead.

The nominal warm-up, measurement and cooldown take 15,600 seconds (4 hours
20 minutes). Validate the full worst-case budget before launch:

```text
startup 300 + warm-up 1,800 + measurement 14,400 + drain 420
    + cooldown 600 + shutdown 60 + margin 420 = 18,000 seconds
```

Warm-up has a hard deadline; its active work and idle windows cannot extend it.
Fail if minimum warm-up work cannot finish in time. Include readiness, baseline
collection and helper initialization in their enclosing phase budgets. A user
stop can require the remaining batch/cleanup drain followed by shutdown; do not
apply the 60-second shutdown limit to that deliberate drain.

Persist an absolute startup deadline and a conservative run expiry before Job
creation. Late-starting containers must reject expired admission, including when
the launching host has disconnected. Use monotonic timers for elapsed phases,
cap them by the remaining run budget and never reset deadlines on reconnection.
Reject profiles that cannot leave the declared shutdown margin. Keep the Job
deadline as a final fallback, not the ordinary mechanism for stopping peers.

Preflight node capacity for both Jobs, including both brokers, clients,
supervisors and the S3 cleanup helper. Launch on the selected node with the
verified imported images.
Package the reviewed images in an OCI archive using `stress_images.py`, recording
manifest digests, configuration digests and source labels separately. Require the
identity document on launch, select immutable manifest references, and verify the
node inventory and terminal runtime IDs against those identities. A matching tag
alone cannot establish reviewed content. Campaign records use schema version 2;
older records without reviewed identities cannot be adopted.
Allow an explicit single-backend selection for diagnostics; never silently omit a
requested backend and declare the complete campaign successful.

## File changes

Paths in this table are relative to `agent/pagebroker/tests/k3s/` unless stated
otherwise. New module names are intended deliverables, not existing commands.

| File | Change and purpose |
| --- | --- |
| `stress_config.py` | New validated run specification, timing budgets, backend selection and result schema versions shared by the runner and CLI. |
| `stress.py` | New in-Pod lifecycle: attach to one broker, execute batches, observe idle windows, coordinate cleanup and shut down once. |
| `stress_supervisor.py` | New independent supervisor for both backends: register peer identities, detect runner failure and own broker shutdown. |
| `stress_evidence.py` | New streaming validation and analysis of batches, process continuity, resource windows and cleanup acknowledgments. |
| `stress_orchestrate.py` | New host CLI with `start`, `status`, `collect` and `stop`; persists run ownership and does not need to remain running. |
| `s3_cleanup.py` | New S3-only helper container: preflight access and ownership, bounded artifact/version cleanup, multipart inspection and final acknowledgment. |
| `stress-requirements.txt` | Pin the cleanup helper's SDK and transitive dependencies with hashes; do not include the emulator in the runtime image. |
| `stress_images.py` | Convert reviewed Docker archives to OCI archives, verify source/configuration/layer hashes and bind runtime IDs to immutable manifest references. |
| `run.py` | Reuse peer discovery, pidfd handling and single-batch validation. Extract small shared helpers only when needed; retain smoke behavior. |
| `monitor.py` | Add optional batch/phase context and verified broker cgroup collection while preserving the existing smoke sample schema. |
| `render.py` | Add an explicit stress profile that overrides deadlines, entry points, labels, volumes, resources and results capacity. Default remains smoke. |
| `job.yaml`, `results.yaml` | Reuse as base templates; stress rendering adds its control volume, supervisor and optional S3 helper. Keep existing smoke defaults. |
| `evidence.py`, `orchestrate.py` | Parameterize shared container/status/export helpers only where necessary. Keep smoke's strict four-case verdict and file validation. |
| `Dockerfile`, `Dockerfile.dockerignore` | Package runner, supervisor, readiness probe, helper and dependencies in an explicit `stress` target. Select each Python role explicitly when rendering containers. |
| Root `Makefile` | Add `docker-build-pagebroker-stress`; make the existing smoke build select `--target runtime` explicitly so a new last stage cannot change it. |
| `test_stress*.py`, `test_s3_cleanup.py` | Add lifecycle, evidence, configuration, cleanup and orchestration regressions. |
| `.github/workflows/ci.yml` | Run the fast harness tests without AWS or a GPU, using the existing pinned Python tooling. |
| `README.md` | Document building/importing the test image, credentials, the four CLI commands, result interpretation and retained resources. |
| Repository `agent/internal/pagebroker/k3s_ready_test.go` | Add an opt-in, readiness-only invocation of the existing Go client and its report tests; perform no checkpoint or restore. |

Reuse `agent/internal/pagebroker/k3s_smoke_test.go` and its helper/fixture tests.
The Go client's three-cycle batch is already bounded and produces a verifiable
report. There is no need to turn it into an hours-long Go test or enlarge the
existing single-report limit. Add the readiness-only test to the same compiled
test binary; leave the existing smoke test's behavior and step counts unchanged.

## Persistent broker lifecycle

Use the following lifecycle inside each Job:

```text
peer registration -> protocol readiness -> staging baseline -> warm-up batches
    -> initial resource reference -> measured batches -> final idle
    -> final sample -> supervised shutdown -> terminal report

batch -> client -> report verification -> cleanup -> idle observation
```

Open the broker pidfd once through the existing Unix-socket peer check. Record
the PID and process start ticks. Keep that handle throughout the run and reject
broker exit in every phase, including idle time. A later successful socket
connection to another broker must not repair a failed continuity check.

Socket connection alone is not readiness. The daemon starts listening before
the Broker constructor creates its permanent staging directories; see
[daemon.cpp](../../daemon.cpp) and [broker.cpp](../../broker.cpp). Run the Go
readiness-only probe and require a successful `Capabilities` response before
recording the staging baseline. Require a fresh report matching this run and
socket plus successful process exit; zero exit with a missing report or skipped
probe is failure. `Capabilities` currently advertises only
`custom_storage_available`, not stores or transfer engines. Record that response;
verify backend/engine behavior through configuration and the existing batches.
Verify the socket still identifies the registered broker after the probe. Keep
the original pidfd open throughout, and bound the probe by the startup budget.

Record staging bytes/inodes on the dedicated mount only after protocol readiness
and before any fixture writes. This baseline includes the daemon's permanent
directories and remains fixed for every batch's cleanup check. It is separate
from the later memory references. S3 admission also requires the cleanup helper's
successful readiness acknowledgment.

Create a fresh numbered result directory for every client invocation. Require
a new client run ID, successful process exit and the exact existing batch report.
Record the batch number, phase, start/end times, step counts and cleanup outcome
in an append-only journal. Keep only bounded aggregates in the runner's memory.

Warm-up batches must pass the same correctness and cleanup checks as measured
batches. Record them separately and exclude them from measured throughput,
minimum batch counts and post-warm-up growth calculations.

Do not stop PageBroker after each client invocation. The existing smoke
launcher's shutdown behavior must remain intact for smoke mode; stress owns its
outer lifecycle independently. Shutdown happens once, after the final sample or
after a failed/interrupted run has preserved diagnostics.

An explicit stop request first stops new admission and allows the active batch
to finish within its remaining deadline. On failure, bound client termination
and broker shutdown, preserve partial evidence, and report any forced kill.
Do not delete S3 data while the broker may still be reading or writing it.
An early user stop is `INTERRUPTED`, never a completed endurance pass.

Define the cleanup interface in step 1, with explicit readiness, batch request,
acknowledgment, deadline and stop outcomes. Until step 3 supplies the S3 helper,
refuse S3 execution before spawning a client; a missing helper is not a successful
no-op. Keep process supervision independent of S3 SDK calls.

### Failure supervision

Run `stress_supervisor.py` in its own container and memory cgroup for both
backends. It receives no AWS credentials or Kubernetes API token. The runner,
supervisor and S3 helper share the existing UID/process namespace and a control
volume. Register run ID, PID and process-start ticks, then open pidfds and
acknowledge registration before any batch is admitted. The supervisor identifies
the broker through the Unix socket itself; reject conflicting registrations.
Both observers emit heartbeats from their control loops, including while waiting
for subprocesses or acknowledgments. A background heartbeat must not mask a
stalled control loop. Helper SDK calls stay outside these loops.

The supervisor alone owns normal broker shutdown. After the final sample, the
runner requests shutdown, and the supervisor sends SIGTERM through the registered
broker pidfd, waits up to 20 seconds and escalates to SIGKILL if necessary. Wait
up to five further seconds to confirm exit. A forced kill or unconfirmed exit
makes the run fail. The S3 helper must finish its final acknowledgment and exit.
The supervisor then writes its completion report but remains alive. The runner
validates it, writes its own terminal report and acknowledges completion. Only
then does the supervisor exit; the runner observes that expected exit and exits
itself. Each wait is bounded by the remaining shutdown budget.

On runner death or a stale heartbeat, the supervisor records the failure and
stops the registered runner/client activity, broker and helper within the
60-second shutdown budget. Use a small pre-exec gate to register active client
identity before allowing it to execute test work, so a surviving child cannot
continue issuing requests after runner failure. Issue no new S3 deletion in this
emergency path; terminate in-flight helper calls, retain the last batch and mark
remote cleanup pending or uncertain. Unexpected broker/helper death takes the
same failure path. Poll identities and deadlines during every phase.

The runner also watches the supervisor pidfd and heartbeat. If the supervisor
dies or stalls, the runner enters failure cleanup, terminates the supervisor if
needed and takes over shutdown without admitting more work. Treat supervisor
exit as expected only after the completion handshake above. A normal shutdown
request must not disable failure detection. Missing completion reports remain incomplete
evidence. Use separate report files per writer; never overwrite another process's
failure record with success.

Startup failure before registration must also stop any broker already identified
through the socket. If the broker has not exposed a socket, or both observers are
lost, the Job active deadline remains the fallback; report that limitation rather
than claiming prompt peer cleanup. Node loss likewise cannot guarantee in-Pod
cleanup. Unit and integration tests must exercise runner SIGKILL, stale heartbeat,
supervisor death, helper death and forced broker shutdown separately from the
uninterrupted campaign.

## Resource collection and interpretation

Retain RSS, PSS, private memory, anonymous/file/shared RSS, high-water RSS, swap,
open descriptors, threads and staging filesystem bytes/inodes. Tag every record
with the backend, batch number, phase, monotonic time and fixed process identity.
Keep cold-start, warm-up, measured workload, cleanup, idle and final-idle records
distinguishable. Report sampled peaks separately from kernel high-water counters.

Add broker cgroup memory usage, limits, memory breakdown and OOM event counters
where attribution can be verified without additional privileged mounts. Never
read the client's own cgroup and label it PageBroker memory. Record unsupported
extra counters explicitly; required process and staging collection failures fail
the run. Track the runner/helper container identities separately so their memory
is not included in broker trends.

Use the last ten samples of each sufficiently long idle window to calculate
median retained resources. Form an initial resource reference from the final
three valid warm-up idle windows and report changes from it descriptively.

The inspected broker intentionally retains terminal transactions for up to
3,600 seconds or 1,024 entries, with reaping every 120 seconds. These constants
are in [broker.cpp](../../broker.cpp) and [daemon.cpp](../../daemon.cpp). A
ten-minute warm-up does not establish steady retained state; a ten-minute final
idle does not empty this cache. Record the retention policy for the actual tested
broker revision. Do not disable retention or subtract estimated cache bytes to
make a resource check pass.

Keep all four measured hours, but assess sustained growth using idle windows
from measured hours 2–4. Define the later reference as the median of the first
three valid idle-window medians starting at or after measured second 3,600.
Require at least three valid windows in each of hours 2, 3 and 4. Report per-hour
distributions and slopes, plus final-idle changes from both references. Label
initial-reference growth as including cache fill; distinguish a growing-then-flat
trace from growth continuing through the later hours. The later reference is a
fixed comparison point, not a claim that unknown native caches have stabilized.

Require minimum warm-up plus the later-reference offset to cover the recorded
retention interval and reap interval (600 + 3,600 >= 3,600 + 120 here).
If a profile or the tested retention policy makes the reference too early,
revise the profile before launch or keep assessment inconclusive. Short
qualification runs lack these later windows and must report `INCONCLUSIVE`,
without claiming four-hour resource coverage. Do not silently increase warm-up:
that requires recalculating Job and credential budgets.

Require sample coverage across all four measured hours. Missing windows or
collection gaps must remain visible, rather than being filled with zeros or
silently omitted. A sampling gap over three configured
intervals makes the resource assessment incomplete unless a separately recorded
collection event explains and bounds it.

For descriptor/thread growth, record bounded diagnostic breakdowns such as
descriptor type counts and thread-name counts at idle checkpoints. Avoid dumping
file contents, process environments, complete command lines or credential paths.

Keep three separate outcomes:

- `executionStatus`: `RUNNING`, `SUCCESS`, `ERROR` or `INTERRUPTED`.
- `evidenceStatus`: `COMPLETE`, `INCOMPLETE` or `ERROR`.
- `resourceAssessment`: `INCONCLUSIVE`, `WITHIN_LIMITS` or `GROWTH_DETECTED`.

The first four-hour campaign is diagnostic for resource growth. Without an
explicit growth-limit configuration, report measured trends and `INCONCLUSIVE`;
do not invent pass thresholds from the same run being judged. A later gated run
must record limits before launch, including allowed idle memory growth and
descriptor/thread changes against the later reference, with any absolute resource
ceilings declared separately. Missing required later windows prevent
`WITHIN_LIMITS`; reported growth is evidence for investigation, not proof of a
leak. Staging bytes and inodes must return to their protocol-ready baseline
after every batch regardless of growth-limit configuration.
Known breaches set `limitsExceeded` independently of assessment completeness and
make the runner and collector return nonzero. Optional cgroup loss cannot hide a
process-metric breach. Absolute ceilings remain enforceable on short runs;
without growth limits the resource assessment stays `INCONCLUSIVE`.

## S3 cleanup and credentials

Give each S3 Job a new store ID and a unique, nonempty run prefix. Require an
empty prefix before writing the ownership marker. The marker includes the run
ID and is excluded from artifact deletion. Fail if the prefix is already owned
or unexpectedly populated. Retain the existing bucket and addressing behavior
of the tested storage configuration.

Run cleanup in a dedicated helper container. Mount AWS credentials only where
needed by PageBroker and the helper; the Go transfer client needs no credentials.
Use a separate small shared control volume for cleanup requests and replies.
Each request identifies the run and batch, and is written atomically. The client
cannot begin another batch until the helper acknowledges cleanup of that batch.

Before the first batch, require a helper readiness acknowledgment after a small
probe beneath the owned prefix verifies the required read, write, multipart and
cleanup permissions. Remove the probe and verify cleanup before admission.

The helper must:

1. Verify run ownership and wait for an explicit quiescent batch boundary.
2. Check for unexpected incomplete multipart uploads and report them as a test
   failure before attempting recovery cleanup.
3. List and delete only objects beneath the owned run prefix, preserving the
   ownership marker. Check pagination and per-object deletion errors.
4. For versioned or suspended-versioning buckets, account for object versions
   and delete markers. Fail preflight if the needed inspection/deletion access
   is unavailable; a delete marker alone is not verified storage cleanup.
5. Verify the prefix is empty of batch data and multipart uploads, then save an
   acknowledgment with object/version counts, byte counts and elapsed time.

Retain at most one failed batch for diagnosis and stop admitting work immediately
on cleanup failure. Check an initial 1 GiB retained-object budget before another
batch; report multipart state separately. Do not add a production deletion API
to PageBroker merely to support this harness.

The current runtime calls `ResolveS3Credentials` at daemon startup and passes
resolved credential values into its upload client and restore sessions. Rotating
a mounted Secret therefore does not establish credential refresh for this image.
See [daemon.cpp](../../daemon.cpp),
[s3_client.cpp](../../transfer/s3/s3_client.cpp) and
[model_streamer_transfer_engine.cpp](../../transfer/engine/model_streamer/model_streamer_transfer_engine.cpp).

For temporary credentials, `start` must obtain credential expiry metadata,
require validity through the
five-hour Job deadline plus 15 minutes, and check again immediately before S3 Job
creation. The AWS CLI credential source may export credentials directly into
memory. A shared-credentials input containing a session token also requires
expiry metadata; unknown expiry cannot pass the four-hour S3 preflight. Do not
create long-lived AWS access keys as part of preparing this test.

Create the Kubernetes Secret without persisting its values in the checkout,
result PVC, generated manifests or logs. Record only expiry and provider type.
Explicit static credentials use the `static-environment` provider, with access
and secret keys only and null expiry. The launcher and helper reject session
tokens and expiration metadata in this mode. Static credentials do not relax
run deadlines, TLS verification, cleanup ownership or evidence validation.

If available temporary credentials are too short-lived, the S3 campaign needs a
separate runtime credential-refresh change or a suitable existing credential source.
Do not substitute an emulator for requested live AWS coverage or restart the
broker to conceal expiry. Emulator testing remains a distinct validation stage.

After autonomous Job completion, `collect` removes the temporary Secret. Until
then, report this host-side cleanup as pending. No host session is required for
batch cleanup or Job completion, and no Kubernetes API token is needed in the
workload, supervisor or helper containers.

## Durable k3s execution and CLI

Provide these subcommands in the committed `stress_orchestrate.py`:

| Command | Contract |
| --- | --- |
| `start` | Validate settings, images, node capacity, S3 access/expiry and evidence budget; create both Jobs and return a durable run record after startup readiness. |
| `status` | Read Job state and bounded progress snapshots; show current phase, batch counts, last progress time and resource measurements. |
| `collect` | For terminal Jobs, export complete or partial evidence, validate it, save Kubernetes status/events/logs, remove temporary Secrets/exporters and produce the campaign report. |
| `stop` | Request a bounded cooperative stop for resources owned by this run, collect evidence and report interruption; retain result PVCs. |

All commands use explicit kubeconfig/context and a nonsecret run record. Record
resource names, UIDs and intended ownership before/after creation. Reconcile lost
create responses by checking exact ownership; never adopt a preexisting foreign
namespace. If starting one backend fails after the other starts, stop the owned
partial campaign and preserve its evidence rather than reporting a full launch.

Reuse the existing Job security settings, shared process namespace, tmpfs staging
and broker/client resource limits. Add a supervisor to both Jobs and a cleanup
helper only to S3. There are three required containers for filesystem and four
for S3; assign the extra containers their own CPU/memory limits. All must
terminate for the Job to complete. Implement the failure-supervision contract
above instead of relying on `restartPolicy: Never` to stop surviving peers.

Set `backoffLimit: 0`, `restartPolicy: Never` and a five-hour active deadline.
The in-Pod runner and supervisor enforce deadlines independently of the host.
Use a 90-second Pod termination grace period to accommodate failure shutdown.
The cooperative `stop` command uses the control channel and allows bounded drain;
Pod SIGTERM/deletion skips that drain and triggers failure/interruption cleanup.
Verify Pod UID, container identity, restart count and terminal exit state during
collection. Every required container must exit cleanly for execution success.

Use a writable results PVC plus a small control volume. Preserve the run spec and
ownership record inside the cluster so `status`/`collect` can recover after a
local terminal failure. `status` reads live files through the existing Job
container; it must not attach a second writer to the results PVC.

Collect terminal results through a read-only exporter after all Job writers have
stopped. Export nested batch reports and resource chunks with a strict manifest,
relative-path checks, link rejection and per-file/total-size limits. Preserve the
existing smoke export checks; do not make its tar extraction unrestricted.

No command upgrades the installed Snapshot release or deletes unrelated objects.
Result PVC/namespace deletion is a separate documented action after collection.

## Evidence layout and bounded storage

```text
run.json
filesystem/
  run-spec.json
  summary.json
  readiness.json
  supervisor.json
  batches.jsonl
  resources/000001.jsonl
  batches/000001/client-report.json
  batches/000001/client.log
  final-job.json
  final-pods.json
  events.json
  diagnostics.json
  container-logs/
s3/
  ...same files...
  cleanup.jsonl
summary.json
```

Write an atomic progress summary at least every ten seconds and at phase/batch
boundaries. Append raw samples and batch events as they occur. Use 16 MiB resource
chunks and a 512 MiB total exported evidence budget per Job for the initial
profile, leaving PVC headroom. Reserve 128 MiB for collection diagnostics and
2 MiB within the remaining 384 MiB for terminal/failure reports. Enforce the
ordinary-writer limit before exhausting that reserve. Bound client/helper logs
and save a diagnostic tail on failure. Exceeding a required-evidence limit must
fail explicitly, never silently discard required evidence and produce success.

PageBroker stdout/stderr is a diagnostic tail, not required complete history.
Collect up to 16 MiB per container after termination using `kubectl logs` and
record `coverage: TAIL_ONLY`, the requested cap and whether retrieval succeeded.
Kubelet rotation can remove earlier output, and the returned size cannot prove
full coverage. An unavailable tail is an explicit diagnostic gap; it does not
invalidate otherwise complete required client/resource/supervisor evidence.
Kubernetes events are also a diagnostic snapshot. Preserve authoritative final
Job/Pod status independently; it remains required for a successful verdict.
Do not add host log mounts or a broker launcher wrapper solely to capture logs.

Persist the client report before requesting S3 deletion. Record cleanup results
even on failure. The terminal summary must agree with the batch journal, sample
stream and Kubernetes status; absence of a terminal summary is incomplete
evidence, not success. Interrupted collection must be resumable into temporary
output without overwriting previously verified exports.

The final report includes broker and harness image IDs/revisions, exact JSON
configuration, source node identity, timing/pacing, warm-up and measured counts,
verified payload bytes, resource trends, sample coverage and outstanding cleanup.
Keep functional completion and the resource assessment visible as separate fields.
`collect` returns nonzero for execution failure, incomplete evidence, cleanup
failure or an exceeded configured growth limit. Diagnostic completion with no
growth limits must prominently retain `resourceAssessment: INCONCLUSIVE`.

## Implementation sequence and acceptance checks

1. **Persistent runner and configuration.** Implement `stress_config.py` and
   `stress.py`, the readiness-only Go probe and `stress_supervisor.py` with
   fake-clock lifecycle tests and real local socket/pidfd tests. Establish the
   supervision/control contract before container wiring. Demonstrate delayed
   initialization before the staging baseline, one broker across clients, correct
   cadence, unique reports, phase-budget rejection and one supervised shutdown.
   Test runner death, stale heartbeat, supervisor death and unconfirmed shutdown.
   Define the cleanup interface and reject S3 admission until step 3. No cloud run.
2. **Resource evidence.** Add phase-aware sampling, idle-window analysis,
   checkpointed progress and bounded exports. Test stable resources, retention
   cache fill followed by a plateau, cache fill with an additional ongoing leak,
   missing later comparison windows, short-profile assessment, missing/reordered
   samples, sampler failure and exhaustion of the ordinary-writer budget.
3. **S3 helper.** Implement ownership, quiescence, pagination, version handling,
   multipart inspection, bounded retries and expiry validation. Test lost replies,
   forbidden prefixes, partial deletion, helper death and retention after failure
   against the existing local TLS emulator where applicable. Wire the cleanup
   interface from step 1 and require readiness before enabling S3 admission.
   Verify emergency supervision stops peers without concurrent S3 deletion.
4. **k3s lifecycle and packaging.** Implement the four CLI commands, stress
   rendering, the image target and build-context allowlist. Wire the supervisor
   into both Jobs and validate the three/four-container contracts. Test terminal
   states, startup expiry after host disconnect, partial starts, lost API replies,
   explicit context use, rerun collisions, cooperative stop versus Pod SIGTERM,
   diagnostic-tail gaps and evidence recovery after host disconnect.
5. **Regression and local integration.** Run existing smoke/Qwen helper tests,
   new harness tests and `make check test build`. Build the test image and run
   multiple real batches against the pinned PageBroker image with local filesystem
   and TLS S3 storage. Verify clean staging and bounded retained S3 data. Prepare
   the exact source revisions, image identities, manifests and commands for
   Astra's implementation review; stop before AWS deployment.
6. **AWS qualification and four hour campaign.** After Astra's implementation
   review and the user's instruction to execute, perform **6A: short AWS
   qualification**. Import the reviewed images, run a short declared profile,
   verify controller reconnection and collect all results. Stop for Astra to
   inspect the actual qualification evidence and four-hour configuration. Only
   after that review and the user's instruction, perform **6B: four-hour run**,
   with fresh expiry/capacity checks, periodic status and final collection.

These review checkpoints are required by the user's requested workflow. Record
each review's revision, images, profile and unresolved findings. A ready review
does not itself launch anything. Relevant code/configuration/image changes after
review require affected validation and review again. Local tests in steps 1–5
remain part of implementation; they do not need a separate AWS execution gate.

The short qualification must exercise several batches, cleanup acknowledgments,
at least two idle comparison windows and final shutdown for both backends. Its
report must identify the short profile and cannot count as four-hour coverage.
Test harness interruption and deliberate failures separately from the uninterrupted
qualification/campaign; record them as harness tests, not successful endurance.

Run the existing Qwen3-0.6B and Qwen3-8B checks separately before/after the campaign
when validating GPU integration. Their installed PageBroker is a different
process from the two standalone Jobs: those checks do not add GPU-memory or
installed-daemon leak coverage to the endurance report. Continuous Qwen load,
higher concurrency and multi-day credential refresh are subsequent campaigns.

## Completion criteria

- [ ] A new PR above #564 contains runner, supervisor, probe, helper, packaging, tests and runbook.
- [ ] All necessary commands work from a clean checkout with documented tools.
- [ ] Existing smoke behavior and required four-case validation remain intact.
- [ ] Both backends run autonomously against one broker each for the full interval.
- [ ] Protocol readiness precedes the fixed staging baseline and all fixture writes.
- [ ] Both Jobs detect runner/supervisor failure and stop registered peers within the defined bounds.
- [ ] The full timing budget fits the Job deadline, including startup, drain and shutdown.
- [ ] At least 200 measured batches per backend pass all existing integrity checks.
- [ ] Staging returns to baseline and S3 retention remains bounded between batches.
- [ ] Credential validity is verified before live S3 execution.
- [ ] No broker replacement, container restart, OOM or unexpected termination occurs.
- [ ] Resource and batch evidence is complete, bounded and independently validated.
- [ ] Resource assessment accounts for terminal-transaction retention and uses later comparison windows.
- [ ] Broker log tails and event snapshots are explicitly distinguished from required complete evidence.
- [ ] Interrupted/failed runs preserve evidence and cannot report a completed pass.
- [ ] The collected report distinguishes functional success from resource assessment.
- [ ] PR validation records name the actual broker/harness revisions and run profile.
- [ ] Astra reviewed the implementation before AWS qualification and its results before the four-hour launch.
