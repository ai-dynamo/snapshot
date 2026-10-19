# Deploy an SGLang replica

This guide makes an SGLang workload snapshot-ready by mounting an entrypoint
into an SGLang runtime image, implementing Snapshot's [workload
contract](../reference/workload-contract.md). The example runs an SGLang
image that includes SGLang, CUDA, and `torch_memory_saver`, unmodified. The
capture manifests pin the exact upstream image, and one program, `app.py`, is
mounted into it from a ConfigMap to prepare SGLang for checkpoint and resume
it after restore. The Snapshot agent injects the restore tooling at runtime.

This is stage 1 of the [usage guides](README.md). Complete
[Before you start](README.md#before-you-start) and
[pick a recipe](README.md#pick-a-recipe) first. The commands run from
`docs/guides`.

## How the example works

The program creates a direct `sglang.Engine`, runs one generation, and calls
`TokenizerManager.pause_generation()` followed by
`Engine.release_memory_occupation(tags=["kv_cache"])` to discard the KV cache
while keeping model weights on the GPU. It writes `ready-for-snapshot` only after
both operations succeed.

The program enables SGLang's memory saver and disables CPU weight backup.
Weights are captured directly from GPU memory without a CPU backup copy. An init
container downloads the selected model into the persistent
[model cache](README.md#model-cache). The source application then loads that
cache with `HF_HUB_OFFLINE=1` so the checkpointed process has no open Hugging
Face connections.

After restore, the checkpointed process calls
`Engine.resume_memory_occupation(tags=["kv_cache"])` and
`TokenizerManager.continue_generation()`. It runs another generation and
starts an API on port 8000. It writes `sglang-restore-ready` only after the
generation succeeds and the API is listening.
[Restore](restore.md#4-verify-the-restored-replica) sends it a request.

The capture manifests run the tested SGLang image unmodified, and mount `app.py`
at `/snapshot-app` from the `sglang-app` ConfigMap created in step 1.

The source and restore pods must use the same immutable image, mount the
Snapshot control volume at `/snapshot-control`, and mount the same model cache
at `/hf-cache`.

## 1. Create the app.py ConfigMap

Create the ConfigMap that the capture manifests mount `app.py` from:

```bash
kubectl create configmap sglang-app \
  --namespace "$SNAPSHOT_NAMESPACE" \
  --from-file=app.py=sglang/app.py
```

`kubectl create configmap` fails if the ConfigMap already exists. To update it
after editing `app.py`, use `apply` instead:

```bash
kubectl create configmap sglang-app \
  --namespace "$SNAPSHOT_NAMESPACE" \
  --from-file=app.py=sglang/app.py \
  --dry-run=client --output yaml | \
  kubectl apply --namespace "$SNAPSHOT_NAMESPACE" --filename -
```

## 2. Choose the model

The recipes already select their model, for example in
[`capture/qwen3-0.6b.yaml`](sglang/capture/qwen3-0.6b.yaml). The init container
downloads it into the [model cache](README.md#model-cache), and the main
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

To run another model, change it in both places in a copy of the manifest, because
the main container runs offline from the model cache. The example configures a
context length of 10240 tokens for a 24 GiB NVIDIA A10 GPU. Reduce
`SGLANG_CONTEXT_LENGTH` for a smaller GPU or increase it only after
validating the resulting memory use. The KV cache page size is set through
`SGLANG_PAGE_SIZE` (default `16`).
`app.py` sets `trust_remote_code=False`; Qwen3 needs no custom model code.
Add other `sglang.Engine` keyword arguments, such as `trust_remote_code` or
`tp_size`, to the same JSON object.

> [!NOTE]
> This example runs SGLang directly through `sglang.Engine` rather than
> `sglang.launch_server`, so the standard server's command-line arguments do not
> apply. Engine keyword arguments come from the JSON argument, and other runtime settings
> are supplied through SGLang's [environment variables](https://docs.sglang.ai/references/environment_variables.html)
> set in the Deployment's Pod template.

**Eight-GPU recipes.** The JSON argument sets `tp_size` and `ep_size` to the GPU
count. Set `dp_size` or `pp_size` in the same object for other layouts. Both places
pin a `revision` of the model.

## 3. Deploy the source replica

Skip this step if you checkpoint with a `SnapshotJob`: it creates the source
replica itself. Continue with
[Checkpoint, Option 2](checkpoint.md#option-2--snapshotjob-checkpoint-a-throwaway-replica).

Deploy the source Deployment of your recipe:

```bash
kubectl apply \
  --namespace "$SNAPSHOT_NAMESPACE" \
  --filename sglang/capture/qwen3-0.6b.yaml
```

The init container reuses cached model files and downloads missing files. The
main container then starts SGLang from the offline cache.

**Eight-GPU recipes.** Substitute the agent image from
[Multi-GPU recipes](README.md#multi-gpu-recipes) as you apply the manifest. For
example, GLM 5.3:

```bash
sed "s|\${SNAPSHOT_AGENT_IMAGE}|$SNAPSHOT_AGENT_IMAGE|" sglang/capture/glm-5.3.yaml | \
  kubectl apply --namespace "$SNAPSHOT_NAMESPACE" --filename -
```

Wait until the SGLang replica finishes initialization and becomes safe to
checkpoint. For another recipe, use its Deployment name from the
[recipe table](README.md#pick-a-recipe) here and in the next command. Allow
`--timeout=60m` for an eight-GPU recipe, because its first run downloads the
model:

```bash
kubectl rollout status \
  --namespace "$SNAPSHOT_NAMESPACE" \
  deployment/sglang-qwen3-0-6b-source \
  --timeout=30m
```

List the generated Pod:

```bash
kubectl get pods \
  --namespace "$SNAPSHOT_NAMESPACE" \
  --selector app=sglang-qwen3-0-6b-source
```

Use that Pod name in the `PodSnapshot` created in the next stage. The
readiness probe succeeds after `app.py` writes `ready-for-snapshot`.

## Next steps

- [Checkpoint a replica](checkpoint.md)
- [Restore a replica](restore.md)
