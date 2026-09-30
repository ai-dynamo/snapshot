<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# SNEP-411: Application profiles for checkpoint and restore

Tracking issue: [#411](https://github.com/ai-dynamo/snapshot/issues/411)

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [Limitations, Risks, and Mitigations](#limitations-risks-and-mitigations)
- [Design Details](#design-details)
  - [API](#api)
    - [Application contract](#application-contract)
    - [SnapshotJob](#snapshotjob)
    - [ApplicationProfile](#applicationprofile)
    - [Checkpoint metadata](#checkpoint-metadata)
  - [Capture and restore execution](#capture-and-restore-execution)
    - [Recovery and startup gating](#recovery-and-startup-gating)
  - [Rollout and compatibility](#rollout-and-compatibility)
  - [Security](#security)
  - [Monitoring](#monitoring)
  - [Test Plan](#test-plan)
  - [Graduation Criteria](#graduation-criteria)
- [Appendix](#appendix)
<!-- /toc -->

## Summary

Introduce administrator-managed application profiles that users
reference when creating a snapshot. A profile defines how Snapshot observes
application quiescence and releases the application after restore, using file
or HTTP observations and file, HTTP, or Unix-signal notifications.

## Motivation

Most users will run an established engine such as vLLM, SGLang, or TensorRT-LLM.
They should select a supported integration without having to discover its
endpoints, signals, or timing settings and repeat them in each snapshot request.
Profiles give administrators a reusable, reviewable integration definition.
Application maintainers define the contract their engine implements; Snapshot
provides the transport machinery without engine-specific logic.

### Goals

- Configure both PodSnapshot and SnapshotJob through the same profile API.
- Record per-container profile names for resolution at capture and restore.
- Retain the existing control-volume behavior when no profile is selected.

### Non-Goals

- Requesting application preparation, draining, or quiescence.
- Continuing the source application after capture.
- Defining application serving-readiness checks or engine recovery steps.
- Running arbitrary commands or general-purpose hook sequences.

## Proposal

An administrator installs an `ApplicationProfile` describing two
interactions: `quiesceProbe` and `restoreRelease`. A snapshot user references
that profile by name for each captured container.

The application establishes its own safe point. A successful quiesce probe
means the application is safe to capture and will remain so until capture.
After restoring process and GPU state, Snapshot delivers the configured release
notification. The application then performs its recovery and advertises serving
readiness through its normal readiness mechanism.

Integration documentation publishes profiles with supported engine versions
and required application configuration. Administrators install profiles for
supported and custom applications.

### Limitations, Risks, and Mitigations

Snapshot trusts the application's quiescence report. Profile installation does
not prove workload compatibility; integration tests must establish that the
application reports a safe point and resumes correctly.

Recreating a profile can change the release behavior of existing checkpoints.
An incompatible replacement may fail restore or release the application
incorrectly. Snapshot does not verify semantic compatibility with the captured
process; administrators are responsible for replacement definitions.

HTTP listeners must be reachable before application release. The initial HTTP
release implementation accepts only one restore destination per Pod because
containers share networking. File and signal release retain per-container
targeting. An unsupported mapping is rejected before restoring any destination.

## Design Details

### API

All resources use `nvidia.com/v1alpha1`. The following Go definitions describe
the proposed JSON structure; CRD validation and defaulting enforce the rules
below.

#### Application contract

PodSnapshot selects profiles through `spec.applicationContract`, keyed by source
container name:

```go
type PodSnapshotSpec struct {
	Source              PodSnapshotSource    `json:"source"`
	ApplicationContract *ApplicationContract `json:"applicationContract,omitempty"`
}

type ApplicationContract struct {
	Containers map[string]ContainerApplicationContract `json:"containers"`
}

type ContainerApplicationContract struct {
	ProfileRef ApplicationProfileReference `json:"profileRef"`
}

type ApplicationProfileReference struct {
	Name string `json:"name"`
}
```

Each `containers` key must name a container selected by
`spec.source.podRef.containers`. Each binding requires a nonempty
`profileRef.name`, a valid Kubernetes resource name identifying a cluster-scoped
profile. A selected container
without a binding uses the existing file contract; a missing explicitly named
profile never falls back to that default.

Profile bindings are immutable with the PodSnapshot or SnapshotJob spec. The
existing string lists selecting capture targets remain unchanged, including
the current single-container capture limit. The binding map accommodates
per-container selection without requiring a future change to its shape.

```yaml
apiVersion: nvidia.com/v1alpha1
kind: PodSnapshot
metadata:
  name: model-snapshot
spec:
  source:
    podRef:
      name: model-source
      containers: [main]
  applicationContract:
    containers:
      main:
        profileRef:
          name: cooperative-http-v1
```

#### SnapshotJob

`SnapshotJob.spec.podSnapshotTemplate.applicationContract` supplies the bindings
for the generated PodSnapshot. The updated structures are:

```go
type SnapshotJobSpec struct {
	PodTemplate           corev1.PodTemplateSpec `json:"podTemplate"`
	ActiveDeadlineSeconds *int64                 `json:"activeDeadlineSeconds,omitempty"`
	PodSnapshotTemplate   PodSnapshotTemplate    `json:"podSnapshotTemplate"`
}

type PodSnapshotTemplate struct {
	Metadata            *PodSnapshotTemplateMetadata `json:"metadata,omitempty"`
	TargetContainers    []string                     `json:"targetContainers,omitempty"`
	ApplicationContract *ApplicationContract         `json:"applicationContract,omitempty"`
}
```

`applicationContract` is the new optional field; the other fields retain their
existing semantics. Its container keys must belong to `targetContainers`.

For example, add this fragment alongside the existing `spec.podTemplate`:

```yaml
spec:
  podSnapshotTemplate:
    targetContainers: [main]
    applicationContract:
      containers:
        main:
          profileRef:
            name: cooperative-http-v1
```

The controller copies these bindings into `PodSnapshot.spec.applicationContract`.

#### ApplicationProfile

The new resource is cluster-scoped and its spec is immutable. Administrators can
publish another profile name or delete and recreate an existing name. References
resolve by name only; they do not bind to a profile UID.

```go
type ApplicationProfile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ApplicationProfileSpec `json:"spec"`
}

type ApplicationProfileSpec struct {
	QuiesceProbe   QuiesceProbe   `json:"quiesceProbe"`
	RestoreRelease RestoreRelease `json:"restoreRelease"`
}

type QuiesceProbe struct {
	File    *ControlFileAction   `json:"file,omitempty"`
	HTTPGet *LifecycleHTTPAction `json:"httpGet,omitempty"`

	InitialDelaySeconds *int32 `json:"initialDelaySeconds,omitempty"`
	PeriodSeconds       *int32 `json:"periodSeconds,omitempty"`
	TimeoutSeconds      *int32 `json:"timeoutSeconds,omitempty"`
	SuccessThreshold    *int32 `json:"successThreshold,omitempty"`
}

type RestoreRelease struct {
	File     *ControlFileAction   `json:"file,omitempty"`
	HTTPPost *LifecycleHTTPAction `json:"httpPost,omitempty"`
	Signal   *SignalAction        `json:"signal,omitempty"`

	PeriodSeconds  *int32 `json:"periodSeconds,omitempty"`
	TimeoutSeconds *int32 `json:"timeoutSeconds,omitempty"`
}

type ControlFileAction struct {
	Name string `json:"name"`
}

type LifecycleHTTPAction struct {
	Path   string `json:"path"`
	Port   int32  `json:"port"`
	Scheme string `json:"scheme,omitempty"`
}

type SignalAction struct {
	Name string `json:"name"`
}
```

Each probe requires exactly one of `file` or `httpGet`; each release requires
exactly one of `file`, `httpPost`, or `signal`.

The probe follows the handler-and-timing structure of
[Kubernetes probes](https://kubernetes.io/docs/concepts/workloads/pods/probes/).
Defaults are `initialDelaySeconds: 0`, `periodSeconds: 1`,
`timeoutSeconds: 1`, and `successThreshold: 1`. The initial delay is nonnegative;
other timing values and the success threshold must be positive. A release
defaults to a one-second period and one-second timeout. The timeout bounds one
attempt; it is not the total operation deadline.

There is no `failureThreshold`: an unsuccessful quiesce observation resets the
success count and keeps capture waiting without restarting the container.
Restarting or replacing the source container invalidates accumulated successes.
The initial delay is measured from that container incarnation's start time.

File names must be single path components within the container's
`SNAPSHOT_CONTROL_DIR`. Absolute paths, separators, `.` and `..` are invalid.
The probe and release cannot use the same name. Internal names such as
`cuda-checkpoint-job` are reserved; `ready-for-snapshot` and `restore-complete`
remain valid for their respective actions.

HTTP paths must begin with `/` and contain no authority, query, or fragment.
Ports range from 1 through 65535. Scheme defaults to `HTTP`; `HTTPS` is also
accepted. The agent derives the host from the target Pod IP. Host overrides,
custom headers, request bodies, and credential references are not exposed.
Signal names are restricted to `SIGUSR1` and `SIGUSR2`.

For example, a cooperative application's profile can declare:

```yaml
apiVersion: nvidia.com/v1alpha1
kind: ApplicationProfile
metadata:
  name: cooperative-http-v1
spec:
  quiesceProbe:
    httpGet:
      path: /snapshot/quiesced
      port: 9001
    periodSeconds: 1
    timeoutSeconds: 2
    successThreshold: 1
  restoreRelease:
    httpPost:
      path: /snapshot/restored
      port: 9001
    periodSeconds: 1
    timeoutSeconds: 2
```

These paths illustrate the proposed application contract; they are not existing
engine endpoints. A file profile instead uses `quiesceProbe.file.name:
ready-for-snapshot` and `restoreRelease.file.name: restore-complete`.

#### Checkpoint metadata

Add the same optional `applicationContract` field to `PodSnapshotContentSpec`
and checkpoint metadata:

```go
// Added to PodSnapshotContentSpec and checkpoint metadata:
// ApplicationContract *ApplicationContract `json:"applicationContract,omitempty"`
```

The operator copies the requested bindings into the immutable content spec;
the agent records them with the checkpoint. Metadata retains profile names,
not definitions or profile UIDs. Restore maps each captured source container's
binding to its destination containers. Destination Pods do not select profiles.

### Capture and restore execution

At the start of a capture attempt, the agent resolves the selected profile by
name and retains that definition in agent-owned attempt state across retries
and restarts. Its `quiesceProbe` replaces the
legacy container-readiness capture gate for that container. SnapshotJob must
not inject the legacy file readiness probe for a container with an explicit
binding. Unbound containers retain existing behavior. The control volume
remains necessary for runtime coordination and startup gating.

Before starting restore, resolve the recorded profile names again and validate
the destination mappings. A missing profile blocks restore before CRIU runs;
reconciliation can retry resolution when it becomes available. Recreating a
profile under the same name makes the checkpoint eligible again, and new
attempts use the recreated definition's `restoreRelease`.

Once a restore attempt starts, persist its resolved definition with the attempt
record. Deleting or recreating the profile does not interrupt that attempt or
change its retries. There is no fallback to the capture-time definition.

A file probe succeeds when its marker exists as a regular file. The application
must clear a stale marker before initializing, as in the existing contract.
HTTP GET succeeds only on status `200`. Once the success threshold is reached,
the agent revalidates source identity before capture.

After successful CRIU and CUDA restore, the agent releases each destination:

- **File:** atomically publish the configured marker in the destination's
  control directory.
- **HTTP:** POST an empty body to the configured endpoint. Status `200` or
  `204` means the release was accepted; other results are retried. The agent
  supplies `Snapshot-Restore-ID`, a stable opaque identifier for this attempt.
- **Signal:** send the selected signal to the verified restored process-tree
  root. Its handler must already be installed at capture. Successful delivery
  does not acknowledge handler completion.

Release may be repeated after a lost response or agent restart; every handler
must tolerate duplicates. Each destination container incarnation gets a unique
attempt identifier, reused for retries. Transport success means delivery, not
that the application is ready to serve. The executor must return the restored
root identity separately from the placeholder PID to target signal release.

SnapshotJob's `activeDeadlineSeconds` bounds its source lifecycle, including
quiesce waiting. A direct PodSnapshot has no additional overall quiesce deadline;
it waits until the probe succeeds, the request is deleted, or the source becomes
terminal. Probe timeouts bound individual observations and do not fail capture.

Restore uses the configured agent restore timeout for the entire attempt,
including release. Persist the absolute deadline when the attempt starts so
reconciliation and agent restarts do not extend it. If release cannot be
delivered before that deadline, restore fails and the destination is cleaned up
through the restore failure path. It is not marked successfully restored.

#### Recovery and startup gating

Application notification and infrastructure restoration are recorded
separately so a failed notification never causes CRIU to run a second time:

1. Persist an attempt record with artifact identity, Pod UID, container
   incarnation, attempt ID, absolute deadline, and resolved profile definition.
   Clear `restore-complete` and any configured file release marker before CRIU;
   a stale marker must not release the restored process early.
2. After CRIU/CUDA succeeds, persist `RuntimeRestored` with the observed process
   identity. Failure to persist this state prevents release.
3. Deliver release, then persist `ReleaseDelivered`. Failure to record delivery
   leaves a retriable notification; it does not by itself justify cleanup.
4. Publish the existing `restore-complete` startup marker. When that marker is
   also the configured release action, its publication occurs in step 3.

Recovery from `RuntimeRestored` retries only delivery, within the original
deadline. `ReleaseDelivered` permits repairing the startup marker and status
even after the deadline expires. If runtime completion cannot be established,
fail and clean up the attempt without replaying restore in the same container.

Persist records atomically in an agent-only, root-owned hostPath directory,
keyed by Pod UID and container incarnation. One owner must hold exclusive access
through execution, recovery, and cleanup across agent instances. Revalidate
container membership, node boot identity, PID, and process start time before
recovery; use a process handle for signaling to avoid PID reuse. Remove records
after the destination is gone.

Legacy restores with neither a profile binding nor an attempt record retain
the existing completion-marker recovery path. Profile-based restores require
the agent-owned record; workload-written markers are not completion evidence.

### Rollout and compatibility

Upgrade all participating node agents before enabling profile selection in the
operator or creating profiled work orders. Older agents do not understand the
new fields, so mixed-version execution of profiled captures/restores is not
supported. Artifacts without application contract metadata continue to use the
legacy contract. Downgrading agents requires first removing profiled work from
their execution scope.

### Security

Only administrators can create, update, or delete profiles. Installed profiles
are available to users already authorized to request snapshots in their
namespace. Selecting a profile grants no additional authority over a target Pod;
source identity checks and namespace boundaries remain mandatory. Content work
orders carry validated profile references, not user-supplied action definitions.

HTTP requests go directly to the selected Pod IP, without redirects or
environment-configured proxies. Host-network Pods are rejected for HTTP
interactions. Pod networking does not isolate containers, so administrators must
review profiles for the whole Pod trust boundary. HTTPS requires ordinary
certificate and IP-identity verification against the agent's trust roots.
The control listener must be restricted to intended callers by the deployment;
this API does not distribute application credentials.

File access is confined to the selected container's control directory using
operations that reject symlinks and traversal. Signal recipients are resolved
from the restored process identity; users cannot supply a host PID. Profile
bindings and attempt records must retain the integrity protections of other
agent-owned metadata.

Profiles contain no credentials. Logs and Events record action type and outcome,
not HTTP response bodies or application memory.

### Monitoring

Expose waiting for quiescence and release failures through existing snapshot
and restore status reporting. Add Events for profile resolution failure,
unsupported lifecycle configuration, and release failure. SnapshotJob deadline
failures retain their existing reporting.
Log profile name, source and destination container, attempt ID, transport, attempt
count, and elapsed time. Ordinary unsuccessful probe polls remain debug-level
logs rather than an Event per poll.

### Test Plan

- API tests cover handler exclusivity, defaults, immutable specs, container keys
  matching selected targets, unbound-container defaults, and propagation from
  SnapshotJob through content and artifact metadata. Existing target-name lists
  must remain compatible.
- Controller tests cover missing profiles, source incarnation changes, probe
  success thresholds, capture cancellation, and persisted restore deadlines.
- Profile resolution tests delete and recreate a profile with different settings:
  new restores must use the replacement, including when it is incompatible;
  in-progress attempts must keep their persisted definition across agent restart.
  Missing profiles must block execution and become resolvable after recreation.
- Restore tests inject crashes before and after runtime completion, release,
  and startup-marker publication. Verify that no uncertain restore is replayed,
  no completed runtime restore is repeated to retry a notification, and release
  retries preserve the attempt identifier. Include persistence failures,
  overlapping agents, and legacy completion-marker recovery after an upgrade.
  A delivered attempt must finish status repair even after its deadline expires.
- Transport tests cover HTTP status handling, redirects, proxies, TLS identity,
  host-network rejection, file traversal/symlinks, stale markers, duplicate
  notifications, and signal delivery to the restored root rather than PID 1.
  Prepopulate a custom release marker before a destination restart and verify
  that it cannot release the next incarnation before runtime restoration.
- End-to-end tests retain existing file integrations and validate a cooperative
  application over HTTP and file-readiness/signal-release. Each supported
  transport combination must also have a real application integration before
  it is advertised as supported. Verify actual inference after restore.
- Restore mapping tests verify file/signal destination isolation and reject
  HTTP release with multiple destinations in one Pod before any restore starts.

### Graduation Criteria

Alpha requires the API validation, legacy compatibility, and transport/recovery
tests above, with documented supported application versions and topologies.
Beta requires operational evidence under repeated agent restarts and a security
review of the privileged execution paths. GA requires a stable application
contract, compatibility coverage for previously produced artifacts, and
documented failure and recovery procedures.

## Appendix

- [Workload contract](../../reference/workload-contract.md)
- [Restore Pod contract](../../reference/restore-pod-contract.md)
- [Kubernetes probes](https://kubernetes.io/docs/concepts/workloads/pods/probes/)
- [Kubernetes lifecycle hooks](https://kubernetes.io/docs/concepts/containers/container-lifecycle-hooks/)
- [gVisor application-driven checkpoint/restore](https://gvisor.dev/docs/user_guide/checkpoint_restore/#application-driven-checkpointrestore)
