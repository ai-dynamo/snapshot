<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Restore Pod contract

A Pod requests restore by naming a ready `PodSnapshot` in its namespace. The
Snapshot node agent restores only Pods that implement this contract.

Go integrations should use:

```go
restored, err := podcontract.Build(
    pod,
    podcontract.Request{
        SnapshotName:    snapshotName,
        SourceContainer: capturedContainer,
        Mappings:        mappings,
    },
    podcontract.Options{
        SeccompProfile:   podcontract.DefaultSeccompLocalhostProfile,
        EnableStartupGate: true,
    },
)
```

This release intentionally moves the Go Pod-contract API from
`api/restorepod` and `api/v1alpha1` to `api/podcontract` without compatibility
aliases. Consumers must update restore annotations, mappings, status reasons,
`RestoreOutcome`, and `ClassifyRestoreOutcome` references to the new package.
Custom-resource types remain in `api/v1alpha1`.

`podcontract.Build` is a pure, atomic transformation: it returns a deep copy or
an error and never mutates its input. Reapplying it with the same arguments is
idempotent. It emits one canonical representation and validates that exact
output. `podcontract.Validate` checks only Snapshot's minimum restore protocol,
without mutation or Kubernetes API reads. Optional workload policy such as
startup gating and seccomp selection remains producer-owned. Conflicting
annotations, volumes, mounts, environment, and requested security settings are
rejected instead of overwritten.

Consumers can interpret the agent-owned `nvidia.com/Restored` Pod condition
without importing Snapshot's custom-resource API:

```go
outcome := podcontract.ClassifyRestoreOutcome(pod.Status.Conditions)
```

The package also owns the shared control-volume, environment, capture-sentinel,
and CUDA job-file names used by Snapshot-managed source and restore Pods.

The producer supplies `SourceContainer` from the referenced `PodSnapshot`.
Empty mappings restore that source into the same-named destination only. The
builder validates one-source-to-many-destination consistency but deliberately
performs no API read to discover the captured source.

Non-Go producers can implement the declarative form directly. This fan-out
example restores the captured `main` process into two destination containers:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: restored-worker
  annotations:
    nvidia.com/restore-from: worker-snapshot
    nvidia.com/restore-container-map: main=engine-0,main=engine-1
spec:
  volumes:
    - name: snapshot-control
      emptyDir: {}
  containers:
    - name: engine-0
      image: worker:latest
      securityContext:
        seccompProfile:
          type: Localhost
          localhostProfile: profiles/block-iouring.json
      # The caller supplies an inert, long-running entrypoint.
      command: ["/bin/sh", "-c", "exec sleep infinity"]
      env:
        - name: SNAPSHOT_CONTROL_DIR
          value: /snapshot-control
      volumeMounts:
        - name: snapshot-control
          mountPath: /snapshot-control
          subPath: engine-0
      startupProbe:
        exec:
          command: ["cat", "/snapshot-control/restore-complete"]
        timeoutSeconds: 1
        periodSeconds: 1
        failureThreshold: 1800
        successThreshold: 1
    - name: engine-1
      image: worker:latest
      securityContext:
        seccompProfile:
          type: Localhost
          localhostProfile: profiles/block-iouring.json
      command: ["/bin/sh", "-c", "exec sleep infinity"]
      env:
        - name: SNAPSHOT_CONTROL_DIR
          value: /snapshot-control
      volumeMounts:
        - name: snapshot-control
          mountPath: /snapshot-control
          subPath: engine-1
      startupProbe:
        exec:
          command: ["cat", "/snapshot-control/restore-complete"]
        timeoutSeconds: 1
        periodSeconds: 1
        failureThreshold: 1800
        successThreshold: 1
```

The container mapping is optional for a single same-name restore. Every
destination mounts the one shared `snapshot-control` `emptyDir` at
`/snapshot-control` with `subPath` equal to its container name. The canonical
`SNAPSHOT_CONTROL_DIR` environment variable points to that mount. It is a
required part of the minimum contract so every Snapshot-managed workload and
tool can discover the control directory without hard-coding its location. The
builder also injects the deprecated `DYN_SNAPSHOT_CONTROL_DIR` alias during
the migration window; hand-authored Pods may omit the alias.

The `restore-complete` startup probe shown above is optional workload lifecycle
policy. Set `Options.EnableStartupGate` when Kubernetes must withhold readiness
and liveness until restore completes. Because Kubernetes supports only one
startup probe, the builder then replaces any existing startup probe with the
canonical restore-completion gate while preserving workload liveness and
readiness probes. Its failure threshold allows 1,800 consecutive one-second
failures; if restoration exceeds that budget, kubelet restarts the placeholder
according to the Pod's restart policy. The node agent always writes the
sentinel but does not require or inspect the optional probe.

`Options.SeccompProfile` controls the localhost profile on restore destination
containers only. The standard Snapshot Helm installation deploys
`DefaultSeccompLocalhostProfile` to block io_uring for CRIU. Applying it at
container scope preserves the Pod-level policy inherited by unrelated
sidecars. An empty value leaves seccomp unmanaged for environments that provide
the restriction elsewhere. A destination container must not override a
requested profile with a conflicting profile.

Snapshot does not modify container commands and does not inject
`SNAPSHOT_RESTORE_STANDBY`, its deprecated
`DYN_SNAPSHOT_RESTORE_STANDBY` alias, or any other workload-specific standby
setting. Both names are exported by `api/podcontract` so application owners can
set the convention they support. The producer must ensure each destination
process remains alive and inert until the agent replaces it with the restored
process.
