<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Helm Chart Values

Use this page to look up the Helm values that configure Snapshot. Set a value
with `--set` or in a values file when you install or upgrade the chart. The
tables show each value's default.

For what the storage values mean, see
[Configure Storage](../operate/configure-storage.md). For the full list of
values, see [values.yaml](../../charts/snapshot/values.yaml).

## Images and Rollout

| Value                         | Default                                 | Description                                                                      |
|-------------------------------|-----------------------------------------|----------------------------------------------------------------------------------|
| `image.operator.repository`   | `ghcr.io/ai-dynamo/snapshot/operator`   | Operator image.                                                                  |
| `image.agent.repository`      | `ghcr.io/ai-dynamo/snapshot/agent`      | Agent image.                                                                     |
| `image.pageBroker.repository` | `ghcr.io/ai-dynamo/snapshot/pagebroker` | PageBroker sidecar image, pulled at `image.agent.tag`.                           |
| `image.operator.tag`          | chart `appVersion`                      | Operator image tag; defaults to the chart's `appVersion` when empty.             |
| `image.agent.tag`             | chart `appVersion`                      | Agent and PageBroker image tag; defaults to the chart's `appVersion` when empty. |
| `crdUpgrade.enabled`          | `true`                                  | Re-apply the CRDs on every rollout via an init container.                        |
| `crdUpgrade.logLevel` | `info` | Init container log level. |
| `runtime.type`                | `containerd`                            | Container runtime: `containerd` or `crio`.                                       |
| `runtime.socketPath`          | `""`                                    | Runtime socket path; empty uses the conventional path for the type.              |
| `runtime.storageDir` | `""` | Host per-container storage dir, mounted at the identical path in the agent (empty = default for `runtime.type`). |
| `openshift.enabled`           | `false`                                 | Enable OpenShift RBAC/SCC pieces. Keep `false` on vanilla Kubernetes.            |

## Storage

