# Restore a replica

Restoring starts a new replica from a snapshot instead of cold-starting it. The
restored pods carry the `nvidia.com/restore-from` annotation, naming the
`PodSnapshot` to restore from; the node agent restores the checkpointed state into
the container during pod startup.

This is stage 3 of the [usage guides](README.md). It starts from the ready
`PodSnapshot` that [checkpoint](checkpoint.md) produced:

```bash
kubectl get podsnapshot --namespace "$WORKLOAD_NAMESPACE"
```

## 1. Choose the restore profile

The restored replica reuses the source's snapshot-ready pod spec. Each framework
ships two restore profiles under `restore/`. Choose the one that matches the
source's GPU count:

| Framework | 1 GPU | 8 GPUs |
|-----------|-------|--------|
| vLLM | [1-gpu.yaml](vllm/restore/1-gpu.yaml) | [8-gpu.yaml](vllm/restore/8-gpu.yaml) |
| SGLang | [1-gpu.yaml](sglang/restore/1-gpu.yaml) | [8-gpu.yaml](sglang/restore/8-gpu.yaml) |
| TensorRT-LLM | [1-gpu.yaml](tensorrt-llm/restore/1-gpu.yaml) | [8-gpu.yaml](tensorrt-llm/restore/8-gpu.yaml) |

A profile works for every model of its framework and GPU count, because the
model configuration comes from the snapshot. It also works for snapshots taken
with or without `SnapshotJob`. Keep the container `image` the same as the
source's.

## 2. Set the snapshot to restore from

Every profile carries the annotation `nvidia.com/restore-from: ${SNAPSHOT_NAME}`,
a placeholder for the `PodSnapshot` to restore from. Open your profile and replace
`${SNAPSHOT_NAME}` with your recipe's snapshot name from the
[recipe table](README.md#choose-your-recipe), then save the file. Whichever method
you used, that is the `SnapshotJob`'s name, which its `PodSnapshot` shares, or the
name you gave the `PodSnapshot` in
[Checkpoint, Option 1](checkpoint.md#option-1--podsnapshot-checkpoint-a-running-replica).
If you named your `PodSnapshot` something else, use that name instead.

## 3. Restore

Apply the profile you edited, then watch the rollout. The Deployment
is named `<framework>-1gpu-restored` or `<framework>-8gpu-restored`, as the
[recipe table](README.md#choose-your-recipe) lists:

```bash
kubectl apply \
  --namespace "$WORKLOAD_NAMESPACE" \
  --filename vllm/restore/1-gpu.yaml

kubectl rollout status \
  --namespace "$WORKLOAD_NAMESPACE" \
  deployment/vllm-1gpu-restored \
  --timeout=30m
```

Snapshot replaces the container's command with its restore proxy, as the
[restore Pod contract](../reference/restore-pod-contract.md) describes, so the
application does not start from scratch. The proxy waits for the restore, then
follows the restored process: the container ends when the restored process
does. Readiness waits on the framework's post-restore sentinel —
`vllm-restore-ready` and its equivalents — not the `ready-for-snapshot` file the
source pod writes.

On nodes without NRI v0.10 or newer, there is no restore proxy. Before applying
the profile, edit it to change the container's command to
`["sleep", "infinity"]`, remove its `args`, and add a liveness probe on the
framework's API. See
[Nodes without the restore proxy](../reference/restore-pod-contract.md#nodes-without-the-restore-proxy).

A Pod gets one restore. The guides use a Deployment, which restarts containers,
so if the restored process exits, the container ends in `CrashLoopBackOff`
instead of starting again. Delete the Pod to restore a new replica.

## 4. Verify the restored replica

The node agent adds a `nvidia.com/Restored` condition to the pod once the restore
completes — watch it, along with pod readiness, to confirm:

```bash
kubectl get pods --namespace "$WORKLOAD_NAMESPACE" \
  --selector app=vllm-1gpu-restored \
  --output jsonpath='{.items[0].status.conditions[?(@.type=="nvidia.com/Restored")].status}'
```

It prints `True`. If the restore does not complete, see
[Troubleshooting](../operations/troubleshooting.md).

In practice, higher-level systems create these restored Deployments rather than
applying them by hand. To generate restore pods programmatically — from a
controller, operator, or serving platform — implement the
[restore Pod contract](../reference/restore-pod-contract.md), which specifies the
required annotations, control volume, and `SNAPSHOT_CONTROL_DIR`, plus the
optional startup gate and seccomp profile a restored pod may carry.

## Restore in another cluster

A checkpoint is files, and the files restore wherever the
[compatibility checks](../reference/api.md#restore) pass. What ties a checkpoint
to the cluster that took it is its identity: the agent finds an artifact at
`artifacts/<uid>/containers/<container>/` under the `PodSnapshotContent` UID that
checkpointed it, and refuses a manifest recording any other UID. A new cluster never
has a content with that UID, so instead of checkpointing, it imports: a `PodSnapshot`
whose source names the artifact rather than a pod.

1. **Keep the artifact.** Copy the content root out of the snapshot store as it
   is, for example to object storage:

   ```bash
   s5cmd sync '/checkpoints/artifacts/<uid>/*' s3://<bucket>/<prefix>/<uid>/
   ```

   Everything a restore needs is in it. The directory names are the two values
   the import takes: `<uid>`, and the container under `containers/`. Leave out
   `.tmp/` if a checkpoint was still running.

2. **Put it where the target cluster's agents restore from**, under the same
   `<uid>`: `artifacts/<uid>/` in that cluster's snapshot store. With a
   shared PVC that is one copy. With node-local storage it is one copy per node
   a restore may land on, put there by whatever pre-warms the nodes.

3. **Import it:**

   ```yaml
   apiVersion: nvidia.com/v1alpha1
   kind: PodSnapshot
   metadata:
     name: <name>
     namespace: <namespace>
   spec:
     source:
       artifact:
         uid: <uid>           # the directory name, never this cluster's own UID
         containers: [main]   # the directory under containers/
   ```

   The operator binds it to a `PodSnapshotContent` that names the artifact and
   marks both `Ready` with reason `ArtifactImported`. That means the reference is
   accepted. It does not mean the bytes are anywhere. The operator cannot check
   node-local storage, and a restore on a node that does not hold the artifact
   yet waits for it (`ArtifactPending`) until it arrives. This is where an import
   differs from a checkpoint taken in the cluster, whose restore fails when its
   artifact is missing: that `Ready` promises the bytes, an import's does not.

4. **Restore** with `nvidia.com/restore-from: <name>`, as above.

The target cluster needs a PVC [snapshot store](../operations/storage.md): an
import is read by path, `artifacts/<uid>/`, and is never bound to the configured
store, because PageBroker never published it there. A store with
`storage.type: s3` has no such path and cannot restore an import yet.

The [compatibility checks](../reference/api.md#restore) run as for a local
checkpoint, against the artifact's manifest. The target node needs the checkpoint
node's CPU architecture, kernel version, NVIDIA driver version, GPU model and
count. The restore pod needs the same image digest, at least the same memory
and CPU limits, and every path the checkpoint mounted.

Deleting the `PodSnapshot` deletes its content but never the artifact, which an
import does not own: other imports, or the cluster that took it, may name
the same one. The orphan sweep reclaims the artifact once nothing names it.
