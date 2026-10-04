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
    - [Story 1](#story-1)
    - [Story 2](#story-2)
  - [Limitations, Risks, and Mitigations](#limitations-risks-and-mitigations)
- [Design Details](#design-details)
  - [Recovery tiers](#recovery-tiers)
  - [Deciding the tier](#deciding-the-tier)
    - [Why not classify on the CRIU error](#why-not-classify-on-the-criu-error)
  - [Attempt lifecycle](#attempt-lifecycle)
  - [Failures that are never retried](#failures-that-are-never-retried)
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
    - [Remaining end-to-end coverage](#remaining-end-to-end-coverage)
  - [Graduation Criteria](#graduation-criteria)
- [Alternatives](#alternatives)
- [Appendix](#appendix)
<!-- /toc -->

## Summary

A checkpoint capture that fails for a transient reason is retried in place,
against the workload that is already running, instead of failing the whole
capture on the first error. Each failure is matched to the cheapest recovery
that can actually resolve it: restarting the attempt when the workload was
never touched, reviving a suspended workload when the capture had already begun,
or republishing data that was captured successfully but not stored. How many
times to retry is configurable cluster-wide and per workload, with attempts
spaced by exponential backoff, and failures that no retry can fix — a
misconfigured capture, an exhausted store, a workload that exited on its own —
still fail immediately rather than using up retries. The workload is never
restarted to retry a capture, so no model is ever reloaded on the retry path.

## Motivation

A capture is expensive to reach. By the time the node agent dumps a container,
the workload has pulled its image, loaded its model into GPU memory, and passed
its readiness probe — minutes of work holding GPUs the whole time. Today a
single transient error anywhere in that capture throws all of it away: the
`PodSnapshotContent` is marked `Failed`, the failure is sticky, the source
process is killed, and nothing retries. The caller has to notice and retrigger
the capture by hand, paying the full setup cost again.

The errors that trigger this are frequently not deterministic. PageBroker
staging can be momentarily exhausted by a concurrent capture on the same node
(#198). A CRIU dump can fail for reasons that do not recur. The overlay diff
step can fail mid-capture (#142). Storage I/O can fail transiently. In each of
these cases a second attempt against the same still-running workload would very
likely succeed, and would cost seconds rather than minutes.

There is a second, sharper problem in the same area. The node agent is a single
process per node serving every capture *and* every restore on that node, and it
currently has no panic recovery anywhere: the capture runs in a bare goroutine,
so one malformed work order can terminate the agent and take every unrelated
in-flight capture and restore on the node with it. A retry mechanism that adds
attempts and new recovery logic to that path without first making it survivable
would multiply the exposure rather than reduce it.

This proposal is the capture-side counterpart of the restore-side work in #244
(fallback to cold start) and #247 (restore retry). It implements the retry half
of #319.

### Goals

1. A capture that fails for a transient, retryable cause succeeds on a
   subsequent attempt without manual intervention and without restarting the
   source workload.
2. Each failure is recovered by the cheapest mechanism that can resolve it,
   decided from the observed state of the system rather than from the text of an
   error.
3. A capture that cannot be recovered fails immediately, without using up its
   retries, and says why.
4. Retry limits are configurable, with a cluster-wide default that an individual
   workload can override, and attempts are spaced by exponential backoff up to a
   bounded delay.
5. An operator can tell from the Kubernetes API why a capture failed and whether
   it was retried: each attempt reports its own cause, a capture that used up
   its retries is distinguishable from one that failed on the first try, and a
   failure that needs operator action — such as a full checkpoint store — is
   reported differently from one caused by the workload.
6. A single capture request cannot terminate the node agent — neither by a panic
   nor by exhausting the agent's memory.

### Non-Goals

- **Restarting the workload to retry a capture.** Every mechanism here operates
  on the already-running source pod. A capture whose source cannot be recovered
  fails; it is not re-run from a fresh pod.
- **Recovering a capture whose agent died.** If the node agent restarts
  mid-capture there is no attempt left to resume, and the work order fails
  through the existing paths. Preventing the agent from dying is in scope;
  resuming across its death is not.
- **Signalling the workload that no checkpoint will arrive.** This is the other
  half of #319 and a change to the workload contract; it is deliberately left to
  its own proposal.
- **Retrying or falling back on the restore path** (#244, #247). The knob naming
  here should stay consistent with #247, but the mechanisms are separate.
- **Retrying on a different node.** In-place retry never leaves the node, so a
  failure that is deterministic for this node's kernel, driver, or CRIU build is
  terminal here.
- **Making retry limits dynamic.** The limits are read when a capture begins;
  changing them does not affect a capture already in flight.

## Proposal

When a capture attempt fails, the node agent determines what state the source
workload is actually in and takes the cheapest recovery that fits:

- **The workload was never touched.** The failure happened while preparing the
  capture — resolving the container, discovering GPUs, staging the destination,
  writing the manifest. The workload is still running and ready. The agent
  discards the attempt and starts another.
- **The workload is suspended mid-capture.** The failure happened after the
  agent had begun checkpointing GPU state or dumping the process. The workload
  is still alive but suspended. The agent returns it to a running state and then
  starts another attempt.
- **The capture succeeded but was not stored.** The dump completed — which by
  design terminates the source process — but publishing it to storage failed.
  There is no workload left to recapture, and none is needed: the captured data
  is still staged, so the agent republishes it.

If none of these applies, or if the workload turns out not to be recoverable,
the capture fails immediately with a reason naming the cause. Retries continue
only while the configured limits allow, and a capture that uses up its retries
reports that distinctly from one that failed on the first try.

### How a suspended workload is recovered

The second case is the one that needs explaining, because a capture that has
already started looks irreversible. It is not, and the mechanism is one Snapshot
already relies on.

Checkpointing a GPU workload is a state machine, not a one-way operation. The
agent moves the workload from `running` to `locked` (new CUDA work is held) to
`checkpointed` (GPU memory is evicted to host memory), and only then does CRIU
dump the process. Each of those transitions has an inverse: `restore` brings GPU
memory back and `unlock` releases the held work, returning the workload to
`running`. Snapshot already performs exactly this pair on the restore path to
bring a restored workload back to life. Recovering a half-captured workload is
the same pair applied to a workload that never left the node, which also means
it needs no GPU remapping — the workload is being handed back the GPUs it still
holds.

CRIU's own behaviour completes the picture. A dump that fails does not leave the
process frozen: CRIU unfreezes and unseizes the task tree as part of failing, so
the process is still there to be recovered. A dump that *succeeds* is the case
that terminates the workload, by design.

Both halves were measured before this proposal was written; the results and
their limits are in [Test Plan](#test-plan). The agent never assumes either of
them at runtime — it checks the workload's actual state before and after
recovering it, and gives up if what it finds does not match.

Two classes of failure never use up a retry, because retrying them cannot help
and costs a workload its time either way: deterministic configuration and
identity errors, and resource conditions that require an operator — a full
checkpoint store, or a workload whose checkpoint image is too large to fit in
the agent's memory budget.

Retry happens entirely within the node agent. Apart from the optional policy a
workload may set, a capture that is retrying is simply a capture that has not
finished yet — a state the `PodSnapshot` and `SnapshotJob` controllers already
handle. No existing field changes meaning, no spec becomes mutable, and no
terminal condition is reinterpreted.

### User Stories

#### Story 1

A platform team captures a large inference workload. Another capture on the same
node is committing at that moment and is holding the node's staging memory, so
the PageBroker declines to stage this one. Today the capture fails and the team
re-runs a job that takes several minutes to reach readiness again. With this
proposal the agent waits briefly, retries against the still-running workload,
and the capture succeeds seconds later. The team sees a warning event recording
the transient failure and a successful capture.

#### Story 2

An operator runs a workload whose checkpoint image would exceed the node agent's
memory budget. Today the agent is OOM-killed partway through writing the image,
which also kills every other capture and restore in flight on that node. With
this proposal the agent detects before staging that the image cannot fit, fails
that one capture immediately with a reason naming the limit, and keeps serving
everything else on the node.

### Limitations, Risks, and Mitigations

**Reviving a suspended workload reverses a documented invariant.** The capture
path today states that a CUDA-locked source is never resumed, and that a failed
CUDA checkpoint must terminate the workload. This proposal narrows that to:
never resumed *unless the agent has verified it can be*.

The danger is a workload that comes back only partly and is then captured
anyway, producing an artifact that restores into a broken process. The
mitigation is that the agent never infers the workload's condition — it reads
it. Before recovering, it checks that the process tree is still alive and asks
each CUDA process what state it is actually in; after recovering, it checks that
every one of them reports `running` again. A workload that does not is
terminated, exactly as it is today. So a wrong assumption about what CRIU or the
CUDA helper left behind costs a failed capture, which is the current outcome
anyway, rather than a corrupt artifact.

**CRIU's behaviour on the failure path is not a stable contract.** The design
depends on a failed dump leaving the task tree alive. This was measured and
holds (see [Test Plan](#test-plan)), but the agent builds CRIU from a moving
upstream branch, so it could change in a later image without any version change
in this repository. Mitigated by the same state check: the agent treats an
unrecoverable tree as terminal rather than trusting the measurement, so a
regression in CRIU degrades retry rather than breaking capture.

**Retry extends how long a workload holds its GPUs.** A capture that retries
occupies the source pod, its GPUs, and a node's staging memory for longer than a
capture that fails immediately. Mitigated by the configured retry limits — a
cluster-wide default that a workload can override — and by exponential backoff
with a bounded maximum delay, so repeated attempts spread out rather than
hammering a resource that is already under pressure. Failures that need operator
action are never retried at all, and the default limit of zero means no capture
holds anything longer than it does today until someone opts in.

**Retrying into the same destination is more constrained than retrying into a
fresh one.** Because the capture is retried in place, every attempt targets the
same artifact destination and must fully release the previous attempt's
transaction and staging before the next one begins. Mitigated by making release
an explicit, confirmed step rather than a best-effort one, and by preserving the
invariant that an artifact path stays empty until a capture has genuinely
committed.

**Recovery logic runs against a live, privileged workload.** Revival manipulates
a running process tree. This is the same capability the agent already exercises
on both the capture and restore paths, so it grants no new privilege, but it is
new code operating on a live tenant workload. Mitigated by the probe, by
terminating rather than guessing when state is ambiguous, and by the panic
recovery described below, so a defect in this logic cannot escalate from one
failed capture to a node-wide outage.

**Accepted:** a capture lost to an agent restart is not resumed, and its
PageBroker staging is reclaimed by the broker's own transaction expiry rather
than promptly. Reclaiming it sooner is a PageBroker concern (#237).

## Design Details

Retry is implemented entirely within the node agent, inside the handling of one
`PodSnapshotContent`, under the in-flight guard and capture lease the agent
already holds for that work order. No controller above the agent participates.

### Recovery tiers

Three recoveries exist, distinguished by what state the source is left in. The
boundaries are set by two existing facts: CRIU dumps with `LeaveRunning: false`,
so a *successful* dump terminates the source; and CUDA state is suspended before
the dump, so a failure between those two points leaves the source alive but not
runnable.

| Tier | Failure occurred | Source state | Recovery | Transaction |
| --- | --- | --- | --- | --- |
| A | before CUDA checkpoint | running, untouched | discard, re-attempt | new |
| B | CUDA checkpoint through CRIU dump | alive, suspended | revive, re-attempt | new |
| C | after a successful dump | terminated | republish only | **reused** |

Tier C deserves emphasis because it is easy to misread as unrecoverable. A
commit failure is a failed *publish*, not a failed capture: the image is staged
and the protocol makes a repeated commit idempotent, so republishing needs no
live source at all.

### Deciding the tier

The tier is decided from observed state, never from the text of an error. Three
inputs, in increasing authority:

1. **Which step failed.** The agent controls the call sequence, so the failing
   step is known exactly and yields a candidate tier at no cost.
2. **How far CRIU progressed**, from a CRIU notification handler. A *hint* only,
   and a weak one — see below.
3. **A probe of the source** — process-tree liveness, and per-CUDA-process state
   from the existing `--get-state` helper action. **The probe decides.** A
   source it cannot account for is terminal regardless of what the first two
   inputs suggested.

| Failing step | Candidate | Probe: running | Probe: suspended | Probe: gone |
| --- | --- | --- | --- | --- |
| before CUDA checkpoint | A | re-attempt | revive, re-attempt | terminal |
| CUDA checkpoint | B | re-attempt | revive, re-attempt | terminal |
| CRIU dump | A or B | re-attempt | revive, re-attempt | terminal |
| commit, transaction live | C | republish | republish | republish |
| commit, transaction gone | — | terminal | terminal | terminal |

Measurement narrowed the notification's role (see [Test Plan](#test-plan)). The
dump-side phases CRIU actually runs are `pre-dump` and `post-dump`, and a
failure after the tree is frozen produces the *same* phase sequence as a
success, while a failure before the freeze produces only `pre-dump`. So the
notification can suggest that CRIU got far enough to touch the tree, but it
cannot be relied on to separate a frozen tree from an untouched one in general
— a failure between the freeze and `post-dump` is indistinguishable from an
early one by phase alone.

This is why the probe is the decision input and the failing step only narrows
what to look for. In practice the distinction between Tier A and Tier B
collapses into a single question the probe answers directly — *is this workload
running, suspended, or gone?* — and the answer is read from the workload, not
inferred from how the attempt failed. Tiers remain a useful description of the
three recoveries; they are not a classification the agent has to get right in
advance.

#### Why not classify on the CRIU error

Classifying a dump failure by its error would be version-sensitive in a way this
repository cannot absorb. The agent builds CRIU from source at a **moving
upstream branch**, so error strings and error numbers can change between two
image builds with no version change anywhere in the tree. The Go CRIU binding
also flattens the structured error into a formatted string and does not export
the call that would return the structured response, so classification would mean
matching text produced by a moving branch.

The inputs used instead are stable. The workload's own state is read from the
CUDA helper and `/proc`, neither of which is a CRIU diagnostic. CRIU's
notification callbacks are part of its RPC contract rather than its error text,
and the restore path in this repository already registers a handler, so the dump
path follows an established pattern rather than introducing one. The binding
also exposes a CRIU version query, which the agent logs per capture — with a
moving CRIU reference, "which CRIU produced this failure" is otherwise
unanswerable from a bug report.

### Attempt lifecycle

Each attempt is a self-contained function invoked from a loop, so that an
attempt's cleanup — staging removal, transaction release — completes at the end
of *that attempt* rather than accumulating until the capture finishes.

For each attempt the agent: prepares a destination transaction and staging;
inspects the container; checkpoints CUDA state; dumps with CRIU; and commits.
On failure it probes, selects a tier, performs the tier's recovery, and either
re-attempts or fails terminally.

Three properties are load-bearing:

- **Tier C must not release the transaction.** The deferred release that runs on
  other failure paths would destroy the staged image that makes Tier C
  recoverable. Release is conditional on the selected tier.
- **Tiers A and B must confirm release before re-preparing.** The destination is
  the same on every attempt, so a new preparation issued while the previous
  transaction is still live conflicts on that destination, and the previous
  staging continues to occupy node memory. Release is a confirmed step: the
  agent proceeds only once the previous transaction is terminal and its
  transfers have drained.
- **The lease is re-checked between attempts.** A lost lease means another
  holder may own the work order; retrying under one is terminal.

### Failures that are never retried

Beyond a denying probe, which is itself the primary rule:

- **Identity and configuration errors** — a stale pod reference, a conflicting
  content binding, a missing content UID, an invalid target container or
  destination. Deterministic; a further attempt reproduces them.
- **A lost or cancelled lease**, for the reason above.
- **An exceeded active deadline**, which is the user's own bound on the capture's
  lifetime.
- **A workload that failed on its own** — the target container exiting non-zero
  before the capture.
- **Environment incompatibility** — an unsupported kernel or CRIU feature, a
  missing seccomp profile, a driver the CUDA helper rejects. Deterministic for
  this node, and in-place retry never leaves the node.
- **Destination store exhaustion**, which needs operator action. Reported
  distinctly from staging pressure.
- **An image too large for the agent's memory budget**, described below.
- **PageBroker rejections that a retry cannot change** — a malformed request, a
  destination conflict, or a transaction that no longer exists when committing.

Transient PageBroker conditions — staging declined under contention, storage
errors, internal errors — are retried with backoff. The protocol explicitly
sanctions releasing and retrying with a new transaction for the latter two.

### Protecting the node agent

Two distinct ways a single capture can currently terminate the agent, and the
agent serves every capture and restore on its node.

**Panics.** There is no panic recovery in the agent today, and the capture runs
in a bare goroutine, so any panic terminates the process. Each goroutine
boundary on the capture and restore paths gains a recovery that converts the
panic into a terminal failure for that one work order — killing the source as a
failed dump does, releasing the guard and lease — and lets the agent continue.
Alongside it, values read from a `PodSnapshotContent` or a pod are treated as
untrusted: index and nil guards turn a violated invariant into a terminal status
with a named reason, extending a pattern the agent already applies to the target
container count.

**Memory exhaustion.** CRIU writes the staged image from the agent's own
container, and memory-backed staging pages are charged to the writer, so a
checkpoint larger than the agent's memory limit terminates the agent. This never
surfaces as an error — the process simply dies — and no panic recovery can catch
it. The agent therefore estimates, before staging, whether the image can fit in
its remaining budget, and fails that capture immediately with a distinct reason
when it provably cannot. This is terminal, not retried: a retry would repeat the
same arithmetic and take the same risk.

The estimate is deliberately conservative. Image size is not known in advance,
but it is bounded below by the target's resident memory plus the GPU state to be
checkpointed, both of which the agent already reads before a capture begins.
Refusing a capture that might have fit costs one capture; accepting one that
does not costs every tenant on the node. A future PageBroker-reported staging
headroom (#237) would sharpen this, and is not required for it.

Note that this is distinct from the PageBroker declining to stage, which is
retryable: there the broker refuses before allocating anything and the agent is
unharmed, and the usual cause is transient contention.

### Storage cleanup

Because every attempt shares one content UID and therefore one destination,
cleanup is a correctness prerequisite and not merely a capacity concern.

- **Staging** is released per attempt, as described in the attempt lifecycle. On
  a terminal failure the work order's artifacts are reclaimed by the existing
  `PodSnapshotContent` cleanup finalizer and the operator's maintenance
  workqueue; no new reclamation path is introduced.
- **The artifact destination must remain empty until a capture has genuinely
  committed.** The agent treats an existing artifact directory with a matching
  manifest as a committed capture and publishes it as ready. Same-destination
  retry is safe only while that invariant holds, so publication must be atomic
  or fully reverted on release. This is a requirement on the storage path and is
  verified by the test plan rather than assumed.

### API

Retry limits are configurable cluster-wide and per workload, so two public
surfaces change. Neither alters existing semantics: no spec becomes mutable, no
pinned identity changes, and no terminal condition changes meaning. A retrying
capture still presents as a capture that has not completed, which `PodSnapshot`
and `SnapshotJob` already handle.

**Cluster-wide default** — new retry settings in the agent configuration,
rendered by the Helm chart. See [Configuration](#configuration).

**Per-workload override** — `SnapshotJob.spec` gains an optional retry policy.
Being optional and additive, it does not affect existing objects, and an absent
value means "use the cluster default" rather than "zero", so the two settings
compose instead of one silently shadowing the other.

The agent, however, must also serve captures driven by a `PodSnapshot` created
directly against an existing pod, where no `SnapshotJob` exists. To keep one
read path in the agent, the per-workload value is carried on the **source pod**
as an annotation, and `SnapshotJob` stamps its spec field onto the pod template
it creates. The agent reads the annotation and never needs to know which object
started the capture.

An unparseable or out-of-range annotation is a capture-time validation failure
with a named reason, not a silent fall back to the default: a workload that
asked for a specific policy and did not get it should say so.

Two details for review, since they are choices rather than consequences: whether
the override belongs on `SnapshotJob.spec` directly or inside the existing
`podSnapshotTemplate`, and whether an operator should be able to cap what a
workload may request — a tenant raising its own retry limit also extends how
long it holds GPUs, which [Security](#security) returns to.

### Security

The proposal introduces no new credentials, network surface, storage location,
or privilege. Revival manipulates a running process tree using the same
capabilities the agent already exercises on the capture and restore paths.
Artifact paths, ownership, and cleanup are unchanged.

Its main security effect is an improvement to **tenant isolation**. The node
agent is shared by every workload on its node, and today a single capture
request can terminate it in two ways — a panic from a malformed work order, or
memory exhaustion from an oversized checkpoint — each of which denies service to
unrelated tenants' captures and restores on that node. Both are closed here, so
the blast radius of one tenant's bad request becomes one failed capture.

Two smaller considerations:

- **Failure detail reaching the API.** Panic recovery and failure classification
  produce diagnostics that could include internal paths or stack traces.
  Conditions and events carry a bounded, descriptive message; full traces stay
  in agent logs, which are already operator-scoped.
- **Resource holding, and who may extend it.** Retry extends how long a capture
  occupies GPUs and node staging memory. Because the limit is settable per
  workload, a tenant can lengthen its own hold on shared resources — a modest
  but real self-service resource decision. Exponential backoff with a bounded
  maximum delay limits the rate, and the open question of whether operators
  should be able to cap the per-workload value is raised in [API](#api). An
  operator who wants no tenant control can leave the annotation unset by policy
  and rely on the cluster default alone.

### Configuration

The retry policy has three settings, applied identically whether they come from
the cluster default or a per-workload override:

- a **retry limit** — how many further attempts may follow the first;
- an **initial backoff**, the delay before the first retry;
- a **maximum backoff**, the ceiling the delay grows to.

Attempts are spaced by **exponential backoff**: the delay doubles from the
initial value and is capped at the maximum, so a capture contending for node
staging memory spreads its attempts out instead of hammering a resource already
under pressure, while a capture hitting a fast transient error still recovers
quickly. The delay ceiling, combined with the retry limit, bounds the total time
a failing capture can hold its workload and its GPUs without needing a separate
wall-clock setting — the worst case is the sum of the backoffs plus the attempts
themselves, which is derivable from the three values.

**Resolution.** The agent reads the per-workload annotation; absent or empty, it
uses the cluster default. Absent is distinct from zero: zero means the workload
explicitly wants no retries, and absent means it expressed no preference. The
effective policy is resolved once when the capture begins and is logged with the
capture, so an operator can see which policy actually applied rather than
inferring it.

**Default.** The cluster default retry limit is **0**, so retry is opt-in and an
upgrade changes no existing capture's behaviour. An operator raises it for the
cluster, or a workload opts itself in, and in both cases the choice is explicit
and attributable. This matches the default #247 states for restore retry; the
naming of all three settings should be agreed with it so the two read as a pair.

The node-agent protections are unaffected by this default. Panic recovery and
the fail-fast memory check are unconditional — they are not retry behaviour and
do not wait on anyone enabling retry.

Naming should stay consistent with whatever shape #247 lands on for restore
retry, so the two knobs read as a pair.

### Performance and Scalability

A successful capture is unaffected: the retry path adds a tier decision and a
probe only after a failure.

A failing capture becomes slower by design, bounded by the retry limit and the
backoff ceiling. The resource consequence is that the source pod and its GPUs
stay occupied for that duration — the explicit trade against re-reaching a ready
workload from scratch, which costs minutes.

Node memory is the scarce resource to watch. Staging is memory-backed and shared
by both containers in the agent pod, so a retry that failed to release its
predecessor's staging would double the demand. Release is therefore a confirmed
step, and the fail-fast size check keeps a capture that cannot fit from being
attempted at all.

### Monitoring

- **Events** on the source pod for each failed attempt, naming the tier taken
  and the cause, using the agent's existing pod-event mechanism.
- **A distinct terminal reason for exhausting the retry limit**, so that "failed
  after exhausting its retries" is distinguishable from "failed once",
  satisfying goal 5.
- **Distinct reasons separating storage conditions**: staging declined under
  contention (retried), destination store exhausted (operator action), and
  image too large for the agent's budget (operator action).
- **Structured logs** per attempt, including the CRIU version in use, which the
  moving CRIU reference makes necessary for any failure report to be actionable.

**Metrics are deliberately out of scope for the first increment.** The agent has
no metrics infrastructure today — no metrics client, endpoint, or scrape
configuration — so adding counters would mean introducing that infrastructure,
a chart change, and a port alongside the retry logic. Attempt counts, tier
outcomes, revival success rate, and exhaustion counts are all worth measuring,
and the revival success rate in particular is what will show whether Tier B
earns its complexity; they should be added as their own change.

Adding a non-terminal condition on `PodSnapshotContent` to expose attempts in
status was considered. The existing ready/failed conditions are a mutually
exclusive pair whose both-false state means in progress, so a third type would
be additive and safe, but events and logs cover the stated goal and the
condition can follow if operators ask for it.

### Dependencies

Everything the design requires exists in the tree, with two qualifications.

In the tree already: the CUDA restore-and-unlock operation used for revival,
which runs in production on the restore path and already tolerates a partially
suspended process tree; CRIU notification support, with a handler already
registered by the restore path; a CRIU version query; process-tree enumeration
for the probe; the pod-event mechanism; the injection seam used to simulate
capture failures in tests; and the artifact cleanup finalizer and maintenance
workqueue (#349, #390).

Two qualifications:

- **Typed PageBroker failure codes.** Classifying broker failures by code rather
  than text needs an accessor on the client's failure type, which #395
  introduces as part of a larger protocol change. A minimal accessor can be
  added here instead, leaving #395 to subsume it, so this proposal does not
  depend on that work landing.
- **A cluster able to host a full Snapshot install** — meaning a `ReadWriteMany`
  storage class — to close the end-to-end gaps listed in
  [Test Plan](#test-plan). The behavioural prerequisites themselves are already
  measured; what remains needs the whole system rather than its parts.

PageBroker-reported staging headroom (#237) would improve the fail-fast check
and is not required by it. This proposal assumes the PageBroker data path
(#251) rather than conflicting with it.

### Test Plan

**Unit.** The recovery decision is a pure function of failing step and probe
result, and is table-tested across every combination, including each probe
outcome for a dump failure. Classification of never-retried failures is tested
per reason. Retry bounds are tested for limit expiry and for backoff growth up
to the ceiling, and for correct reporting of exhaustion. Policy resolution is
tested for precedence, for absent versus explicit zero, and for rejection of an
invalid override. Release behaviour is tested for the
property that Tier C does not release and Tiers A and B do not re-prepare before
release is confirmed. Panic recovery is tested by panicking inside the existing
capture injection seam and asserting that the agent survives and the work order
is terminally failed — the seam exists already and needs no new harness.

**Integration.** Agent controller tests cover a capture that fails once and
succeeds on retry, a capture that exhausts its retries, and a capture failing a
never-retried classification, each asserting the resulting conditions and
events. The invariant that the artifact destination stays empty until a genuine
commit is asserted directly, since same-destination retry depends on it.

#### Measured behaviour

The two assumptions the design rests on were measured on a GPU cluster before
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
— it actively restores the tree rather than abandoning it frozen.

**A suspended CUDA workload can be returned to running.** Against a workload
holding GPU memory on a real T4:
`running → lock → locked → checkpoint → checkpointed → restore → locked →
unlock → running`. Every step succeeded, the process still held its GPU memory
afterwards, and the workload continued. **The cycle was then repeated a second
time with identical results**, which is what a retry does. The restore used an
empty device map, as the design proposes.

Three limits of that run, which is why the probe and not the measurement is the
runtime authority:

1. **A mid-failure was not produced** — one after the freeze but before
   `post-dump`. This is what demoted the CRIU notification from a decision input
   to a hint; see [Deciding the tier](#deciding-the-tier).
2. **The Go notification delivery path was not exercised.** Phases were read
   from CRIU's own log. That CRIU *runs* them is established; that the Go
   binding surfaces them to the agent is not, and remains to be covered.
3. **No CRIU dump of a GPU process** was attempted, so "a capture succeeds after
   revival" is not yet shown end to end. It needs the agent's external-mount and
   netns handling, i.e. a full Snapshot install, which that cluster could not
   host for want of a `ReadWriteMany` storage class.

#### Remaining end-to-end coverage

1. *Close the gaps above*, on a cluster that can host a full Snapshot install:
   a capture succeeding after a revival, and the notification path exercised
   through the agent rather than through CRIU's log.
2. *Behavioural coverage.* Induced transient failures for each recovery against
   the framework workloads already used by the end-to-end suite, asserting that
   the capture succeeds on retry and that the restored checkpoint is valid — a
   retried capture must produce an artifact indistinguishable from a
   first-attempt one.
3. *Policy resolution.* A per-workload override and the cluster default,
   asserting the effective policy and that an invalid override fails the capture
   with its named reason rather than silently falling back.

### Graduation Criteria

**Alpha.** Retry is available, defaulting to zero retries, configurable
cluster-wide and per workload, with
unit and integration coverage for the recoveries that do not require reviving a
suspended workload. The node-agent protections — panic recovery and the
fail-fast memory check — are in place and are unconditional, independent of
whether retry is enabled. Failure classification distinguishes retryable from
terminal causes, and storage conditions are reported distinctly.

**Beta.** Revival is implemented and enabled, and the end-to-end gaps listed in
[Test Plan](#test-plan) are closed on a cluster hosting a full Snapshot install:
a capture succeeding after a revival, and the notification path exercised
through the agent. Behavioural coverage demonstrates each recovery against the
framework workloads, and a retried capture is shown to restore correctly.
Metrics covering attempts, recovery outcomes, revival success rate, and
exhaustion are available, and operational evidence informs whether the default
retry limit should change.

**GA.** Field evidence that the default configuration is right, that revival
succeeds at a rate justifying its complexity, and that no capture has been
observed to terminate a node agent. Configuration naming is settled jointly with
restore retry (#247).

## Alternatives

**Restarting the workload to retry.** The only mechanism able to recover a
capture whose source is genuinely gone, and the natural home for it would be
`SnapshotJob`, which owns running the workload. Rejected because each retry
costs a full workload restart — for an inference workload, reloading the model
while holding its GPUs — and because it would require breaking the one-shot
identity pinning that `SnapshotJob`, `PodSnapshot`, and `PodSnapshotContent`
each deliberately enforce. The in-place tiers recover the common transient
failures at a fraction of the cost; a genuinely dead source remains terminal.

**A second `PodSnapshotContent` per attempt against the same pod.** The agent
already selects the oldest non-terminal work order for a pod, so a further work
order would be picked up, and each attempt would get a distinct artifact
destination, removing the same-destination constraints entirely. Rejected
because it only helps for failures where the source is untouched — which the
in-agent path already covers more cheaply — while requiring `PodSnapshot` to
bind more than one content and making retry visible as API churn.

**Classifying dump failures by CRIU's error.** Rejected for version sensitivity;
see [Deciding the tier](#deciding-the-tier).

**Retrying a capture whose image cannot fit the agent's memory budget.**
Rejected: the retry re-runs the same arithmetic against the same limit and
re-risks terminating the agent, so the correct response is to fail fast.

## Appendix

- #319 — the feature request this proposal implements the retry half of.
- #247, #244 — the restore-side counterparts.
- #237, #251 — PageBroker integration and its adoption as the data path.
- #349, #390 — the artifact cleanup machinery this proposal reuses.
