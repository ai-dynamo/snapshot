# Checkpoint a Pod

Checkpointing saves an initialized replica's state as a snapshot. There
are two ways to do it, depending on the use case:

| Method | Choose it when… | Implication |
|--------|-----------------|-------------|
| **`PodSnapshot`** | The running replica can be controlled and tracked | It checkpoints a replica you run yourself. It needs orchestration: bringing the replica up, waiting until it is ready, then triggering the checkpoint. |
| **`SnapshotJob`** | The running pod cannot be tracked directly | Snapshot runs the whole flow: it creates the replica, checkpoints it, and tears it down. Self-contained, but the source is discarded, so every replica (including the first) comes up via [restore](restore.md). |

When you finish, you have a Ready `PodSnapshot` with a name. You use that name in
[Restore a Pod](restore.md) to start a new pod from the snapshot.

This is stage 2 of [Checkpoint and Restore](README.md): checkpoint a running
pod. It continues from stage 1, the framework page you used
([vLLM](supported-frameworks/vllm.md), [SGLang](supported-frameworks/sglang.md), or
[TensorRT-LLM](supported-frameworks/tensorrt-llm.md)): a `PodSnapshot` needs the
source replica that page deployed, and a `SnapshotJob` needs only the ConfigMap it
created and the [Model Cache](README.md#model-cache).

Run the commands from [Choose Your Recipe](README.md#choose-your-recipe) first, in
the same terminal. The commands on this page use the values they set.

## Option 1 — `PodSnapshot` (Checkpoint a Running Pod)

Point at the source replica that is already up and ready.

1. Save a `PodSnapshot` naming the pod and the container to checkpoint as
   `vllm-qwen3-0-6b-snapshot.yaml`. Give it the name of your recipe's
   `SnapshotJob` from the [recipe table](README.md#choose-your-recipe), so that
   the restore step finds it whichever method you use. The names below are for
   the vLLM Qwen3 0.6B recipe. For another recipe, replace them with your own,
   in the YAML, the file name, and the commands in step 2:

   ```yaml
   apiVersion: nvidia.com/v1alpha1
   kind: PodSnapshot
   metadata:
     name: vllm-qwen3-0-6b-snapshot
   spec:
     source:
       podRef:
         name: vllm-qwen3-0-6b-source-<pod-id>
         containers:
           - main
   ```

2. Apply it and wait until the `PodSnapshot` is ready:

   ```bash
   kubectl apply \
     --namespace "$WORKLOAD_NAMESPACE" \
     --filename vllm-qwen3-0-6b-snapshot.yaml

   kubectl wait \
     --namespace "$WORKLOAD_NAMESPACE" \
     --for=condition=Ready podsnapshot/vllm-qwen3-0-6b-snapshot \
     --timeout=30m
   ```

The operator binds a cluster-scoped `PodSnapshotContent` and records the artifact.
The source replica is terminated once the checkpoint completes, so it does not keep
serving.

**Eight-GPU recipes.** The `PodSnapshot` is the same. The source Deployment
already carries the multi-GPU annotation, init container, and launcher that
[Multi-GPU Workloads](../integrate/workload-contract.md#multi-gpu-workloads)
describes.

## Option 2 — `SnapshotJob` (Checkpoint a Throwaway Pod)

`SnapshotJob` runs a replica from a pod template, checkpoints it once ready, and
completes from the resulting `PodSnapshot` — removing the source replica. There is
no long-running replica to manage, which fits pipeline use cases.

Apply the `SnapshotJob` of your recipe and wait for it to complete:

```bash
kubectl apply \
  --namespace "$WORKLOAD_NAMESPACE" \
  --filename "$SNAPSHOTJOB_MANIFEST"

kubectl wait \
  --namespace "$WORKLOAD_NAMESPACE" \
  --for=condition=Completed snapshotjob/"$SNAPSHOT_NAME" \
  --timeout="$WAIT"

# the resulting PodSnapshot to restore from:
kubectl get snapshotjob "$SNAPSHOT_NAME" \
  --namespace "$WORKLOAD_NAMESPACE" \
  --output jsonpath='{.status.podSnapshotName}'
```

The `PodSnapshot` has the same name as its `SnapshotJob`. Because the source
replica is deleted, every serving replica — including the first — is brought up
via [restore](restore.md).

**Eight-GPU recipes.** The `SnapshotJob` carries the
`nvidia.com/snapshot-multi-gpu: enabled` annotation on `spec.podTemplate`, and the
controller prepares the source Pod for multi-GPU capture.

To write your own `SnapshotJob`, see
[Write Your Own `SnapshotJob`](../integrate/write-your-own-snapshotjob.md).

## Next Steps

If a checkpoint does not become Ready, see
[Troubleshoot Checkpoints](../troubleshooting/checkpoints.md#a-checkpoint-never-becomes-ready).

- [Restore a Pod](restore.md) uses the `PodSnapshot` name from this page.
