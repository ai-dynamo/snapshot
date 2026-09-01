# Build and deploy a TensorRT-LLM replica

Snapshot restores a replica by injecting its checkpointed state into a
snapshot-ready image: a TensorRT-LLM runtime image prepared with the application and container
layout Snapshot expects. The Snapshot agent injects the restore tooling at
runtime.

> [!NOTE]
> TensorRT-LLM support is experimental and currently limited to a single GPU.

## Build

Start with the TensorRT-LLM runtime image, which includes TensorRT-LLM and its
runtime dependencies. Add one program that prepares TensorRT-LLM for checkpoint
and validates it after restore. Select the model when deploying the source pod.

### 1. Download the example files

Download [`app.py`](tensorrt-llm/app.py),
[`Dockerfile.tensorrt-llm`](tensorrt-llm/Dockerfile.tensorrt-llm), and
[`deployment.yaml`](tensorrt-llm/deployment.yaml) from the repository:

```bash
mkdir -p tensorrt-llm-snapshot-image
cd tensorrt-llm-snapshot-image

curl --fail --location \
  --output app.py \
  https://raw.githubusercontent.com/ai-dynamo/snapshot/main/docs/guides/tensorrt-llm/app.py

curl --fail --location \
  --output Dockerfile.tensorrt-llm \
  https://raw.githubusercontent.com/ai-dynamo/snapshot/main/docs/guides/tensorrt-llm/Dockerfile.tensorrt-llm

curl --fail --location \
  --output deployment.yaml \
  https://raw.githubusercontent.com/ai-dynamo/snapshot/main/docs/guides/tensorrt-llm/deployment.yaml
```

The program loads the model selected in `deployment.yaml` and calls
`LLM.generate()` to initialize TensorRT-LLM. The synchronous call returns only
after generation finishes, so no request remains in flight. The program runs
`gc.collect()` and writes `ready-for-snapshot` when it reaches the safe checkpoint
point.

TensorRT-LLM does not use a framework pause or sleep call in this example. The
model and initialized CUDA state remain resident. After restore, the checkpointed
process calls `LLM.generate()` again and starts an API on port 8000. It writes
`trtllm-restore-ready` only after the generation succeeds and the API is
listening. To validate the restored replica, send a `POST` request to
`/generate` with a JSON body such as
`{"prompt":"What is the capital of Italy?"}`.

The Dockerfile starts from the tested TensorRT-LLM 1.3.0 release candidate
image, creates `/snapshot-control`, and adds `app.py`.
`TLLM_NCCL_SYMMETRIC_ZERO_COPY=0` disables NCCL registered windows that CUDA
checkpoint does not support. `UCX_TLS=tcp,self` avoids RDMA mappings that CRIU
cannot restore.

The source and restore pods must use the same immutable image and mount the
Snapshot control volume at `/snapshot-control`.

### 2. Build the image

```bash
export TENSORRT_LLM_RUNTIME_IMAGE=nvcr.io/nvidia/tensorrt-llm/release:1.3.0rc23
export TENSORRT_LLM_SNAPSHOT_IMAGE=<registry>/tensorrt-llm-snapshot:<tag>

docker build \
  --platform linux/amd64 \
  --build-arg TENSORRT_LLM_RUNTIME_IMAGE="$TENSORRT_LLM_RUNTIME_IMAGE" \
  -f Dockerfile.tensorrt-llm \
  -t "$TENSORRT_LLM_SNAPSHOT_IMAGE" .

docker push "$TENSORRT_LLM_SNAPSHOT_IMAGE"
```

The `docker push` command uploads the newly built image to the registry named
in `$TENSORRT_LLM_SNAPSHOT_IMAGE`. Step 3 deploys that image as the source pod.
Use the same full image name and tag for restored pods.

Verify that the packaged image contains TensorRT-LLM and `app.py`:

```bash
docker run --rm \
  --platform linux/amd64 \
  --entrypoint python3 \
  "$TENSORRT_LLM_SNAPSHOT_IMAGE" \
  -c 'import pathlib; import tensorrt_llm; assert pathlib.Path("/app/app.py").is_file()'
```

The command produces no output when both TensorRT-LLM and `/app/app.py` are
present. Any failure prints an error and returns a non-zero exit status.

### 3. Deploy TensorRT-LLM

Set the namespace where the TensorRT-LLM pod will run:

```bash
export TENSORRT_LLM_NAMESPACE=<namespace>
kubectl get namespace "$TENSORRT_LLM_NAMESPACE"
```

Set a model supported by the selected TensorRT-LLM image through the
`SNAPSHOT_MODEL` environment variable in
[`deployment.yaml`](tensorrt-llm/deployment.yaml):

```yaml
env:
  - name: SNAPSHOT_MODEL
    value: Qwen/Qwen3-0.6B
```

The example uses one GPU, the PyTorch backend, and a maximum sequence length of
512 tokens. Revalidate checkpoint and restore before changing the model,
TensorRT-LLM image, GPU count, backend, or engine settings.

> [!NOTE]
> This example runs TensorRT-LLM through the `LLM` API rather than `trtllm-serve`,
> so the standard `trtllm-serve` command-line arguments do not apply. The model is
> selected with `SNAPSHOT_MODEL`, and other engine settings are configured on the
> [`LLM` API](https://nvidia.github.io/TensorRT-LLM/llm-api/reference.html) in
> `app.py`.

Use [`deployment.yaml`](tensorrt-llm/deployment.yaml) to deploy the image built
in step 2:

```bash
kubectl set image \
  --local \
  --filename deployment.yaml \
  main="$TENSORRT_LLM_SNAPSHOT_IMAGE" \
  --output yaml |
  kubectl apply \
    --namespace "$TENSORRT_LLM_NAMESPACE" \
    --filename -
```

The command replaces the example image value in `deployment.yaml` with
`$TENSORRT_LLM_SNAPSHOT_IMAGE` before creating the Deployment. It does not
modify the local file.

Wait until the TensorRT-LLM replica finishes initialization and becomes safe to
checkpoint:

```bash
kubectl rollout status \
  --namespace "$TENSORRT_LLM_NAMESPACE" \
  deployment/tensorrt-llm-source \
  --timeout=30m
```

List the generated Pod:

```bash
kubectl get pods \
  --namespace "$TENSORRT_LLM_NAMESPACE" \
  --selector app=tensorrt-llm-source
```

Use that Pod name in the `PodSnapshot` created during the next step. The
readiness probe succeeds after `app.py` writes `ready-for-snapshot`.

## Next steps

- [Checkpoint a replica](checkpoint.md)
- [Restore a replica](restore.md)
