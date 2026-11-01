# Installation

Snapshot installs as a single per-cluster Helm release. Install one release for
the cluster: every node agent watches checkpoint and restore pods in all
namespaces. Install it in its own namespace, and run GPU workloads in separate
namespaces.

Review the [Prerequisites](prerequisites.md) before installing.

## What the Chart Installs

- The operator Deployment, which reconciles `SnapshotJob`, `PodSnapshot`, and
  `PodSnapshotContent`.
- The node agent DaemonSet on eligible GPU nodes. It runs privileged to perform
  checkpoint and restore.
- The snapshot volume claim, `snapshot-pvc`, or the wiring to an existing claim.
- Cluster-scoped RBAC for the agent and the operator.
- The seccomp profile that CRIU needs.

## Allow the Privileged Node Agent

The node agent needs elevated privileges to checkpoint and restore processes:

- The `snapshot-agent` DaemonSet runs privileged, with `hostPID`, `hostIPC`, and
  `hostNetwork`, so it can run CRIU and `cuda-checkpoint` against live processes
  on the node. Workloads do not need to be privileged. Only the agent does.
- The agent's namespace must allow privileged pods. On clusters that enforce
  [Pod Security Standards](https://kubernetes.io/docs/concepts/security/pod-security-standards/),
  apply the `privileged` level, or an equivalent exception, to that namespace
  before you install.
- CRIU needs a seccomp profile to checkpoint and restore. The chart installs the
  profile the agent needs.

## Install from a Release

Install the published chart. Replace `<version>` with the release you want from
the [releases page](https://github.com/ai-dynamo/snapshot/releases). Every
command on this page uses the same placeholder.

```bash
helm install snapshot oci://ghcr.io/ai-dynamo/snapshot/snapshot \
  --version <version> \
  --namespace snapshot --create-namespace
```

### Snapshot Volume

- By default, the chart creates a `ReadWriteMany` snapshot volume that every
  snapshot shares.
- If your cluster has no default storage class, add
  `--set storage.pvc.storageClass=<storage-class>`.
- To reuse an existing `ReadWriteMany` claim, add
  `--set storage.pvc.create=false --set storage.pvc.name=<pvc-name>`. The chart
  does not check the claim's access mode, so verify that it is `ReadWriteMany`
  before you install.

See [Configure Storage](../operate/configure-storage.md) for the volume model and
other options.

### Node Agent Resources

- Each agent pod runs two containers: the agent and the PageBroker sidecar.
  PageBroker stages snapshot data in memory and publishes it to the snapshot
  volume.
- By default, the agent pod requests 10 CPU and 33Gi of memory on each GPU node.
- The pod's memory limit sets the largest snapshot it can take.

See [PageBroker Staging](../operate/configure-storage.md#pagebroker-staging) for how to
size it.

## Install on OpenShift

OpenShift needs two settings that a standard install does not, for its
container runtime and its security rules. Set these values when you install:

- `runtime.type=crio`. OpenShift uses CRI-O as its container runtime.
- `openshift.enabled=true`. This creates the SCC-use RBAC and the required-SCC
  pod annotations for both components. The agent uses the `privileged` SCC, and
  the operator uses `anyuid`.

The chart creates these SCC permissions only while `rbac.create` is `true`, the
default. If you set it to `false`, grant the agent and the operator permission
to use their SCCs yourself.

Create the snapshot volume through a `ReadWriteMany` storage class, such as an
EFS storage class:

```bash
export RWX_STORAGE_CLASS=efs-rwx

helm install snapshot oci://ghcr.io/ai-dynamo/snapshot/snapshot \
  --version <version> \
  --namespace snapshot --create-namespace \
  --set storage.pvc.create=true \
  --set storage.pvc.storageClass="${RWX_STORAGE_CLASS}" \
  --set runtime.type=crio \
  --set openshift.enabled=true
```

Or reuse an existing `ReadWriteMany` claim in the installation namespace:

```bash
export EXISTING_RWX_PVC=snapshot-efs-pvc

helm install snapshot oci://ghcr.io/ai-dynamo/snapshot/snapshot \
  --version <version> \
  --namespace snapshot --create-namespace \
  --set storage.pvc.create=false \
  --set storage.pvc.name="${EXISTING_RWX_PVC}" \
  --set runtime.type=crio \
  --set openshift.enabled=true
```

## Install on RKE2 and k3s

RKE2 and k3s are Kubernetes distributions from Rancher. Both run containerd,
but they keep its socket and its storage in their own directories, not in the
standard locations. The node agent needs both paths to reach containerd, so set
both values together:

| Value | RKE2 | k3s |
| --- | --- | --- |
| `runtime.socketPath` | `/run/k3s/containerd/containerd.sock` | `/run/k3s/containerd/containerd.sock` |
| `runtime.storageDir` | `/var/lib/rancher/rke2/agent/containerd` | `/var/lib/rancher/k3s/agent/containerd` |

For example, on RKE2:

```bash
helm install snapshot oci://ghcr.io/ai-dynamo/snapshot/snapshot \
  --version <version> \
  --namespace snapshot --create-namespace \
  --set runtime.socketPath=/run/k3s/containerd/containerd.sock \
  --set runtime.storageDir=/var/lib/rancher/rke2/agent/containerd
```

Set `runtime.storageDir` to the exact host path. The chart mounts it into the
agent at the same path, and the agent reads the container's writable layer at
the path containerd records. It is not a relocatable prefix.

## Images the Chart Pulls

Mirror these images when your cluster pulls from a private or air-gapped
registry:

- **Operator:** `ghcr.io/ai-dynamo/snapshot/operator`
- **Node agent:** `ghcr.io/ai-dynamo/snapshot/agent`
- **PageBroker:** `ghcr.io/ai-dynamo/snapshot/pagebroker`, always pulled at the
  node agent's tag
- **busybox:** `busybox:1.37.0`, digest-pinned, from Docker Hub. A short-lived
  init container uses it to write the seccomp profile. Override it with
  `daemonset.initContainer.image`.

## Use a Private Node Agent Image

- The node agent uses public GHCR images by default and needs no image pull
  secret.
- Set `daemonset.imagePullSecrets` only when you override the agent image with
  a private one.
- When you upgrade, see [Upgrade Snapshot and the CRDs](../operate/upgrade.md#upgrade-snapshot).

## Verify the Installation

```bash
kubectl get pods --namespace snapshot
kubectl get pvc --namespace snapshot
kubectl rollout status daemonset/snapshot-agent --namespace snapshot
```

The operator and the `snapshot-agent` DaemonSet become ready once the node agent
and its PageBroker sidecar are running on each GPU node. Each agent pod reports
`2/2` containers ready. The snapshot volume claim reports `Bound`.

## Uninstall

```bash
helm uninstall snapshot --namespace snapshot
```

Chart-created snapshot volumes are retained on uninstall, so snapshots survive
removal. See [Retention](../operate/configure-storage.md#retention). To clear the
stored snapshots, delete the claim yourself:

```bash
kubectl delete pvc <pvc-name> --namespace snapshot
```
