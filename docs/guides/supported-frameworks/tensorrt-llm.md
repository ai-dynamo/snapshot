# Prepare a TensorRT-LLM Workload

This guide makes a TensorRT-LLM workload snapshot-ready by mounting an
entrypoint into a TensorRT-LLM runtime image, implementing Snapshot's [Workload
Contract](../../integrate/workload-contract.md).

The example runs the TensorRT-LLM image that includes TensorRT-LLM and its
runtime dependencies, unmodified. The capture manifests pin the exact upstream
image, and one program, `app.py`, is mounted into it from a ConfigMap to prepare
TensorRT-LLM for checkpoint and validate it after restore. The Snapshot agent
injects the restore tooling at runtime.

This is stage 1 of [Checkpoint and Restore](../README.md): prepare the workload. Complete
[Before You Start](../README.md#before-you-start) and
[pick a recipe](../README.md#choose-your-recipe) first. The commands run from
`docs/guides`.

> [!NOTE]
> TensorRT-LLM support is experimental.
> On arm64, set `TLLM_WORKER_USE_SINGLE_PROCESS=1` in the container `env`; without
> it the checkpoint hangs.

## How the Example Works

### Before the Checkpoint

The program loads the model selected in the capture manifest and calls
`LLM.generate()` to initialize TensorRT-LLM. The synchronous call returns only
after generation finishes, so no request remains in flight. The program then
releases KV cache on every rank, runs `gc.collect()`, and writes
`ready-for-snapshot` when it reaches the safe checkpoint point.

The recipes use the pinned rc24 image's private executor collective RPC for
KV-only `sleep` and `wakeup`. `sleep_config` sets the KV restore mode to `NONE`,
so cache contents are discarded. Weights remain on the GPU with no CPU backup.
KV manager V1 and disabled block reuse avoid retaining prefix-cache entries
across this release. These settings are enforced by `app.py`.

### After the Restore

After restore, the process recreates KV cache, calls `LLM.generate()` again, and
starts an API on port 8000. It writes `trtllm-restore-ready` only after that
generation succeeds and the API is listening.
[Restore](../restore.md#verify-the-restored-pod) shows how to confirm the
restore.

### Image and Settings

The capture manifests run the TensorRT-LLM `1.3.0rc24` release image, pinned by
digest, unmodified, and mount `app.py` at `/snapshot-app` from the
`tensorrt-llm-app` ConfigMap created in
[Create the app.py ConfigMap](#create-the-apppy-configmap). A release candidate is used
deliberately: the `1.2.1` GA image fails at `import tensorrt` because
`libnvonnxparser.so.10` is missing from it, and no 1.3.0 GA image exists yet.
Move to the first 1.3.x GA once it is published.

`OMPI_MCA_pml=ob1` and `OMPI_MCA_btl=tcp,self` keep MPI communication on TCP to
avoid RDMA mappings that CRIU cannot restore. `TORCHINDUCTOR_COMPILE_THREADS=1`
disables forked asynchronous compiler workers, which can inherit CUDA device
mappings that prevent CRIU capture.

### Model Cache

An init container downloads the selected model into the persistent
[Model Cache](../README.md#model-cache). The source application then loads that
cache with `HF_HUB_OFFLINE=1`, so the checkpointed process has no open Hugging
Face connections.

### Pod Requirements

The source and restore pods must use the same immutable image and mount the
Snapshot control volume at `/snapshot-control`.

The program writes `ready-for-snapshot` and `trtllm-restore-ready` into the control
volume. Snapshot and the pod's readiness check read those files to know when the
process is safe to checkpoint and when the restore is complete. The recipe
manifests already mount it. If you write your own manifest, see
[Pod Requirements](../../integrate/workload-contract.md#pod-requirements).

## Create the app.py ConfigMap

Create the ConfigMap that the capture manifests mount `app.py` from:

```bash
kubectl create configmap tensorrt-llm-app \
  --namespace "$WORKLOAD_NAMESPACE" \
  --from-file=app.py=tensorrt-llm/app.py
```

`kubectl create configmap` fails if the ConfigMap already exists. To update it
after editing `app.py`, use `apply` instead:

```bash
kubectl create configmap tensorrt-llm-app \
  --namespace "$WORKLOAD_NAMESPACE" \
  --from-file=app.py=tensorrt-llm/app.py \
  --dry-run=client --output yaml | \
  kubectl apply --namespace "$WORKLOAD_NAMESPACE" --filename -
```

## Choose the Model

The recipes already select their model, for example in
[`capture/qwen3-0.6b.yaml`](../tensorrt-llm/capture/qwen3-0.6b.yaml). The init
container downloads it into the [Model Cache](../README.md#model-cache), and the main
container's last argument is a JSON object of `LLM` keyword arguments:

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

To run another model, select one supported by the chosen TensorRT-LLM image and
change it in both places in a copy of the manifest, because the main container runs
offline from the model cache.

### Engine Sizing

The example uses one GPU, the PyTorch backend, and a maximum sequence length of
512 tokens. Engine sizing is set through `TRTLLM_MAX_NUM_TOKENS` (default
`1024`), `TRTLLM_MAX_BATCH_SIZE` (default `1`), and
`TRTLLM_FREE_GPU_MEMORY_FRACTION` (default `0.10`).

### Engine Arguments

`app.py` sets `trust_remote_code=False`; Qwen3 needs no custom model code. Add
other `LLM` keyword arguments, such as `trust_remote_code` or
`tensor_parallel_size`, to the same JSON object. Revalidate checkpoint and
restore before changing the model, TensorRT-LLM image, GPU count, backend, or
engine settings.

> [!NOTE]
> This example runs TensorRT-LLM through the `LLM` API rather than `trtllm-serve`,
> so the standard `trtllm-serve` command-line arguments do not apply. Engine keyword
> arguments come from the JSON argument and are passed to the
> [`LLM` API](https://nvidia.github.io/TensorRT-LLM/llm-api/reference.html) in
> `app.py`.

### Eight-GPU Recipes

The JSON argument sets `tensor_parallel_size` and `moe_expert_parallel_size` to
the GPU count, and selects `allreduce_strategy="NCCL"`. With the pinned engine,
AUTO produced intermittent incorrect source answers before checkpointing in
validation. NCCL passed source inference, KV release and resume, and three
restored inference checks on the same eight B200 GPUs. Both places pin a
`revision` of the model.

## Deploy the Source Pod

Deploy the source pod that Option 1 (`PodSnapshot`) checkpoints. When you finish,
you have a running pod that is ready to checkpoint. You need its name for the
`PodSnapshot` that you create in [Checkpoint a Pod](../checkpoint.md).

Skip this step if you checkpoint with a `SnapshotJob`: it creates the source
replica itself. Continue with
[Checkpoint, Option 2](../checkpoint.md#option-2--snapshotjob-checkpoint-a-throwaway-pod).

1. Apply the source manifest of your recipe. The commands on this page are for
   the TensorRT-LLM Qwen3 0.6B recipe. For another recipe, use its manifest and its
   source name (`$SOURCE_NAME`, from [Set the Names](../README.md#set-the-names)).

   ```bash
   kubectl apply \
     --namespace "$WORKLOAD_NAMESPACE" \
     --filename tensorrt-llm/capture/qwen3-0.6b.yaml
   ```

   **Eight-GPU recipes.** First set the agent image in the manifest, as
   [Eight-GPU Recipes](../README.md#eight-gpu-recipes) describes: replace
   `${SNAPSHOT_AGENT_IMAGE}` in the `snapshot-cuda-install` init container. Then
   apply that manifest instead of the one above. For example, GLM 5.3:

   ```bash
   kubectl apply \
     --namespace "$WORKLOAD_NAMESPACE" \
     --filename tensorrt-llm/capture/glm-5.3.yaml
   ```

2. Wait until the TensorRT-LLM replica finishes initialization and becomes safe to
   checkpoint. The readiness probe succeeds after `app.py` writes
   `ready-for-snapshot`. Allow `--timeout=60m` for an eight-GPU recipe, because
   its first run downloads the model:

   ```bash
   kubectl rollout status \
     --namespace "$WORKLOAD_NAMESPACE" \
     deployment/tensorrt-llm-qwen3-0-6b-source \
     --timeout=30m
   ```

3. List the generated pod. Use its name in the `PodSnapshot` that you create in
   [Checkpoint a Pod](../checkpoint.md):

   ```bash
   kubectl get pods \
     --namespace "$WORKLOAD_NAMESPACE" \
     --selector app=tensorrt-llm-qwen3-0-6b-source
   ```

## Next Steps

- [Checkpoint a Pod](../checkpoint.md)
- [Restore a Pod](../restore.md)
