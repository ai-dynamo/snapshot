# Deploy a TensorRT-LLM replica

This guide makes a TensorRT-LLM workload snapshot-ready by mounting an
entrypoint into a TensorRT-LLM runtime image, implementing Snapshot's
[workload contract](../reference/workload-contract.md). The example runs the
TensorRT-LLM image that includes TensorRT-LLM and its runtime dependencies,
unmodified. `capture/qwen3-0.6b.yaml` pins the exact upstream image, and one
program, `app.py`, is mounted into it from a ConfigMap to prepare
TensorRT-LLM for checkpoint and validate it after restore. The Snapshot
agent injects the restore tooling at runtime.

> [!NOTE]
> TensorRT-LLM support is experimental.

## 1. Download the example files

Download [`app.py`](tensorrt-llm/app.py),
[`capture/qwen3-0.6b.yaml`](tensorrt-llm/capture/qwen3-0.6b.yaml), and
[`restore/single-gpu.yaml`](tensorrt-llm/restore/single-gpu.yaml) from the
repository, along with the [compiler-cache PVC](compiler-cache-pvc.yaml):

```bash
mkdir -p tensorrt-llm-snapshot
cd tensorrt-llm-snapshot
mkdir -p capture restore

curl --fail --location \
  --output compiler-cache-pvc.yaml \
  https://raw.githubusercontent.com/ai-dynamo/snapshot/main/docs/guides/compiler-cache-pvc.yaml

curl --fail --location \
  --output app.py \
  https://raw.githubusercontent.com/ai-dynamo/snapshot/main/docs/guides/tensorrt-llm/app.py

curl --fail --location \
  --output capture/qwen3-0.6b.yaml \
  https://raw.githubusercontent.com/ai-dynamo/snapshot/main/docs/guides/tensorrt-llm/capture/qwen3-0.6b.yaml

curl --fail --location \
  --output restore/single-gpu.yaml \
  https://raw.githubusercontent.com/ai-dynamo/snapshot/main/docs/guides/tensorrt-llm/restore/single-gpu.yaml

curl --fail --location \
  --output capture/qwen3-0.6b-snapshotjob.yaml \
  https://raw.githubusercontent.com/ai-dynamo/snapshot/main/docs/guides/tensorrt-llm/capture/qwen3-0.6b-snapshotjob.yaml
```

The program loads the model selected in `capture/qwen3-0.6b.yaml` and calls
`LLM.generate()` to initialize TensorRT-LLM. The synchronous call returns only
after generation finishes, so no request remains in flight. The program then
releases KV cache on every rank, runs `gc.collect()`, and writes
`ready-for-snapshot` when it reaches the safe checkpoint point.

The recipe uses the pinned rc24 image's private executor collective RPC for
KV-only `sleep` and `wakeup`. `sleep_config` sets the KV restore mode to `NONE`,
so cache contents are discarded. Weights remain on the GPU with no CPU backup.
KV manager V1 and disabled block reuse avoid retaining prefix-cache entries
across this release. These settings are enforced by `app.py`.

`TORCHINDUCTOR_COMPILE_THREADS=1` disables forked asynchronous compiler workers,
which can inherit CUDA device mappings that prevent CRIU capture. Compilation
still uses the configured compiler cache.

After restore, the process recreates KV cache, calls `LLM.generate()` again,
and starts an API on port 8000. It writes `trtllm-restore-ready` only after
that generation succeeds and the API is listening. To validate the restored
replica, send a `POST` request to `/generate` with a JSON body such as
`{"prompt":"What is the capital of Italy?"}`.

`capture/qwen3-0.6b.yaml` runs the TensorRT-LLM `1.3.0rc24` release image, pinned by
digest, unmodified, and mounts `app.py` at `/snapshot-app` from the
`tensorrt-llm-app` ConfigMap created in step 2. A release candidate is used
deliberately: the `1.2.1` GA image fails at `import tensorrt` because
`libnvonnxparser.so.10` is missing from it, and no 1.3.0 GA image exists yet.
Move to the first 1.3.x GA once it is published.
`OMPI_MCA_pml=ob1` and `OMPI_MCA_btl=tcp,self` keep MPI communication on TCP
to avoid RDMA mappings that CRIU cannot restore.

The source and restore pods must use the same immutable image and mount the
Snapshot control volume at `/snapshot-control`.

The capture and restore manifests mount a [persistent compiler cache](compiler-cache.md).
Use a ReadWriteMany storage class, such as `vast` on nscale, and retain this PVC
for every checkpoint that uses it. Set `storageClassName` in
`compiler-cache-pvc.yaml` when the default class does not provide RWX storage.

