# Deploy an SGLang replica

Snapshot restores a replica by injecting its checkpointed process back into a
running container. This example runs an SGLang image that includes SGLang,
CUDA, and `torch_memory_saver`, unmodified -- there is no Snapshot-specific
image to build or push. `deployment.yaml` pins the exact upstream image, and
one program, `app.py`, is mounted into it from a ConfigMap to prepare SGLang
for checkpoint and resume it after restore. The Snapshot agent injects the
restore tooling at runtime.

## 1. Download the example files

Download [`app.py`](sglang/app.py),
[`model-cache-pvc.yaml`](sglang/model-cache-pvc.yaml),
[`deployment.yaml`](sglang/deployment.yaml), and
[`restore-deployment.yaml`](sglang/restore-deployment.yaml) from the
repository:

```bash
mkdir -p sglang-snapshot
cd sglang-snapshot

curl --fail --location \
  --output app.py \
  https://raw.githubusercontent.com/ai-dynamo/snapshot/main/docs/guides/sglang/app.py

curl --fail --location \
  --output model-cache-pvc.yaml \
  https://raw.githubusercontent.com/ai-dynamo/snapshot/main/docs/guides/sglang/model-cache-pvc.yaml

curl --fail --location \
  --output deployment.yaml \
  https://raw.githubusercontent.com/ai-dynamo/snapshot/main/docs/guides/sglang/deployment.yaml

curl --fail --location \
  --output restore-deployment.yaml \
  https://raw.githubusercontent.com/ai-dynamo/snapshot/main/docs/guides/sglang/restore-deployment.yaml
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

`deployment.yaml` runs the tested SGLang image unmodified, and mounts `app.py`
at `/snapshot-app` from the `sglang-app` ConfigMap created in step 2.

The source and restore pods must use the same immutable image, mount the
Snapshot control volume at `/snapshot-control`, and mount the same model cache
at `/hf-cache`.

## 2. Create the app.py ConfigMap

Set the namespace where the SGLang pod will run, and create the ConfigMap
`deployment.yaml` mounts `app.py` from:

```bash
export SNAPSHOT_NAMESPACE=<namespace>
kubectl get namespace "$SNAPSHOT_NAMESPACE"

kubectl create configmap sglang-app \
  --namespace "$SNAPSHOT_NAMESPACE" \
  --from-file=app.py
```

Re-run this command after editing `app.py` -- `kubectl create configmap` fails
if the ConfigMap already exists; add `--dry-run=client -o yaml | kubectl apply
-f -` to update it in place instead.

## 3. Deploy SGLang

Select the model through `SNAPSHOT_MODEL` in [`deployment.yaml`](sglang/deployment.yaml).
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
`SGLANG_PAGE_SIZE` (default `16`); the engine runs with `tp_size=1`.
`app.py` sets `trust_remote_code=False`; Qwen3 needs no custom model code.
Edit that line in `app.py` for a checkpoint that ships its own modeling code.

> [!NOTE]
> This example runs SGLang directly through `sglang.Engine` rather than
> `sglang.launch_server`, so the standard server's command-line arguments do not
> apply. The model is selected with `SNAPSHOT_MODEL`, and other runtime settings
> are supplied through SGLang's [environment variables](https://docs.sglang.ai/references/environment_variables.html)
> set in the Deployment's Pod template.

Create the persistent model cache:

```bash
kubectl apply \
  --namespace "$SNAPSHOT_NAMESPACE" \
  --filename model-cache-pvc.yaml
```

Deploy the edited manifest:

```bash
kubectl apply \
  --namespace "$SNAPSHOT_NAMESPACE" \
  --filename deployment.yaml
```

The init container downloads the model when its cache marker does not exist. The
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

## Next steps

- [Checkpoint a replica](checkpoint.md)
- [Restore a replica](restore.md)
