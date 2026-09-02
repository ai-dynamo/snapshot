# Build and deploy a vLLM replica

Snapshot restores a replica by injecting its checkpointed state into a
snapshot-ready image: a vLLM runtime image prepared with the application and
container layout Snapshot expects. The Snapshot agent injects the restore
tooling at runtime.

> [!NOTE]
> This example is validated on vLLM 0.27.1 (the pinned `vllm/vllm-openai:v0.27.1`
> image) and does not work on vLLM 0.28.

## Build

Start with the official vLLM image, which includes vLLM and its runtime
dependencies. Add one program that prepares vLLM for checkpoint and resumes it
after restore. Select the model when deploying the source pod.

### 1. Download the example files

Download [`app.py`](vllm/app.py),
[`Dockerfile.vllm`](vllm/Dockerfile.vllm), and
[`deployment.yaml`](vllm/deployment.yaml) from the repository:

```bash
mkdir -p vllm-snapshot-image
cd vllm-snapshot-image

curl --fail --location \
  --output app.py \
  https://raw.githubusercontent.com/ai-dynamo/snapshot/main/docs/guides/vllm/app.py

curl --fail --location \
  --output Dockerfile.vllm \
  https://raw.githubusercontent.com/ai-dynamo/snapshot/main/docs/guides/vllm/Dockerfile.vllm

curl --fail --location \
  --output deployment.yaml \
  https://raw.githubusercontent.com/ai-dynamo/snapshot/main/docs/guides/vllm/deployment.yaml
```

The program loads the model selected in `deployment.yaml`, runs one
generation to initialize vLLM and records its output in `vllm-precheck`, and
then calls `pause_generation()` and `sleep()`. It writes
`ready-for-snapshot` only when the process is safe to checkpoint. In a restore
container, it waits in standby until Snapshot injects the checkpointed process.
That process calls `wake_up()` and `resume_generation()`, runs another
generation, starts an API, and writes `vllm-restore-ready` when the API is
listening. To validate the restored replica, send a `POST` request to
`/generate` with a JSON body such as
`{"prompt":"What is the capital of Italy?"}`.

The Dockerfile starts from the official vLLM 0.27.1 image and installs the
Ubuntu 24.04 glibc required by the current Snapshot restore bundle. It creates
`/snapshot-control` and adds `app.py`.
`HF_HUB_DISABLE_XET=1` prevents the model downloader from leaving an open cache
log that CRIU cannot reopen after restore.

The source and restore pods must mount the Snapshot control volume at
`/snapshot-control`.

### 2. Build the image

```bash
export VLLM_RUNTIME_IMAGE=vllm/vllm-openai:v0.27.1@sha256:0a51ea5b4ae2dc5d81890e5173f54203d2a3ae0cfffe51b8fd2afd4391bfd967
export VLLM_SNAPSHOT_IMAGE=<registry>/vllm-snapshot:<tag>

docker build \
  --platform linux/amd64 \
  --build-arg VLLM_RUNTIME_IMAGE="$VLLM_RUNTIME_IMAGE" \
  -f Dockerfile.vllm \
  -t "$VLLM_SNAPSHOT_IMAGE" .

docker push "$VLLM_SNAPSHOT_IMAGE"
```

The `docker push` command uploads the newly built image to the registry named
in `$VLLM_SNAPSHOT_IMAGE`. Step 3 deploys that image as the source pod. Use the
same full image name and tag for restored pods.

Verify that the packaged image contains vLLM and `app.py`:

```bash
docker run --rm \
  --platform linux/amd64 \
  --entrypoint python3 \
  "$VLLM_SNAPSHOT_IMAGE" \
  -c 'import pathlib; import vllm; assert pathlib.Path("/app/app.py").is_file()'
```

The command produces no output when both vLLM and `/app/app.py` are present.
Any failure prints an error and returns a non-zero exit status.

### 3. Deploy vLLM

Set the namespace where the vLLM pod will run:

```bash
export VLLM_NAMESPACE=<namespace>
kubectl get namespace "$VLLM_NAMESPACE"
```

Set the model through the `SNAPSHOT_MODEL` environment variable in
[`deployment.yaml`](vllm/deployment.yaml):

```yaml
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
the resulting memory use.

> [!NOTE]
> This example runs vLLM directly through `AsyncLLM` rather than `vllm serve`, so
> the standard `vllm serve` command-line arguments do not apply. The model is
> selected with `SNAPSHOT_MODEL`, and other runtime settings are supplied through
> vLLM's [environment variables](https://docs.vllm.ai/en/v0.27.1/configuration/env_vars/)
> set in the Deployment's Pod template.

Use [`deployment.yaml`](vllm/deployment.yaml) to deploy the image built in
step 2:

```bash
kubectl set image \
  --local \
  --filename deployment.yaml \
  main="$VLLM_SNAPSHOT_IMAGE" \
  --output yaml |
  kubectl apply \
    --namespace "$VLLM_NAMESPACE" \
    --filename -
```

The command replaces the example image value in `deployment.yaml` with
`$VLLM_SNAPSHOT_IMAGE` before creating the Deployment. It does not modify the
local file.

Wait until the vLLM replica finishes initialization and becomes safe to
checkpoint:

```bash
kubectl rollout status \
  --namespace "$VLLM_NAMESPACE" \
  deployment/vllm-source \
  --timeout=30m
```

List the generated Pod:

```bash
kubectl get pods \
  --namespace "$VLLM_NAMESPACE" \
  --selector app=vllm-source
```

Use that Pod name in the `PodSnapshot` created during the next step. The
readiness probe succeeds after `app.py` writes `ready-for-snapshot`.

## Next steps

- [Checkpoint a replica](checkpoint.md)
- [Restore a replica](restore.md)
