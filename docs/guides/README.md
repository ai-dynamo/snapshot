# Usage guides

Snapshot saves a running inference replica as a checkpoint, so a new replica
starts from it in seconds instead of loading the model from scratch. These
guides take you through it once, end to end:

1. **Prepare** the workload.
2. **Checkpoint** a running replica.
3. **Restore** a new replica from the checkpoint and send it a request.

The commands are the same for every inference engine (vLLM, SGLang and
TensorRT-LLM) and every recipe. You choose your recipe once, [below](#choose-your-recipe),
and every later command uses that choice. You are done when the restored replica
answers a request without cold-starting.

> [!NOTE]
> These guides use `kubectl` to show the resources and the flow. In production, a
> controller or platform creates and watches these resources through the Kubernetes
> API as part of its own control loop — `kubectl` here is just for illustration and
> for trying things out by hand.

## Before you start

1. **Install Snapshot.** The cluster [prerequisites](../../README.md#prerequisites)
   and [installation](../../README.md#installation) steps are in the README.
   Nothing else in these guides repeats them.
2. **Download the example files.** The guides use ready-made Kubernetes
   manifests and small entrypoint programs. They all live in the `docs/guides`
   folder of the Snapshot repository. Clone it and open that folder: every
   command in the guides is written to run from there.

   ```bash
   git clone --branch release/0.2 https://github.com/ai-dynamo/snapshot.git
   cd snapshot/docs/guides
   ```

3. **Pick the namespace** where the workload runs, creating it if it does not
   exist. This is your own namespace for the replicas, not the one Snapshot is
   installed in (`snapshot` in the README's install command). The guides read it
   from `WORKLOAD_NAMESPACE`:

   ```bash
   export WORKLOAD_NAMESPACE=<namespace>
   kubectl get namespace "$WORKLOAD_NAMESPACE" || kubectl create namespace "$WORKLOAD_NAMESPACE"
   ```

4. **Create the model cache.** Every recipe reads its model from it, as the
   next section describes.

### Model cache

Every recipe reads its model from a model cache: a PVC named `model-cache`,
mounted at `/hf-cache`, so that the source and the restored Pods read the same
files. An init container downloads the model from Hugging Face into it on first
use, so the pods need network access to `huggingface.co`. The first run downloads
about 1.5 GB for Qwen3 0.6B, 465 GB for GLM 5.3 NVFP4 and 510 GB for DeepSeek
V4.1 Flash, and later runs reuse the files. The model cache is a plain Kubernetes
volume, separate from the [checkpoint volume](../operations/storage.md) that
Snapshot installs.

The PVC requests 2 TiB with `ReadWriteMany` access from the default storage class,
which holds all three models. Set `storageClassName` in
[`model-cache-pvc.yaml`](model-cache-pvc.yaml) if the default does not support
`ReadWriteMany`, then create it once:

```bash
kubectl apply \
  --namespace "$WORKLOAD_NAMESPACE" \
  --filename model-cache-pvc.yaml
```

## Choose your recipe

A recipe is one inference engine serving one model on a fixed number of GPUs.
Pick a row and set the three values from it:

| Recipe | `FRAMEWORK` | `MODEL` | `GPUS` |
|--------|-------------|---------|--------|
| vLLM, Qwen3 0.6B | `vllm` | `qwen3-0.6b` | `1` |
| vLLM, GLM 5.3 NVFP4 | `vllm` | `glm-5.3` | `8` |
| vLLM, DeepSeek V4.1 Flash | `vllm` | `deepseek-v4.1-flash` | `8` |
| SGLang, Qwen3 0.6B | `sglang` | `qwen3-0.6b` | `1` |
| SGLang, GLM 5.3 NVFP4 | `sglang` | `glm-5.3` | `8` |
| SGLang, DeepSeek V4.1 Flash | `sglang` | `deepseek-v4.1-flash` | `8` |
| TensorRT-LLM, Qwen3 0.6B | `tensorrt-llm` | `qwen3-0.6b` | `1` |
| TensorRT-LLM, GLM 5.3 NVFP4 | `tensorrt-llm` | `glm-5.3` | `8` |

```bash
export FRAMEWORK=vllm
export MODEL=qwen3-0.6b
export GPUS=1
```

Then paste this block as it is. It works out the files and resource names for
your recipe from those three values, so nothing in it needs editing:

```bash
NAME="$FRAMEWORK-${MODEL//./-}"
export SOURCE_MANIFEST="$FRAMEWORK/capture/$MODEL.yaml"
export SNAPSHOTJOB_MANIFEST="$FRAMEWORK/capture/$MODEL-snapshotjob.yaml"
export RESTORE_MANIFEST="$FRAMEWORK/restore/$GPUS-gpu.yaml"
export SOURCE_NAME="$NAME-source"
export SNAPSHOT_NAME="$NAME-snapshot"
export RESTORED_NAME="$FRAMEWORK-${GPUS}gpu-restored"
export WAIT=$([ "$GPUS" = 1 ] && echo 30m || echo 120m)
```

If you chose a recipe with eight GPUs, also read the agent image from the
installed Snapshot agent. The source replica needs it to prepare itself for
multi-GPU capture. Use the namespace Snapshot is installed in (`snapshot` in the
README's install command):

```bash
export SNAPSHOT_AGENT_IMAGE=$(kubectl get daemonset snapshot-agent \
  --namespace snapshot \
  --output jsonpath='{.spec.template.spec.containers[?(@.name=="agent")].image}')
```

A multi-GPU recipe runs one model across the GPUs of a single node;
multi-node is not supported yet. The manifests select eight B200 GPUs, so change
`nodeSelector` and the parallelism settings for other GPUs. See
[Multi-GPU workloads](../reference/workload-contract.md#multi-gpu-workloads) for
what the extra configuration does.

## 1. Prepare the workload

Each engine runs its unmodified runtime image and mounts a small program,
`app.py`, from a ConfigMap. The program prepares the engine for checkpoint and
resumes it after restore, implementing the
[workload contract](../reference/workload-contract.md). Create the ConfigMap:

```bash
kubectl create configmap "$FRAMEWORK-app" \
  --namespace "$WORKLOAD_NAMESPACE" \
  --from-file=app.py="$FRAMEWORK/app.py"
```

The engine guides explain how each program works and how to change the model:
[vLLM](vllm.md), [SGLang](sglang.md) and [TensorRT-LLM](tensorrt-llm.md).

## 2. Checkpoint

There are two ways to take the checkpoint. Pick one:

| | Choose it when… |
|---|---|
| **Option A: `PodSnapshot`** | You run the replica yourself and checkpoint it while it keeps serving. |
| **Option B: `SnapshotJob`** | You want Snapshot to run the replica, checkpoint it and remove it. |

### Option A: `PodSnapshot`

Start the source replica and wait until it is ready to be checkpointed. The first
run downloads the model, so this can take a while:

```bash
sed "s|\${SNAPSHOT_AGENT_IMAGE}|$SNAPSHOT_AGENT_IMAGE|" "$SOURCE_MANIFEST" | \
  kubectl apply --namespace "$WORKLOAD_NAMESPACE" --filename -

kubectl rollout status \
  --namespace "$WORKLOAD_NAMESPACE" \
  deployment/"$SOURCE_NAME" \
  --timeout="$WAIT"
```

Then ask Snapshot to checkpoint that replica, and wait until the checkpoint is
ready:

```bash
POD=$(kubectl get pods --namespace "$WORKLOAD_NAMESPACE" \
  --selector app="$SOURCE_NAME" --output jsonpath='{.items[0].metadata.name}')

kubectl apply --namespace "$WORKLOAD_NAMESPACE" --filename - <<EOF
apiVersion: nvidia.com/v1alpha1
kind: PodSnapshot
metadata:
  name: $SNAPSHOT_NAME
spec:
  source:
    podRef:
      name: $POD
      containers:
        - main
EOF

kubectl wait --namespace "$WORKLOAD_NAMESPACE" \
  --for=condition=Ready podsnapshot/"$SNAPSHOT_NAME" \
  --timeout="$WAIT"
```

The source replica keeps running and serving.

### Option B: `SnapshotJob`

```bash
kubectl apply --namespace "$WORKLOAD_NAMESPACE" --filename "$SNAPSHOTJOB_MANIFEST"

kubectl wait --namespace "$WORKLOAD_NAMESPACE" \
  --for=condition=Completed snapshotjob/"$SNAPSHOT_NAME" \
  --timeout="$WAIT"
```

Snapshot runs the replica, checkpoints it and removes it, so every replica,
including the first, comes up by restore. The checkpoint is a `PodSnapshot` with
the same name as the job.

If the checkpoint does not become Ready, see
[Troubleshooting](../operations/troubleshooting.md#a-checkpoint-never-becomes-ready).

## 3. Restore

Start a new replica from the checkpoint. It is a new Deployment whose pods carry
the `nvidia.com/restore-from` annotation naming your checkpoint, and it starts in
seconds, because the node agent restores the saved state into it instead of
loading the model:

```bash
sed "s|\${SNAPSHOT_NAME}|$SNAPSHOT_NAME|" "$RESTORE_MANIFEST" | \
  kubectl apply --namespace "$WORKLOAD_NAMESPACE" --filename -

kubectl rollout status \
  --namespace "$WORKLOAD_NAMESPACE" \
  deployment/"$RESTORED_NAME" \
  --timeout="$WAIT"
```

Then send it a request. In one terminal, forward its port:

```bash
kubectl port-forward \
  --namespace "$WORKLOAD_NAMESPACE" \
  deployment/"$RESTORED_NAME" 8000:8000
```

and in a second terminal, ask it a question:

```bash
curl --silent --request POST http://localhost:8000/generate \
  --header 'Content-Type: application/json' \
  --data '{"prompt": "What is the capital of Italy?"}'
```

It answers with generated text in a `text` field, without loading the model
again. The pod also carries a `nvidia.com/Restored` condition once the restore
completes. If the restore does not complete, see
[Troubleshooting](../operations/troubleshooting.md).

In practice, higher-level systems create these restored Deployments rather than
applying them by hand. See the
[restore Pod contract](../reference/restore-pod-contract.md) to generate restore
pods from a controller, operator or serving platform.

## Use your own engine or image

The recipes above show three engines, but Snapshot works with any inference
engine whose workload meets the
[workload contract](../reference/workload-contract.md): how it prepares for a
checkpoint, how it resumes after a restore, and what its pod must carry.

- To package an engine the way these guides do, with the runtime image
  unmodified and a small program mounted from a ConfigMap, or to bake the
  program into your own image, see
  [Packaging methods](../reference/workload-contract.md#packaging-methods).
- To add support for an engine that is not covered here, follow
  [Support a new inference server](../reference/workload-contract.md#support-a-new-inference-server).
  The [engine guides](vllm.md) are worked examples of the same contract.

See [Installation](../operations/install.md) for installing and uninstalling
Snapshot and the [API reference](../reference/api.md) for full resource detail.
