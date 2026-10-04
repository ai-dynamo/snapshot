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
or republishing data that was captured successfully but not stored. Retries are
bounded by a configurable attempt count and a wall-clock limit, and failures
that no retry can fix — a misconfigured capture, an exhausted store, a workload
that exited on its own — still fail immediately rather than consuming the
budget. The workload is never restarted to retry a capture, so no model is ever
reloaded on the retry path.

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
3. A capture that cannot be recovered fails immediately, without consuming the
   retry budget, and says why.
4. Retries are bounded by both an attempt count and a wall-clock limit, so a
   failing capture cannot hold a workload and its GPUs indefinitely.
5. Every attempt, and the final exhausted state, is attributable to a specific
   cause through the Kubernetes API, with storage exhaustion distinguishable
   from other failures because it needs operator action rather than a workload
   change.
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
- **Changing any CRD.** See [API](#api).

## Proposal

When a capture attempt fails, the node agent determines what state the source
workload is actually in and takes the cheapest recovery that fits:

- **The workload was never touched.** The failure happened while preparing the
  capture — resolving the container, discovering GPUs, staging the destination,
  writing the manifest. The workload is still running and ready. The agent
  discards the attempt and starts another.
- **The workload is suspended mid-capture.** The failure happened after the
  agent had begun checkpointing GPU state or dumping the process. The workload
  is still alive but its CUDA state is suspended. The agent returns the workload
  to a running state and then starts another attempt.
- **The capture succeeded but was not stored.** The dump completed — which by
  design terminates the source process — but publishing it to storage failed.
  There is no workload left to recapture, and none is needed: the captured data
  is still staged, so the agent republishes it.

If none of these applies, or if the workload turns out not to be recoverable,
the capture fails immediately with a reason naming the cause. Retries are
attempted only while the attempt budget and the wall-clock limit both allow it,
and a capture that exhausts its budget reports that distinctly from one that
failed once.

Two classes of failure never consume the budget, because retrying them cannot
help and costs a workload its time either way: deterministic configuration and
identity errors, and resource conditions that require an operator — a full
checkpoint store, or a workload whose checkpoint image is too large to fit in
the agent's memory budget.

Retry is invisible to everything above the node agent. A capture that is
retrying is simply a capture that has not finished yet, which is a state the
`PodSnapshot` and `SnapshotJob` controllers already handle. No object's spec,
status schema, or terminal semantics change.

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
never resumed *unless the agent has verified it can be*. The mitigation is that
revival is always preceded by a probe of the actual process and CUDA state and
is verified afterwards; a workload that does not come back cleanly is terminated
exactly as it is today. The risk of a half-revived workload being checkpointed —
producing a corrupt artifact — is therefore bounded by the probe, not by an
assumption.

**The behaviour of CRIU after a failed dump is not yet verified.** The design
assumes a failed dump leaves the process tree alive and recoverable. This has
not been confirmed on a GPU node, and the CRIU build is tracked from a moving
upstream branch, so it could also change. Mitigated two ways: the probe treats
an unrecoverable tree as terminal rather than trusting the assumption, and the
validation is a prerequisite for enabling revival (see
[Test Plan](#test-plan)). Until it is validated, revival is not enabled, and
the other two recovery paths work without it.

**Retry extends how long a workload holds its GPUs.** A capture that retries
occupies the source pod, its GPUs, and a node's staging memory for longer than a
capture that fails immediately. Mitigated by bounding retries with both an
attempt count and a wall-clock limit, defaulting the attempt count to zero so
the behaviour is opt-in, and by never retrying a failure that an operator must
resolve.

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
2. **How far CRIU progressed**, from a CRIU notification handler. This is needed
   only to split a dump failure, which is the one step whose internal progress
   cannot be inferred from outside.
3. **A probe of the source** — process-tree liveness, and per-PID CUDA state
   from the existing `--get-state` helper action. The probe confirms or demotes
   the candidate, and always wins: a source the probe cannot account for is
   terminal regardless of what the first two inputs suggested.

| Failing step | Candidate | Probe confirms | Probe denies |
| --- | --- | --- | --- |
| before CUDA checkpoint | A | re-attempt | terminal |
| CUDA checkpoint | B | revive, re-attempt | terminal |
| CRIU dump, no notification fired | A | re-attempt | re-test as B |
| CRIU dump, notification fired | B | revive, re-attempt | terminal |
| commit, transaction live | C | republish | terminal |
| commit, transaction gone | — | terminal | terminal |

A dump failure with no notification is treated optimistically as Tier A, but a
denying probe demotes it to a Tier B attempt rather than straight to terminal,
covering a dump that perturbed the tree before reporting any phase.

#### Why not classify on the CRIU error

Classifying a dump failure by its error would be version-sensitive in a way this
repository cannot absorb. The agent builds CRIU from source at a **moving
upstream branch**, so error strings and error numbers can change between two
image builds with no version change anywhere in the tree. The Go CRIU binding
also flattens the structured error into a formatted string and does not export
the call that would return the structured response, so classification would mean
matching text produced by a moving branch.

Both inputs used instead are stable. CRIU's notification callbacks are part of
its RPC contract rather than its diagnostics, and the restore path in this
repository already registers a handler, so the dump path follows an established
pattern rather than introducing one. The binding also exposes a CRIU version
query, which the agent logs per capture — with a moving CRIU reference, "which
CRIU produced this failure" is otherwise unanswerable from a bug report.

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

**No CRD changes.** `PodSnapshot`, `PodSnapshotContent`, and `SnapshotJob` keep
their current schemas, their immutable specs, their pinned identities, and their
sticky terminal conditions. A retrying capture presents as a capture that has
not completed, which all three already handle.

The one public surface that changes is agent configuration, delivered through
the Helm chart: new retry settings with defaults that preserve today's
behaviour exactly. See [Configuration](#configuration).

Whether a per-workload override is worth exposing later — and if so, whether as
a `SnapshotJob` field — is deliberately deferred. The mechanism does not depend
on it, and adding one later requires no change to the agent's design.

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
- **Resource holding.** Retry extends how long a capture occupies GPUs and node
  staging memory, which an abusive or badly behaved workload could prolong. The
  attempt and wall-clock bounds cap this, and the default of zero retries means
  it is not reachable without an operator opting in.

### Configuration

New agent settings, rendered by the chart into the agent configuration:

- an **attempt budget** — additional attempts after the first, defaulting to
  **0**, which preserves current behaviour exactly and makes retry opt-in;
- a **wall-clock limit** on the total time a capture may spend across all
  attempts, which bounds how long a failing capture can hold its workload's
  GPUs even when attempts are individually slow;
- a **backoff** between attempts, so that a retry after staging contention waits
  for the competing capture rather than racing it.

Both bounds apply: a capture stops at whichever is reached first. Setting the
budget to zero disables retry and leaves every behaviour in this proposal except
the node-agent protections, which are unconditional.

Naming should stay consistent with whatever shape #247 lands on for restore
retry, so the two knobs read as a pair.

### Performance and Scalability

A successful capture is unaffected: the retry path adds a tier decision and a
probe only after a failure.

A failing capture becomes slower by design, bounded by the configured attempt
count, backoff, and wall-clock limit. The resource consequence is that the
source pod and its GPUs stay occupied for that duration — the explicit trade
against re-reaching a ready workload from scratch, which costs minutes.

Node memory is the scarce resource to watch. Staging is memory-backed and shared
by both containers in the agent pod, so a retry that failed to release its
predecessor's staging would double the demand. Release is therefore a confirmed
step, and the fail-fast size check keeps a capture that cannot fit from being
attempted at all.

### Monitoring

- **Events** on the source pod for each failed attempt, naming the tier taken
  and the cause, using the agent's existing pod-event mechanism.
- **A distinct terminal reason for budget exhaustion**, so that "failed after
  exhausting its retries" is distinguishable from "failed once", satisfying
  goal 5.
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
- **Validation of CRIU's post-failure behaviour**, required before revival is
  enabled. This is an empirical prerequisite rather than a missing component;
  see [Test Plan](#test-plan).

PageBroker-reported staging headroom (#237) would improve the fail-fast check
and is not required by it. This proposal assumes the PageBroker data path
(#251) rather than conflicting with it.

### Test Plan

**Unit.** The tier decision table is a pure function of failing step,
notification state, and probe result, and is table-tested across every row,
including the demotion path. Classification of never-retried failures is tested
per reason. Attempt bounds are tested for both budget and wall-clock expiry, and
for correct reporting of exhaustion. Release behaviour is tested for the
property that Tier C does not release and Tiers A and B do not re-prepare before
release is confirmed. Panic recovery is tested by panicking inside the existing
capture injection seam and asserting that the agent survives and the work order
is terminally failed — the seam exists already and needs no new harness.

**Integration.** Agent controller tests cover a capture that fails once and
succeeds on retry, a capture that exhausts its budget, and a capture failing a
never-retried classification, each asserting the resulting conditions and
events. The invariant that the artifact destination stays empty until a genuine
commit is asserted directly, since same-destination retry depends on it.

**End-to-end, on a GPU cluster.** Two groups:

1. *Prerequisite validation for revival.* Induce CRIU dump failures against a
   real GPU workload and establish what is actually left behind: whether the
   process tree survives, what each CUDA process reports, which CRIU
   notifications fired, and whether restore-and-unlock returns the workload to
   running such that a subsequent capture succeeds. **Revival is not enabled
   until this is established**, and the result belongs in this document rather
   than being assumed by it.
2. *Behavioural coverage.* Induced transient failures in each tier against the
   framework workloads already used by the end-to-end suite, asserting that the
   capture succeeds on retry and that the restored checkpoint is valid — a
   retried capture must produce an artifact indistinguishable from a
   first-attempt one.

### Graduation Criteria

**Alpha.** Retry is available and defaults to disabled. Tiers A and C are
implemented with unit and integration coverage. The node-agent protections —
panic recovery and the fail-fast memory check — are in place and are
unconditional, independent of whether retry is enabled. Failure classification
distinguishes retryable from terminal causes, and storage conditions are
reported distinctly.

**Beta.** Revival is implemented and enabled, with the prerequisite validation
recorded in this document. End-to-end coverage demonstrates recovery in each
tier on a GPU cluster, and a retried capture is shown to restore correctly.
Metrics covering attempts, tier outcomes, revival success rate, and exhaustion
are available, and operational evidence informs whether the default attempt
budget should change.

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
