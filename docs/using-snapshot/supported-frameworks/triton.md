# Prepare a Triton Inference Server Workload

This guide makes a Triton Inference Server workload snapshot-ready. You do not
add a program to the container. Triton runs from its stock image with its own
command. Snapshot reads the readiness endpoint of Triton to know when to
checkpoint.

Before you start, complete [Before You Start](../README.md#before-you-start) and
[Choose Your Recipe](../README.md#choose-your-recipe). Run the commands from
`docs/using-snapshot`.

> [!NOTE]
> This example is validated on Triton Inference Server 2.68.0 (the pinned
> `nvcr.io/nvidia/tritonserver:26.04-py3` image). It serves one ONNX model on
> one GPU.

## How the Example Works

### Before the Checkpoint

Triton loads the models in its model repository. Then it answers
`GET /v2/health/ready` with a 200 status. The recipe uses this endpoint as the
signal that the server is safe to checkpoint:

- **`SnapshotJob`** - The job sets a `checkpointReadinessProbe` on
  `/v2/health/ready`. This probe is one of the
  [Lifecycle Hooks](../../reference/workload-contract.md#lifecycle-hooks).
  Snapshot checkpoints the container when the probe succeeds.
- **`PodSnapshot`** - The readiness probe of the source Deployment is
  `/v2/health/ready`. Snapshot checkpoints the container when the container is
  Ready.

The `config.pbtxt` file of the model has a `model_warmup` entry. Triton runs
this entry while it loads the model, and reports the model as ready only after
that. As a result, the snapshot contains a model that already ran one inference.

Triton has no call that pauses inference. Nothing sends requests to the source
Pod, so no inference is in flight when Snapshot checkpoints it.

### After the Restore

In a restore container, Snapshot runs its restore proxy in place of the Triton
command. Snapshot then restores the checkpointed server with its models loaded.
Triton needs no call to resume. For this reason, the recipe sets no
`postRestore` hook.

The restore Pod has two probes. A startup probe waits for the
`restore-complete` file, which Snapshot writes when the restore is complete.
The readiness probe, on `/v2/health/ready`, starts only after that. As a
result, the Pod is Ready when the server is restored and answers. To make sure
that the restore worked, see
[Verify the Restored Pod](../restore.md#verify-the-restored-pod).

### Image and Settings

The manifests run the Triton `26.04-py3` image without changes, and pin it by
digest. Triton starts with its own command, `tritonserver`, and two arguments:

| Argument | What it does |
|----------|--------------|
| `--model-repository=/models/triton` | Serves the models in the [Model Cache](#model-cache). |
| `--allow-gpu-metrics=false` | Turns off the GPU metrics of Triton, which are on by default. With GPU metrics on, the checkpoint fails at [dumping with CRIU](../../troubleshooting/checkpoints.md#dumping-with-criu). The other Triton metrics stay on. |

### Model Cache

An init container downloads the model from Hugging Face into the persistent
[Model Cache](../README.md#model-cache). The model is DenseNet-121 in ONNX
format. The init container also writes the `config.pbtxt` file of the model.

The recipe mounts the model cache at `/models`. The model repository of Triton
is the `triton` directory of the model cache:

```text
/models/triton/densenet_onnx/config.pbtxt
/models/triton/densenet_onnx/1/model.onnx
```

### Pod Requirements

Triton writes nothing to the Snapshot control volume. The source and restore
Pods must still mount it at `/snapshot-control`. Snapshot keeps its own files
there, for example the restore proxy.

The Deployments of the recipe already mount the volume, and a `SnapshotJob`
adds it to its source Pod. If you write your own manifest, see
[Pod Requirements](../../reference/workload-contract.md#pod-requirements).

## Choose the Model

The recipe already selects its model, in
[`capture/densenet-onnx.yaml`](../triton/capture/densenet-onnx.yaml). The
`model-cache` init container takes three arguments after its download script.
They are the model URL, the model directory in the model repository, and the
`config.pbtxt` content:

```yaml
initContainers:
  - name: model-cache
    args:
      - <download script>
      - https://huggingface.co/onnxmodelzoo/densenet-7/resolve/<revision>/densenet-7.onnx
      - /models/triton/densenet_onnx
      - |
        model_warmup [
          <one request of zeros for the model input>
        ]
```

Triton fills in the rest of the model configuration from the ONNX file. To
serve a different ONNX model, change the three arguments in a copy of the
manifest.

## Deploy the Source Pod

This section is for a checkpoint with a `PodSnapshot`. It deploys the source Pod
that the `PodSnapshot` checkpoints. At the end, you have a running pod that is
ready to checkpoint. You need its name for the `PodSnapshot` that you create in
[Checkpoint a Pod](../checkpoint.md).

If you checkpoint with a `SnapshotJob`, skip this section. The job creates the
source replica itself. Continue with
[Checkpoint, Option 2](../checkpoint.md#option-2---snapshotjob-checkpoint-a-throwaway-pod).

1. Apply the source manifest:

   ```bash
   kubectl apply \
     --namespace "$WORKLOAD_NAMESPACE" \
     --filename triton/capture/densenet-onnx.yaml
   ```

2. Wait until Triton loads the model and is safe to checkpoint. The readiness
   probe succeeds when Triton answers `/v2/health/ready`:

   ```bash
   kubectl rollout status \
     --namespace "$WORKLOAD_NAMESPACE" \
     deployment/triton-densenet-onnx-source \
     --timeout=30m
   ```

3. List the generated pod. Use its name in the `PodSnapshot` that you create in
   [Checkpoint a Pod](../checkpoint.md):

   ```bash
   kubectl get pods \
     --namespace "$WORKLOAD_NAMESPACE" \
     --selector app=triton-densenet-onnx-source
   ```

## Next Steps

- [Checkpoint a Pod](../checkpoint.md) - Create a snapshot of the source replica
- [Restore a Pod](../restore.md) - Start a new pod from the snapshot
