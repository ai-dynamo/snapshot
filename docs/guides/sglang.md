# Deploy an SGLang replica

This guide makes an SGLang workload snapshot-ready by mounting an entrypoint
into an SGLang runtime image, implementing Snapshot's [workload
contract](../reference/workload-contract.md). The example runs an SGLang
image that includes SGLang, CUDA, and `torch_memory_saver`, unmodified.
`capture/qwen3-0.6b.yaml` pins the exact upstream image, and one program, `app.py`, is
mounted into it from a ConfigMap to prepare SGLang for checkpoint and resume
it after restore. The Snapshot agent injects the restore tooling at runtime.

This single-GPU example uses native CUDA checkpoint and restore. The separate
[multi-GPU examples](cuda-shared-memory.md#multi-gpu-models) enable CUDA
shared-memory support and install the matching cuInterpose bundle.

## 1. Download the example files

Download [`app.py`](sglang/app.py),
[`model-cache-pvc.yaml`](sglang/model-cache-pvc.yaml),
[`capture/qwen3-0.6b.yaml`](sglang/capture/qwen3-0.6b.yaml), and
[`restore/qwen3-0.6b.yaml`](sglang/restore/qwen3-0.6b.yaml) from the
repository:

```bash
mkdir -p sglang-snapshot
cd sglang-snapshot
mkdir -p capture restore

curl --fail --location \
  --output app.py \
  https://raw.githubusercontent.com/ai-dynamo/snapshot/main/docs/guides/sglang/app.py

curl --fail --location \
  --output model-cache-pvc.yaml \
  https://raw.githubusercontent.com/ai-dynamo/snapshot/main/docs/guides/sglang/model-cache-pvc.yaml

curl --fail --location \
  --output capture/qwen3-0.6b.yaml \
  https://raw.githubusercontent.com/ai-dynamo/snapshot/main/docs/guides/sglang/capture/qwen3-0.6b.yaml

curl --fail --location \
  --output restore/qwen3-0.6b.yaml \
  https://raw.githubusercontent.com/ai-dynamo/snapshot/main/docs/guides/sglang/restore/qwen3-0.6b.yaml
```

The program creates a direct `sglang.Engine`, runs one generation, and calls
`TokenizerManager.pause_generation()` followed by
`Engine.release_memory_occupation()`. It writes `ready-for-snapshot` only after
both operations succeed.

The Deployment enables SGLang's memory saver and CPU weight backup through the
program. An init container downloads the selected model into a persistent
cache. The source application then loads that cache with `HF_HUB_OFFLINE=1` so
the checkpointed process has no open Hugging Face connections.

After restore, the checkpointed process calls
`Engine.resume_memory_occupation()` and
`TokenizerManager.continue_generation()`. It runs another generation and
starts an API on port 8000. It writes `sglang-restore-ready` only after the
generation succeeds and the API is listening. To validate the restored replica,
send a `POST` request to `/generate` with a JSON body such as
`{"prompt":"What is the capital of Italy?"}`.

`capture/qwen3-0.6b.yaml` runs the pinned SGLang 0.5.21 image unmodified, and mounts `app.py`
at `/snapshot-app` from the `sglang-app` ConfigMap created in step 2.

The source and restore pods must use the same immutable image, mount the
Snapshot control volume at `/snapshot-control`, and mount the same model cache
at `/hf-cache`.

The recipe leaves CUDA allocation and multicast choices to SGLang. Removing
recipe overrides does not prove that those paths run. Engine options and runtime
probes still determine which paths are selected.
The remaining IB, RAS, and PyTorch monitoring settings address network transport
and checkpoint pauses. Shared-memory support does not replace those safeguards.

## 2. Create the app.py ConfigMap

Set the namespace where the SGLang pod will run, and create the ConfigMap
`capture/qwen3-0.6b.yaml` mounts `app.py` from:

```bash
export SNAPSHOT_NAMESPACE=<namespace>
kubectl get namespace "$SNAPSHOT_NAMESPACE"

kubectl create configmap sglang-app \
  --namespace "$SNAPSHOT_NAMESPACE" \
  --from-file=app.py
```

`kubectl create configmap` fails if the ConfigMap already exists. To update it
after editing `app.py`, use `apply` instead:

```bash
kubectl create configmap sglang-app \
  --namespace "$SNAPSHOT_NAMESPACE" \
  --from-file=app.py \
  --dry-run=client -o yaml | kubectl apply -f -
```

## 3. Deploy SGLang

Select the model through `SNAPSHOT_MODEL` in [`capture/qwen3-0.6b.yaml`](sglang/capture/qwen3-0.6b.yaml).
Both the init container and the main container carry the value:

```yaml
initContainers:
  - name: model-cache
    env:
      - name: SNAPSHOT_MODEL
        value: Qwen/Qwen3-0.6B
containers:
  - name: main
    env:
      - name: SNAPSHOT_MODEL
        value: Qwen/Qwen3-0.6B
```

The example configures a context length of 10240 tokens for a 24 GiB NVIDIA A10
GPU. Reduce `SGLANG_CONTEXT_LENGTH` for a smaller GPU or increase it only after
validating the resulting memory use. The KV cache page size is set through
`SGLANG_PAGE_SIZE` (default `16`).
`app.py` sets `trust_remote_code=False`; Qwen3 needs no custom model code.
A model-specific `SGLANG_ENGINE_ARGS` JSON object can override the defaults
with `sglang.Engine` keyword arguments, including `trust_remote_code`.

> [!NOTE]
> This example runs SGLang directly through `sglang.Engine` rather than
> `sglang.launch_server`, so the standard server's command-line arguments do not
> apply. Select the model with `SNAPSHOT_MODEL`, supply API keyword arguments
> through `SGLANG_ENGINE_ARGS`, and process settings through SGLang's
> [environment variables](https://docs.sglang.ai/references/environment_variables.html).

Create the persistent model cache:

```bash
kubectl apply \
  --namespace "$SNAPSHOT_NAMESPACE" \
  --filename model-cache-pvc.yaml
```

Deploy the edited manifest:

```bash
kubectl apply --namespace "$SNAPSHOT_NAMESPACE" --filename capture/qwen3-0.6b.yaml
```

The init container reuses cached model files and downloads missing files. The
main container then starts SGLang from the offline cache.

Wait until the SGLang replica finishes initialization and becomes safe to
checkpoint:

```bash
kubectl rollout status \
  --namespace "$SNAPSHOT_NAMESPACE" \
  deployment/sglang-source \
  --timeout=30m
```

List the generated Pod:

```bash
kubectl get pods \
  --namespace "$SNAPSHOT_NAMESPACE" \
  --selector app=sglang-source
```

Use that Pod name in the `PodSnapshot` created during the next step. The
readiness probe succeeds after `app.py` writes `ready-for-snapshot`.

For the separate GLM 5.3 and DeepSeek V4.1 Flash multi-GPU manifests, see
[Multi-GPU models](cuda-shared-memory.md#multi-gpu-models). They reuse this
program and ConfigMap.

## Next steps

- [Checkpoint a replica](checkpoint.md)
- [Restore a replica](restore.md)
