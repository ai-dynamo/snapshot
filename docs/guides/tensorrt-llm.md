# Deploy a TensorRT-LLM replica

This guide makes a TensorRT-LLM workload snapshot-ready by mounting an
entrypoint into a TensorRT-LLM runtime image, implementing Snapshot's
[workload contract](../reference/workload-contract.md). The example runs the
TensorRT-LLM image that includes TensorRT-LLM and its runtime dependencies,
unmodified. `deployment.yaml` pins the exact upstream image, and one
program, `app.py`, is mounted into it from a ConfigMap to prepare
TensorRT-LLM for checkpoint and validate it after restore. The Snapshot
agent injects the restore tooling at runtime.

> [!NOTE]
> TensorRT-LLM support is experimental.

This single-GPU example uses native CUDA checkpoint and restore. The separate
[multi-GPU examples](cuda-shared-memory.md#multi-gpu-models) enable CUDA
shared-memory support and install the matching cuInterpose bundle.

## 1. Download the example files

Download [`app.py`](tensorrt-llm/app.py),
[`deployment.yaml`](tensorrt-llm/deployment.yaml), and
[`restore-deployment.yaml`](tensorrt-llm/restore-deployment.yaml) from the
repository:

```bash
mkdir -p tensorrt-llm-snapshot
cd tensorrt-llm-snapshot

curl --fail --location \
  --output app.py \
  https://raw.githubusercontent.com/ai-dynamo/snapshot/main/docs/guides/tensorrt-llm/app.py

curl --fail --location \
  --output deployment.yaml \
  https://raw.githubusercontent.com/ai-dynamo/snapshot/main/docs/guides/tensorrt-llm/deployment.yaml

curl --fail --location \
  --output restore-deployment.yaml \
  https://raw.githubusercontent.com/ai-dynamo/snapshot/main/docs/guides/tensorrt-llm/restore-deployment.yaml
```

The program loads the model selected in `deployment.yaml` and calls
`LLM.generate()` to initialize TensorRT-LLM. The synchronous call returns only
after generation finishes, so no request remains in flight. The program then
runs `gc.collect()` and writes `ready-for-snapshot` when it reaches the safe
checkpoint point.

TensorRT-LLM does not use a framework pause or sleep call in this example. The
model and initialized CUDA state remain resident. After restore, the checkpointed
process calls `LLM.generate()` again and starts an API on port 8000. It writes
`trtllm-restore-ready` only after the generation succeeds and the API is
listening. To validate the restored replica, send a `POST` request to
`/generate` with a JSON body such as
`{"prompt":"What is the capital of Italy?"}`.

`deployment.yaml` runs the TensorRT-LLM `1.3.0rc24` release image, pinned by
digest, unmodified, and mounts `app.py` at `/snapshot-app` from the
`tensorrt-llm-app` ConfigMap created in step 2. A release candidate is used
deliberately: the `1.2.1` GA image fails at `import tensorrt` because
`libnvonnxparser.so.10` is missing from it, and no 1.3.0 GA image exists yet.
Move to the first 1.3.x GA once it is published.

The recipe leaves CUDA graphs, allreduce selection, and
`TLLM_NCCL_SYMMETRIC_ZERO_COPY` at their framework defaults. This single-GPU
example does not exercise inter-GPU collectives.

`OMPI_MCA_pml=ob1` and `OMPI_MCA_btl=tcp,self` keep MPI communication on TCP.
These settings avoid RDMA mappings that CRIU cannot restore and UCX TCP
keepalive timeouts during a large checkpoint. These MPI settings do
not disable NCCL or TensorRT-LLM's GPU collective algorithms.

If the application requires the UCX MPI transport, use `OMPI_MCA_pml=ucx`,
`UCX_TLS=tcp,self`, and `UCX_TCP_KEEPIDLE=inf` instead, before starting the source
Pod. Disabling UCX TCP keepalive prevents it from detecting dead peers, so
revalidate that configuration with the application's failure handling.

The source and restore pods must use the same immutable image and mount the
Snapshot control volume at `/snapshot-control`.

## 2. Create the app.py ConfigMap

Set the namespace where the TensorRT-LLM pod will run, and create the
ConfigMap `deployment.yaml` mounts `app.py` from:

```bash
export SNAPSHOT_NAMESPACE=<namespace>
kubectl get namespace "$SNAPSHOT_NAMESPACE"

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
`SNAPSHOT_MODEL` in [`deployment.yaml`](tensorrt-llm/deployment.yaml):

```yaml
containers:
  - name: main
    env:
      - name: SNAPSHOT_MODEL
        value: Qwen/Qwen3-0.6B
```

The example defaults to one GPU, the PyTorch backend, and a maximum sequence
length of 512 tokens. Engine sizing is set through `TRTLLM_MAX_NUM_TOKENS` (default
`1024`), `TRTLLM_MAX_BATCH_SIZE` (default `1`), and
`TRTLLM_FREE_GPU_MEMORY_FRACTION` (default `0.10`). `app.py` sets
`trust_remote_code=False`; Qwen3 needs no custom model code. A model-specific
`TRTLLM_ENGINE_ARGS` JSON object can override the defaults with `LLM` keyword
arguments, including `trust_remote_code`. Revalidate checkpoint and restore before changing the model,
TensorRT-LLM image, GPU count, backend, or engine settings.

> [!NOTE]
> This example runs TensorRT-LLM through the `LLM` API rather than `trtllm-serve`,
> so the standard `trtllm-serve` command-line arguments do not apply. The model is
> selected with `SNAPSHOT_MODEL`, and `TRTLLM_ENGINE_ARGS` supplies
> [`LLM` API](https://nvidia.github.io/TensorRT-LLM/llm-api/reference.html) keyword
> arguments.

Deploy the edited manifest:

```bash
kubectl apply --namespace "$SNAPSHOT_NAMESPACE" --filename deployment.yaml
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

For the separate GLM 5.3 multi-GPU manifests, see
[Multi-GPU models](cuda-shared-memory.md#multi-gpu-models). They reuse this
program and ConfigMap.

## Next steps

- [Checkpoint a replica](checkpoint.md)
- [Restore a replica](restore.md)
