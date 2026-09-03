# Build and deploy an SGLang replica

Snapshot restores a replica by injecting its checkpointed state into a
snapshot-ready image: an SGLang runtime image prepared with the application and container
layout Snapshot expects. The Snapshot agent injects the restore tooling at
runtime.

## Build

Start with an SGLang image that includes SGLang, CUDA, and
`torch_memory_saver`. Add one program that prepares SGLang for checkpoint and
resumes it after restore. Select the model when deploying the source pod.

### 1. Download the example files

Download [`app.py`](sglang/app.py),
[`Dockerfile.sglang`](sglang/Dockerfile.sglang),
[`model-cache-pvc.yaml`](sglang/model-cache-pvc.yaml), and
[`deployment.yaml`](sglang/deployment.yaml) from the repository:

```bash
mkdir -p sglang-snapshot-image
cd sglang-snapshot-image

curl --fail --location \
  --output app.py \
  https://raw.githubusercontent.com/ai-dynamo/snapshot/main/docs/guides/sglang/app.py

curl --fail --location \
  --output Dockerfile.sglang \
  https://raw.githubusercontent.com/ai-dynamo/snapshot/main/docs/guides/sglang/Dockerfile.sglang

curl --fail --location \
  --output model-cache-pvc.yaml \
  https://raw.githubusercontent.com/ai-dynamo/snapshot/main/docs/guides/sglang/model-cache-pvc.yaml

curl --fail --location \
  --output deployment.yaml \
  https://raw.githubusercontent.com/ai-dynamo/snapshot/main/docs/guides/sglang/deployment.yaml
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

The Dockerfile starts from the tested SGLang image, creates
`/snapshot-control`, and adds `app.py`. The source and restore pods must use the
same immutable image, mount the Snapshot control volume at
`/snapshot-control`, and mount the same model cache at `/hf-cache`.

### 2. Build the image

```bash
export SGLANG_RUNTIME_IMAGE=lmsysorg/sglang@sha256:9e148f5ac788e856a06166bd6347a831831eb9fcfab4d1770874823a7c29a1a1
export SGLANG_SNAPSHOT_IMAGE=<registry>/sglang-snapshot:<tag>

docker build \
  --platform linux/amd64 \
  --build-arg SGLANG_RUNTIME_IMAGE="$SGLANG_RUNTIME_IMAGE" \
  -f Dockerfile.sglang \
  -t "$SGLANG_SNAPSHOT_IMAGE" .

docker push "$SGLANG_SNAPSHOT_IMAGE"
```

The `docker push` command uploads the newly built image to the registry named
in `$SGLANG_SNAPSHOT_IMAGE`. Step 3 deploys that image as the source pod. Use
the same full image name and tag for restored pods.

Verify that the packaged image contains SGLang, `torch_memory_saver`, and
`app.py`:

```bash
docker run --rm \
  --platform linux/amd64 \
  --entrypoint python3 \
  "$SGLANG_SNAPSHOT_IMAGE" \
  -c 'import pathlib; import sglang; import torch_memory_saver; assert pathlib.Path("/app/app.py").is_file()'
```

The command produces no output when all three components are present. Any
failure prints an error and returns a non-zero exit status.

### 3. Deploy SGLang

Set the namespace where the SGLang pod will run:

```bash
export SGLANG_NAMESPACE=<namespace>
kubectl get namespace "$SGLANG_NAMESPACE"
```

Set the model through the `SNAPSHOT_MODEL` environment variable in both the
init container and the main container in
[`deployment.yaml`](sglang/deployment.yaml):

```yaml
env:
  - name: SNAPSHOT_MODEL
    value: Qwen/Qwen3-0.6B
```

The example configures a context length of 10240 tokens for a 24 GiB NVIDIA A10
GPU. Reduce `SGLANG_CONTEXT_LENGTH` for a smaller GPU or increase it only after
validating the resulting memory use.

> [!NOTE]
> This example runs SGLang directly through `sglang.Engine` rather than
> `sglang.launch_server`, so the standard server's command-line arguments do not
> apply. The model is selected with `SNAPSHOT_MODEL`, and other runtime settings
> are supplied through SGLang's [environment variables](https://docs.sglang.ai/references/environment_variables.html)
> set in the Deployment's Pod template.

Create the persistent model cache:

```bash
kubectl apply \
  --namespace "$SGLANG_NAMESPACE" \
  --filename model-cache-pvc.yaml
```

Use [`deployment.yaml`](sglang/deployment.yaml) to deploy the image built in
step 2:

```bash
kubectl set image \
  --local \
  --filename deployment.yaml \
  model-cache="$SGLANG_SNAPSHOT_IMAGE" \
  main="$SGLANG_SNAPSHOT_IMAGE" \
  --output yaml |
  kubectl apply \
    --namespace "$SGLANG_NAMESPACE" \
    --filename -
```

The command replaces both example image values in `deployment.yaml` with
`$SGLANG_SNAPSHOT_IMAGE` before creating the Deployment. It does not modify the
local file. The init container downloads the model when its cache marker does
not exist. The main container then starts SGLang from the offline cache.

Wait until the SGLang replica finishes initialization and becomes safe to
checkpoint:

```bash
kubectl rollout status \
  --namespace "$SGLANG_NAMESPACE" \
  deployment/sglang-source \
  --timeout=30m
```

List the generated Pod:

```bash
kubectl get pods \
  --namespace "$SGLANG_NAMESPACE" \
  --selector app=sglang-source
```

Use that Pod name in the `PodSnapshot` created during the next step. The
readiness probe succeeds after `app.py` writes `ready-for-snapshot`.

## Next steps

- [Checkpoint a replica](checkpoint.md)
- [Restore a replica](restore.md)
