# Checkpoint a Pod

Checkpointing saves an initialized replica's state as a snapshot. When you finish,
you have a Ready `PodSnapshot` with a name. You use that name in
[Restore a Pod](restore.md) to start a new pod from the snapshot.

## Before You Start

- Run the commands from [Choose Your Recipe](README.md#choose-your-recipe) in the
  same terminal. The commands on this page use the values they set.
- Prepare the workload on the page for your framework
  ([vLLM](supported-frameworks/vllm.md), [SGLang](supported-frameworks/sglang.md) or
  [TensorRT-LLM](supported-frameworks/tensorrt-llm.md)). What you need from it
  depends on the method you choose.
- The examples on this page use the vLLM Qwen3 0.6B recipe. For another recipe,
  change the names to match yours: in the YAML, in the file name and in the
  commands.

## Choose a Method

| Method | Choose it when… | What it does | What you need |
|--------|-----------------|--------------|---------------|
| **`PodSnapshot`** | The running replica can be controlled and tracked | Checkpoints a replica you run yourself. You orchestrate it: bring the replica up, wait until it is ready, then trigger the checkpoint. | The source replica that the framework page deployed |
| **`SnapshotJob`** | The running pod cannot be tracked directly | Snapshot runs the whole flow: it creates the replica, checkpoints it and tears it down. The source is discarded, so every replica, including the first, comes up via [restore](restore.md). | The ConfigMap from the framework page and the [Model Cache](README.md#model-cache) |

## Option 1 - `PodSnapshot` (Checkpoint a Running Pod)

### What the Pod Needs

Point at the source replica that is already up and ready. Snapshot does not
create this pod, so the pod must already have what checkpointing needs: a
`/snapshot-control` volume where the workload signals Snapshot, the seccomp profile
that checkpointing requires, and a readiness gate that holds the pod until it is
safe to checkpoint. The source Deployment of every recipe already has all three.
See the [Workload Contract](../reference/workload-contract.md#pod-requirements) if
you use your own pod.

Eight-GPU recipes need nothing different here. Their source Deployment also carries
the multi-GPU annotation, init container and launcher that
[Multi-GPU Workloads](../reference/workload-contract.md#multi-gpu-workloads)
describes.

### Run the Checkpoint

1. Find the name of the source pod you deployed on the framework page
   ([Deploy the Source Pod](supported-frameworks/vllm.md#deploy-the-source-pod)):

   ```bash
   kubectl get pods \
     --namespace "$WORKLOAD_NAMESPACE" \
     --selector app="$SOURCE_NAME"
   ```

2. Save a `PodSnapshot` as `vllm-qwen3-0-6b-snapshot.yaml`. It names the source pod
   and the container to checkpoint. Replace `<pod-id>` with the suffix of the pod
   name from step 1:

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

   The `PodSnapshot` name matches `$SNAPSHOT_NAME`, the name a `SnapshotJob` uses
   for your recipe, so the restore step finds it whichever method you use.

3. Apply it and wait until the `PodSnapshot` is ready:

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

## Option 2 - `SnapshotJob` (Checkpoint a Throwaway Pod)

A `SnapshotJob` runs a replica from a pod template and checkpoints it once the
replica is ready. When the resulting `PodSnapshot` is ready, the job completes and
removes the source replica. There is no long-running replica to manage, which fits
pipeline use cases.

### What the Job Contains

- **You provide** the workload's own container spec, in `spec.podTemplate`: the
  image, command, resources and any volumes it mounts.
- **The controller adds** the rest before it creates the source pod: the
  `/snapshot-control` volume and mount, `SNAPSHOT_CONTROL_DIR`, the
  `ready-for-snapshot` readiness probe and the seccomp profile.

The framework recipes do not need `/dev/net/tun`. If the device is absent, CRIU can
log a nonfatal TUN capability-probe error. See
[TUN probe errors](../troubleshooting/agent.md#criu-reports-unable-to-create-tun).

Each recipe ships a ready-to-apply `SnapshotJob`. Its shape is:

```yaml
apiVersion: nvidia.com/v1alpha1
kind: SnapshotJob
metadata:
  name: vllm-qwen3-0-6b-snapshot
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

### Eight-GPU Recipes

The recipe's `SnapshotJob` carries the `nvidia.com/snapshot-multi-gpu: enabled`
annotation on `spec.podTemplate`, and the controller prepares the source pod for
multi-GPU capture. If you write your own `SnapshotJob`, add the annotation the same
way:

```yaml
spec:
  podTemplate:
    metadata:
      annotations:
        nvidia.com/snapshot-multi-gpu: enabled
```

### Run the Checkpoint

1. Apply the `SnapshotJob` of your recipe:

   ```bash
   kubectl apply \
     --namespace "$WORKLOAD_NAMESPACE" \
     --filename "$SNAPSHOTJOB_MANIFEST"
   ```

2. Wait for it to complete:

   ```bash
   kubectl wait \
     --namespace "$WORKLOAD_NAMESPACE" \
     --for=condition=Completed snapshotjob/"$SNAPSHOT_NAME" \
     --timeout="$WAIT"
   ```

3. Read the name of the resulting `PodSnapshot` to restore from:

   ```bash
   kubectl get snapshotjob "$SNAPSHOT_NAME" \
     --namespace "$WORKLOAD_NAMESPACE" \
     --output jsonpath='{.status.podSnapshotName}'
   ```

The `PodSnapshot` has the same name as its `SnapshotJob`. The source replica is
deleted, so every serving replica, including the first, is brought up via
[restore](restore.md).

## Next Steps

- [Restore a Pod](restore.md) - Start a new pod from the snapshot, using the
  `PodSnapshot` name from this page
- [Troubleshoot Checkpoints](../troubleshooting/checkpoints.md#a-checkpoint-never-becomes-ready) -
  Fix a checkpoint that does not become Ready
