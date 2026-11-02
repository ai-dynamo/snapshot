# Prepare a vLLM Workload

This guide makes a vLLM workload snapshot-ready by mounting an entrypoint into a
vLLM runtime image, implementing Snapshot's [workload
contract](../../reference/workload-contract.md).

The example runs the official vLLM image that includes vLLM and its runtime
dependencies, unmodified. The capture manifests pin the exact upstream image,
and one program, `app.py`, is mounted into it from a ConfigMap to prepare vLLM
for checkpoint and resume it after restore. The Snapshot agent injects the
restore tooling at runtime.

This is stage 1 of [Using Snapshot](../README.md): prepare the workload. Complete
[Before You Start](../README.md#before-you-start) and
[pick a recipe](../README.md#choose-your-recipe) first. The commands run from
`docs/using-snapshot`.

> [!NOTE]
> This example is validated on vLLM 0.31.0 (the pinned
> `vllm/vllm-openai:v0.31.0-ubuntu2404` image).

## How the Example Works

### Before the Checkpoint

The program loads the model selected in the capture manifest, runs one
generation to initialize vLLM, and then calls `pause_generation()` and
`release_kv_cache_memory()`. This discards the KV cache and keeps weights on the
GPU for capture, without a CPU copy of the weights. It writes
`ready-for-snapshot` only when the process is safe to checkpoint.

### After the Restore

In a restore container, Snapshot runs its restore proxy in place of the program
and restores the checkpointed process. That process calls `wake_up()` and
`resume_generation()`, runs another generation, starts an API on port 8000, and
writes `vllm-restore-ready` when the API is listening.
[Restore](../restore.md#verify-the-restored-pod) shows how to confirm the
restore.

### Image and Settings

The capture manifests run vLLM's own Ubuntu 24.04 build of the 0.31.0 image
(`v0.31.0-ubuntu2404`) unmodified, which already matches the glibc floor the
current Snapshot restore bundle requires, and mount `app.py` at `/snapshot-app`
from the `vllm-app` ConfigMap created in [Create the app.py
ConfigMap](#create-the-apppy-configmap). `HF_HUB_DISABLE_XET=1` prevents the
model downloader from leaving an open cache log that CRIU cannot reopen after
restore.

### Model Cache

An init container downloads the selected model into the persistent
[Model Cache](../README.md#model-cache). The source application then loads that
cache with `HF_HUB_OFFLINE=1`, so the checkpointed process has no open Hugging
Face connections.

### Pod Requirements

The program writes `ready-for-snapshot` and `vllm-restore-ready` into the
Snapshot control volume, which the source and restore pods mount at
`/snapshot-control`. Snapshot and the pod's readiness check read those files to
know when the process is safe to checkpoint and when the restore is complete. The
recipe manifests already mount it. If you write your own manifest, see
[Pod Requirements](../../reference/workload-contract.md#pod-requirements).

## Create the app.py ConfigMap

Create the ConfigMap that the capture manifests mount `app.py` from:

```bash
kubectl create configmap vllm-app \
  --namespace "$WORKLOAD_NAMESPACE" \
  --from-file=app.py=vllm/app.py
```

`kubectl create configmap` fails if the ConfigMap already exists. To update it
after editing `app.py`, use `apply` instead:

```bash
kubectl create configmap vllm-app \
  --namespace "$WORKLOAD_NAMESPACE" \
  --from-file=app.py=vllm/app.py \
  --dry-run=client --output yaml | \
  kubectl apply --namespace "$WORKLOAD_NAMESPACE" --filename -
```

## Choose the Model

The recipes already select their model, for example in
[`capture/qwen3-0.6b.yaml`](../vllm/capture/qwen3-0.6b.yaml). The init container
downloads it into the [Model Cache](../README.md#model-cache), and the main
container's last argument is a JSON object of `AsyncEngineArgs` keyword arguments:

```yaml
initContainers:
  - name: model-cache
    args:
      - <download script>
      - Qwen/Qwen3-0.6B
containers:
  - name: main
    args:
      - '{"model": "Qwen/Qwen3-0.6B"}'
```

### Run Another Model

To run another model, change it in both places in a copy of the manifest, because
the main container runs offline from the model cache. Other values include
`TinyLlama/TinyLlama-1.1B-Chat-v1.0`. A model path mounted from elsewhere, such as
`/models/Qwen3-0.6B`, must be available to both the source and restored containers.

### Engine Sizing

The example sizes the engine for a small single-GPU deployment through
`VLLM_MAX_MODEL_LEN` (default `2048`) and `VLLM_GPU_MEMORY_UTILIZATION` (default
`0.30`). Raise them only after validating checkpoint and restore with the
resulting memory use.

### Engine Arguments

`app.py` sets `trust_remote_code=False`; Qwen3 needs no custom model code. Add
other `AsyncEngineArgs` keyword arguments, such as `trust_remote_code` or
`tensor_parallel_size`, to the same JSON object.

> [!NOTE]
> This example runs vLLM directly through `AsyncLLM` rather than `vllm serve`, so
> the standard `vllm serve` command-line arguments do not apply. Engine keyword
> arguments come from the JSON argument, and other runtime settings are supplied through
> vLLM's [environment variables](https://docs.vllm.ai/en/v0.31.0/configuration/env_vars/)
> set in the Deployment's Pod template.

### Worker Multiprocessing

`app.py` also sets `VLLM_WORKER_MULTIPROC_METHOD=spawn` before importing vLLM.
Calling `AsyncLLM` directly rather than vLLM's CLI wrapper skips the wrapper's
automatic default; without it, worker startup falls back to `fork` (or
switches to `spawn` only if vLLM detects CUDA already initialized), which is
unreliable across checkpoint/restore.

### Eight-GPU Recipes

The JSON argument sets `tensor_parallel_size` to the GPU
count, and the MoE recipes also set `enable_expert_parallel`. Both places pin a
`revision` of the model.

## Deploy the Source Pod

Deploy the source pod that Option 1 (`PodSnapshot`) checkpoints. When you finish,
you have a running pod that is ready to checkpoint. You need its name for the
`PodSnapshot` that you create in [Checkpoint a Pod](../checkpoint.md).

Skip this step if you checkpoint with a `SnapshotJob`: it creates the source
replica itself. Continue with
[Checkpoint, Option 2](../checkpoint.md#option-2--snapshotjob-checkpoint-a-throwaway-pod).

1. Apply the source manifest of your recipe. The commands on this page are for
   the vLLM Qwen3 0.6B recipe. For another recipe, use its manifest and its
   source name (`$SOURCE_NAME`, from [Set the Names](../README.md#set-the-names)).

   ```bash
   kubectl apply \
     --namespace "$WORKLOAD_NAMESPACE" \
     --filename vllm/capture/qwen3-0.6b.yaml
   ```

   **Eight-GPU recipes.** First set the agent image in the manifest, as
   [Eight-GPU Recipes](../README.md#eight-gpu-recipes) describes: replace
   `${SNAPSHOT_AGENT_IMAGE}` in the `snapshot-cuda-install` init container. Then
   apply that manifest instead of the one above. For example, GLM 5.3:

   ```bash
   kubectl apply \
     --namespace "$WORKLOAD_NAMESPACE" \
     --filename vllm/capture/glm-5.3.yaml
   ```

2. Wait until the vLLM replica finishes initialization and becomes safe to
   checkpoint. The readiness probe succeeds after `app.py` writes
   `ready-for-snapshot`. Allow `--timeout=60m` for an eight-GPU recipe, because
   its first run downloads the model:

   ```bash
   kubectl rollout status \
     --namespace "$WORKLOAD_NAMESPACE" \
     deployment/vllm-qwen3-0-6b-source \
     --timeout=30m
   ```

3. List the generated pod. Use its name in the `PodSnapshot` that you create in
   [Checkpoint a Pod](../checkpoint.md):

   ```bash
   kubectl get pods \
     --namespace "$WORKLOAD_NAMESPACE" \
     --selector app=vllm-qwen3-0-6b-source
   ```

## Next Steps

- [Checkpoint a Pod](../checkpoint.md)
- [Restore a Pod](../restore.md)
