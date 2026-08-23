# Snapshot Helm Chart

> Experimental feature. The agent runs as a privileged DaemonSet to perform
> CRIU checkpoint and restore operations.

This chart installs the snapshot infrastructure:

- the operator Deployment that reconciles `SnapshotJob`, `PodSnapshot`, and
  `PodSnapshotContent`
- the agent DaemonSet on eligible GPU nodes
- `snapshot-pvc`, or wiring to an existing PVC
- cluster-scoped agent and operator RBAC
- the seccomp profile CRIU needs

Install one Snapshot release for the cluster. Every node agent mounts the same
shared checkpoint PVC directly and watches checkpoint/restore pods in all
namespaces. Workload pods never mount checkpoint storage.

## Prerequisites

- Kubernetes cluster with x86_64 GPU nodes
- NVIDIA driver 580.xx or newer
- **containerd** or **CRI-O** (chart defaults to containerd; see below for CRI-O / OpenShift)
- a cluster where a privileged DaemonSet with `hostPID`, `hostIPC`, and `hostNetwork` is acceptable

The checkpoint PVC must support `ReadWriteMany` because agents on multiple nodes
mount it concurrently. Chart-created claims always request `ReadWriteMany` and
are retained when the Helm release is removed.

An existing `ReadWriteOnce` claim cannot be upgraded in place because PVC access
modes are immutable. When `storage.pvc.create=false`, the chart assumes the named
existing claim supports `ReadWriteMany`; verify its access mode before installing
or upgrading. To migrate, create a new RWX claim, copy the retained checkpoint
data once if it must be preserved, then set `storage.pvc.create=false` and
`storage.pvc.name` to the new claim. The old retained claim remains untouched.

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
Applied CRD  {"name": "podsnapshotcontents.nvidia.com", "action": "unchanged"}
Applied CRD  {"name": "snapshotjobs.nvidia.com", "action": "unchanged"}
CRDs already up to date, no changes applied  {"count": 3}
```

The installer runs whenever the operator pod is recreated, which a `helm upgrade`
does as soon as the image tag moves — `image.operator.tag` defaults to the
chart's `appVersion`, so a release that changes the CRDs changes the tag too. If
you pin a mutable tag such as `latest`, rebuilding it does not change the pod
spec and nothing rolls; restart the Deployment yourself to pick up new
definitions.

`rbac.create=true` grants the operator ServiceAccount `get`/`patch` on the three
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
PVC_NAME="${PVC_NAME:-snapshot-pvc}" # match storage.pvc.name
kubectl get pvc "${PVC_NAME}" -n "${NAMESPACE}"
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
| `storage.pvc.create` | Create `snapshot-pvc` instead of using an existing shared PVC | `true` |
| `storage.pvc.name` | Shared RWX checkpoint PVC mounted by every agent | `snapshot-pvc` |
| `storage.pvc.size` | Requested PVC size | `1Ti` |
| `storage.pvc.storageClass` | Storage class name | `""` |
| `storage.pvc.basePath` | Fixed checkpoint mount path enforced by the privileged helper | `/checkpoints` |
| `seccomp.deploy` | Deploy the CRIU seccomp profile ConfigMap and init container. Use this field name; `seccomp.enabled` is not a chart value | `true` |
| `runtime.type` | CRI backend: `containerd` or `crio` | `containerd` |
| `runtime.socketPath` | CRI socket (empty = default for `runtime.type`) | `""` |
| `crdUpgrade.enabled` | Install and upgrade the CRDs from an operator init container (see below) | `true` |
| `crdUpgrade.logLevel` | Init container log level | `info` |
| `rbac.create` | Create agent and operator RBAC | `true` |
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
PVC_NAME="${PVC_NAME:-snapshot-pvc}" # match storage.pvc.name
kubectl delete pvc "${PVC_NAME}" -n "${NAMESPACE}"
```

## Notice and disclaimer

Installing this chart causes your cluster to retrieve container images that are
not distributed with the chart, including the `busybox` image used as a
short-lived init container to write the seccomp profile (overridable via
`daemonset.initContainer.image`).

> **NOTICE AND DISCLAIMER:** This software automatically retrieves, accesses or
> interacts with external materials. Those retrieved materials are not
> distributed with this software and are governed solely by separate terms,
> conditions and licenses. You are solely responsible for finding, reviewing and
> complying with all applicable terms, conditions, and licenses, and for
> verifying the security, integrity and suitability of any retrieved materials
> for your specific use case. This software is provided "AS IS", without
> warranty of any kind. The author makes no representations or warranties
> regarding any retrieved materials, and assumes no liability for any losses,
> damages, liabilities or legal consequences from your use or inability to use
> this software or any retrieved materials. Use this software and the retrieved
> materials at your own risk.

Materials this chart causes to be retrieved:

| Image | Default reference | License | Retrieved from |
|---|---|---|---|
| Snapshot operator | `ghcr.io/ai-dynamo/snapshot/operator` | Apache-2.0 (NVIDIA) | GHCR |
| Snapshot agent | `ghcr.io/ai-dynamo/snapshot/agent` | Apache-2.0 (NVIDIA) | GHCR |
| busybox init container | `busybox:1.37.0` (digest-pinned) | GPL-2.0 | Docker Hub |

Third-party attribution and corresponding source for the two NVIDIA images are
shipped inside those images, at `/legal/THIRD-PARTY.txt` and `/legal/source/`.
