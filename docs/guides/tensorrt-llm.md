# Deploy a TensorRT-LLM replica

This guide makes a TensorRT-LLM workload snapshot-ready by mounting an
entrypoint into a TensorRT-LLM runtime image, implementing Snapshot's
[workload contract](../reference/workload-contract.md). The example runs the
TensorRT-LLM image that includes TensorRT-LLM and its runtime dependencies,
unmodified. The capture manifests pin the exact upstream image, and one
program, `app.py`, is mounted into it from a ConfigMap to prepare
TensorRT-LLM for checkpoint and validate it after restore. The Snapshot
agent injects the restore tooling at runtime.

This is stage 1 of the [usage guides](README.md). Complete
[Before you start](README.md#before-you-start) and
[pick a recipe](README.md#pick-a-recipe) first. The commands run from
`docs/guides`.

> [!NOTE]
> TensorRT-LLM support is experimental.
> On arm64, set `TLLM_WORKER_USE_SINGLE_PROCESS=1` in the container `env`; without
> it the checkpoint hangs.

## How the example works

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

`TORCHINDUCTOR_COMPILE_THREADS=1` disables forked asynchronous compiler workers,
which can inherit CUDA device mappings that prevent CRIU capture.

After restore, the process recreates KV cache, calls `LLM.generate()` again,
and starts an API on port 8000. It writes `trtllm-restore-ready` only after
that generation succeeds and the API is listening.
[Restore](restore.md#4-verify-the-restored-replica) sends it a request.

The capture manifests run the TensorRT-LLM `1.3.0rc24` release image, pinned by
digest, unmodified, and mount `app.py` at `/snapshot-app` from the
`tensorrt-llm-app` ConfigMap created in step 1. A release candidate is used
deliberately: the `1.2.1` GA image fails at `import tensorrt` because
`libnvonnxparser.so.10` is missing from it, and no 1.3.0 GA image exists yet.
Move to the first 1.3.x GA once it is published.
`OMPI_MCA_pml=ob1` and `OMPI_MCA_btl=tcp,self` keep MPI communication on TCP
to avoid RDMA mappings that CRIU cannot restore.

An init container downloads the selected model into the persistent
[model cache](README.md#model-cache). The source application then loads that
cache with `HF_HUB_OFFLINE=1`, so the checkpointed process has no open Hugging
Face connections.

The source and restore pods must use the same immutable image and mount the
Snapshot control volume at `/snapshot-control`.

## 1. Create the app.py ConfigMap

Create the ConfigMap that the capture manifests mount `app.py` from:

```bash
kubectl create configmap tensorrt-llm-app \
  --namespace "$SNAPSHOT_NAMESPACE" \
  --from-file=app.py=tensorrt-llm/app.py
```

`kubectl create configmap` fails if the ConfigMap already exists. To update it
after editing `app.py`, use `apply` instead:

```bash
kubectl create configmap tensorrt-llm-app \
  --namespace "$SNAPSHOT_NAMESPACE" \
  --from-file=app.py=tensorrt-llm/app.py \
  --dry-run=client --output yaml | \
  kubectl apply --namespace "$SNAPSHOT_NAMESPACE" --filename -
```

## 2. Choose the model

The recipes already select their model, for example in
[`capture/qwen3-0.6b.yaml`](tensorrt-llm/capture/qwen3-0.6b.yaml). The init
container downloads it into the [model cache](README.md#model-cache), and the main
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

To run another model, select one supported by the chosen TensorRT-LLM image and
change it in both places in a copy of the manifest, because the main container runs
offline from the model cache.

The example uses one GPU, the PyTorch backend, and a maximum sequence length of
512 tokens. Engine sizing is set through `TRTLLM_MAX_NUM_TOKENS` (default
`1024`), `TRTLLM_MAX_BATCH_SIZE` (default `1`), and
`TRTLLM_FREE_GPU_MEMORY_FRACTION` (default `0.10`). `app.py` sets
`trust_remote_code=False`; Qwen3 needs no custom model code. Add other `LLM`
keyword arguments, such as `trust_remote_code` or `tensor_parallel_size`, to the
same JSON object. Revalidate checkpoint and restore before changing the model,
TensorRT-LLM image, GPU count, backend, or engine settings.

> [!NOTE]
> This example runs TensorRT-LLM through the `LLM` API rather than `trtllm-serve`,
> so the standard `trtllm-serve` command-line arguments do not apply. Engine keyword
> arguments come from the JSON argument and are passed to the
> [`LLM` API](https://nvidia.github.io/TensorRT-LLM/llm-api/reference.html) in
> `app.py`.

**Eight-GPU recipes.** The JSON argument sets `tensor_parallel_size` and
`moe_expert_parallel_size` to the GPU count, and selects
`allreduce_strategy="NCCL"`. With the pinned engine, AUTO produced intermittent
incorrect source answers before checkpointing in validation. NCCL passed source
inference, KV release and resume, and three restored inference checks on the same
eight B200 GPUs. Both places pin a `revision` of the model.

## 3. Deploy the source replica

Skip this step if you checkpoint with a `SnapshotJob`: it creates the source
replica itself. Continue with
[Checkpoint, Option 2](checkpoint.md#option-2--snapshotjob-checkpoint-a-throwaway-replica).

Deploy the source Deployment of your recipe:

```bash
kubectl apply \
  --namespace "$SNAPSHOT_NAMESPACE" \
  --filename tensorrt-llm/capture/qwen3-0.6b.yaml
```

**Eight-GPU recipes.** Substitute the agent image from
[Eight-GPU recipes](README.md#eight-gpu-recipes) as you apply the manifest. For
example, GLM 5.3:

```bash
sed "s|\${SNAPSHOT_AGENT_IMAGE}|$SNAPSHOT_AGENT_IMAGE|" tensorrt-llm/capture/glm-5.3.yaml | \
  kubectl apply --namespace "$SNAPSHOT_NAMESPACE" --filename -
```

Wait until the TensorRT-LLM replica finishes initialization and becomes safe to
checkpoint. For another recipe, use its Deployment name from the
[recipe table](README.md#pick-a-recipe) here and in the next command. Allow
`--timeout=60m` for an eight-GPU recipe, because its first run downloads the
model:

```bash
kubectl rollout status \
  --namespace "$SNAPSHOT_NAMESPACE" \
  deployment/tensorrt-llm-qwen3-0-6b-source \
  --timeout=30m
```

List the generated Pod:

```bash
kubectl get pods \
  --namespace "$SNAPSHOT_NAMESPACE" \
  --selector app=tensorrt-llm-qwen3-0-6b-source
```

Use that Pod name in the `PodSnapshot` created in the next stage. The
readiness probe succeeds after `app.py` writes `ready-for-snapshot`.

## Next steps

- [Checkpoint a replica](checkpoint.md)
- [Restore a replica](restore.md)
