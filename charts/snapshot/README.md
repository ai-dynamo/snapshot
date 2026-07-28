# Snapshot Helm Chart

> Experimental feature. The agent runs as a privileged DaemonSet to perform
> CRIU checkpoint and restore operations.

This chart installs the snapshot infrastructure:

- the operator Deployment that reconciles `PodSnapshot` and `PodSnapshotContent`
- the agent DaemonSet on eligible GPU nodes
- `snapshot-pvc`, or wiring to an existing PVC
- namespace-scoped agent RBAC and cluster-scoped operator RBAC
- the seccomp profile CRIU needs

By default, install the chart in each namespace where you want checkpoint and
restore; in that mode the chart can create or reuse that namespace's PVC.

Alternatively, install one cluster-scoped agent in an infrastructure namespace
by setting `storage.accessMode=podMount` and `rbac.namespaceRestricted=false`.
In that mode the DaemonSet does not mount the checkpoint PVC directly. Instead,
the operator mounts the namespace-local checkpoint PVC into checkpoint/restore
workload pods, and the agent reaches that PVC through the target pod's mount
namespace. The operator can create those namespace-local workload PVCs, or it
can require that they already exist.

## Prerequisites

- Kubernetes cluster with x86_64 GPU nodes
- NVIDIA driver 580.xx or newer
- **containerd** or **CRI-O** (chart defaults to containerd; see below for CRI-O / OpenShift)
- a cluster where a privileged DaemonSet with `hostPID`, `hostIPC`, and `hostNetwork` is acceptable

In the default `agentMount` mode, the agent DaemonSet mounts the checkpoint PVC
directly. On a multi-node GPU cluster that means agent pods on multiple nodes
may mount the same PVC, so the PVC generally needs `ReadWriteMany`. The chart
defaults to that mode.

`podMount` removes that agent-side RWX requirement. The agent does not mount the
PVC directly; only the active checkpoint/restore workload pod mounts it, and the
agent reaches it through that pod's mount namespace. That allows suitable
`ReadWriteOnce` storage classes for sequential checkpoint/restore workflows, as
long as the backend can attach the volume to the node running that workload pod.

Because `podMount` reaches storage through `/host/proc/<pid>/root`, the target
container must still be alive and visible through host proc when the agent starts
checkpoint or restore. A container restart, exited placeholder, runtime PID
lookup failure, or node security policy that blocks host proc traversal will fail
or defer that attempt until reconciliation sees a fresh container.

## CRI-O and OpenShift

For CRI-O nodes set `runtime.type=crio`. Only set `runtime.socketPath` if the CRI
socket is not the default for that type (see `values.yaml`). On OpenShift, set
`openshift.enabled=true` so the chart emits the extra RBAC and pod annotations
the agent needs. Example:

```bash
helm upgrade --install snapshot ./charts/snapshot \
  --namespace "${NAMESPACE}" --create-namespace \
  --set storage.pvc.create=true \
  --set runtime.type=crio \
  --set openshift.enabled=true
```

## Minimal install

Create the checkpoint PVC and the agent:

```bash
helm upgrade --install snapshot ./charts/snapshot \
  --namespace ${NAMESPACE} \
  --create-namespace \
  --set storage.pvc.create=true
```

If your cluster does not use a default storage class, also set
`storage.pvc.storageClass`.

Reuse an existing PVC instead:

```bash
helm upgrade --install snapshot ./charts/snapshot \
  --namespace ${NAMESPACE} \
  --create-namespace \
  --set storage.pvc.create=false \
  --set storage.pvc.name=my-snapshot-pvc
```

## Verify

```bash
kubectl get pvc snapshot-pvc -n ${NAMESPACE}
kubectl rollout status daemonset/snapshot-agent -n ${NAMESPACE}
kubectl get pods -n ${NAMESPACE} -l app.kubernetes.io/name=snapshot -o wide
```

## Important values