| Value                                     | Default        | Description                                                                                                                                                                                               |
|-------------------------------------------|----------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `pageBroker.restoreMode`                  | `direct`       | PageBroker restore source: `staged` copies CPU images into staging, `direct` reads the original artifact.                                                                                                 |
| `storage.type`                            | `pvc`          | Snapshot store backend: `pvc` or `s3`.                                                                                                                                                                  |
| `storage.pvc.create`                      | `true`         | Create the PVC; set `false` to use an existing one.                                                                                                                                                       |
| `storage.pvc.name`                        | `snapshot-pvc` | Shared PVC name.                                                                                                                                                                                          |
| `storage.pvc.size`                        | `1Ti`          | Requested size.                                                                                                                                                                                           |
| `storage.pvc.storageClass`                | `""`           | Storage class; empty uses the cluster default. Must support `ReadWriteMany`.                                                                                                                              |
| `storage.pvc.basePath`                    | `/checkpoints` | Fixed agent mount path; cannot be changed.                                                                                                                                                                |
| `storage.s3.endpoint`                     | `""`           | Required with `storage.type: s3`. S3-compatible endpoint, an absolute http(s) URL.                                                                                                                        |
| `storage.s3.region`                       | `""`           | Required with `storage.type: s3`. Bucket region.                                                                                                                                                          |
| `storage.s3.bucket`                       | `""`           | Required with `storage.type: s3`. Bucket holding the snapshots.                                                                                                                                         |
| `storage.s3.prefix`                       | `""`           | Key prefix inside the bucket; empty is the bucket root.                                                                                                                                                   |
| `storage.s3.credentialsSecretRef`         | `""`           | Required with `storage.type: s3`. Secret holding a shared-credentials file, mounted into PageBroker and the operator.                                                                                     |
| `storage.s3.credentialsSecretKey`         | `credentials`  | Key in that Secret holding the credentials file.                                                                                                                                                          |
| `storage.s3.addressing`                   | `""`           | Bucket addressing PageBroker uses: `virtual` (its default when empty) or `path`.                                                                                                                          |
| `storage.s3.allowHttp`                    | `false`        | Accept a plain-http endpoint.                                                                                                                                                                             |
| `storage.s3.caBundleConfigMapRef`         | `""`           | ConfigMap holding the CA bundle for the endpoint's TLS certificate, mounted into PageBroker and the operator.                                                                                             |
| `storage.s3.caBundleConfigMapKey`         | `ca.crt`       | Key in that ConfigMap holding the bundle.                                                                                                                                                                 |
| `pageBroker.artifactAddressing`           | `false`        | Pass store configuration to a compatible PageBroker artifact backend. Deploy this before enabling `operator.bindNewContents`.                                                                             |
| `operator.bindNewContents`                | `false`        | Bind new snapshots to the configured store. Enable after all agents have completed the compatible artifact-backend rollout; see [storage activation](../operate/configure-storage.md#enable-artifact-addressing). Required for `storage.type: s3`. |
| `pageBroker.s3.limits`                    | `{}`           | PageBroker's S3 transfer and transaction limits; see [S3 Transfer Limits](#s3-transfer-limits) for the keys and bounds.                                                                                |
| `pageBroker.transferEngine.modelStreamer` | `{}`           | Tuning for the engine that reads S3 restores, written as given; keys are in [Model Streamer configuration](../../agent/pagebroker/MODEL_STREAMER.md).                                                |
| `pageBroker.transferEngine.modelStreamer.restoreBatchBytes` | `10000000000` | S3 restore batch and shared admission budget in bytes; an oversized file runs alone. Does not cap total staging memory. |
| `pageBroker.transferEngine.modelStreamer.restorePreparationSlots` | `3` | Retained batches per S3 restore, including preparation in progress; 1–20. Does not increase native download admission limits. |

## S3 Transfer Limits

`pageBroker.s3.limits` accepts PageBroker's own keys, with its bounds. The chart
rejects a key PageBroker does not know, an `activeTransactions` at or above
`pageBroker.maxConcurrentRequests`, and an `addressing` other than `path` or
`virtual`; PageBroker enforces the remaining bounds at startup.

| Key | Default | Bounds |
|-----|---------|--------|
| `transactionSeconds` | 7200 | 1 to 7200 |
| `stagingBytes` | the staging volume cap, `pageBroker.staging.sizeLimit` as lowered in [How the Cap Is Chosen](../operate/configure-storage.md#how-the-cap-is-chosen), and at most 1 TiB | 1 byte to 1 TiB |
| `activeTransactions` | 8 | 1 to 1024, below `pageBroker.maxConcurrentRequests` |
| `uploadPartBytes` | 64 MiB | 8 MiB to 1 GiB |
| `uploadBufferBytes` | 1 GiB | at most 16 GiB, and at least `uploadActiveFiles` × (`uploadPartBytes` rounded up to a whole MiB, plus 1 MiB) |
| `uploadActiveFiles` | 2 | 1 to 64 |
| `requestSeconds` | 60 | 1 to 60 |
| `uploadRequestRetries` | 3 | 0 to 10 |
| `uploadConnectSeconds` | the smaller of 5 and `requestSeconds` | 1 to `requestSeconds` |

## PageBroker GPU Engine

PageBroker runs in a persistent container in the agent DaemonSet. Its GPU engine
uses CustomStorage for checkpoints when the driver supports it; otherwise a
checkpoint uses the driver-managed format. The agent runs CRIU. GPU payloads
are read directly from the original snapshot.
`pageBroker.restoreMode` selects whether CRIU reads CPU images directly (the
default) or from tmpfs staging. Restore uses the format recorded in the snapshot.

| Value                              | Default                                    | Description                                       |
|------------------------------------|--------------------------------------------|---------------------------------------------------|
| `pageBroker.maxConcurrentRequests` | `16`                                       | Concurrent control request limit.                 |
| `pageBroker.transferBufferCount`   | `32`                                       | Persistent ring slots per visible GPU.            |
| `pageBroker.transferChunkBytes`    | `134217728`                                | Bytes per slot before allocation rounding.        |
| `pageBroker.maxPinnedBytes`        | `0`                                        | Total pinned memory limit. Zero sets no limit.    |
| `pageBroker.resources`             | 8 CPU / 32Gi request, 32 CPU / 256Gi limit | Resources for the broker and embedded GPU engine. |

The default rings reserve 4 GiB per GPU, or 32 GiB for eight GPUs, plus overhead.
PageBroker checks the memory budget before it reports GPU readiness. It checks
manifest structure and exact extent lengths. It does not generate or verify GPU
payload checksums.

## Operator

| Value | Default | Description |
|-------|---------|-------------|
| `operator.resources` | 50m CPU / 64Mi request, 500m CPU / 256Mi limit | CPU and memory requests/limits for the operator manager. |
| `operator.nodeSelector` | `{}` | Node selector for the operator pod. |
| `operator.affinity` | `{}` | Affinity rules for the operator pod. |
| `operator.tolerations` | `[]` | Tolerations for the operator pod. |

## Agent DaemonSet and Access

| Value                          | Default                                 | Description                                                                                                                               |
|--------------------------------|-----------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------|
| `daemonset.snapshotLogLevel`   | `info`                                  | Agent log level (`trace`/`debug`/`info`/`warn`/`error`).                                                                                  |
| `daemonset.resources` | 2 CPU / 1Gi request, 4 CPU / 64Gi limit | Agent resource requests and limits. The memory limit also bounds the largest CRIU image, which CRIU writes into PageBroker staging. A GPU CustomStorage checkpoint writes directly to the PVC instead and is not bound by this. |
| `pageBroker.staging.sizeLimit` | `56Gi` | Cap on the memory-backed staging volume shared by the agent and PageBroker, used for a checkpoint (except GPU CustomStorage) and a staged restore. |
| `daemonset.nodeSelector`       | `nvidia.com/gpu.present: "true"`        | Targets GPU nodes.                                                                                                                        |
| `daemonset.tolerations`        | GPU + `dedicated`                       | Node tolerations.                                                                                                                         |
| `daemonset.affinity` | `{}` | Affinity for the agent pod. The chart adds `kubernetes.io/arch In (amd64, arm64)` to every non-empty required node selector term, so a term with `example.com/pool DoesNotExist` keeps the agent off those nodes on supported architectures. Empty and null terms still match no nodes. |
| `daemonset.runtimeClassName`   | `nvidia`                                | RuntimeClass for the agent pod; `""` omits it, for nodes where the NVIDIA runtime is containerd's default.                                |
| `daemonset.imagePullSecrets`   | `[]`                                    | Pull secrets for the agent image; empty for the public GHCR image.                                                                        |
| `seccomp.deploy`               | `true`                                  | Install the block-iouring seccomp profile (required for CRIU; set `false` on RHCOS 9.6+).                                                 |
| `rbac.create`                  | `true`                                  | Create agent and operator RBAC.                                                                                                           |
| `serviceAccount.create`        | `true`                                  | Create the agent service account.                                                                                                         |

## Pod Groups

Pod groups are alpha. With `podGroups.enabled`, each agent pod runs the pod
group coordinator in a container of its own, `coordinator`, which listens on
the node's IP. It runs as a non-root user with no capabilities, on the host's
network, so it cannot bind a port below 1024. It shares the agent's
ServiceAccount, and with it the agent's API rights. Pod groups need a newer
Kubernetes than the baseline; see
[Prerequisites](../get-started/prerequisites.md#pod-groups).

| Value                                 | Default                                     | Description                                                                                                                        |
|---------------------------------------|---------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------|
| `podGroups.enabled`                   | `false`                                     | Turn pod groups on, with everything the chart deploys for them. The chart installs their CRDs either way.                          |
| `coordinator.port`                    | `4224`                                      | Port agents connect to over TLS, on the node's IP. 1024 to 65535.                                                                  |
| `coordinator.healthPort`              | `4225`                                      | Port of the coordinator's health endpoint, on the node's IP. 1024 to 65535. The agent pod is ready only once the coordinator is.   |
| `coordinator.maxMemoryBytes`          | `1073741824`                                | Memory, in bytes, that every group's session shares, at least about 8 MiB. A group's writes fail once it is full.                  |
| `coordinator.maxValueBytes`           | `32505856`                                  | Largest value, in bytes, an agent may write to a session.                                                                          |
| `coordinator.maxLibraryValueBytes`    | `4096`                                      | Largest value, in bytes, a communication library may write to a session.                                                           |
| `coordinator.maxLibraryKeys`          | `4096`                                      | How many keys communication libraries may write to a session, deleted ones included.                                               |
| `coordinator.resources`               | 100m CPU / 128Mi request, 1 CPU / 2Gi limit | Coordinator resources. The memory limit is also its `GOMEMLIMIT`. Keep it at about twice `coordinator.maxMemoryBytes`.             |
| `coordinator.tls.serving.secretName`  | `""`                                        | Your own `kubernetes.io/tls` Secret with `tls.crt` and `tls.key`, in the release namespace. Empty: the operator issues one.        |
| `coordinator.tls.trust.configMapName` | `""`                                        | Your own ConfigMap holding the CA bundle agents trust, in the release namespace.                                                   |
| `coordinator.tls.trust.secretName`    | `""`                                        | Your own Secret holding that CA bundle, in the release namespace.                                                                  |
| `coordinator.tls.trust.key`           | `ca.crt`                                    | Key holding the CA bundle in your ConfigMap or Secret.                                                                             |
| `coordinator.tls.serverName`          | `""`                                        | DNS name your serving certificate carries, which agents verify. It is never resolved. Empty: `<fullname>-coordinator.<namespace>`. |

With `podGroups.enabled`, the chart also deploys these:

- With `rbac.create`, the coordinator's ClusterRole and ClusterRoleBinding,
  `<fullname>-agent-coordinator`, for the agents' ServiceAccount.
- The operator's issuer of the coordinator's TLS certificate, unless you bring
  your own. With `rbac.create`, its Role and RoleBinding,
  `<fullname>-operator-coordinator-tls`, for the operator's ServiceAccount.

The coordinator's `/tmp` is an `emptyDir` in memory, sized to
`coordinator.maxMemoryBytes` plus 64 MiB. It holds the control buckets' file
store, which `coordinator.maxMemoryBytes` also bounds.

By default, the operator issues the coordinator's certificate under a private
CA, and writes three resources in the release namespace:
`<fullname>-coordinator-ca`, `<fullname>-coordinator-tls`, and
`<fullname>-coordinator-ca-bundle`. To bring your own instead, for example from
cert-manager and trust-manager, set `coordinator.tls.serving.secretName` and
exactly one of `coordinator.tls.trust.configMapName` and
`coordinator.tls.trust.secretName`. The operator then issues nothing. The chart
refuses one without the other, `coordinator.tls.serverName` or
`coordinator.tls.trust.key` without your own resources, and a name the operator
writes. See
[Security Boundaries](../overview/architecture-overview.md#security-boundaries).

The operator replaces its CA near expiry, or when the CA Secret is lost or
damaged. After a replacement near expiry, the trust bundle keeps the replaced
CA until it expires; a lost or damaged CA Secret loses it. A replacement can
disrupt group attempts that reconnect while it propagates. Recovery needs both
sides to reload from their mounted files: each coordinator its serving
certificate, and each agent its trust in `ca.crt`. The kubelet refreshes those
files on each node's own schedule.

### Turn Pod Groups Off

To turn pod groups off, delete the group resources before you change the
value. With pod groups off, their finalizers keep group resources from being
deleted:

1. Delete every `PodGroupSnapshot` and `PodGroupRestore`. `kubectl delete`
   waits until they are gone:

   ```bash
   kubectl delete podgroupsnapshots,podgrouprestores --all --all-namespaces
   ```

2. Check that none is left. The command prints `No resources found`:

   ```bash
   kubectl get podgroupsnapshots,podgrouprestores --all-namespaces
   ```

3. Set `podGroups.enabled=false` with `helm upgrade`.

## Agent Config (`config.*`)

`config.*` renders into the agent ConfigMap.

- `config.overlay.exclusions` - Rootfs-diff tar exclusions. Default: `/proc`, `/sys`, `/dev`, `*/.cache/huggingface`, `*/__pycache__`, `*.pyc`. A file the checkpointed processes hold open or mapped at dump time is archived even when an exclusion matches it, so CRIU can reopen it. Other excluded files are not restored; the workload must recreate them if needed.
- `config.checkpoint.timeoutSeconds` - Maximum seconds for a checkpoint before the agent marks it failed. Default `3600`.
- `config.restore.timeoutSeconds` - Maximum seconds for a restore before the agent marks it failed. Default `7200`.

`config.criu.*` - CRIU options:

| Value               | Default                                | Description                                                                              |
|---------------------|----------------------------------------|------------------------------------------------------------------------------------------|
| `binaryPath`        | `/usr/local/sbin/criu`                 | Path to the criu binary.                                                                 |
| `ghostLimit`        | `536870912`                            | Max size in bytes of a deleted-but-open file saved inline as a ghost file.               |
| `logLevel`          | `4`                                    | CRIU verbosity (0–4).                                                                    |
| `workDir`           | `/var/criu-work`                       | CRIU temporary-file directory.                                                           |
| `shellJob`          | `true`                                 | Treat containers as session leaders.                                                     |
| `tcpClose`          | `false`                                | Close non-listening TCP sockets on restore.                                              |
| `tcpEstablished`    | `true`                                 | Preserve established TCP sockets. `tcpClose` and `tcpEstablished` cannot both be `true`. |
| `fileLocks`         | `true`                                 | Preserve file locks.                                                                     |
| `orphanPtsMaster`   | `true`                                 | Support containers with TTYs.                                                            |
| `extUnixSk`         | `true`                                 | External Unix sockets.                                                                   |
| `linkRemap`         | `true`                                 | Support deleted-but-open files (e.g. `/dev/shm` semaphores).                             |
| `extMasters`        | `true`                                 | External bind-mount masters.                                                             |
| `manageCgroupsMode` | `soft`                                 | CRIU cgroup mode: `ignore` / `soft` / `full` / `strict`.                                 |
| `imageIoMode`       | `direct`                               | CRIU image I/O: `writeback` or `direct`.                                                 |
| `rstSibling`        | `true`                                 | Restore as a sibling process (required for go-criu swrk mode).                           |
| `mntnsCompatMode`   | `false`                                | Mount-namespace compatibility mode, applied during restore.                              |
| `evasiveDevices`    | `true`                                 | Use any device path when the original is inaccessible.                                   |
| `forceIrmap`        | `true`                                 | Force resolving inotify/fsnotify watch names.                                            |
| `autoDedup`         | `false`                                | Auto-deduplicate memory pages.                                                           |
| `lazyPages`         | `false`                                | Lazy page migration (experimental).                                                      |
| `libDir`            | `/usr/local/lib/snapshot/criu-plugins` | CRIU plugin directory used by the chart.                                                 |
| `allowUprobes`      | `true`                                 | Kernel/userspace probe compatibility.                                                    |
| `skipInFlight`      | `true`                                 | Skip in-flight TCP connections.                                                          |
