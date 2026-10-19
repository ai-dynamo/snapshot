# Checkpoint a replica

Checkpointing saves an initialized replica's state as a checkpoint artifact. There
are two ways to do it, depending on the use case:

| Method | Choose it when… | Implication |
|--------|-----------------|-------------|
| **`PodSnapshot`** | The running replica can be controlled and tracked — for example, by a controller or platform that manages inference pods | The most efficient path — it checkpoints a replica that stays serving. It needs orchestration: bringing the replica up, waiting until it is ready, then triggering the checkpoint. |
| **`SnapshotJob`** | The running pod cannot be tracked directly — for example, in a pipeline that submits the work | Snapshot runs the whole flow: it creates the replica, checkpoints it, and tears it down. Self-contained, but the source is discarded, so every replica (including the first) comes up via [restore](restore.md). |

This is stage 2 of the [usage guides](README.md). It continues from
[stage 1](README.md#1-make-the-workload-snapshot-ready): a `PodSnapshot` needs the
source replica that stage deployed, and a `SnapshotJob` needs only the ConfigMap
and model cache it created.

## Option 1 — `PodSnapshot` (checkpoint a running replica)

Point at the source replica that is already up and serving. Snapshot does not
create this pod, so its spec must carry everything the
[workload contract](../reference/workload-contract.md#pod-requirements) requires:
the `/snapshot-control` volume mount, the seccomp profile that checkpointing
requires, and a readiness gate on `/snapshot-control/ready-for-snapshot`. The
source Deployment of every recipe does.

Save a `PodSnapshot` naming the pod and the container to checkpoint as
`vllm-snapshot.yaml`. Give it the name of your recipe's `SnapshotJob` from the
[recipe table](README.md#pick-a-recipe), so that the restore profiles find it
whichever method you use:

```yaml
apiVersion: nvidia.com/v1alpha1
kind: PodSnapshot
metadata:
  name: vllm-snapshot
spec:
  source:
    podRef:
      name: vllm-source-<pod-id>
      containers:
        - main
```

```bash
kubectl apply \
  --namespace "$SNAPSHOT_NAMESPACE" \
  --filename vllm-snapshot.yaml

kubectl wait \
  --namespace "$SNAPSHOT_NAMESPACE" \
  --for=condition=Ready podsnapshot/vllm-snapshot \
  --timeout=30m
```

The operator binds a cluster-scoped `PodSnapshotContent` and records the artifact.
Because the replica keeps running and serving, this is the faster path — the
trade-off is the orchestration it requires: bringing the replica up, waiting for
readiness, then triggering the checkpoint.

**Eight-GPU recipes.** The `PodSnapshot` is the same. The source Deployment
already carries the multi-GPU annotation, init container, and launcher that
[Multi-GPU workloads](../reference/workload-contract.md#multi-gpu-workloads)
describes.

## Option 2 — `SnapshotJob` (checkpoint a throwaway replica)

`SnapshotJob` runs a replica from a pod template, checkpoints it once ready, and
completes from the resulting `PodSnapshot` — removing the source replica. There is
no long-running replica to manage, which fits pipeline use cases.

`spec.podTemplate` only needs the workload's own container spec — image,
command, resources, and any volumes it mounts. The controller injects the
`/snapshot-control` volume and mount, `SNAPSHOT_CONTROL_DIR`, the
`ready-for-snapshot` readiness probe, and the seccomp profile before creating the
source pod. The framework recipes do not need `/dev/net/tun`. CRIU can log a
nonfatal TUN capability-probe error when the device is absent. See
[TUN probe errors](../operations/troubleshooting.md#criu-reports-unable-to-create-tun).

Each recipe ships a ready-to-apply `SnapshotJob`. Its shape is:

```yaml
apiVersion: nvidia.com/v1alpha1
kind: SnapshotJob
metadata:
  name: vllm-snapshot
spec:
  podSnapshotTemplate:
    targetContainers:
      - main
  podTemplate:
    spec:
      containers:
        - name: main
          # The workload's own container spec: image, command, args,
          # resources, and the volumes it mounts, such as the app.py ConfigMap.
```

Apply the `SnapshotJob` of your recipe from the [recipe table](README.md#pick-a-recipe)
and wait for it to complete:

```bash
kubectl apply \
  --namespace "$SNAPSHOT_NAMESPACE" \
  --filename vllm/capture/qwen3-0.6b-snapshotjob.yaml

kubectl wait \
  --namespace "$SNAPSHOT_NAMESPACE" \
  --for=condition=Completed snapshotjob/vllm-snapshot \
  --timeout=60m

# the resulting PodSnapshot to restore from:
kubectl get snapshotjob vllm-snapshot \
  --namespace "$SNAPSHOT_NAMESPACE" \
  --output jsonpath='{.status.podSnapshotName}'
```

The `PodSnapshot` has the same name as its `SnapshotJob`.

**Eight-GPU recipes.** The `SnapshotJob` carries the
`nvidia.com/snapshot-multi-gpu: enabled` annotation on `spec.podTemplate`, and the
controller prepares the source Pod for multi-GPU capture. Add the annotation to
your own `SnapshotJob` the same way:

```yaml
spec:
  podTemplate:
    metadata:
      annotations:
        nvidia.com/snapshot-multi-gpu: enabled
```

The first run downloads the model, so allow `--timeout=120m`.

Because the source replica is deleted, every serving replica — including the
first — is brought up via [restore](restore.md).

## Next steps

- [Restore a replica](restore.md)
