<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Try Snapshot Without a GPU

This guide checkpoints a pod and restores it on a local cluster that has one
node and no GPU. The pod runs a counter. The restored pod continues the count
from the checkpoint.

Use it to see the checkpoint and restore flow before you have a GPU cluster.
The cluster is for a trial only. It does not meet the
[Prerequisites](prerequisites.md) of a Snapshot installation. This guide does
not checkpoint GPU memory, and it does not restore on another node. For a GPU
workload, see [Quickstart: Checkpoint and Restore One Pod](quickstart.md).

## Prerequisites

You need the following on your machine:

- Docker on Linux or macOS, on x86_64 or arm64, with about 20 GB of free disk
  space for the images and 2 GB of memory for the cluster. A Linux host needs
  the kernel version in [Prerequisites](prerequisites.md).
- [k3d](https://k3d.io) v5.8.3 or newer, which runs a Kubernetes cluster in
  Docker.
- `kubectl` and the [Helm](https://helm.sh/docs/intro/install) CLI.

The commands on this page were run on macOS on Apple silicon, with Docker
Desktop. They were not run on Linux. On Linux x86_64, the Snapshot CPU
end-to-end tests checkpoint and restore pods on a k3d cluster with the same
Kubernetes version.

## 1. Create the Cluster

Create a cluster with one node:

```bash
k3d cluster create snapshot-demo --image rancher/k3s:v1.34.12-k3s1
```

The `--image` option sets the Kubernetes version of the cluster. Without it,
the version depends on your k3d release, and the default of k3d v5.8.3 is too
old for Snapshot. k3d also points `kubectl` and `helm` at the new cluster.

## 2. Install Snapshot

The chart's own snapshot volume claim requests `ReadWriteMany`, which the
storage class of this cluster does not provide. One node needs only
`ReadWriteOnce`. Create the namespace and a claim of your own:

```bash
kubectl create namespace snapshot

kubectl apply --namespace snapshot --filename - <<'EOF'
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: snapshot-pvc
spec:
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: 2Gi
EOF
```

Replace `<version>` with release `v0.3.0` or newer from the
[releases page](https://github.com/ai-dynamo/snapshot/releases), without the
leading `v`. For release `v0.3.0`, use `0.3.0`. Then install the chart:

```bash
helm install snapshot oci://ghcr.io/ai-dynamo/snapshot/snapshot \
  --version <version> \
  --namespace snapshot \
  --set storage.pvc.create=false \
  --set storage.pvc.name=snapshot-pvc \
  --set runtime.socketPath=/run/k3s/containerd/containerd.sock \
  --set runtime.storageDir=/var/lib/rancher/k3s/agent/containerd \
  --set daemonset.nodeSelector=null \
  --set daemonset.runtimeClassName= \
  --set daemonset.resources.requests.cpu=200m \
  --set pageBroker.resources.requests.cpu=200m \
  --set pageBroker.resources.requests.memory=256Mi
```

The values adapt the chart to this cluster:

| Values | Why this cluster needs them |
| --- | --- |
| `storage.pvc.create`, `storage.pvc.name` | The chart uses the claim that you created. |
| `runtime.socketPath`, `runtime.storageDir` | k3s keeps containerd in its own directories. See [Installation](installation.md#install-on-rke2-and-k3s). |
| `daemonset.nodeSelector`, `daemonset.runtimeClassName` | By default, the node agent runs only on GPU nodes, with the NVIDIA container runtime. This node has neither. |
| `daemonset.resources.requests.cpu`, `pageBroker.resources.requests.*` | The default requests are for a GPU node: 10 CPU and 33Gi of memory. |

The first rollout pulls the node agent image, about 6 GB, which can take 10
minutes or more. Wait for the node agent:

```bash
kubectl rollout status daemonset/snapshot-agent \
  --namespace snapshot --timeout=20m
```

## 3. Checkpoint

A `SnapshotJob` starts a pod, waits until it is ready, checkpoints it, and
removes it.

The pod in this job adds one count each second to `/tmp/counter.log`. When the
count is 5, it creates the `ready-for-snapshot` file. That file tells Snapshot
that the process is ready for the checkpoint.

Create the `SnapshotJob` and wait until it completes:

```bash
kubectl apply --filename - <<'EOF'
apiVersion: nvidia.com/v1alpha1
kind: SnapshotJob
metadata:
  name: counter-snapshot
spec:
  podSnapshotTemplate:
    targetContainers:
      - main
  podTemplate:
    spec:
      terminationGracePeriodSeconds: 1
      containers:
        - name: main
          image: ubuntu:24.04
          command:
            - /bin/bash
            - -c
            - |
              count=0
              while true; do
                echo "count=$count" >> /tmp/counter.log
                if [ "$count" -eq 5 ]; then
                  touch "$SNAPSHOT_CONTROL_DIR/ready-for-snapshot"
                fi
                count=$((count + 1))
                sleep 1
              done
EOF

kubectl wait --for=condition=Completed snapshotjob/counter-snapshot --timeout=5m
```

## 4. Restore

The snapshot is a `PodSnapshot` named `counter-snapshot`. A new pod restores
from it when its `nvidia.com/restore-from` annotation names the snapshot.

Create the pod and wait until the restore completes:

```bash
kubectl apply --filename - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: counter-restored
  annotations:
    nvidia.com/restore-from: counter-snapshot
spec:
  terminationGracePeriodSeconds: 1
  securityContext:
    seccompProfile:
      type: Localhost
      localhostProfile: profiles/block-iouring.json
  containers:
    - name: main
      image: ubuntu:24.04
      command:
        - sleep
        - infinity
      env:
        - name: SNAPSHOT_CONTROL_DIR
          value: /snapshot-control
      volumeMounts:
        - name: snapshot-control
          mountPath: /snapshot-control
          subPath: main
  volumes:
    - name: snapshot-control
      emptyDir: {}
EOF

kubectl wait pod/counter-restored --timeout=5m \
  --for=jsonpath='{.status.conditions[?(@.type=="nvidia.com/Restored")].status}'=True
```

Read the last counts in the restored pod:

```bash
kubectl exec counter-restored -- tail -n 3 /tmp/counter.log
```

The log holds the counts from before the checkpoint, and the count continues
from there. Run the command again to see higher counts. The command of this pod
is `sleep infinity`, so only the restored process writes counts.

For the volume, the environment variable, and the seccomp profile that the pod
carries, see [Restore Pod Contract](../reference/restore-pod-contract.md).

## 5. Delete the Cluster

Delete the cluster when you finish:

```bash
k3d cluster delete snapshot-demo
```

## Next Steps

- [Quickstart: Checkpoint and Restore One Pod](quickstart.md) - Checkpoint and
  restore a vLLM replica on a GPU cluster
- [Workload Contract](../reference/workload-contract.md) - What a workload does
  to be ready for a checkpoint and to resume after a restore
- [Restore Pod Contract](../reference/restore-pod-contract.md) - What a restore
  pod must carry
