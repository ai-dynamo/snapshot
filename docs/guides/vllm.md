# Deploy a vLLM replica

This guide makes a vLLM workload snapshot-ready by mounting an entrypoint
into a vLLM runtime image, implementing Snapshot's [workload
contract](../reference/workload-contract.md). The example runs the official
vLLM image that includes vLLM and its runtime dependencies, unmodified.
`capture/qwen3-0.6b.yaml` pins the exact upstream image, and one program, `app.py`, is
mounted into it from a ConfigMap to prepare vLLM for checkpoint and resume it
after restore. The Snapshot agent injects the restore tooling at runtime.

This single-GPU example uses native CUDA checkpoint and restore. The separate
[multi-GPU examples](cuda-shared-memory.md#multi-gpu-models) enable CUDA
shared-memory support and install the matching cuInterpose bundle.

## 1. Download the example files

Download [`app.py`](vllm/app.py), [`capture/qwen3-0.6b.yaml`](vllm/capture/qwen3-0.6b.yaml),
and [`restore/qwen3-0.6b.yaml`](vllm/restore/qwen3-0.6b.yaml) from the
repository:

```bash
mkdir -p vllm-snapshot
cd vllm-snapshot
mkdir -p capture restore

curl --fail --location \
  --output app.py \
  https://raw.githubusercontent.com/ai-dynamo/snapshot/main/docs/guides/vllm/app.py

curl --fail --location \
  --output capture/qwen3-0.6b.yaml \
  https://raw.githubusercontent.com/ai-dynamo/snapshot/main/docs/guides/vllm/capture/qwen3-0.6b.yaml

curl --fail --location \
  --output restore/qwen3-0.6b.yaml \
  https://raw.githubusercontent.com/ai-dynamo/snapshot/main/docs/guides/vllm/restore/qwen3-0.6b.yaml
```

The program loads the model selected in `capture/qwen3-0.6b.yaml`, runs one
generation to initialize vLLM, and then calls `pause_generation()` and
`sleep()`. It writes
`ready-for-snapshot` only when the process is safe to checkpoint. In a restore
container, it waits in standby until Snapshot injects the checkpointed process.
That process calls `wake_up()` and `resume_generation()`, runs another
generation, starts an API, and writes `vllm-restore-ready` when the API is
listening. To validate the restored replica, send a `POST` request to
`/generate` with a JSON body such as
`{"prompt":"What is the capital of Italy?"}`.

`capture/qwen3-0.6b.yaml` runs vLLM's own Ubuntu 24.04 build of the 0.31.0 image
(`v0.31.0-ubuntu2404`) unmodified, which already matches the glibc floor the
current Snapshot restore bundle requires, and mounts `app.py` at
`/snapshot-app` from the `vllm-app` ConfigMap created in step 2.
`HF_HUB_DISABLE_XET=1` prevents the model downloader from leaving an open cache
log that CRIU cannot reopen after restore.

The source and restore pods must mount the Snapshot control volume at
`/snapshot-control`.

## 2. Create the app.py ConfigMap

Set the namespace where the vLLM pod will run, and create the ConfigMap
`capture/qwen3-0.6b.yaml` mounts `app.py` from:

```bash
export SNAPSHOT_NAMESPACE=<namespace>
kubectl get namespace "$SNAPSHOT_NAMESPACE"

kubectl create configmap vllm-app \
  --namespace "$SNAPSHOT_NAMESPACE" \
  --from-file=app.py
```

`kubectl create configmap` fails if the ConfigMap already exists. To update it
after editing `app.py`, use `apply` instead:

```bash
kubectl create configmap vllm-app \
  --namespace "$SNAPSHOT_NAMESPACE" \
  --from-file=app.py \
  --dry-run=client -o yaml | kubectl apply -f -
```

## 3. Deploy vLLM

Select the model through `SNAPSHOT_MODEL` in [`capture/qwen3-0.6b.yaml`](vllm/capture/qwen3-0.6b.yaml):

```yaml
containers:
  - name: main
    env:
      - name: SNAPSHOT_MODEL
        value: Qwen/Qwen3-0.6B
```

Other values include `TinyLlama/TinyLlama-1.1B-Chat-v1.0` or a mounted model
path such as `/models/Qwen3-0.6B`. A mounted path must be available to both the
source and restored containers.

The example sizes the engine for a small single-GPU deployment through
`VLLM_MAX_MODEL_LEN` (default `2048`) and `VLLM_GPU_MEMORY_UTILIZATION`
(default `0.30`). Raise them only after validating checkpoint and restore with
the resulting memory use. `app.py` sets `trust_remote_code=False`; Qwen3 needs
no custom model code. A model-specific `VLLM_ENGINE_ARGS` JSON object can
override the defaults with `AsyncEngineArgs` keyword arguments, including
`trust_remote_code` when required by the selected checkpoint.

> [!NOTE]
> This example runs vLLM directly through `AsyncLLM` rather than `vllm serve`, so
> the standard `vllm serve` command-line arguments do not apply. The model is
> selected with `SNAPSHOT_MODEL`. Supply API keyword arguments through
> `VLLM_ENGINE_ARGS`, and process settings through vLLM's
> [environment variables](https://docs.vllm.ai/en/v0.31.0/configuration/env_vars/).

`app.py` also sets `VLLM_WORKER_MULTIPROC_METHOD=spawn` before importing vLLM.
Calling `AsyncLLM` directly rather than vLLM's CLI wrapper skips the wrapper's
automatic default; without it, worker startup falls back to `fork` (or
switches to `spawn` only if vLLM detects CUDA already initialized), which is
unreliable across checkpoint/restore.

Deploy the edited manifest:

```bash
kubectl apply --namespace "$SNAPSHOT_NAMESPACE" --filename capture/qwen3-0.6b.yaml
```

Wait until the vLLM replica finishes initialization and becomes safe to
checkpoint:

```bash
kubectl rollout status \
  --namespace "$SNAPSHOT_NAMESPACE" \
  deployment/vllm-source \
  --timeout=30m
```

List the generated Pod:

```bash
kubectl get pods \
  --namespace "$SNAPSHOT_NAMESPACE" \
  --selector app=vllm-source
```

Use that Pod name in the `PodSnapshot` created during the next step. The
readiness probe succeeds after `app.py` writes `ready-for-snapshot`.

For the separate GLM 5.3 and DeepSeek V4.1 Flash multi-GPU manifests, see
[Multi-GPU models](cuda-shared-memory.md#multi-gpu-models). They reuse this
program and ConfigMap.

## Next steps

- [Checkpoint a replica](checkpoint.md)
- [Restore a replica](restore.md)
