# Prepare an SGLang Workload

This guide makes an SGLang workload snapshot-ready by mounting an entrypoint
into an SGLang runtime image, implementing Snapshot's [workload
contract](../../reference/workload-contract.md).

The example runs an SGLang image that includes SGLang, CUDA, and
`torch_memory_saver`, unmodified. The capture manifests pin the exact upstream
image, and one program, `app.py`, is mounted into it from a ConfigMap to prepare
SGLang for checkpoint and resume it after restore. The Snapshot agent injects
the restore tooling at runtime.

Before you start, complete [Before You Start](../README.md#before-you-start) and
[Choose Your Recipe](../README.md#choose-your-recipe). Run the commands from
`docs/using-snapshot`.

## How the Example Works

### Before the Checkpoint

The program creates a direct `sglang.Engine`, runs one generation, and calls
`TokenizerManager.pause_generation()` followed by
`Engine.release_memory_occupation(tags=["kv_cache"])` to discard the KV cache
while keeping model weights on the GPU. It writes `ready-for-snapshot` only
after both operations succeed.

The program enables SGLang's memory saver and disables CPU weight backup.
Weights are saved directly from GPU memory without a CPU backup copy.

### After the Restore

After restore, the checkpointed process calls
`Engine.resume_memory_occupation(tags=["kv_cache"])` and
`TokenizerManager.continue_generation()`. It runs another generation and starts
an API on port 8000. It writes `sglang-restore-ready` only after the generation
succeeds and the API is listening.
To check that the restore worked, see
[Verify the Restored Pod](../restore.md#verify-the-restored-pod).

### Image and Settings

The capture manifests run the tested SGLang image unmodified, and mount `app.py`
at `/snapshot-app` from the `sglang-app` ConfigMap created in
[Create the app.py ConfigMap](#create-the-apppy-configmap).

### Model Cache

An init container downloads the selected model into the persistent [Model
Cache](../README.md#model-cache). The source application then loads that cache
with `HF_HUB_OFFLINE=1` so the checkpointed process has no open Hugging Face
connections.

### Pod Requirements

The source and restore pods must use the same immutable image, mount the
Snapshot control volume at `/snapshot-control`, and mount the same model cache
at `/hf-cache`.

The program writes `ready-for-snapshot` and `sglang-restore-ready` into the control
volume. Snapshot and the pod's readiness check read those files to know when the
process is safe to checkpoint and when the restore is complete. The recipe
manifests already mount it. If you write your own manifest, see
[Pod Requirements](../../reference/workload-contract.md#pod-requirements).

## Create the app.py ConfigMap

Create the ConfigMap that the capture manifests mount `app.py` from:

```bash
kubectl create configmap sglang-app \
  --namespace "$WORKLOAD_NAMESPACE" \
  --from-file=app.py=sglang/app.py
```

`kubectl create configmap` fails if the ConfigMap already exists. To update it
after editing `app.py`, use `apply` instead:

```bash
kubectl create configmap sglang-app \
  --namespace "$WORKLOAD_NAMESPACE" \
  --from-file=app.py=sglang/app.py \
  --dry-run=client --output yaml | \
  kubectl apply --namespace "$WORKLOAD_NAMESPACE" --filename -
```

## Choose the Model

The recipes already select their model, for example in
[`capture/qwen3-0.6b.yaml`](../sglang/capture/qwen3-0.6b.yaml). The init container
downloads it into the [Model Cache](../README.md#model-cache), and the main
container's last argument is a JSON object of `sglang.Engine` keyword arguments:

```yaml
initContainers:
  - name: model-cache
    args:
      - <download script>
      - Qwen/Qwen3-0.6B
containers:
  - name: main
    args:
      - --mode
      - snapshot
      - '{"model_path": "Qwen/Qwen3-0.6B"}'
```

### Run Another Model

To run another model, change the model name in both places in a copy of the
manifest: the `model-cache` init container, which downloads the model, and the
`main` container's `model_path` argument. The main container runs offline from the
model cache, so it can only load a model the init container has downloaded.

### Engine Sizing

The example configures a context length of 10240 tokens for a 24 GiB NVIDIA A10
GPU. Reduce `SGLANG_CONTEXT_LENGTH` for a smaller GPU or increase it only after
validating the resulting memory use. The KV cache page size is set through
`SGLANG_PAGE_SIZE` (default `16`).

### Engine Arguments

`app.py` sets `trust_remote_code=False`; Qwen3 needs no custom model code. Add
other `sglang.Engine` keyword arguments, such as `trust_remote_code` or
`tp_size`, to the same JSON object.

> [!NOTE]
> This example runs SGLang directly through `sglang.Engine` rather than
> `sglang.launch_server`, so the standard server's command-line arguments do not
> apply. Engine keyword arguments come from the JSON argument, and other runtime settings
> are supplied through SGLang's [environment variables](https://docs.sglang.ai/references/environment_variables.html)
> set in the Deployment's Pod template.

### Eight-GPU Recipes

The JSON argument sets `tp_size` and `ep_size` to the GPU count. Set `dp_size`
or `pp_size` in the same object for other layouts. Both places pin a `revision`
of the model.

## Deploy the Source Pod

This section is for checkpointing with a `PodSnapshot`. It deploys the source pod
that the `PodSnapshot` checkpoints. When you finish, you have a running pod that is
ready to checkpoint. You need its name for the `PodSnapshot` that you create in
[Checkpoint a Pod](../checkpoint.md).

If you checkpoint with a `SnapshotJob`, skip this section: the job creates the
source replica itself. Continue with
[Checkpoint, Option 2](../checkpoint.md#option-2---snapshotjob-checkpoint-a-throwaway-pod).

1. Apply the source manifest of your recipe. The commands on this page are for
   the SGLang Qwen3 0.6B recipe. For another recipe, use its manifest and its
   source name (`$SOURCE_NAME`, from [Set the Names](../README.md#set-the-names)).

   ```bash
   kubectl apply \
     --namespace "$WORKLOAD_NAMESPACE" \
     --filename sglang/capture/qwen3-0.6b.yaml
   ```

   The init container reuses cached model files and downloads missing files. The
   main container then starts SGLang from the offline cache.

   **Eight-GPU recipes.** Apply the recipe manifest instead of the one above.
   For example, GLM 5.3:

   ```bash
   kubectl apply \
     --namespace "$WORKLOAD_NAMESPACE" \
     --filename sglang/capture/glm-5.3.yaml
   ```

2. Wait until the SGLang replica finishes initialization and becomes safe to
   checkpoint. The readiness probe succeeds after `app.py` writes
   `ready-for-snapshot`. Allow `--timeout=60m` for an eight-GPU recipe, because
   its first run downloads the model:

   ```bash
   kubectl rollout status \
     --namespace "$WORKLOAD_NAMESPACE" \
     deployment/sglang-qwen3-0-6b-source \
     --timeout=30m
   ```

3. List the generated pod. Use its name in the `PodSnapshot` that you create in
   [Checkpoint a Pod](../checkpoint.md):

   ```bash
   kubectl get pods \
     --namespace "$WORKLOAD_NAMESPACE" \
     --selector app=sglang-qwen3-0-6b-source
   ```

## Next Steps

- [Checkpoint a Pod](../checkpoint.md) - Create a snapshot of the source replica
- [Restore a Pod](../restore.md) - Start a new pod from the snapshot