| Parameter | Meaning | Default |
|-----------|---------|---------|
| `image.operator.repository` | Operator image repository | `ghcr.io/ai-dynamo/snapshot/operator` |
| `image.agent.repository` | Agent image repository | `ghcr.io/ai-dynamo/snapshot/agent` |
| `image.agent.tag` | Agent image tag (empty = chart appVersion) | `""` |
| `storage.type` | Snapshot-owned storage backend | `pvc` |
| `storage.accessMode` | `agentMount` for namespace-local agent PVC mount, or `podMount` for a single cluster agent that accesses workload-mounted PVCs | `agentMount` |
| `storage.pvc.create` | Create `snapshot-pvc` instead of using an existing PVC in `agentMount` mode | `true` |
| `storage.pvc.name` | Checkpoint PVC name. Mounted by the agent in `agentMount` mode, or by workload pods in `podMount` mode | `snapshot-pvc` |
| `storage.pvc.size` | Requested PVC size | `1Ti` |
| `storage.pvc.storageClass` | Storage class name | `""` |
| `storage.pvc.accessMode` | Access mode for the checkpoint PVC. `ReadWriteMany` is safest; `ReadWriteOnce` can be used with `podMount` for sequential checkpoint/restore on suitable storage backends | `ReadWriteMany` |
| `storage.pvc.basePath` | Mount path for checkpoint storage: inside the agent pod in `agentMount`, or inside checkpoint/restore workload pods in `podMount` | `/checkpoints` |
| `seccomp.deploy` | Deploy the CRIU seccomp profile ConfigMap and init container. Use this field name; `seccomp.enabled` is not a chart value | `true` |
| `runtime.type` | CRI backend: `containerd` or `crio` | `containerd` |
| `runtime.socketPath` | CRI socket (empty = default for `runtime.type`) | `""` |
| `rbac.create` | Create agent and operator RBAC | `true` |
| `rbac.namespaceRestricted` | Namespace-scoped agent RBAC (required for PVC storage) | `true` |
| `openshift.enabled` | OpenShift RBAC / SCC-related chart pieces | `false` |
| `crds.enabled` | Render the CRDs as chart templates (set false to manage them out-of-band) | `true` |
| `crds.keep` | Add `helm.sh/resource-policy: keep` so uninstall does not delete the CRDs (set false to cascade-delete CRDs and their CRs on uninstall) | `true` |

Reserved `s3` and `oci` values remain chart-owned placeholders for future
snapshot backends, but only `pvc` is implemented today.

## CRDs

The CRDs render as ordinary chart templates (under `templates/crd/`), so
`helm upgrade` reapplies the current schema — unlike Helm's `crds/` directory,
which installs CRDs only on the first `helm install`. By default each CRD carries
`helm.sh/resource-policy: keep` (`crds.keep=true`), so `helm uninstall` leaves the
CRDs (and their custom resources) in place rather than cascade-deleting them. Set
`crds.keep=false` to let uninstall remove the CRDs — which cascade-deletes every
`PodSnapshot` / `PodSnapshotContent` in the cluster (objects that still hold
finalizers may sit in `Terminating` until their controller releases them).

They are single-version (`nvidia.com/v1alpha1`) with no conversion webhook, so
schema changes must stay backward-compatible — the new schema is applied in place
over the existing one.

Set `crds.enabled=false` to manage the CRDs out-of-band. ArgoCD and similar GitOps
controllers ignore `helm.sh/resource-policy` and apply/prune CRDs on their own, so
those users should set `crds.enabled=false` and manage the CRDs in the GitOps tool.

### Upgrading from a build that installed CRDs via `crds/`

If a cluster already has these CRDs — from an earlier chart version that shipped
them under `crds/`, or from another installer — the first templated `helm upgrade`
fails with `invalid ownership metadata` because the existing CRDs are not owned by
this release. Adopt them once before upgrading (or use `helm upgrade --take-ownership`
where supported):

```bash
REL=snapshot; NS=<release-namespace>
for c in podsnapshots.nvidia.com podsnapshotcontents.nvidia.com; do
  kubectl label  crd "$c" app.kubernetes.io/managed-by=Helm --overwrite
  kubectl annotate crd "$c" \
    meta.helm.sh/release-name="$REL" \
    meta.helm.sh/release-namespace="$NS" --overwrite
done
```

Fresh clusters need none of this. The same step applies if you later reinstall
under a different release name (the kept CRD is again unowned by the new release).

See [values.yaml](./values.yaml) for the full configuration surface.

## Uninstall

```bash
helm uninstall snapshot -n ${NAMESPACE}
```

The chart does not delete checkpoint data automatically. Remove the PVC yourself
if you want to clear stored checkpoints:

```bash
kubectl delete pvc snapshot-pvc -n ${NAMESPACE}
```
