# Deploy a vLLM replica

This guide makes a vLLM workload snapshot-ready by mounting an entrypoint
into a vLLM runtime image, implementing Snapshot's [workload
contract](../reference/workload-contract.md). The example runs the official
vLLM image that includes vLLM and its runtime dependencies, unmodified.
`deployment.yaml` pins the exact upstream image, and one program, `app.py`, is
mounted into it from a ConfigMap to prepare vLLM for checkpoint and resume it
after restore. The Snapshot agent injects the restore tooling at runtime.

This example uses **vLLM 0.30.0**. Both manifests pin the same official
`v0.30.0-ubuntu2404` image by digest.
For customers using 0.27.1, see [Earlier vLLM versions](#earlier-vllm-versions-0271).

Follow the numbered steps for the single-GPU path: deploy vLLM with
`app.py`'s `spawn` setting, checkpoint the initialized pod with `PodSnapshot`,
then restore it and verify inference. No shared-memory annotation is needed
for this path. Other launch methods are covered under
[Alternative launch methods](#alternative-launch-methods).

## 1. Download the example files

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
`ready-for-snapshot` only when the process is safe to checkpoint. The restore
container starts an inert `sleep infinity` placeholder; Snapshot restores
the checkpointed application into that container. The restored process
calls `wake_up()` and `resume_generation()`, runs another
generation, starts an API, and writes `vllm-restore-ready` when the API is
listening. To validate the restored replica, send a `POST` request to
`/generate` with a JSON body such as
`{"prompt":"What is the capital of Italy?"}`.

`deployment.yaml` runs vLLM's own Ubuntu 24.04 build of the 0.30.0 image
(`v0.30.0-ubuntu2404`) unmodified, which already matches the glibc floor the
current Snapshot restore bundle requires, and mounts `app.py` at
`/snapshot-app` from the `vllm-app` ConfigMap created in step 2.
`HF_HUB_DISABLE_XET=1` prevents the model downloader from leaving an open cache
log that CRIU cannot reopen after restore.

The source and restore pods must mount the Snapshot control volume at
`/snapshot-control`.

## 2. Create the app.py ConfigMap

Set the namespace where the vLLM pod will run, and create the ConfigMap
`deployment.yaml` mounts `app.py` from:

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
> vLLM's [environment variables](https://docs.vllm.ai/en/latest/configuration/env_vars/)
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

## 4. Checkpoint and restore

Follow [Checkpoint a replica](checkpoint.md#option-1--podsnapshot-checkpoint-a-running-replica)
to create a `PodSnapshot` for the source pod and wait for it to become Ready.
Then follow [Restore a replica](restore.md), using
`restore-deployment.yaml` and the same runtime image as the source.
Verify restored inference through the application's `/generate` endpoint.

## Alternative launch methods

The numbered walkthrough uses `spawn`. When vLLM is used as a Python
library, workers can default to
[`fork`, rather than `spawn`](https://docs.vllm.ai/en/latest/design/multiprocessing/).
Forked workers can inherit CUDA state and NVIDIA device handles. CUDA
checkpointing must handle that state across the worker process tree.

### Single-node workloads with cuInterpose

cuInterpose supports single-node workloads with **one or multiple GPUs**.
If you are still using Snapshot 0.1.0, [install](../operations/install.md#install-from-a-release)
or [upgrade](../operations/install.md#upgrade-an-existing-release) to Snapshot
0.2 or later before enabling it. On a `SnapshotJob`, enable it in the Pod template:

```yaml
spec:
  podTemplate:
    metadata:
      annotations:
        nvidia.com/cuda-shared-memory-support: enabled
```

This is a configuration fragment, not a complete manifest. SnapshotJob
installs the matching libraries and automatically adds the launcher; no
manual native CUDA job wrapper is needed. See
[Checkpoint a replica](checkpoint.md#option-2--snapshotjob-checkpoint-a-throwaway-replica)
for the SnapshotJob workflow, using this guide's runtime image.

The annotation does not configure vLLM parallelism or GPU resource requests.
Multiple GPUs also require configuring vLLM's `tensor_parallel_size` and
matching GPU requests in the source and restore pods. The application must
still quiesce before capture and resume only after `restore-complete`.
Adding the annotation to an ordinary Deployment does not provide
SnapshotJob's automatic launcher setup.

### Native CUDA job wrapping

For a single-GPU workload, launching the application with
[`cuda-checkpoint --launch-job`](https://github.com/NVIDIA/cuda-checkpoint)
groups its CUDA processes into a job so checkpointing can coordinate their
state. This is an alternative to changing worker startup to `spawn`, not
an additional prerequisite. SnapshotJob does **not** automatically add native
CUDA job wrapping.

NVIDIA [recommends native job wrapping only with driver 610 or newer](https://github.com/NVIDIA/cuda-checkpoint#610-features).
Do not treat an earlier single-GPU result on driver 595.58.03 as general
support for native job wrapping on older drivers.

## Earlier vLLM versions (0.27.1)

For customers remaining on vLLM 0.27.1, use the same application and steps
above with the previously validated `v0.27.1-ubuntu2404` image. After
downloading the files in step 1, and before deploying or taking a checkpoint,
set **both** local manifests to that image:

```bash
export VLLM_IMAGE=vllm/vllm-openai:v0.27.1-ubuntu2404@sha256:dafea057f24b7d42716331a48e2db4e1f204f877a3aa759cb7e4c37e64ca2eee

for manifest in deployment.yaml restore-deployment.yaml; do
  kubectl set image --local --filename "$manifest" \
    main="$VLLM_IMAGE" --output yaml > "$manifest.tmp" &&
    mv "$manifest.tmp" "$manifest"
done
```

Continue with step 2, then checkpoint and restore as usual. Source and
restore must use the same image; do not restore a 0.27.1 checkpoint into
the default 0.30.0 image. This section covers 0.27.1, not every older release.
