<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# SNEP-394: Retry mechanism for capture failures

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
- [Proposal](#proposal)
  - [How a suspended workload is recovered](#how-a-suspended-workload-is-recovered)
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
    - [Remaining end-to-end coverage](#remaining-end-to-end-coverage)
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
that fails immediately. Bounded by the configured retry limit and backoff
ceiling ([Configuration](#configuration)), and failures needing operator action
are not retried.

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

The agent reads the workload's state and acts on what it finds. Two inputs
narrow the question and one answers it:

1. **Which step failed.** The agent controls the call sequence, so the failing
   step is known exactly and says whether a staged image might exist — the only
   thing distinguishing Tier C.
2. **How far CRIU progressed**, from a CRIU notification handler. A hint, useful
   for diagnostics.
3. **A probe of the source** — process-tree liveness, and per-CUDA-process state
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
right in advance. Measurement is what established this: a dump failure after the
tree is frozen runs the same CRIU phases as a success, so the notification
cannot separate a frozen tree from an untouched one, and only the workload's own
state can ([Test Plan](#test-plan)).

Reading state also keeps the decision stable across CRIU versions. The agent
builds CRIU from a moving upstream branch, and the Go binding flattens CRIU's
structured error into a formatted string without exporting the structured
response, so any error-text classification would be matching strings produced by
a branch that moves underneath it. Workload state comes from the CUDA helper and
`/proc` instead, neither of which is a CRIU diagnostic.

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
can catch it. The agent therefore estimates, before staging, whether the image
fits in its remaining budget, and fails that capture immediately with a distinct
reason when it provably cannot.

The estimate is deliberately conservative. Image size is not known in advance,
but it is bounded below by the target's resident memory plus the GPU state to be
checkpointed, both of which the agent already reads before a capture begins.
Refusing a capture that might have fit costs one capture; accepting one that
does not costs every tenant on the node. PageBroker-reported staging headroom
(#237) would sharpen this and is not required for it.

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

Retry limits are configurable cluster-wide and per workload, so two public
surfaces change. No spec becomes mutable, no pinned identity changes, and no
terminal condition changes meaning.

**Cluster-wide default** — new retry settings in the agent configuration,
rendered by the Helm chart ([Configuration](#configuration)).

**Per-workload override** — `SnapshotJob.spec` gains an optional retry policy.
Being optional and additive, it leaves existing objects unaffected, and an
absent value means "use the cluster default", so the two settings compose.

The agent also serves captures driven by a `PodSnapshot` created directly
against an existing pod, where no `SnapshotJob` exists. To keep one read path in
the agent, the per-workload value is carried on the **source pod** as an
annotation, and `SnapshotJob` stamps its spec field onto the pod template it
creates. The agent reads the annotation and never needs to know which object
started the capture.

An unparseable or out-of-range annotation is a capture-time validation failure
with a named reason: a workload that asked for a specific policy and did not get
it should say so.

Two choices for review: whether the override belongs on `SnapshotJob.spec`
directly or inside the existing `podSnapshotTemplate`, and whether an operator
should be able to cap what a workload may request.

### Security

The proposal introduces no new credentials, network surface, storage location,
or privilege. Revival uses capabilities the agent already exercises on the
capture and restore paths, and artifact paths, ownership, and cleanup are
unchanged.

Its main security effect is an improvement to **tenant isolation**. The node
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
  settable per workload, a tenant can lengthen its own hold on shared GPUs and
  staging memory — a modest but real self-service resource decision. Backoff
  limits the rate, and whether operators should be able to cap the per-workload
  value is the open question raised in [API](#api). An operator wanting no
  tenant control can leave the annotation unset by policy.

### Configuration

The retry policy has three settings, applied identically whether they come from
the cluster default or a per-workload override:

- a **retry limit** — how many further attempts may follow the first;
- an **initial backoff**, the delay before the first retry;
- a **maximum backoff**, the ceiling the delay grows to.

Attempts are spaced by **exponential backoff**: the delay doubles from the
initial value and is capped at the maximum, so a capture contending for node
staging memory spreads its attempts out while one hitting a fast transient error
still recovers quickly. The ceiling and the retry limit together bound the total
time a failing capture holds its workload — the worst case is the sum of the
backoffs plus the attempts, derivable from the three values.

**Resolution.** The agent reads the per-workload annotation; absent or empty, it
uses the cluster default. Absent is distinct from zero: zero means the workload
explicitly wants no retries, absent means it expressed no preference. The
effective policy is resolved once when the capture begins and logged with the
capture, so an operator can see which policy actually applied.

**Default.** The cluster default retry limit is **0**, so retry is opt-in and an
upgrade changes no existing capture's behaviour. An operator raises it for the
cluster, or a workload opts itself in; either way the choice is explicit and
attributable. This matches the default #247 states for restore retry, and the
naming of all three settings should be agreed with it so the two read as a pair.

The node-agent protections are independent of this default. Panic recovery and
the fail-fast memory check are unconditional.

### Performance and Scalability

A successful capture is unaffected: the probe and the recovery decision run only
after a failure.

A failing capture becomes slower by design, bounded by the retry limit and the
backoff ceiling, and its source pod and GPUs stay occupied for that duration —
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

Metrics follow as their own change. The agent has no metrics infrastructure
today — no client, endpoint, or scrape configuration — so attempt counts,
recovery outcomes, revival success rate, and exhaustion counts would each arrive
with that infrastructure, a chart change, and a port. The revival success rate
is the measure that will show whether Tier B earns its complexity.

A non-terminal condition on `PodSnapshotContent` exposing attempts in status
would be additive and safe, since the existing ready/failed conditions are a
mutually exclusive pair whose both-false state means in progress. Events and
logs cover goal 5, so the condition can follow if operators ask for it.

### Dependencies

In the tree already: the CUDA restore-and-unlock operation used for revival,
which runs in production on the restore path and already tolerates a partially
suspended process tree; CRIU notification support, with a handler already
registered by the restore path; a CRIU version query; process-tree enumeration
for the probe; the pod-event mechanism; the injection seam used to simulate
capture failures in tests; and the artifact cleanup machinery (#349, #390).

Two qualifications:

- **Typed PageBroker failure codes.** Classifying broker failures by code rather
  than text needs an accessor on the client's failure type, which #395
  introduces as part of a larger protocol change. A minimal accessor can be
  added here instead, leaving #395 to subsume it, so this proposal does not
  depend on that work landing.
- **A cluster able to host a full Snapshot install** — meaning a `ReadWriteMany`
  storage class — to close the end-to-end gaps in [Test Plan](#test-plan). The
  behavioural prerequisites are already measured; what remains needs the whole
  system rather than its parts.

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
made the workload probe the decision input rather than the notification.

**A suspended CUDA workload can be returned to running.** Against a workload
holding GPU memory on a T4:
`running → lock → locked → checkpoint → checkpointed → restore → locked →
unlock → running`. Every step succeeded, the process still held its GPU memory
afterwards, and the workload continued. **The cycle was then repeated a second
time with identical results**, which is what a retry does. The restore used an
empty device map, as the design proposes.

Three limits of that run, which is why the probe and not the measurement is the
runtime authority:

1. **A mid-failure was not produced** — one after the freeze but before
   `post-dump`.
2. **The Go notification delivery path was not exercised.** Phases were read
   from CRIU's own log, so that CRIU runs them is established; that the Go
   binding surfaces them to the agent is not.
3. **No CRIU dump of a GPU process** was attempted, so "a capture succeeds after
   revival" is not yet shown end to end. It needs the agent's external-mount and
   netns handling, i.e. a full Snapshot install, which that cluster could not
   host for want of a `ReadWriteMany` storage class.

#### Remaining end-to-end coverage

1. *Close the gaps above*, on a cluster that can host a full Snapshot install: a
   capture succeeding after a revival, and the notification path exercised
   through the agent rather than through CRIU's log.
2. *Behavioural coverage.* Induced transient failures for each recovery against
   the framework workloads already used by the end-to-end suite, asserting the
   capture succeeds on retry and the restored checkpoint is valid — a retried
   capture must produce an artifact indistinguishable from a first-attempt one.
3. *Policy resolution.* A per-workload override and the cluster default,
   asserting the effective policy and that an invalid override fails the capture
   with its named reason.

### Graduation Criteria

**Alpha.** Retry is available, defaulting to zero retries, configurable
cluster-wide and per workload, with unit and integration coverage for the
recoveries that do not require reviving a suspended workload. The node-agent
protections are in place and unconditional. Failure classification distinguishes
retryable from immediate causes, and storage conditions are reported distinctly.

**Beta.** Revival is implemented and enabled, and the end-to-end gaps are closed
on a cluster hosting a full Snapshot install. Behavioural coverage demonstrates
each recovery against the framework workloads, and a retried capture is shown to
restore correctly. Metrics covering attempts, recovery outcomes, revival success
rate, and exhaustion are available, and operational evidence informs whether the
default retry limit should change.

**GA.** Field evidence that the default configuration is right, that revival
succeeds at a rate justifying its complexity, and that no capture has been
observed to terminate a node agent. Configuration naming is settled jointly with
restore retry (#247).

## Appendix

- #319 — the feature request this proposal implements the retry half of.
- #247, #244 — the restore-side counterparts.
- #237, #251 — PageBroker integration and its adoption as the data path.
- #349, #390 — the artifact cleanup machinery this proposal reuses.
