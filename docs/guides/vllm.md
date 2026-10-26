# Deploy a vLLM replica

This guide makes a vLLM workload snapshot-ready by mounting an entrypoint
into a vLLM runtime image, implementing Snapshot's [workload
contract](../reference/workload-contract.md). The example runs the official
vLLM image that includes vLLM and its runtime dependencies, unmodified. The
capture manifests pin the exact upstream image, and one program, `app.py`, is
mounted into it from a ConfigMap to prepare vLLM for checkpoint and resume it
after restore. The Snapshot agent injects the restore tooling at runtime.

This is stage 1 of the [usage guides](README.md). Complete
[Before you start](README.md#before-you-start) and
[pick a recipe](README.md#choose-your-recipe) first. The commands run from
`docs/guides`.

> [!NOTE]
> This example is validated on vLLM 0.31.0 (the pinned
> `vllm/vllm-openai:v0.31.0-ubuntu2404` image).

For customers using 0.27.1, see
[Earlier vLLM versions](#earlier-vllm-versions-0271). The numbered steps use
`spawn` for the single-GPU path. For other configurations, see
[Alternative launch methods](#alternative-launch-methods).

## How the example works

The program loads the model selected in the capture manifest, runs one
generation to initialize vLLM, and then calls `pause_generation()` and
`release_kv_cache_memory()`. This discards the KV cache and keeps weights on
the GPU for capture, without a CPU copy of the weights. It writes
`ready-for-snapshot` only when the process is safe to checkpoint. In a restore
container, Snapshot runs its restore proxy in place of the program and restores
the checkpointed process. That process calls `wake_up()` and
`resume_generation()`, runs another generation, starts an API on port 8000,
and writes `vllm-restore-ready` when the API is listening.
[Restore](restore.md#4-verify-the-restored-replica) shows how to confirm the restore.

The capture manifests run vLLM's own Ubuntu 24.04 build of the 0.31.0 image
(`v0.31.0-ubuntu2404`) unmodified, which already matches the glibc floor the
current Snapshot restore bundle requires, and mount `app.py` at
`/snapshot-app` from the `vllm-app` ConfigMap created in step 1.
`HF_HUB_DISABLE_XET=1` prevents the model downloader from leaving an open cache
log that CRIU cannot reopen after restore.

An init container downloads the selected model into the persistent
[model cache](README.md#model-cache). The source application then loads that
cache with `HF_HUB_OFFLINE=1`, so the checkpointed process has no open Hugging
Face connections.

The source and restore pods must mount the Snapshot control volume at
`/snapshot-control`.

## 1. Create the app.py ConfigMap

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

## 2. Choose the model

The recipes already select their model, for example in
[`capture/qwen3-0.6b.yaml`](vllm/capture/qwen3-0.6b.yaml). The init container
downloads it into the [model cache](README.md#model-cache), and the main
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

To run another model, change it in both places in a copy of the manifest, because
the main container runs offline from the model cache. Other values include
`TinyLlama/TinyLlama-1.1B-Chat-v1.0`. A model path mounted from elsewhere, such as
`/models/Qwen3-0.6B`, must be available to both the source and restored containers.

The example sizes the engine for a small single-GPU deployment through
`VLLM_MAX_MODEL_LEN` (default `2048`) and `VLLM_GPU_MEMORY_UTILIZATION`
(default `0.30`). Raise them only after validating checkpoint and restore with
the resulting memory use. `app.py` sets `trust_remote_code=False`; Qwen3 needs
no custom model code. Add other `AsyncEngineArgs` keyword arguments, such as
`trust_remote_code` or `tensor_parallel_size`, to the same JSON object.

> [!NOTE]
> This example runs vLLM directly through `AsyncLLM` rather than `vllm serve`, so
> the standard `vllm serve` command-line arguments do not apply. Engine keyword
> arguments come from the JSON argument, and other runtime settings come from
> vLLM's [environment variables](https://docs.vllm.ai/en/v0.31.0/configuration/env_vars/)
> set in the Deployment's Pod template.

`app.py` also sets `VLLM_WORKER_MULTIPROC_METHOD=spawn` before importing vLLM.
Calling `AsyncLLM` directly rather than vLLM's CLI wrapper skips the wrapper's
automatic default; without it, worker startup falls back to `fork` (or
switches to `spawn` only if vLLM detects CUDA already initialized), which is
unreliable across checkpoint/restore.

**Eight-GPU recipes.** The JSON argument sets `tensor_parallel_size` to the GPU
count, and the MoE recipes also set `enable_expert_parallel`. Both places pin a
`revision` of the model.

## 3. Deploy the source replica

Skip this step if you checkpoint with a `SnapshotJob`: it creates the source
replica itself. Continue with
[Checkpoint, Option 2](checkpoint.md#option-2--snapshotjob-checkpoint-a-throwaway-replica).

Deploy the source Deployment of your recipe:

```bash
kubectl apply \
  --namespace "$WORKLOAD_NAMESPACE" \
  --filename vllm/capture/qwen3-0.6b.yaml
```

**Eight-GPU recipes.** First set the agent image in the manifest, as
[Multi-GPU recipes](README.md#choose-your-recipe) describes: replace
`${SNAPSHOT_AGENT_IMAGE}` in the `snapshot-cuda-install` init container. Then
apply it. For example, GLM 5.3:

```bash
kubectl apply \
  --namespace "$WORKLOAD_NAMESPACE" \
  --filename vllm/capture/glm-5.3.yaml
```

Wait until the vLLM replica finishes initialization and becomes safe to
checkpoint. For another recipe, use its Deployment name from the
[recipe table](README.md#choose-your-recipe) here and in the next command. Allow
`--timeout=60m` for an eight-GPU recipe, because its first run downloads the
model:

```bash
kubectl rollout status \
  --namespace "$WORKLOAD_NAMESPACE" \
  deployment/vllm-qwen3-0-6b-source \
  --timeout=30m
```

List the generated Pod:

```bash
kubectl get pods \
  --namespace "$WORKLOAD_NAMESPACE" \
  --selector app=vllm-qwen3-0-6b-source
```

Use that Pod name in the `PodSnapshot` created in the next stage. The
readiness probe succeeds after `app.py` writes `ready-for-snapshot`.

## 4. Checkpoint and restore

Follow [Checkpoint a replica](checkpoint.md#option-1--podsnapshot-checkpoint-a-running-replica)
to create a `PodSnapshot` for the source pod and wait for it to become Ready.
Then follow [Restore a replica](restore.md), using your recipe's restore
profile and the same runtime image as the source.
Verify restored inference through the application's `/generate` endpoint.

## Alternative launch methods

The numbered walkthrough uses `spawn`. When vLLM is used as a Python
library, workers can default to
[`fork`, rather than `spawn`](https://docs.vllm.ai/en/latest/design/multiprocessing/).
Forked workers can inherit CUDA state and NVIDIA device handles. CUDA
checkpointing must handle that state across the worker process tree.

### Single-node multi-GPU workloads

If you are still using Snapshot 0.1.0, first
[install](../operations/install.md#install-from-a-release) or
[upgrade](../operations/install.md#upgrade-an-existing-release) to Snapshot
0.2 or later. Use a [multi-GPU recipe](README.md#choose-your-recipe) or follow
the [SnapshotJob setup](checkpoint.md#option-2--snapshotjob-checkpoint-a-throwaway-replica).
The opt-in prepares the CUDA processes for checkpointing; it does not
configure vLLM parallelism or GPU requests. Match the source and restore
GPU counts and follow the [workload contract](../reference/workload-contract.md#multi-gpu-workloads).

### Native CUDA job wrapping

For a single-GPU workload, launching the application with
[`cuda-checkpoint --launch-job`](https://github.com/NVIDIA/cuda-checkpoint)
groups its CUDA processes into a job so checkpointing can coordinate their
state. This is an alternative to changing worker startup to `spawn`, not
an additional prerequisite. SnapshotJob does **not** automatically add native
CUDA job wrapping.

Use the [driver version recommended for native CUDA job wrapping](../reference/compatibility.md#minimum-versions-by-feature).

## Earlier vLLM versions (0.27.1)

For vLLM 0.27.1, use the matching
[`app.py`](https://github.com/ai-dynamo/snapshot/blob/ec327cc34c9de2a55b0f541c3a3039504f72429b/docs/guides/vllm/app.py),
[source manifest](https://github.com/ai-dynamo/snapshot/blob/ec327cc34c9de2a55b0f541c3a3039504f72429b/docs/guides/vllm/deployment.yaml),
and [restore manifest](https://github.com/ai-dynamo/snapshot/blob/ec327cc34c9de2a55b0f541c3a3039504f72429b/docs/guides/vllm/restore-deployment.yaml)
from that revision. They pin the previously validated
`v0.27.1-ubuntu2404` image. Create the `vllm-app` ConfigMap from that application
and deploy those manifests; do not substitute the current `app.py` or
recipes. Source and restore must use the same image as the checkpoint.
