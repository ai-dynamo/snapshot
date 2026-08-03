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

Annotated checkpoints using `nvidia.com/snapshot-pagebroker: "true"` require
`storage.accessMode=agentMount`, because PageBroker must write and promote a
temporary directory on the checkpoint PVC. With `podMount`, PageBroker cannot
access the PVC directly and the agent falls back to its existing checkpoint
staging path. Restore behavior for the annotation is unchanged.

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

## CRD upgrades

Helm creates the CRDs in [crds/](./crds) on a fresh install and then leaves them
alone: `helm upgrade` never updates them. To close that gap the operator
Deployment runs a `crd-installer` init container before the manager starts. It
server-side applies the CRDs embedded in the `api` module — the same manifests
`make generate` mirrors into `crds/` — so every rollout converges the cluster on
definitions the running binary agrees with. Nothing to update means nothing is
written:

```bash
kubectl logs deployment/snapshot-operator -n ${NAMESPACE} -c crd-installer
```

```text
Applied CRD  {"name": "podsnapshots.nvidia.com", "action": "unchanged"}
CRDs already up to date, no changes applied  {"count": 2}
```

The installer runs whenever the operator pod is recreated, which a `helm upgrade`
does as soon as the image tag moves — `image.operator.tag` defaults to the
chart's `appVersion`, so a release that changes the CRDs changes the tag too. If
you pin a mutable tag such as `latest`, rebuilding it does not change the pod
spec and nothing rolls; restart the Deployment yourself to pick up new
definitions.

`rbac.create=true` grants the operator ServiceAccount `get`/`patch` on the two
CRDs in [crds/](./crds) by name, plus an unscoped `create` — RBAC cannot match
`resourceNames` on create, so that verb only ever lets the operator add a CRD,
never modify one it does not own.

Set `crdUpgrade.enabled=false` when CRDs are managed out of band, for example by
GitOps or by a cluster admin who does not want the operator holding
`apiextensions` permissions. That drops the init container and the extra RBAC —
and makes updating the CRDs your responsibility after every chart upgrade:

```bash
kubectl apply --server-side --force-conflicts -f ./charts/snapshot/crds/
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
| `crdUpgrade.enabled` | Install and upgrade the CRDs from an operator init container (see below) | `true` |
| `crdUpgrade.logLevel` | Init container log level | `info` |
| `rbac.create` | Create agent and operator RBAC | `true` |
| `rbac.namespaceRestricted` | Namespace-scoped agent RBAC (required for PVC storage) | `true` |
| `openshift.enabled` | OpenShift RBAC / SCC-related chart pieces | `false` |

Reserved `s3` and `oci` values remain chart-owned placeholders for future
snapshot backends, but only `pvc` is implemented today.

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