## 2. Create the app.py ConfigMap

Set the namespace where the TensorRT-LLM pod will run, and create the
ConfigMap `capture/qwen3-0.6b.yaml` mounts `app.py` from:

```bash
export SNAPSHOT_NAMESPACE=<namespace>
kubectl get namespace "$SNAPSHOT_NAMESPACE"

kubectl apply --namespace "$SNAPSHOT_NAMESPACE" --filename compiler-cache-pvc.yaml

kubectl create configmap tensorrt-llm-app \
  --namespace "$SNAPSHOT_NAMESPACE" \
  --from-file=app.py
```

`kubectl create configmap` fails if the ConfigMap already exists. To update it
after editing `app.py`, use `apply` instead:

```bash
kubectl create configmap tensorrt-llm-app \
  --namespace "$SNAPSHOT_NAMESPACE" \
  --from-file=app.py \
  --dry-run=client -o yaml | kubectl apply -f -
```

## 3. Deploy TensorRT-LLM

Select a model supported by the chosen TensorRT-LLM image through
`SNAPSHOT_MODEL` in [`capture/qwen3-0.6b.yaml`](tensorrt-llm/capture/qwen3-0.6b.yaml):

```yaml
containers:
  - name: main
    env:
      - name: SNAPSHOT_MODEL
        value: Qwen/Qwen3-0.6B
```

The example uses one GPU, the PyTorch backend, and a maximum sequence length of
512 tokens. Engine sizing is set through `TRTLLM_MAX_NUM_TOKENS` (default
`1024`), `TRTLLM_MAX_BATCH_SIZE` (default `1`), and
`TRTLLM_FREE_GPU_MEMORY_FRACTION` (default `0.10`). `app.py` sets
`trust_remote_code=False`; Qwen3 needs no custom model code. Set additional
`LLM` keyword arguments, including `trust_remote_code`, through the
`TRTLLM_ENGINE_ARGS` JSON object. Revalidate checkpoint and restore before changing the model,
TensorRT-LLM image, GPU count, backend, or engine settings.

The eight-GPU GLM-5.3 profiles explicitly select `allreduce_strategy="NCCL"`.
With the pinned engine, AUTO produced intermittent incorrect source answers
before checkpointing in validation. NCCL passed source inference, KV release
and resume, and three restored inference checks on the same eight B200 GPUs.

> [!NOTE]
> This example runs TensorRT-LLM through the `LLM` API rather than `trtllm-serve`,
> so the standard `trtllm-serve` command-line arguments do not apply. The model is
> selected with `SNAPSHOT_MODEL`, and other engine settings are configured on the
> [`LLM` API](https://nvidia.github.io/TensorRT-LLM/llm-api/reference.html) in
> `app.py`.

To capture a temporary replica automatically, use [SnapshotJob](#capture-with-snapshotjob)
instead of the following Deployment steps.

Deploy the edited manifest:

```bash
kubectl apply \
  --namespace "$SNAPSHOT_NAMESPACE" \
  --filename capture/qwen3-0.6b.yaml
```

Wait until the TensorRT-LLM replica finishes initialization and becomes safe to
checkpoint:

```bash
kubectl rollout status \
  --namespace "$SNAPSHOT_NAMESPACE" \
  deployment/tensorrt-llm-source \
  --timeout=30m
```

List the generated Pod:

```bash
kubectl get pods \
  --namespace "$SNAPSHOT_NAMESPACE" \
  --selector app=tensorrt-llm-source
```

Use that Pod name in the `PodSnapshot` created during the next step. The
readiness probe succeeds after `app.py` writes `ready-for-snapshot`.

### Capture with SnapshotJob

Instead of deploying and checkpointing the source manually, apply
[`capture/qwen3-0.6b-snapshotjob.yaml`](tensorrt-llm/capture/qwen3-0.6b-snapshotjob.yaml)
after creating the ConfigMap:

```bash
kubectl apply --namespace "$SNAPSHOT_NAMESPACE" \
  --filename capture/qwen3-0.6b-snapshotjob.yaml

kubectl wait --namespace "$SNAPSHOT_NAMESPACE" \
  --for=condition=Completed snapshotjob/tensorrt-llm-snapshot --timeout=60m
```

Then use the same [restore manifest](tensorrt-llm/restore/single-gpu.yaml).

For multi-GPU examples, see [Multi-GPU models](cuda-shared-memory.md#multi-gpu-models).

## Next steps

- [Checkpoint a replica](checkpoint.md)
- [Restore a replica](restore.md)
