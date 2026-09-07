# Build and deploy a vLLM replica

Snapshot restores a replica by injecting its checkpointed state into a
snapshot-ready image: a vLLM runtime image prepared with the application and
container layout Snapshot expects. The Snapshot agent injects the restore
tooling at runtime.

> [!NOTE]
> This example is validated on vLLM 0.27.1 (the pinned
> `vllm/vllm-openai:v0.27.1-ubuntu2404` image) and does not work on vLLM
> 0.28.

## Build

Start with the official vLLM image, which includes vLLM and its runtime
dependencies -- unmodified, with one program mounted into it that prepares
vLLM for checkpoint and resumes it after restore. There is no Snapshot-specific
image to build or push: `deployment.yaml` pins the exact upstream image, and
`app.py` is mounted from a ConfigMap. Select the model when deploying the
source pod.

### 1. Download the example files

Download [`app.py`](vllm/app.py), [`deployment.yaml`](vllm/deployment.yaml),
and [`restore-deployment.yaml`](vllm/restore-deployment.yaml) from the
repository:

```bash
mkdir -p vllm-snapshot
cd vllm-snapshot

curl --fail --location \
  --output app.py \
  https://raw.githubusercontent.com/ai-dynamo/snapshot/main/docs/guides/vllm/app.py

curl --fail --location \
  --output deployment.yaml \
  https://raw.githubusercontent.com/ai-dynamo/snapshot/main/docs/guides/vllm/deployment.yaml

curl --fail --location \
  --output restore-deployment.yaml \
  https://raw.githubusercontent.com/ai-dynamo/snapshot/main/docs/guides/vllm/restore-deployment.yaml
```

The program loads the model selected in `deployment.yaml`, runs one
generation to initialize vLLM, and then calls `pause_generation()` and
`sleep()`. It writes
`ready-for-snapshot` only when the process is safe to checkpoint. In a restore
container, it waits in standby until Snapshot injects the checkpointed process.
That process calls `wake_up()` and `resume_generation()`, runs another
generation, starts an API, and writes `vllm-restore-ready` when the API is
listening. To validate the restored replica, send a `POST` request to
`/generate` with a JSON body such as
`{"prompt":"What is the capital of Italy?"}`.

`deployment.yaml` runs vLLM's own Ubuntu 24.04 build of the 0.27.1 image
(`v0.27.1-ubuntu2404`) unmodified, which already matches the glibc floor the
current Snapshot restore bundle requires, and mounts `app.py` at
`/snapshot-app` from the `vllm-app` ConfigMap created in step 2.
`HF_HUB_DISABLE_XET=1` prevents the model downloader from leaving an open cache
log that CRIU cannot reopen after restore.

The source and restore pods must mount the Snapshot control volume at
`/snapshot-control`.

### 2. Create the app.py ConfigMap

Set the namespace where the vLLM pod will run, and create the ConfigMap
`deployment.yaml` mounts `app.py` from:

```bash
export SNAPSHOT_NAMESPACE=<namespace>
kubectl get namespace "$SNAPSHOT_NAMESPACE"

kubectl create configmap vllm-app \
  --namespace "$SNAPSHOT_NAMESPACE" \
  --from-file=app.py
```

Re-run this command after editing `app.py` (for example, to try a different
model's `trust_remote_code` needs) -- `kubectl create configmap` fails if the
ConfigMap already exists; add `--dry-run=client -o yaml | kubectl apply -f -`
to update it in place instead.

### 3. Deploy vLLM

Select the model through `SNAPSHOT_MODEL` in [`deployment.yaml`](vllm/deployment.yaml):

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
no custom model code. Edit `TRUST_REMOTE_CODE` in `app.py` for a checkpoint
that ships its own modeling code.

> [!NOTE]
> This example runs vLLM directly through `AsyncLLM` rather than `vllm serve`, so
> the standard `vllm serve` command-line arguments do not apply. The model is
> selected with `SNAPSHOT_MODEL`, and other runtime settings are supplied through
> vLLM's [environment variables](https://docs.vllm.ai/en/v0.27.1/configuration/env_vars/)
> set in the Deployment's Pod template.

`app.py` also sets `VLLM_WORKER_MULTIPROC_METHOD=spawn` before importing vLLM.
Calling `AsyncLLM` directly rather than vLLM's CLI wrapper skips the wrapper's
automatic default; without it, worker startup falls back to `fork` (or
switches to `spawn` only if vLLM detects CUDA already initialized), which is
unreliable across checkpoint/restore.

Deploy the edited manifest:

```bash
kubectl apply \
  --namespace "$SNAPSHOT_NAMESPACE" \
  --filename deployment.yaml
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

## Next steps

- [Checkpoint a replica](checkpoint.md)
- [Restore a replica](restore.md)
