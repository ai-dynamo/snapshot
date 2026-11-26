<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# SNEP-247: Retry restore with a limit of retries

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [User Stories](#user-stories)
    - [Story 1: a scale-up lands several replicas on one busy node](#story-1-a-scale-up-lands-several-replicas-on-one-busy-node)
    - [Story 2: teams need different limits](#story-2-teams-need-different-limits)
  - [Limitations, Risks, and Mitigations](#limitations-risks-and-mitigations)
- [Design Details](#design-details)
  - [Failure types](#failure-types)
    - [Failures that end the restore](#failures-that-end-the-restore)
    - [Failures not counted toward the budget](#failures-not-counted-toward-the-budget)
    - [Failures counted toward the budget](#failures-counted-toward-the-budget)
  - [In-place retry](#in-place-retry)
  - [Attempt failure](#attempt-failure)
  - [What nsrestore reports](#what-nsrestore-reports)
  - [Recovery decision](#recovery-decision)
  - [Scheduling the next attempt](#scheduling-the-next-attempt)
  - [Attempt record](#attempt-record)
  - [Picking up a pending restore](#picking-up-a-pending-restore)
  - [Deadline](#deadline)
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

A restore that passes the compatibility checks and then fails before it changes the container will be retried in the same container. A failure after the restore started changing the container will end the restore, as today. The retry limit will be set cluster-wide and per restore Pod, with a default of 0. Failures known to be final, including compatibility refusals, will end the restore immediately, and a restore that exhausts its retries will be reported as exhausted. All attempts will share one restore timeout, and the Pod will show each failed attempt and its cause.

## Motivation

[#247](https://github.com/ai-dynamo/snapshot/issues/247) was opened with the compatibility checks of [#231](https://github.com/ai-dynamo/snapshot/issues/231) and the cold-start fallback of [#244](https://github.com/ai-dynamo/snapshot/issues/244) to make a failed restore safe for autoscaling inference replicas, with retry before fallback. Today any restore failure is final for its Pod. A Pod under a Deployment crash-loops on its GPUs until something deletes it ([limitations](../../limitations.md)), and fallback alone would turn every failure into a cold start, even one another attempt would have restored.

Some failures are transient by design and come with load, so they peak during a scale-up. The node refuses a restore past its GPU connection limit ([daemon.cpp](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/pagebroker/daemon.cpp#L468-L470)) and drops new connections past its request cap ([daemon.cpp](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/pagebroker/daemon.cpp#L673-L676)). Storage and internal errors are meant to be retried ([pagebroker.proto](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/pagebroker/v1/pagebroker.proto#L223-L226)), but nothing retries them today.

### Goals

- **Restores succeed more often.** A failure that does not recur costs an attempt, not the Pod.
- **Retriable failures are told apart from final ones.** A failure known to be final ends the restore at once.
- **Users choose how much to retry.** Each workload sets its own limit, and one that sets none behaves as today.
- **Retries are visible.** An operator can see that a restore was retried and why.

### Non-Goals

- **Recovering elsewhere.** Every retry stays in the same Pod. Falling back to a cold start is [#244](https://github.com/ai-dynamo/snapshot/issues/244).
- **Retrying after the container changed.** A failure after the restore started changing the container ends the restore. The agent will not restart the container to get another attempt.
- **Repairing a failed restore.** Retry will not fix what made an attempt fail. It only tries again.
- **Alerting.** Retries will show as Pod events, not as metrics or alerts. The agent has no metrics endpoint, so alerting on a rising retry rate is left to the cluster's monitoring.

## Proposal

The agent will retry a failed attempt in the same container when its cause allows another attempt and the attempt left the container untouched. Every other failure ends the restore, since a final cause allows no other attempt and a container the restore started changing cannot be used again. Today every failure ends the restore.

An attempt changes the container once it starts restoring the checkpoint into it. Everything before that, such as fetching and staging the checkpoint, leaves the container untouched.

Along the restore path, each function's errors lead to one recovery. Dashed arrows are errors, and the parts in green are new.

```mermaid
%%{init: {"flowchart": {"wrappingWidth": 400}}}%%
flowchart TD
    Preflight["preflightRestore"] -->|ready| Dest["restoreDestination<br/>resolveRestoreContainerID"]
    Preflight -.->|"not ready yet:<br/>wait, no attempt spent"| Preflight
    Preflight -.->|"incompatible,<br/>invalid request"| End["End the restore"]
    Dest -.->|"before RetryAt: wait"| Dest
    Dest --> Prepare["fetchArtifactMetadata<br/>validateRestoreManifest<br/>inspectRestore<br/>prepareRestoreSource<br/>mountRestoreSource"]
    Prepare -.->|"incompatible, a mismatch,<br/>unusable checkpoint,<br/>no restore proxy"| End
    Prepare -.->|"the rest of the errors"| InPlace["Retry in place"]
    Prepare --> NsPrepare["nsrestore: RestoreInNamespace"]
    NsPrepare -.->|"every error"| InPlace
    NsPrepare -->|"sends the<br/>progress message"| NsChange["nsrestore: executeRestore"]
    NsChange -.->|"every error"| End
    NsChange --> Finish["validateRestoredProcess<br/>completeRestore"]
    Finish -.->|"every error"| End
    Finish --> Done["Restored"]

    classDef new fill:#dafbe1,stroke:#1a7f37,stroke-width:2px,color:#0f3d1f
    class InPlace new
```

The retry limit defaults to 0, can be set for the whole cluster, and can be overridden on a restore Pod. One restore timeout covers all attempts together. Each failed attempt shows as a warning event, and the Pod shows the current attempt and where and why the last one failed. The restore stays in progress while attempts remain, and a restore that runs out of retries says so.

The agent will keep each container's attempt count, deadline, and last failure in memory, and copy them to an annotation on the restore Pod when an attempt starts and when it fails. An agent that restarts or is upgraded will pick the restore up from that annotation, so an agent restart does not reset the count or the timeout. An attempt the restart cut off runs again with the same number, as an interrupted restore runs again today. With the default of 0, the agent keeps no record and adds no events or retry status, so the restore runs as it does today.

### User Stories

#### Story 1: a scale-up lands several replicas on one busy node

A platform team scales an inference service from a checkpoint, and several replicas land on the same node at once. The node is at its limit and refuses one of the restores before anything in its container has changed.

Today that replica's Pod fails and crash-loops until someone deletes it. With this proposal the agent waits and tries again, the replica restores shortly after, and the team sees a warning event for the refused attempt.

#### Story 2: teams need different limits

A cluster admin sets a default of two retries. One team validating a new checkpoint wants every failure to surface at once, and another serves a model that takes many minutes to load cold.

Today neither can retry. With this proposal the first team's restore Pods set a limit of 0 and fail on the first error, the second team's allow five retries within the same restore timeout, and every other Pod uses the cluster default.

### Limitations, Risks, and Mitigations

**CRIU and CUDA failures are not retried.** They happen after the restore started changing the container, so each ends the restore on its first attempt, even one that would not recur. The failure is marked as one that changed the container, so a later fallback can tell it apart from one no attempt can fix.

**An unrecognized error is retried.** An error while preparing that would repeat every time still spends every attempt before the restore fails.

**An agent restart can repeat an attempt or restart the timeout.** The agent copies a record to the annotation in the background, so the agent can restart before the copy lands. A restarted agent that finds the annotation one step behind runs the attempt it shows again, and one that finds no entry starts over at the first attempt with a new deadline. Each such restart costs at most one more attempt, on the node and GPUs the Pod already holds.

## Design Details

Retry will live in the node agent's handling of one destination container, `restoreDestination` ([controller.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/controller/controller.go#L953-L992)), which will open each pass by reading the container's attempt record to decide whether to start an attempt, wait, or end the restore. A failed attempt will return one error carrying what the decision needs, and one function will choose the recovery from it, leaving a retry in the record for the next pass.

### Failure types

#### Failures that end the restore

- **Compatibility refusals** from the preflight or the inspect gate.
- **An invalid restore request**: a malformed restore annotation, including the new retry limit, a destination container the Pod does not have, or a Pod that does not match its PodSnapshot. These already fail before any attempt.
- **An artifact this node cannot use**, each setting `Final` ([Attempt failure](#attempt-failure)):
  - a manifest that does not match the request ([restore.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/executor/restore.go#L265-L267))
  - a cuinterpose library whose identity differs ([restore.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/executor/restore.go#L268-L274))
  - a placeholder without the restore proxy where one is required ([restore.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/executor/restore.go#L587-L591))
  - a mismatched CustomStorage mode ([restore.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/executor/restore.go#L276-L279))
- **PageBroker failures that name the artifact or the request**: `ARTIFACT_NOT_FOUND`, `ARTIFACT_CORRUPT`, `UNSUPPORTED_ARTIFACT`, `ACCESS_DENIED`, `STORE_MISMATCH`, and `INVALID_REQUEST`.
- **A PageBroker abort that failed**, since the transaction may still be open ([restore.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/executor/restore.go#L205-L218)). It sets `Final` too.
- **The restore timeout.**
- **A failure that changed the container**, in each case listed under [In-place retry](#in-place-retry).
- **A failed `restore-complete` write or post-restore action**, since the restored process already runs.
- **The failure of the last allowed attempt**, after `RestoreRetriesExhausted`.

#### Failures not counted toward the budget

These bring the agent back to the Pod later without spending an attempt. Before the first attempt they spend no time either. After it they spend time from the deadline. The agent comes back on the Pod's next update or after 30 seconds, whichever comes first ([controller.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/controller/controller.go#L293-L298), [controller.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/controller/controller.go#L468-L474)).

- **The PodSnapshot isn't ready.** It doesn't exist yet, isn't Ready yet, or the agent can't read it ([controller.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/controller/controller.go#L604-L613), [controller.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/controller/controller.go#L630-L633)).
- **The PodSnapshotContent isn't ready.** The same three cases for the content the PodSnapshot is bound to ([controller.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/controller/controller.go#L640-L649), [controller.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/controller/controller.go#L669-L671)).
- **The checkpoint isn't on this node yet.** The agent can't inspect the checkpoint's directory, or the content doesn't list the container's published artifact yet ([controller.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/controller/controller.go#L740-L746), [controller.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/controller/controller.go#L781-L782)).
- **The Pod has no IP yet.** Only when established TCP connections are restored, which is on by default ([controller.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/controller/controller.go#L591-L593)).
- **The destination container isn't running yet.** The agent asks the container runtime for up to 30 seconds, then ends the pass and comes back ([controller.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/controller/controller.go#L962-L965), [controller.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/controller/controller.go#L997-L1047)). The NRI hook reporting the container also brings the agent back ([controller.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/controller/controller.go#L418-L426)).
- **The agent can't read the Pod from its cache** ([controller.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/controller/controller.go#L486-L489)).
- **The agent can't add its finalizer to the Pod** ([controller.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/controller/controller.go#L551-L555)).
- **The agent can't write the Pod's `Restored` condition**, when a pass starts ([controller.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/controller/controller.go#L794-L797)) or with the final outcome ([controller.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/controller/controller.go#L1327-L1330)). After a failed final write, the next pass starts again from the preflight.
- **The agent can't remove its finalizer** after the outcome is recorded or while the Pod is being deleted ([controller.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/controller/controller.go#L1337-L1341), [controller.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/controller/controller.go#L504-L507)).

#### Failures counted toward the budget

Every other failure before the restore starts changing the container spends an attempt and is retried in place while attempts and time remain, such as:

- **PageBroker error codes**: `UNAVAILABLE`, `INSUFFICIENT_STORAGE`, `STORAGE_ERROR`, `STORAGE_UNAVAILABLE`, `INTERNAL_ERROR`, `TRANSACTION_NOT_FOUND`, `TRANSACTION_EXPIRED`, and `TRANSACTION_CONFLICT`.
- **A failure to reach PageBroker at all.**

### In-place retry

An in-place retry runs the next attempt in the same container after an exponential backoff. It applies only when the failed attempt left the container untouched. Nothing undoes a change, and the agent will not restart the container, so a failed attempt that changed the container ends the restore.

An attempt will count as having changed the container when:

- nsrestore reported that it started changing the container ([What nsrestore reports](#what-nsrestore-reports)). It applies the rootfs diff and unmounts `/dev/shm` before CRIU runs ([nsrestore.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/executor/nsrestore.go#L154-L188)), and a CUDA failure after CRIU leaves restored processes behind.
- validating the restored process failed after nsrestore returned ([restore.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/executor/restore.go#L383-L385))
- a restore mount could not be unmounted, since it stays in the container ([restore.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/executor/restore.go#L196-L199))
- the container stopped during the attempt, whether the workload's liveness probe, the startup gate, or anything else stopped it

### Attempt failure

`Restore` will return one `executor.RestoreFailure` for every failed attempt. Today it returns a `StepError` whose cause may be typed or plain text, with cleanup errors joined in ([step.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/executor/step.go#L51-L67), [restore.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/executor/restore.go#L201-L235)). The new type keeps the step and adds only what the decision reads.

```go
type RestoreFailure struct {
	Step             Step  // as StepError carries today
	Cause            error // typed where the failing code knows the type
	Final            bool  // no further attempt can succeed
	ContainerChanged bool  // another attempt would need a new container
	Cleanup          error // reported, never read as the cause
}
```

- **`Step` and `Cause` come from `failStep`**, as today ([step.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/executor/step.go#L62-L76)). A typed cause keeps its type, such as `*compat.IncompatibleError` for a compatibility refusal.
- **`Final` is set for a failure no attempt can fix**, in each case marked under [Failures that end the restore](#failures-that-end-the-restore). When it is set, the restore will end without another attempt.
- **`ContainerChanged` is set for an attempt that changed the container**, in each case listed under [In-place retry](#in-place-retry). When it is set, the restore will end. It stays apart from `Final`, so the event and a later fallback can tell a changed container from a failure no attempt can fix.
- **`Cleanup` holds the unmount and abort errors**, marked with capture's `CleanupFailure` ([checkpoint.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/executor/checkpoint.go#L45-L46)). `Restore` returns them today, joined into the attempt's error ([restore.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/executor/restore.go#L212-L214), [restore.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/executor/restore.go#L234)). A field of their own will keep them from mixing with `Cause`, so the reported cause and the decision see only what broke the attempt.

### What nsrestore reports

nsrestore runs inside the container and reports to the agent through one JSON result on stdout ([main.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/cmd/nsrestore/main.go#L81-L88)). The recovery decision will need two more things from it:

- **Whether nsrestore started changing the container**, to decide whether the next attempt can run in the same container.
- **The code of its failure**, to tell a failure no retry can fix, such as a corrupt artifact, from one worth retrying.

```go
// Sent on stdout right before the rootfs diff.
type RestoreInNamespaceProgress struct {
	ChangingContainer bool `json:"changingContainer"`
}

type RestoreInNamespaceResult struct {
	// existing fields, including Error and Step
	Code string `json:"code,omitempty"` // the failure's code, if it has one
}
```

The progress message will tell the agent that the container changed. `executeRestore` will send it right before `ApplyRootfsDiff`, the first change nsrestore makes in the container ([nsrestore.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/executor/nsrestore.go#L154-L157)). The agent will treat no progress message as untouched.

Today stdout carries only the result, and the agent parses all of it as one result ([nsrestore_output.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/executor/nsrestore_output.go#L30-L33), [nsrestore_output.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/executor/nsrestore_output.go#L58-L61)). The progress message will go there too, so the agent will split stdout into lines, look for the progress message in every line, and take the last line as the result. The agent keeps all of stdout ([process_output.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/runtime/process_output.go#L40-L47)), so a progress message sent before nsrestore fails or is killed still counts. `reportedCause` will rebuild a typed failure from the result's code ([nsrestore_output.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/executor/nsrestore_output.go#L84-L90)), so the decision reads it as it reads the same failure on the host.

### Recovery decision

`chooseRecovery` will run in `runRestore` ([controller.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/controller/controller.go#L1081-L1096)), the one place holding everything the choice needs:

- the attempts and time left from the attempt record
- the timeout from the restore context `runRestore` creates

Like capture's `classifyCaptureFailure` ([capture_failure.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/controller/capture_failure.go#L36-L64)), it will read a failure code by what the code means to its own operation, so a code that ends a capture may be retried by a restore.

```go
func chooseRecovery(f *executor.RestoreFailure, ctxErr error, left budget) recovery {
	switch {
	case ctxErr != nil, f.Final, finalCodes[failureCode(f.Cause)], f.ContainerChanged:
		return endRestore
	case left.attempts <= 0 || left.time <= 0:
		return retriesExhausted
	default:
		return retryInPlace
	}
}
```

### Scheduling the next attempt

Restore will split an attempt from its outcome, as capture does since [#682](https://github.com/ai-dynamo/snapshot/pull/682). After each attempt, the switch below will choose what follows it.

```go
if ctx.Err() != nil {
	return errRestorePending // the agent is shutting down, and its replacement decides
}
recovery := chooseRecovery(failure, restoreCtx.Err(), op.budget())
op.recordFailure(failure, recovery)
switch recovery {
case retryInPlace:
	return errRestorePending // the record holds the retry, and the next pass carries it out
default:
	return op.failRestore(ctx, err) // writes restore-failed and kills, as today
}
```

### Attempt record

Each container's record will live in the agent's memory, keyed by the Pod's UID and the container's name, until the Pod is deleted. It holds the same fields as the container's entry in the retry status annotation below.

The agent will update the record before each attempt, with the new `Attempt` and `ContainerID` and `RetryAt` cleared, and after each failure, with its `LastFailure` and, for an in-place retry, its `RetryAt`.

Each update will also queue the Pod for a write of the `nvidia.com/restore-retry-status` annotation, so a user or a controller can read which attempt a restore is on and why the last one failed, without parsing messages, and a restarted agent can pick the restore up. One goroutine in the agent will make the queued writes, outside the restore, and each write will take every record of the Pod as it is when the write runs. The containers of a Pod restore in parallel, so no container's write will drop another's entry, and updates queued while a write waits will go out as one write. A write that fails will be retried a few times and then reported as a warning event, without failing the restore.

The annotation holds one entry per container that started an attempt with a nonzero limit, so a restore that succeeds on its first attempt gets one too, and the outcome leaves it on the Pod. It is discarded with the Pod, so a new Pod starts a new restore with a fresh budget. `podcontract` gains the annotation's name, its type, and a parser.

```go
const RestoreRetryStatusAnnotation = "nvidia.com/restore-retry-status"

type RestoreRetryStatus map[string]ContainerRetryStatus // keyed by container name

type ContainerRetryStatus struct {
	Attempt     int             `json:"attempt"`               // the current or last attempt, from 1
	RetryLimit  int             `json:"retryLimit"`            // the limit resolved at the first attempt
	LastFailure *FailureSummary `json:"lastFailure,omitempty"` // the last failed attempt
	RetryAt     time.Time       `json:"retryAt,omitzero"`      // an in-place retry's due time
	Deadline    time.Time       `json:"deadline,omitzero"`     // set at the first attempt, ends every attempt
	ContainerID string          `json:"containerID,omitempty"` // the container the attempt ran in
	PodUID      types.UID       `json:"podUID,omitempty"`      // the Pod the entry was written for
}

type FailureSummary struct {
	Step             string `json:"step"`                       // where the attempt failed
	Cause            string `json:"cause"`                      // cut to 1,024 bytes, the limit of an event message
	SnapshotUnusable bool   `json:"snapshotUnusable,omitempty"` // the snapshot itself is missing, corrupt, or unsupported
}
```

### Picking up a pending restore

On every pass, including the first after an agent restart, the agent will read each destination container's record and decide from it whether to start an attempt, wait, or end the restore.

A record missing from memory, as on the first pass after an agent restart or upgrade, will be read from the container's entry in the retry status annotation. The agent will take the entry only when its `PodUID` is the Pod's own, so an annotation copied from another Pod does not carry that Pod's attempts over, and it will end the restore if the annotation does not parse. While the agent runs, it reads only its memory, since the Pod cache can lag the agent's own writes ([controller.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/controller/controller.go#L482)).

- **`restore-complete` exists.** The restore already succeeded, so the agent reports it again, as an agent restart does today ([controller.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/controller/controller.go#L1100-L1119)).
- **No record.** The first attempt starts, and the agent resolves the limit and the deadline.
- **`RetryAt` is still ahead.** The agent waits. A pending restore gets a pass on the Pod's next update or 30 seconds after the last one ([controller.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/controller/controller.go#L293-L298), [controller.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/controller/controller.go#L468-L474)), so the in-place retry will start no later than 30 seconds after it falls due. An earlier pass, such as one the agent's own update of the Pod starts, leaves the restore pending.
- **`RetryAt` is not set, and the record names the current container.** The agent restarted during that attempt, since one agent never runs two passes of the same Pod at once. No agent saw the attempt end, so it will run again with the same number and deadline, as an interrupted restore runs again today.
- **Otherwise**, such as an in-place retry that is due, the next attempt starts if attempts and time remain, and the restore ends as exhausted if not ([Deadline](#deadline)).

An agent that is shutting down will leave its attempt as it is, without recording a failure or killing the container, so its replacement finds an attempt that never finished. The agent exits without waiting for running restores ([main.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/cmd/agent/main.go#L107-L111), [controller.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/controller/controller.go#L464)), so a decision started during shutdown could stop halfway.

### Deadline

`restore.timeoutSeconds` will be one deadline for all attempts of a destination container, counted from the first attempt's start. Waits between attempts count toward it. A restore whose next attempt cannot start before the deadline will end as one that ran out of retries. Today each restore run gets its own timeout, and an agent restart starts it again ([controller.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/controller/controller.go#L1074-L1079)).

```go
restoreCtx, cancel := context.WithDeadline(ctx, record.Deadline)
```

### API

**Cluster-wide default.** `restore.retryLimit` in the agent configuration, rendered by the Helm chart. It pairs with `checkpoint.retryLimit`, which [#684](https://github.com/ai-dynamo/snapshot/pull/684) added for capture with this name in mind.

**Per-restore value.** An optional `nvidia.com/restore-retry-limit` annotation on the restore Pod, next to `nvidia.com/restore-from`, will override the default. Callers of `podcontract.Build` will set it through a new `podcontract.Options` field, and `podcontract.Validate` will reject a negative or non-numeric value.

**Retry status on the Pod.** The `nvidia.com/restore-retry-status` annotation and its `podcontract` type, described under [Attempt record](#attempt-record).

**The restore Pod contract** changes in one sentence. "A Pod gets one restore" becomes "a Pod gets one successful restore, after at most the retry limit plus one attempts".

### Security

Anyone who can update the restore Pod can rewrite its retry status annotation, so the agent will treat the annotation as untrusted input and end the restore if it does not parse. The agent reads it only after a restart, so a rewrite takes effect only then. A rewrite can reset the Pod's attempt count, which gives it no more than the retry limit annotation already lets the same writer set, or move its deadline. Either way the extra attempts restore only that Pod, on the node and GPUs it already holds.

### Configuration

| Setting | Where | Default |
| --- | --- | --- |
| `restore.retryLimit` | agent configuration, Helm `config.restore.retryLimit` | `0` |
| `nvidia.com/restore-retry-limit` | restore Pod annotation | absent, uses the cluster default |

The default of 0 keeps every restore at one attempt after an upgrade.

### Performance and Scalability

With a nonzero limit, each attempt adds up to two writes of the restore Pod, one when it starts and one when it fails, so a restore that succeeds on its first attempt adds one. The writes run outside the restore, and the updates of a Pod queued while a write waits go out as one write. Each write is a Pod update sent to all of the Pod's watchers.

A retried restore is slower by design. An in-place retry costs its backoff, or up to 30 seconds more until the agent's next pass, and the preparation again. The limit and the shared deadline bound it.

### Monitoring

- **Logs**
- **Warning events on the restore Pod.**
  - `RestoreAttemptFailed` for each failed attempt, naming the container, the attempt, the step, the cause, and the recovery.
  - `RestoreRetriesExhausted` when retries run out, before the final `RestoreFailed`.
  - `RestoreStatusUpdateFailed` when the agent cannot write the retry status annotation, as it reports a failed write of the `Restored` condition today.
- **The retry status annotation**, showing the current attempt and why the last one failed ([Attempt record](#attempt-record)).

### Dependencies

N/A

### Test Plan

**Unit.** Each part this design adds or changes will get unit tests.

**Integration.** Agent controller tests, alongside `TestRunRestoreFailureKillsPlaceholder` ([controller_test.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/controller/controller_test.go#L1775)), with the restore function replaced to inject failures:

- A restore that fails once while preparing and succeeds in place.
- A restore that fails after the rootfs diff and ends on that attempt, with `restore-failed` and the kill, as today.
- A container that stops during an attempt, which ends the restore.
- A restore that uses up its retries and ends `RestoreFailed` after `RestoreRetriesExhausted`.
- An agent restart during an attempt, picked up from the annotation, and an agent shutdown that leaves its attempt to the restarted agent.
- Two containers of one Pod that fail in the same pass, each keeping its entry in the annotation.

**End-to-end.** In the lifecycle suite ([test_snapshot_lifecycle.py](../../../e2e/tests/test_snapshot_lifecycle.py)), on CPU and GPU nodes, a restore Pod whose staged manifest is damaged and repaired after the first attempt fails. The next attempt runs in the same container, with no restart, and the restored workload matches the source. A damaged inventory, which CRIU reads after the rootfs diff, ends the restore on its first attempt.

### Graduation Criteria

**GA.** Retry will ship straight to GA. Its default of 0 keeps every restore at one attempt, and a retry runs only for a restore that has already failed, within its existing timeout. It ships once unit and end-to-end tests show an in-place retry restoring the workload, and a failure that ends the restore ending it on the attempt that hit it.

## Alternatives

**Retry in a new container after a change.** The agent could kill a container the failed attempt changed, without writing `restore-failed`, so kubelet restarts it and the next attempt restores into the new one. A retry would then restart a container that nothing else asked to restart. It would also work only for Pods whose restart policy restarts containers, while the restore Pod contract recommends `restartPolicy: Never`, and only on nodes with the restore proxy. Recreating the Pod instead would give up the node and GPUs the restore was placed on, work only for Pods with an owner, and overlap the cold-start fallback of [#244](https://github.com/ai-dynamo/snapshot/issues/244).

**Keep the attempt record in a file on the Pod's volume.** The agent could write each record to a `restore-attempts` file on the Pod's snapshot-control volume, next to the restore sentinels, where it would outlive an agent restart and be written before each attempt goes on. The annotation fits better:

- The Pod's containers can write that volume, so the agent would open the file as untrusted input on the node, without following links and with a cap on its size.
- The node names the volume's path after the Pod's UID on the node, which in a vCluster differs from the Pod's own, so every read would first ask the container runtime for it.
- The retry status annotation already holds most of the record, so the file would add a second copy in a format of its own.

**Keep the attempt record only in the annotation.** The agent could read the annotation on every pass instead of keeping the record in memory. The agent reads the Pod from a cache that can lag its own writes ([controller.go](https://github.com/ai-dynamo/snapshot/blob/b6343d0641aac414c9b51876ee579723f63e1801/agent/internal/controller/controller.go#L482)), so a pass could start from an attempt older than the last one, and each attempt would wait for its write to land.

**Keep the retry status in the Pod's status.** The agent could write it as a Pod condition next to `Restored`, instead of the `nvidia.com/restore-retry-status` annotation. Only writers with `pods/status` access could change it, and it would outlive container restarts, since kubelet keeps the conditions it does not own ([status_manager.go](https://github.com/kubernetes/kubernetes/blob/3c690c189e5d367715dbd056a5c2009a476c8d77/pkg/kubelet/status/status_manager.go#L1452-L1459)). A condition carries data only as text ([types.go](https://github.com/kubernetes/kubernetes/blob/3c690c189e5d367715dbd056a5c2009a476c8d77/staging/src/k8s.io/api/core/v1/types.go#L3940-L3963)), which consumers and a restarted agent would have to parse.

## Appendix

- [#247](https://github.com/ai-dynamo/snapshot/issues/247), the tracking issue.
- [SNEP-394](../394-capture-retry/README.md), with [#319](https://github.com/ai-dynamo/snapshot/issues/319) and [#632](https://github.com/ai-dynamo/snapshot/issues/632), capture retry and its implementation.
- [#244](https://github.com/ai-dynamo/snapshot/issues/244), cold-start fallback.
- [SNEP-411](https://github.com/ai-dynamo/snapshot/pull/437), application contracts, including the restore release and its single deadline.
- [Restore Pod contract](../../reference/restore-pod-contract.md).
