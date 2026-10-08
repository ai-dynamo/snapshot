# Deploy a vLLM replica

This guide makes a vLLM workload snapshot-ready by mounting an entrypoint
into a vLLM runtime image, implementing Snapshot's [workload
contract](../reference/workload-contract.md). The example runs the official
vLLM image that includes vLLM and its runtime dependencies, unmodified.
`capture/qwen3-0.6b.yaml` pins the exact upstream image, and one program, `app.py`, is
mounted into it from a ConfigMap to prepare vLLM for checkpoint and resume it
after restore. The Snapshot agent injects the restore tooling at runtime.

> [!NOTE]
> This example is validated on vLLM 0.31.0 (the pinned
> `vllm/vllm-openai:v0.31.0-ubuntu2404` image).

## 1. Download the example files

Download [`app.py`](vllm/app.py), [`capture/qwen3-0.6b.yaml`](vllm/capture/qwen3-0.6b.yaml),
and [`restore/single-gpu.yaml`](vllm/restore/single-gpu.yaml) from the
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
  --output restore/single-gpu.yaml \
  https://raw.githubusercontent.com/ai-dynamo/snapshot/main/docs/guides/vllm/restore/single-gpu.yaml

curl --fail --location \
  --output capture/qwen3-0.6b-snapshotjob.yaml \
  https://raw.githubusercontent.com/ai-dynamo/snapshot/main/docs/guides/vllm/capture/qwen3-0.6b-snapshotjob.yaml
```

The program loads the model selected in `capture/qwen3-0.6b.yaml`, runs one
generation to initialize vLLM, and then calls `pause_generation()` and
`sleep(level=1)`, followed by `wake_up(tags=["weights"])`. This keeps weights
on the GPU for capture while leaving KV cache released. It writes
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

The capture manifests set `PYTORCH_ALLOC_CONF` to
`pinned_max_round_threshold_mb:1,pinned_max_cached_size_mb:1`. This avoids
power-of-two padding and caching for pinned host allocations larger than 1 MiB,
so CPU weight backups are released after weights wake up. Smaller allocations
keep the allocator defaults.

These settings affect pinned allocations throughout the process, including
after restore. They do not release live host KV caches or CPU-offloaded weights.
Workloads that repeatedly allocate large pinned staging buffers may pay more
allocation overhead. Qualify throughput and the connector's sleep/restore
lifecycle separately when adding host KV caching or CPU offload. The DeepSeek
example keeps Engram weights on the GPU and does not enable a host KV connector.

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
no custom model code. Set additional `AsyncEngineArgs` keyword arguments,
including `trust_remote_code`, through the `VLLM_ENGINE_ARGS` JSON object.

> [!NOTE]
> This example runs vLLM directly through `AsyncLLM` rather than `vllm serve`, so
> the standard `vllm serve` command-line arguments do not apply. The model is
> selected with `SNAPSHOT_MODEL`, and other runtime settings are supplied through
> vLLM's [environment variables](https://docs.vllm.ai/en/v0.31.0/configuration/env_vars/)
> set in the Deployment's Pod template.

`app.py` also sets `VLLM_WORKER_MULTIPROC_METHOD=spawn` before importing vLLM.
Calling `AsyncLLM` directly rather than vLLM's CLI wrapper skips the wrapper's
automatic default; without it, worker startup falls back to `fork` (or
switches to `spawn` only if vLLM detects CUDA already initialized), which is
unreliable across checkpoint/restore.

To capture a temporary replica automatically, use [SnapshotJob](#capture-with-snapshotjob)
instead of the following Deployment steps.

Deploy the edited manifest:

```bash
kubectl apply \
  --namespace "$SNAPSHOT_NAMESPACE" \
  --filename capture/qwen3-0.6b.yaml
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

### Capture with SnapshotJob

Instead of deploying and checkpointing the source manually, apply
[`capture/qwen3-0.6b-snapshotjob.yaml`](vllm/capture/qwen3-0.6b-snapshotjob.yaml)
after creating the ConfigMap:

```bash
kubectl apply --namespace "$SNAPSHOT_NAMESPACE" \
  --filename capture/qwen3-0.6b-snapshotjob.yaml

kubectl wait --namespace "$SNAPSHOT_NAMESPACE" \
  --for=condition=Completed snapshotjob/vllm-snapshot --timeout=60m
```

Then use the same [restore manifest](vllm/restore/single-gpu.yaml).

For multi-GPU examples, see [Multi-GPU models](cuda-shared-memory.md#multi-gpu-models).

## Next steps

- [Checkpoint a replica](checkpoint.md)
- [Restore a replica](restore.md)
