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
  - [Defaults and user probes](#defaults-and-user-probes)
  - [Limitations, Risks, and Mitigations](#limitations-risks-and-mitigations)
    - [Backward compatibility and migration](#backward-compatibility-and-migration)
- [Design Details](#design-details)
  - [API](#api)
    - [Application contract](#application-contract)
    - [SnapshotJob](#snapshotjob)
    - [ApplicationProfile](#applicationprofile)
    - [Checkpoint metadata](#checkpoint-metadata)
  - [Capture and restore execution](#capture-and-restore-execution)
    - [Recovery and startup gating](#recovery-and-startup-gating)
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

Users of engines such as vLLM, SGLang, and TensorRT-LLM should select a supported
integration without discovering and repeating its endpoints, signals, and timing
settings in every request. Profiles let administrators publish those settings
once. Application maintainers define the interface; Snapshot implements its
transports without engine-specific logic.

### Goals

- Configure both PodSnapshot and SnapshotJob through the same profile API.
- Record per-container profile names for resolution at capture and restore.
- Keep the legacy file integration available without requiring a profile.

### Non-Goals

- Requesting application preparation, draining, or quiescence.
- Continuing the source application after capture.
- Defining application serving-readiness checks or engine recovery steps.
- Running arbitrary commands or general-purpose hook sequences.

## Proposal

An administrator installs an `ApplicationProfile` with optional
`quiesceProbe` and `restoreRelease` actions. A snapshot user selects a profile
by name for each captured container.

The application owns capture safety and recovery. Snapshot observes the
configured capture signal and, after restoring process and GPU state, sends
any configured release notification. Application readiness determines when
the restored workload can serve.

Integration documentation publishes profiles with supported engine versions
and required application configuration. Administrators install profiles for
supported and custom applications.

### Defaults and user probes

Both actions are optional; an empty profile is valid. Defaults depend on
whether a profile is selected:

- **No profile:** capture waits for the source Pod's `Ready` condition; restore
  publishes the legacy `restore-complete` file notification. SnapshotJob injects
  file readiness only when the target has no user-defined readiness probe.
- **Selected profile:** an omitted `quiesceProbe` uses Pod readiness; an omitted
  `restoreRelease` sends no application notification.
- **All cases:** preserve user-defined readiness, startup, and liveness probes.
  When restore startup gating is enabled, inject the `restore-complete` startup
  probe only if the container has no user-defined startup probe.

A custom startup probe must prevent premature startup success. Liveness must
tolerate recovery, and readiness must stay false until serving is available.
Snapshot tracks restoration independently of these probes.

### Limitations, Risks, and Mitigations

This proposal retains the single-container capture limit. Per-container profile
bindings do not enable multi-container capture. Profiles configure interfaces
an application already implements; they cannot make an ordinary serving
endpoint usable while the application is waiting for release. An HTTP control
endpoint must work after runtime restore, before release, including at a new
Pod IP. Only unencrypted HTTP is supported; HTTPS and application credential
distribution are outside this API.

Snapshot observes capture readiness but does not establish or verify application
safety. Application integrations must validate the complete capture/restore
cycle, including the deployment's probes and networking.

| Risk | Effect | Mitigation or owner responsibility |
| --- | --- | --- |
| Premature capture signal | A checkpoint can contain unsafe application state. | The application must remain safe after reporting readiness; integration tests must verify successful recovery and inference. |
| Recreated profile name | Existing checkpoints use the replacement on their next restore, which may fail or release incorrectly. | Administrators validate compatibility or publish a new name. Active attempts retain their resolved definition. |
| Lost release response | Retrying can deliver the same notification again; deadline expiry can terminate an application that received it. | Application handlers must be idempotent. Snapshot persists attempt progress and uses a fixed deadline. |
| Custom probes during recovery | Premature startup success or liveness failure can restart a recovering container. | Workload owners configure probes to tolerate restoration and keep readiness false until serving works. |
| Shared HTTP endpoint | Requests for several destinations in one Pod are indistinguishable. | The application owns listener compatibility and release coordination, including destinations restored at different times. Snapshot does not prohibit this topology. |
| Unprotected HTTP listener | An unintended caller could release the application. | The deployment restricts access to the control endpoint; installing a profile alone does not secure it. |

#### Backward compatibility and migration

Existing target-container lists and checkpoint artifacts remain valid. Users do
not need a profile for the legacy file integration, and artifacts without
profile metadata retain the legacy release notification. Two behaviors
intentionally change:

- User-defined probes are preserved instead of replaced or removed. Workloads
  that relied on those overrides must adjust their probes for capture and restore.
- Without an explicit quiesce probe, capture waits for the whole Pod's `Ready`
  condition. Unready sidecars or readiness gates can delay captures previously
  allowed by the selected container's readiness alone.

Upgrade all participating node agents before enabling profile selection in the
operator or creating profiled work orders. Older agents ignore the new fields;
mixed-version execution of profiled work is unsupported. Validate application
profiles and probe configuration before migrating workloads. Before downgrading,
remove profiled captures and restores from the affected agents' execution scope;
existing profiled artifacts must remain assigned to compatible agents.

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
profile. A selected container without a binding uses the default behavior
described below; a missing explicitly named profile never falls back to it.

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
	QuiesceProbe   *QuiesceProbe   `json:"quiesceProbe,omitempty"`
	RestoreRelease *RestoreRelease `json:"restoreRelease,omitempty"`
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
	Path string `json:"path"`
	Port int32  `json:"port"`
}

type SignalAction struct {
	Name string `json:"name"`
}
```

When present, `quiesceProbe` requires exactly one of `file` or `httpGet`, and
`restoreRelease` requires exactly one of `file`, `httpPost`, or `signal`.
An empty profile spec is valid; omission follows the [defaults above](#defaults-and-user-probes).

Probe handlers and timing fields follow the structure of
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
Ports range from 1 through 65535. Only HTTP is supported. The agent derives
the host from the target Pod IP. Host overrides, custom headers, request bodies,
and credential references are not exposed.
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

**Capture.** The agent resolves a selected profile at the start of an attempt
and retains its definition across retries and agent restarts. An explicit
`quiesceProbe` is the sole application capture gate: a regular file marker or
HTTP GET status `200` indicates success. The application must clear stale file
markers before initialization. Once the success threshold is reached, the agent
revalidates source identity before capture.

The Pod readiness fallback includes other containers and readiness gates.
Whichever signal is used, it must indicate that the application is safe to
capture and will remain so until capture.

**Restore and release.** Recover matching persisted attempts using their saved
profile definitions before looking up current profiles. A new attempt resolves
the checkpoint's recorded names and validates destination mappings before CRIU
runs. Missing profiles block the attempt until reconciliation can resolve them. New attempts use replacement
definitions; existing attempts retain their saved definitions. There is no
fallback to a capture-time definition.

After successful CRIU and CUDA restore, perform the configured
action for each destination:

- **File:** atomically publish the marker in the destination's control directory.
- **HTTP:** POST an empty body. Status `200` or `204` acknowledges acceptance
  into the application's recovery logic; recovery may continue asynchronously.
  All other statuses and connection failures are retried until the restore
  deadline. GET and POST response bodies are ignored; no attempt ID is sent.
- **Signal:** send `SIGUSR1` or `SIGUSR2` to the verified restored process-tree
  root, not the placeholder process. The handler must be installed before
  capture. Successful delivery does not acknowledge handler completion.
- **Omitted:** proceed without an application notification.

Release can be repeated after a lost response or agent restart. Snapshot records
completion only after the configured action succeeds or is omitted.

HTTP requests address a Pod IP, port, and path without a container identifier.
A shared endpoint must make each acknowledgment cover the required release.

**Deadlines.** SnapshotJob's `activeDeadlineSeconds` bounds its source lifecycle,
including capture readiness. A direct PodSnapshot waits until its capture gate
succeeds, the request is deleted, or the source becomes terminal. Probe timeouts
bound individual observations, not the total wait.

The agent restore timeout covers runtime restoration and release. Persist its
absolute deadline so retries and restarts cannot extend it. If the release step
has not been recorded complete at that deadline, fail and clean up the
destination; the application may have received a notification whose response
was lost.

#### Recovery and startup gating

The control volume remains required for runtime coordination. Record runtime
restoration and release separately so a notification failure never repeats CRIU:

1. Persist an attempt record with artifact identity, Pod UID, container
   incarnation, attempt ID, absolute deadline, and resolved profile definition.
   Clear `restore-complete` and any configured file release marker before CRIU;
   a stale marker must not release the restored process early.
2. After CRIU/CUDA succeeds, persist `RuntimeRestored` with the observed process
   identity. Failure to persist this state prevents release.
3. Deliver the configured release, or skip notification if omitted, then persist
   `ReleaseComplete`. Failure to record this state leaves a retriable step; it
   does not by itself justify cleanup.
4. Publish the existing `restore-complete` startup marker. When that marker is
   also the configured release action, its publication occurs in step 3.

Recovery from `RuntimeRestored` retries only the release step, within the original
deadline. Once `ReleaseComplete` is recorded, marker or status write failures
require repair, not destructive cleanup, even after the deadline expires.
If runtime completion cannot be established, fail and clean up the attempt
without replaying restore in the same container.

Persist records atomically in an agent-only, root-owned hostPath directory,
keyed by Pod UID and container incarnation. One owner must hold exclusive access
through execution, recovery, and cleanup across agent instances. Revalidate
container membership, node boot identity, PID, and process start time before
recovery; use a process handle for signaling to avoid PID reuse. Remove records
after the destination is gone.

Legacy restores with neither a profile binding nor an attempt record retain
the existing completion-marker recovery path. Profile-based restores require
the agent-owned record; workload-written markers are not completion evidence.

### Security

Only administrators can create, update, or delete profiles. Installed profiles
are available to users already authorized to request snapshots in their
namespace. Selecting a profile grants no additional authority over a target Pod;
source identity checks and namespace boundaries remain mandatory. Content work
orders carry validated profile references, not user-supplied action definitions.

HTTP requests go directly to the selected Pod IP, without redirects or
environment-configured proxies. Host-network Pods are rejected for HTTP
interactions. Pod networking does not isolate containers, so administrators must
review profiles for the whole Pod trust boundary. The deployment must restrict
the unencrypted control listener to intended callers; this API does not
distribute application credentials.

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
invalid lifecycle configuration, and release failure. SnapshotJob deadline
failures retain their existing reporting.
Log profile name, source and destination container, attempt ID, transport, attempt
count, elapsed time, and the last HTTP failure status when applicable.
Ordinary unsuccessful probe polls remain debug-level logs rather than an Event
per poll.

### Test Plan

- **API and propagation:** optional actions and empty profiles, handler
  exclusivity, defaults, immutable specs, target-container validation, and
  bindings copied from SnapshotJob through content and checkpoint metadata.
  Preserve the existing target-name lists and single-container capture limit.
- **Capture and probes:** explicit quiesce probes versus Pod readiness fallback,
  success thresholds, source incarnation changes, cancellation, and deadlines.
  Preserve every user-defined probe; inject default probes only when eligible.
- **Profile resolution:** missing names block new attempts and become usable
  after recreation. New attempts use replacement definitions; existing attempts
  retain saved definitions across profile deletion and agent restart.
- **Restore recovery:** inject failures around runtime completion, release,
  record persistence, and startup-marker publication. Never replay an uncertain
  or completed runtime restore. Retry release idempotently within the original
  deadline; repair recorded completion without destructive cleanup. Cover
  overlapping agents, omitted release, and legacy marker recovery.
- **Transports:** HTTP success codes, ignored response bodies, retries, redirects,
  proxies, host-network rejection, file confinement, stale markers, and signal
  delivery to the restored root. Verify destination isolation for file/signal
  and application-owned coordination for shared HTTP endpoints.
- **Application integrations:** retain existing file integrations and validate
  HTTP and file-readiness/signal-release with actual inference after restore.
  Cover a changed Pod IP, release before serving starts, asynchronous recovery,
  and compatible custom probes. Advertise a transport combination only after a
  real application integration validates it.

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
