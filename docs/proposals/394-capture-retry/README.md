<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# SNEP-394: Retry mechanism for capture failures

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [How a suspended workload is recovered](#how-a-suspended-workload-is-recovered)
  - [User Stories](#user-stories)
    - [Story 1: a concurrent capture is holding the node's staging memory](#story-1-a-concurrent-capture-is-holding-the-nodes-staging-memory)
    - [Story 2: the workload is too large to capture on this node](#story-2-the-workload-is-too-large-to-capture-on-this-node)
    - [Story 3: the CRIU dump fails after the workload is already suspended](#story-3-the-criu-dump-fails-after-the-workload-is-already-suspended)
    - [Story 4: the dump succeeded but the store was briefly unavailable](#story-4-the-dump-succeeded-but-the-store-was-briefly-unavailable)
  - [Limitations, Risks, and Mitigations](#limitations-risks-and-mitigations)
- [Design Details](#design-details)
  - [Recovery tiers](#recovery-tiers)
  - [Deciding the tier](#deciding-the-tier)
  - [Attempt lifecycle](#attempt-lifecycle)
  - [Failures that fail immediately](#failures-that-fail-immediately)
  - [Protecting the node agent](#protecting-the-node-agent)
  - [Storage cleanup](#storage-cleanup)
  - [API](#api)
  - [Security](#security)
  - [Configuration](#configuration)
  - [Performance and Scalability](#performance-and-scalability)
  - [Monitoring](#monitoring)
  - [Dependencies](#dependencies)
  - [Test Plan](#test-plan)
    - [Measured behaviour](#measured-behaviour)
  - [Graduation Criteria](#graduation-criteria)
- [Appendix](#appendix)
<!-- /toc -->

## Summary

A checkpoint capture that fails for a transient reason is retried in place,
against the workload that is already running. Each failure is matched to the
cheapest recovery that can resolve it: restarting the attempt when the workload
was never touched, reviving a suspended workload when the capture had already
begun, or republishing data that was captured but not stored. How many times to
retry is configurable cluster-wide and per workload, with attempts spaced by
exponential backoff. Failures that a retry cannot fix fail immediately, with a
reason naming the cause.

## Motivation

A capture is expensive to reach. By the time the node agent dumps a container,
the workload has pulled its image, loaded its model into GPU memory, and passed
its readiness probe — minutes of work holding GPUs throughout. Today a single
transient error anywhere in that capture throws all of it away: the
`PodSnapshotContent` is marked `Failed`, the failure is sticky, the source
process is killed, and nothing retries. The caller has to notice and retrigger
the capture by hand, paying the full setup cost again.

The errors that trigger this are frequently not deterministic. PageBroker
staging can be momentarily exhausted by a concurrent capture on the same node
(#198). A CRIU dump can fail for reasons that do not recur. The overlay diff
step can fail mid-capture (#142). Storage I/O can fail transiently. In each
case a second attempt against the same still-running workload would very likely
succeed, and would cost seconds rather than minutes.

A second problem sits on the same path. The node agent is one process per node
serving every capture *and* every restore on that node, and it has no panic
recovery: the capture runs in a bare goroutine, so one malformed work order can
terminate the agent and take every unrelated in-flight capture and restore with
it. Adding attempts and recovery logic to that path without first making it
survivable would multiply the exposure.

This proposal is the capture-side counterpart of the restore-side work in #244
(fallback to cold start) and #247 (restore retry). It implements the retry half
of #319.

### Goals

1. A capture that fails for a transient cause succeeds on a subsequent attempt
   without manual intervention, against the source pod that is already running.
2. Each failure is recovered by the cheapest mechanism that can resolve it,
   decided from the observed state of the workload.
3. A capture that cannot be recovered fails immediately, keeping its retries,
   and says why.
4. Retry limits are configurable, with a cluster-wide default an individual
   workload can override, and attempts are spaced by exponential backoff up to
   a bounded delay.
5. An operator can tell from the Kubernetes API why a capture failed and whether
   it was retried: each attempt reports its own cause, a capture that used up
   its retries reads differently from one that failed on the first try, and a
   failure needing operator action — such as a full checkpoint store — reads
   differently from one caused by the workload.
6. A single capture request cannot terminate the node agent, by panic or by
   exhausting the agent's memory.

### Non-Goals

1. **Restarting the source pod.** No second source Job, `PodSnapshot`, or
   `PodSnapshotContent` is created. Every recovery acts on the pod already named
   by `PodSnapshotContent.spec.source.podRef`, so a capture whose source process
   is gone with nothing staged fails rather than running the workload again.
2. **Surviving a node-agent restart.** An attempt in flight when the agent dies
   is not resumed; the work order reaches a terminal failure through the
   existing content resync and capture-lease expiry, and its PageBroker staging
   is reclaimed by the broker's transaction expiry. Keeping the agent alive is
   goal 6; resuming across its death is not.
3. **Telling the workload that no checkpoint is coming.** The other half of
   #319. It changes the workload contract rather than the agent's capture path,
   and belongs in its own proposal.
4. **Restore-path retry or cold-start fallback** (#247, #244). Setting names are
   aligned with #247 so the two read as a pair, but the mechanisms are separate.

## Proposal

When a capture attempt fails, the node agent reads what state the source
workload is actually in and takes the recovery that fits:

- **The workload is running.** The failure happened while preparing the capture
  — resolving the container, discovering GPUs, staging the destination, writing
  the manifest — and left the workload untouched. The agent discards the attempt
  and starts another.
- **The workload is suspended.** The failure happened after the agent began
  checkpointing GPU state or dumping the process. The agent returns the workload
  to running, then starts another attempt.
- **The workload is gone but the data is staged.** The dump completed, which by
  design terminates the source process, and publishing it to storage failed.
  The captured data is still staged, so the agent republishes it; no live source
  is needed.

Anything else fails immediately with a reason naming the cause. Retries continue
while the configured limits allow, and a capture that uses up its retries
reports that distinctly from one that failed on the first try.

Every recovery operates on the source pod that is already running, so a retry
never reloads a model. Apart from the optional policy a workload may set, a
retrying capture presents as a capture that has not finished yet — a state the
`PodSnapshot` and `SnapshotJob` controllers already handle.

### How a suspended workload is recovered

The second case needs explaining, because a capture that has already started
looks irreversible. It is not, and the mechanism is one Snapshot already relies
on.

Checkpointing a GPU workload is a state machine. The agent moves the workload
from `running` to `locked` (new CUDA work is held) to `checkpointed` (GPU memory
is evicted to host memory), and only then does CRIU dump the process. Each
transition has an inverse: `restore` brings GPU memory back and `unlock`
releases the held work. Snapshot already performs exactly this pair on the
restore path. Recovering a half-captured workload applies it to a workload that
never left the node, which also means no GPU remapping — the workload is handed
back the GPUs it still holds.

CRIU completes the picture: a dump that fails unfreezes and unseizes the task
tree as part of failing, so the process is still there to recover. A dump that
*succeeds* is the case that terminates the workload.

Both halves were measured before this proposal was written
([Test Plan](#test-plan)).

### User Stories

#### Story 1: a concurrent capture is holding the node's staging memory

A platform team captures a vLLM workload with a `SnapshotJob`. A second capture
on the same node is committing at that moment and holds the node's memory-backed
staging, so PageBroker answers the agent's `PrepareCheckpoint` with
`INSUFFICIENT_STORAGE` — refusing before it allocates anything, leaving the
workload untouched and still serving.

The staging is large enough for this model; it is merely occupied right now.
The agent establishes that by comparing its size estimate against total staging
capacity, not against what happens to be free, so it knows the shortage will
clear when the competing capture commits.

Today this is terminal: `PodSnapshotContent` goes `Failed`, the source process
is killed, and the team re-runs the `SnapshotJob`, reloading the model into GPU
memory from scratch. With this proposal the agent probes, finds the workload
running, waits, and prepares again. The capture succeeds, having cost a wait
rather than a model load. A warning event on the source pod records the
transient failure.

#### Story 2: the workload is too large to capture on this node

A different workload's checkpoint image is bigger than the node can stage. That
shows up two ways: PageBroker answers with the same `INSUFFICIENT_STORAGE` when
the image exceeds total staging capacity, and — because CRIU writes the staged
image from the agent's own container, where memory-backed staging pages are
charged to the writer — an image exceeding the agent's memory limit OOM-kills
the agent partway through, taking every other capture and restore on that node
with it, including other tenants'.

No amount of waiting helps either case: there is no moment at which this image
fits. The agent separates them from Story 1 twice over — by comparing its size
estimate against total staging capacity rather than against what is free, and,
when the estimate was too optimistic, by where the failure lands: Story 1 is
refused before any bytes are written, while an image that cannot fit overruns
the staging `sizeLimit` mid-write
([Protecting the node agent](#protecting-the-node-agent)). Either way the
shortfall is reported as terminal and the capture fails rather than the agent,
so the operator learns to resize staging or move the workload. Retries are not spent on it, the workload is not held
while they are, and the node keeps serving everyone else.

#### Story 3: the CRIU dump fails after the workload is already suspended

The same capture gets further on a later run. The agent has locked and
checkpointed the workload's CUDA state — GPU memory is evicted to host memory —
and the CRIU dump then fails. The workload is alive but suspended, and its
`cuda-checkpoint-helper` state reads `checkpointed`.

Today this is the case that kills the workload: a failed dump means the source
is SIGKILLed and the model is gone. With this proposal the agent probes, finds
the process tree alive and the CUDA state suspended, issues `restore` and
`unlock` to return it to `running`, confirms every CUDA process reports
`running` again, and starts another attempt against the revived workload. If any
of those checks disagrees, the workload is terminated exactly as it is today.

#### Story 4: the dump succeeded but the store was briefly unavailable

A capture completes its CRIU dump — which terminates the source process by
design — and the commit to the checkpoint store fails on a transient I/O error.
The checkpoint data is staged and intact; only its publication failed.

Today the capture is lost despite the expensive part having worked, and the
workload it came from no longer exists to repeat it. With this proposal the
agent recognises that no live source is needed, keeps the transaction rather
than aborting it, and commits again. The repeated commit is idempotent by
protocol, so the capture completes from data that was already on the node.

### Limitations, Risks, and Mitigations

**Reviving a suspended workload narrows a documented invariant.** The capture
path today states that a CUDA-locked source is never resumed. This proposal
narrows that to: never resumed *unless the agent has verified it can be*. The
danger is a workload that comes back only partly and is then captured anyway,
producing an artifact that restores into a broken process. The mitigation is
that the agent reads the workload's state rather than inferring it — before
recovering, and again afterwards, requiring every CUDA process to report
`running` — and terminates a workload that does not, exactly as today. A wrong
assumption therefore costs a failed capture, the current outcome anyway, rather
than a corrupt artifact.

**CRIU's failure-path behaviour is not a stable contract.** The design depends
on a failed dump leaving the task tree alive. This was measured and holds, but
the agent builds CRIU from a moving upstream branch, so it could change in a
later image with no version change in this repository. The same state check
mitigates it: an unrecoverable tree is terminal, so a CRIU regression degrades
retry rather than breaking capture.

**Retry extends how long a workload holds its GPUs.** A capture that retries
occupies the source pod, its GPUs, and node staging memory for longer than one
that fails immediately. Bounded by the configured retry limit and, absolutely,
by the capture timeout the workload already sets: retries happen inside that
budget rather than extending it. Failures needing operator action are not
retried.

**Recovery logic runs against a live, privileged workload.** Revival manipulates
a running process tree, using the capability the agent already exercises on the
capture and restore paths, but it is new code acting on a live tenant workload.
Mitigated by the state check, by terminating when state is ambiguous, and by the
panic recovery in [Protecting the node agent](#protecting-the-node-agent), so a
defect here cannot escalate from one failed capture to a node-wide outage.

**Accepted:** a capture lost to an agent restart is not resumed, and its
PageBroker staging is reclaimed by the broker's transaction expiry rather than
promptly. Reclaiming it sooner is a PageBroker concern (#237).

## Design Details

Retry lives entirely within the node agent, inside the handling of one
`PodSnapshotContent`, under the in-flight guard and capture lease the agent
already holds for that work order. No controller above the agent participates.

### Recovery tiers

Three recoveries, distinguished by the state the source is left in. The
boundaries follow from two existing facts: CRIU dumps with `LeaveRunning: false`,
so a successful dump terminates the source; and CUDA state is suspended before
the dump, so a failure between those points leaves the source alive but not
runnable.

| Tier | Source state | Recovery | Transaction |
| --- | --- | --- | --- |
| A | running, untouched | discard, re-attempt | new |
| B | alive, suspended | revive, re-attempt | new |
| C | terminated, data staged | republish only | **reused** |

Tier C is easy to misread as unrecoverable. A commit failure is a failed
*publish*: the image is staged and the protocol makes a repeated commit
idempotent.

### Deciding the tier

The agent reads the workload's state and acts on what it finds. Two inputs:

1. **Which step failed.** The agent controls the call sequence, so the failing
   step is known exactly and says whether a staged image might exist — the only
   thing distinguishing Tier C.
2. **A probe of the source** — process-tree liveness, and per-CUDA-process state
   from the existing `--get-state` helper action. **The probe decides.**

| Failing step | Probe: running | Probe: suspended | Probe: gone |
| --- | --- | --- | --- |
| before CUDA checkpoint | re-attempt | revive, re-attempt | terminal |
| CUDA checkpoint | re-attempt | revive, re-attempt | terminal |
| CRIU dump | re-attempt | revive, re-attempt | terminal |
| commit, transaction live | republish | republish | republish |
| commit, transaction gone | terminal | terminal | terminal |

So the choice between Tier A and Tier B collapses into one question the probe
answers directly — *is this workload running, suspended, or gone?* The tiers
describe the three recoveries; they are not a classification the agent must get
right in advance.

Two inputs are enough because the probe reads the workload rather than CRIU.
Measurement confirmed that nothing in CRIU's own reporting adds to this: a dump
failure after the tree is frozen runs the same phases as a success, so how far
CRIU progressed cannot separate a frozen tree from an untouched one
([Test Plan](#test-plan)). The workload's state answers directly what CRIU's
would only hint at.

### Attempt lifecycle

Each attempt is a self-contained function invoked from a loop, so an attempt's
cleanup — staging removal, transaction release — completes at the end of *that
attempt* rather than accumulating until the capture finishes.

Per attempt the agent prepares a destination transaction and staging, inspects
the container, checkpoints CUDA state, dumps with CRIU, and commits. On failure
it probes, recovers, and either re-attempts or fails terminally.

Three properties are load-bearing:

- **Tier C retains the transaction.** The deferred release that runs on other
  failure paths would destroy the staged image that makes Tier C recoverable, so
  release is conditional on the recovery chosen.
- **Tiers A and B confirm release before re-preparing.** Every attempt targets
  the same destination, so a preparation issued while the previous transaction
  is still live conflicts on that destination and leaves the previous staging
  occupying node memory. The agent proceeds only once the previous transaction
  is terminal and its transfers have drained.
- **The lease is re-checked between attempts.** A lost lease means another
  holder may own the work order; retrying under one is terminal.

### Failures that fail immediately

A probe that cannot account for the workload is the primary rule. Alongside it:

- **Identity and configuration errors** — a stale pod reference, a conflicting
  content binding, a missing content UID, an invalid target container or
  destination. Deterministic; a further attempt reproduces them.
- **A lost or cancelled lease**, for the reason above.
- **An exceeded active deadline**, the user's own bound on the capture's
  lifetime.
- **A workload that failed on its own** — the target container exiting non-zero
  before the capture.
- **Environment incompatibility** — an unsupported kernel or CRIU feature, a
  missing seccomp profile, a driver the CUDA helper rejects. Deterministic for
  this node, and an in-place retry stays on this node.
- **Destination store exhaustion**, needing operator action, reported distinctly
  from staging pressure.
- **An image too large for the agent's memory budget**
  ([Protecting the node agent](#protecting-the-node-agent)).
- **PageBroker rejections a retry cannot change** — a malformed request, a
  destination conflict, or a transaction that no longer exists when committing.

Transient PageBroker conditions are retried with backoff: staging declined under
contention, storage errors, and internal errors. The broker declining to stage
is distinct from the agent running out of memory — the broker refuses before
allocating anything and the agent is unharmed, and the usual cause is a
concurrent capture that will release. The protocol explicitly sanctions
releasing and retrying with a new transaction for storage and internal errors.

### Protecting the node agent

Two ways a single capture can terminate the agent that serves every capture and
restore on its node.

**Panics.** The capture runs in a bare goroutine with no recovery, so any panic
terminates the process. Each goroutine boundary on the capture and restore paths
gains a recovery that converts the panic into a terminal failure for that one
work order — killing the source as a failed dump does, releasing the guard and
lease — and lets the agent continue. Alongside it, values read from a
`PodSnapshotContent` or a pod are treated as untrusted: index and nil guards
turn a violated invariant into a terminal status with a named reason, extending
a pattern the agent already applies to the target container count.

**Memory exhaustion.** CRIU writes the staged image from the agent's own
container, and memory-backed staging pages are charged to the writer, so a
checkpoint larger than the agent's memory limit terminates the agent. This
surfaces as no error at all — the process simply dies — and no panic recovery
can catch it.

The containment is a bound the kernel enforces, not a prediction. Staging is a
memory-backed `emptyDir`, which today carries no `sizeLimit` and so may grow
until the agent's cgroup kills it. Giving it a `sizeLimit` inverts that: the
tmpfs fills first and the write fails with `ENOSPC`, which PageBroker reports as
`INSUFFICIENT_STORAGE` and the agent handles as an ordinary capture failure.

The limit has to leave room, not merely match. Staging pages and the agent's own
allocations are charged to the same cgroup, so a `sizeLimit` equal to the agent's
memory limit still lets the two together reach that limit before the volume is
full — the agent is OOM-killed with the tmpfs under its cap. The invariant the
chart must hold is therefore **staging `sizeLimit` ≤ agent memory limit −
reserve**, where the reserve covers the agent's peak non-staging use. Sizing
that reserve, and testing that a staging write returns `ENOSPC` before the agent
reaches its memory limit, are part of the work rather than assumptions of it.

A pre-staging size estimate is an optimisation on top of that, not the
safeguard. Image size is not known in advance; it is only bounded *below* by the
target's resident memory plus the GPU state to be checkpointed, both of which
the agent already reads. A lower bound can prove an image will *not* fit, never
that it will, so the estimate is used in that direction only: when the bound
already exceeds staging capacity the capture fails immediately, before spending
a model's worth of time discovering it. When the bound fits, the capture
proceeds and the `sizeLimit` is what holds. PageBroker-reported staging headroom
(#237) would sharpen the estimate and is not what makes this safe.

**Telling a full node from an image that will never fit.** Both reach the agent
as `INSUFFICIENT_STORAGE`, and confusing them would spend the retry budget on an
image that cannot succeed. The failing step separates them, which the agent
already knows:

- **Refused at `PrepareCheckpoint`**, before any bytes are written. The broker
  declined to allocate staging because the node's staging is occupied. Nothing
  about this image is implicated, so it is retryable.
- **`ENOSPC` while the dump is writing.** Staging was allocated and the image
  overran the `sizeLimit`. That the write reached the cap is proof the image
  exceeds capacity — the upper bound the pre-staging estimate could not supply —
  so it is terminal, with the reason naming the measured shortfall.

The failed attempt therefore produces the evidence the estimate lacked, and no
second attempt is needed to learn it.

### Storage cleanup

Every attempt shares one content UID and therefore one destination, which makes
cleanup a correctness prerequisite rather than a capacity concern.

- **Staging** is released per attempt, as in the attempt lifecycle. On a
  terminal failure the work order's artifacts are reclaimed by the existing
  `PodSnapshotContent` cleanup finalizer and the operator's maintenance
  workqueue (#349, #390).
- **The artifact destination stays empty until a capture has genuinely
  committed.** The agent treats an existing artifact directory with a matching
  manifest as a committed capture and publishes it as ready, so same-destination
  retry is safe only while that invariant holds. Publication must therefore be
  atomic or fully reverted on release — a requirement on the storage path,
  verified by the test plan.

### API

The retry limit is configurable cluster-wide and per capture, so two surfaces
change.

**Cluster-wide default** — a retry setting in the agent configuration, rendered
by the Helm chart ([Configuration](#configuration)).

**Per-capture value** — an optional field on `PodSnapshot.spec`. `PodSnapshot`
is the object that represents a capture request, so it is where a caller
expresses how that capture should behave, and the field follows the same path
every other capture parameter already takes:

- A caller creating a `PodSnapshot` directly sets it there.
- `SnapshotJob` exposes it on `spec.podSnapshotTemplate`, alongside
  `targetContainers`, and copies it into the `PodSnapshot` it creates — the same
  propagation that template already performs.
- The `PodSnapshotReconciler` copies it into `PodSnapshotContent.spec` when it
  creates the content, as it already does for the source pod reference and
  target containers.

The agent therefore reads the value from the `PodSnapshotContent` it is already
holding: one API read it already performs, and no need to know which object
started the capture. Both specs stay immutable, since the value is set at
creation and never changes afterwards.

An absent value means "use the cluster default", so the two surfaces compose.
An out-of-range value is rejected by CRD validation at admission.

One choice for review: whether an operator should be able to cap what a caller
may request.

### Security

Revival uses capabilities the agent already exercises on the capture and restore
paths, and artifact paths, ownership, and cleanup are unchanged.

The proposal's main security effect is an improvement to **tenant isolation**. The node
agent is shared by every workload on its node, and today a single capture
request can terminate it by panic or by memory exhaustion, denying service to
unrelated tenants' captures and restores. Both are closed
([Protecting the node agent](#protecting-the-node-agent)), so the blast radius
of one tenant's bad request becomes one failed capture.

Two smaller considerations:

- **Failure detail reaching the API.** Panic recovery and failure classification
  produce diagnostics that could include internal paths or stack traces.
  Conditions and events carry a bounded, descriptive message; full traces stay
  in agent logs, which are already operator-scoped.
- **Resource holding, and who may extend it.** Because the retry limit is
  settable per capture, a caller can lengthen its own hold on shared GPUs and
  staging memory — a modest but real self-service resource decision, bounded by
  the capture timeout. Whether operators should be able to cap it is the open
  question raised in [API](#api).

### Configuration

**The retry limit is the only setting.** It is how many further attempts may
follow the first, and it is the one value an operator or a workload chooses.

Attempts are spaced by exponential backoff whose initial delay and ceiling are
**hard-coded constants**. The total time a retrying capture can consume is
already bounded by the capture timeout the workload sets, so the backoff needs
no budget of its own.

**Resolution.** The agent reads the per-capture value; absent, it uses the
cluster default. Absent is distinct from zero: zero means the workload
explicitly wants no retries, absent means it expressed no preference. The
effective limit is resolved once when the capture begins and logged with the
capture, so an operator can see which policy actually applied.

**Default.** The cluster default retry limit is **0**, so retry is opt-in and an
upgrade changes no existing capture's behaviour. An operator raises it for the
cluster, or a workload opts itself in; either way the choice is explicit and
attributable. This matches the default #247 states for restore retry, and the
setting's name should be agreed with it so the two read as a pair.

The node-agent protections are independent of this default. Panic recovery and
the fail-fast size check are unconditional.

### Performance and Scalability

A successful capture is unaffected: the probe and the recovery decision run only
after a failure.

A failing capture becomes slower by design, bounded by the retry limit and the
capture timeout, and its source pod and GPUs stay occupied for that duration —
the explicit trade against re-reaching a ready workload from scratch.

Node memory is the scarce resource. Staging is memory-backed and shared by both
containers in the agent pod, so a retry that failed to release its predecessor's
staging would double the demand; release is therefore a confirmed step, and the
size check keeps a capture that cannot fit from being attempted at all.

### Monitoring

- **Events** on the source pod for each failed attempt, naming the recovery
  taken and the cause, using the agent's existing pod-event mechanism.
- **A distinct terminal reason for exhausting the retry limit**, so "failed
  after exhausting its retries" reads differently from "failed once".
- **Distinct reasons separating storage conditions**: staging declined under
  contention (retried), destination store exhausted (operator action), and image
  too large for the agent's budget (operator action).
- **Structured logs** per attempt, including the effective retry policy and the
  CRIU version in use — with a moving CRIU reference, "which CRIU produced this
  failure" is otherwise unanswerable from a bug report.

A non-terminal condition on `PodSnapshotContent` exposing attempts in status
would be additive and safe, since the existing ready/failed conditions are a
mutually exclusive pair whose both-false state means in progress. Events and
logs cover goal 5, so the condition can follow if operators ask for it.

### Dependencies

In the tree already: the CUDA restore-and-unlock operation used for revival,
which runs in production on the restore path and already tolerates a partially
suspended process tree; a CRIU version query; process-tree enumeration for the
probe; the pod-event mechanism; the injection seam used to simulate capture
failures in tests; and the artifact cleanup machinery (#349, #390).

One qualification: **typed PageBroker failure codes.** Classifying broker
failures by code rather than text needs an accessor on the client's failure
type, which #395 introduces as part of a larger protocol change. A minimal
accessor can be added here instead, leaving #395 to subsume it, so this proposal
does not depend on that work landing.

This proposal assumes the PageBroker data path (#251).

### Test Plan

**Unit.** The recovery decision is a pure function of failing step and probe
result, table-tested across every combination. Immediate-failure classification
is tested per reason. Retry bounds are tested for limit expiry, backoff growth
to the ceiling, and correct reporting of exhaustion. Policy resolution is tested
for precedence, for absent versus explicit zero, and for rejection of an invalid
override. Release behaviour is tested for the property that Tier C retains its
transaction and that Tiers A and B re-prepare only after confirmed release.
Panic recovery is tested by panicking inside the existing capture injection seam
and asserting the agent survives with the work order terminally failed.

**Integration.** Agent controller tests cover a capture that fails once and
succeeds on retry, a capture that exhausts its retries, and a capture hitting an
immediate-failure classification, each asserting the resulting conditions and
events. The invariant that the artifact destination stays empty until a genuine
commit is asserted directly, since same-destination retry depends on it.

#### Measured behaviour

The two behaviours the design rests on were measured on a GPU cluster before
this proposal was finalised, driving the shipped agent image's own binaries
(`agent:v0.1.0`, CRIU **4.2.1**, GitID `c5ba2ab`) against a T4 node, driver
`595.71.05`, kernel `7.0.0-1013-aws`.

**A failed CRIU dump leaves the task tree alive and running.**

| Case | criu exit | phases run | froze the tree | tree afterwards |
| --- | --- | --- | --- | --- |
| success (control) | 0 | `pre-dump`, `post-dump` | yes | **killed** — matches `LeaveRunning: false` |
| failure before the freeze | 1 | `pre-dump` | no | **alive, still progressing** |
| failure after the freeze | 255 | `pre-dump`, `post-dump` | yes | **alive, still progressing** |

In the post-freeze case CRIU had seized the tree and written its images, and on
failure logged `Unfreezing tasks` and `Unseizing <pid>` before `Dumping FAILED`
— it actively restores the tree rather than abandoning it frozen. The two
failure rows also share a phase sequence with the success row, which is what
established that only the workload's own state can separate them.

**A suspended CUDA workload can be returned to running.** Against a workload
holding GPU memory on a T4:
`running → lock → locked → checkpoint → checkpointed → restore → locked →
unlock → running`. Every step succeeded, the process still held its GPU memory
afterwards, and the workload continued. **The cycle was then repeated a second
time with identical results**, which is what a retry does. The restore used an
empty device map, as the design proposes.

Two limits of that run, which is why the probe and not the measurement is the
runtime authority: a failure between the freeze and `post-dump` was not
produced, and no CRIU dump of a GPU process was attempted, so "a capture
succeeds after revival" is covered end to end rather than assumed from this.

**End-to-end.** Induced transient failures for each recovery against the
framework workloads already used by the end-to-end suite, asserting the capture
succeeds on retry and that a retried capture produces an artifact
indistinguishable from a first-attempt one — including a capture that succeeds
after reviving a suspended workload. Retry-limit resolution is covered for a
per-capture value and the cluster default.

### Graduation Criteria

**Alpha.** Retry is available, defaulting to zero retries, configurable
cluster-wide and per workload, with unit and integration coverage for the
recoveries that do not require reviving a suspended workload. The node-agent
protections are in place and unconditional. Failure classification distinguishes
retryable from immediate causes, and storage conditions are reported distinctly.

**Beta.** Revival is implemented and enabled. End-to-end coverage demonstrates
each recovery against the framework workloads, including a capture that succeeds
after a revival, and a retried capture is shown to restore correctly.

**GA.** Field evidence that the default configuration is right, that revival
succeeds at a rate justifying its complexity, and that no capture has been
observed to terminate a node agent. Configuration naming is settled jointly with
restore retry (#247).

## Appendix

- #319 — the feature request this proposal implements the retry half of.
- #247, #244 — the restore-side counterparts.
- #237, #251 — PageBroker integration and its adoption as the data path.
- #349, #390 — the artifact cleanup machinery this proposal reuses.
