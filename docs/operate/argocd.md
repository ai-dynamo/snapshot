<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Gate restore on checkpoint completion with Argo CD

Put a `SnapshotJob` in an earlier Argo CD sync wave and a restoring
`Deployment` in a later wave. A custom health check holds restore until
`Completed=True`. `Captured=True` is insufficient: non-target containers may
still be running, and their failure must block restore.

Use ordinary Sync resources in **one Application**. Waves do not coordinate
separate Applications, and selective sync bypasses this ordering. See
[Argo CD sync waves](https://argo-cd.readthedocs.io/en/release-3.0/user-guide/sync-waves/).

## Prerequisites

Install [Snapshot](../get-started/installation.md), configure the
[snapshot store](configure-storage.md), and meet the
[vLLM guide prerequisites](../get-started/prerequisites.md).
The examples use vLLM with Qwen3-0.6B, one NVIDIA GPU, and a model-cache PVC.
The restore command stays inert on nodes without the restore proxy. Readiness
and liveness check `/health`, which calls the vLLM engine's health check. The
startup probe waits for restore completion and API readiness before those
checks begin. See the
[Restore Pod contract](../reference/restore-pod-contract.md#nodes-without-the-restore-proxy)
for the differences in process exit and signal handling.

Use an existing [Argo CD installation](https://argo-cd.readthedocs.io/en/stable/operator-manual/installation/).
An Argo administrator must configure the health check before the Application
syncs.

Validated with Argo CD 3.0.6 and Snapshot commit
`67664dfa43f08f1f9f92a2e9ec00668f00c7b476`. Cross-node restore, shared-store HA,
other GPU/driver/Argo CD combinations, and cascading deletion were not tested.
The no-proxy path was smoke-tested for engine health and startup gating. It has
not been validated end to end through restore and inference.

## Configure health

Merge the `data` entries from [argocd-cm.yaml](argocd/argocd-cm.yaml) into the
administrator-owned `argocd-cm`, preserving its other keys. If Helm or GitOps
owns that ConfigMap, update its source. Keep this fragment outside the workload
Application.

The health script checks failure first, regardless of condition order:

| `SnapshotJob` conditions                            | Argo health   |
| --------------------------------------------------- | ------------- |
| No terminal condition, including `Captured=True`    | Progressing   |
| `Completed=True`, without `Failed=True`             | Healthy       |
| `Failed=True`, including alongside `Completed=True` | Degraded      |

Condition statuses are strings (`"True"`). The failure diagnostic uses the
condition message, then reason, then a fallback. See
[custom health checks](https://argo-cd.readthedocs.io/en/release-3.0/operator-manual/health/).

Inspect the `SnapshotJob` health through the UI or `argocd app get`. In Argo CD
3.0, resource health is absent from the Application CR by default; the overall
Application may show Missing while the later-wave `Deployment` is absent. See
[Argo CD 3.0 health status](https://argo-cd.readthedocs.io/en/release-3.0/operator-manual/upgrading/2.14-3.0/#health-status-in-the-application-cr).

The fragment also maps `spec.podTemplate.spec` to `core/v1/PodSpec` through
`resource.customizations.knownTypeFields.nvidia.com_SnapshotJob`. This
normalizes equivalent PodSpec values, such as resource quantities; it does not
control health. See
[known Kubernetes types in CRDs](https://argo-cd.readthedocs.io/en/release-3.0/user-guide/diffing/#known-kubernetes-types-in-crds-resource-limits-volume-mounts-etc).

## Prepare the Application

The [checkpoint manifest](argocd/capture.yaml) creates `worker-r1` in wave `0`.
The [restore manifest](argocd/restore.yaml) creates `worker-r1-restored` in wave
`1`. From this repository's root, prepare their files and dependencies:

```bash
mkdir -p /tmp/snapshot-argocd-workload
cp docs/operate/argocd/capture.yaml docs/operate/argocd/restore.yaml \
  /tmp/snapshot-argocd-workload/
kubectl annotate --local -f docs/using-snapshot/model-cache-pvc.yaml \
  argocd.argoproj.io/sync-wave=-1 -o yaml \
  > /tmp/snapshot-argocd-workload/model-cache.yaml
kubectl create configmap vllm-app \
  --from-file=app.py=docs/using-snapshot/vllm/app.py --dry-run=client -o yaml \
  | kubectl annotate --local -f - argocd.argoproj.io/sync-wave=-1 -o yaml \
  > /tmp/snapshot-argocd-workload/app.yaml
```

The ConfigMap and model-cache PVC use wave `-1`. Adjust the PVC's size and
storage class; the copied claim requests 2 TiB RWX. Its binding must finish
without a consumer Pod: `WaitForFirstConsumer` can block this wave.

Commit these four files to an Argo-readable Git repository. Configure a
directory Application targeting a pre-created namespace. Disable automatic
pruning for the first verification. Configure any serving Service to select
`app: worker-r1-restored`, excluding source Pods. Disable injected sidecars or
configure them to finish with the checkpoint workload.

The restore annotation belongs on the `Deployment` pod template and names the
same-namespace `PodSnapshot`, `worker-r1`. Keep it consistent with the
`SnapshotJob` name. Both manifests use container name `main` and follow the
[Restore Pod contract](../reference/restore-pod-contract.md).

A `Deployment` requires `restartPolicy: Always`, but a Pod gets only one
restore. If its restored process exits, replace the Pod; container restart
cannot repeat the restore and may enter `CrashLoopBackOff`.

## Verify success and failure

Set `APP` to the Application name and `WORKLOAD_NAMESPACE` to its destination
namespace. Start a full sync:

```bash
argocd app sync "$APP"
```

From another terminal, inspect the checkpoint and restore:

```bash
argocd app get "$APP"
kubectl --namespace "$WORKLOAD_NAMESPACE" get snapshotjob worker-r1 -o yaml
kubectl --namespace "$WORKLOAD_NAMESPACE" get deployment worker-r1-restored
```

Before completion, expect Progressing health and no restore `Deployment` in a
fresh Application. After `Completed=True`, expect Healthy and creation of the
`Deployment`. Confirm the restore Pod has `nvidia.com/Restored=True` and is
Ready, then test inference:

```bash
kubectl --namespace "$WORKLOAD_NAMESPACE" get pods -l app=worker-r1-restored -o yaml
kubectl --namespace "$WORKLOAD_NAMESPACE" rollout status \
  deployment/worker-r1-restored --timeout=30m
kubectl --namespace "$WORKLOAD_NAMESPACE" port-forward \
  deployment/worker-r1-restored 8000:8000
```

From another terminal:

```bash
curl --fail http://localhost:8000/generate \
  -H 'Content-Type: application/json' \
  -d '{"prompt":"Explain GPU checkpointing in one sentence."}'
```

Test failure in a separate Application and namespace with new resource names
and a matching restore reference. In its checkpoint manifest:

- Set `spec.activeDeadlineSeconds: 60`.
- Remove the model-download init container.
- Set `main.command` to `["/bin/sh", "-c", "exit 1"]` and `main.args` to `[]`.

Sync and verify `Failed=True`, Degraded health, and no later-wave `Deployment`.
Inspect the source Job and Pods if these differ. Degraded health may precede
sync-operation failure; inspect both in `argocd app get`.

After terminal `Failed=True`, retry with a new `SnapshotJob` name and restore
reference. Snapshot manages checkpoint retries before that terminal state. Argo
self-heal does not take a new snapshot after completion or repair missing
artifacts.

## Reuse and retain snapshots

Keep `worker-r1` when resyncing, scaling to zero and back, or replacing a
restore Pod. Use a new name, such as `worker-r2`, for a new image, model, or
checkpoint configuration, and update the restore reference. Do not edit a
completed `SnapshotJob`'s pod template or use checkpoint hooks recreated on each
sync.

The checkpoint manifest uses `Prune=false,Delete=false` to retain the
`SnapshotJob` during pruning and Application deletion. Keep revisions needed
for serving or rollback in Git, along with their ConfigMaps and model-cache
data. Removing a retained `SnapshotJob` from Git leaves the Application OutOfSync.

For Application retirement, use non-cascading deletion. Retain the
`SnapshotJob`, generated `PodSnapshot`, bound `PodSnapshotContent`, and
snapshot artifacts until no serving or rollback reference needs them.
Namespace, PVC, and direct API deletion bypass Argo retention. Follow
[storage retention](configure-storage.md#retention) for cleanup.

On disposable resources, compare snapshot resource UIDs and content binding
after resync, scale to zero/back, and restore Pod replacement. Confirm reuse
with an inference request. Test pruning and non-cascading Application deletion
separately before relying on retention.

## Inspect differences

The examples need no `ignoreDifferences` rule. Inspect the desired/live diff
before adding one; scope it to the observed field and named resource, and
explain the drift it hides. Do not ignore an entire pod template.
`RespectIgnoreDifferences=true` also changes sync behavior. See
[diff customization](https://argo-cd.readthedocs.io/en/release-3.0/user-guide/diffing/)
and
[respecting ignored differences during sync](https://argo-cd.readthedocs.io/en/release-3.0/user-guide/sync-options/#respect-ignore-differences-configs).
