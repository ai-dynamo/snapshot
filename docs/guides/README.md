# Usage guides

Using Snapshot is a three-stage flow:

1. **Make the workload snapshot-ready** — the per-framework guides below run
   the framework's runtime image unmodified and mount an entrypoint into it
   from a ConfigMap, implementing the
   [workload contract](../reference/workload-contract.md).
2. **Checkpoint** the running replica — with a `PodSnapshot` or a `SnapshotJob`.
3. **Restore** into new pods — with the `nvidia.com/restore-from` annotation.

The flow is the same for every framework and for single-GPU and eight-GPU
workloads. The [recipe](#pick-a-recipe) you pick decides which manifests you
apply, and each stage calls out the few steps where an eight-GPU recipe differs.
You are done when a restored replica answers a request without cold-starting.

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
   exist. The guides read it from `SNAPSHOT_NAMESPACE`:

   ```bash
   export SNAPSHOT_NAMESPACE=<namespace>
   kubectl get namespace "$SNAPSHOT_NAMESPACE" || kubectl create namespace "$SNAPSHOT_NAMESPACE"
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
  --namespace "$SNAPSHOT_NAMESPACE" \
  --filename model-cache-pvc.yaml
```

## Pick a recipe

A recipe is one inference framework serving one model on a fixed number of GPUs.
Each recipe ships a source Deployment and a `SnapshotJob` for
[checkpointing](checkpoint.md), and its framework ships a restore profile for the
GPU count. Each name below links to its manifest, and the guides use these names
in their `kubectl` commands.

| Framework | Model | GPUs | Source Deployment | `SnapshotJob` | Restore Deployment |
|-----------|-------|------|-------------------|---------------|--------------------|
| [vLLM](vllm.md) | Qwen3 0.6B | 1 | [`vllm-qwen3-0-6b-source`](vllm/capture/qwen3-0.6b.yaml) | [`vllm-qwen3-0-6b-snapshot`](vllm/capture/qwen3-0.6b-snapshotjob.yaml) | [`vllm-1gpu-restored`](vllm/restore/1-gpu.yaml) |
| [vLLM](vllm.md) | GLM 5.3 NVFP4 | 8 | [`vllm-glm-5-3-source`](vllm/capture/glm-5.3.yaml) | [`vllm-glm-5-3-snapshot`](vllm/capture/glm-5.3-snapshotjob.yaml) | [`vllm-8gpu-restored`](vllm/restore/8-gpu.yaml) |
| [vLLM](vllm.md) | DeepSeek V4.1 Flash | 8 | [`vllm-deepseek-v4-1-flash-source`](vllm/capture/deepseek-v4.1-flash.yaml) | [`vllm-deepseek-v4-1-flash-snapshot`](vllm/capture/deepseek-v4.1-flash-snapshotjob.yaml) | [`vllm-8gpu-restored`](vllm/restore/8-gpu.yaml) |
| [SGLang](sglang.md) | Qwen3 0.6B | 1 | [`sglang-qwen3-0-6b-source`](sglang/capture/qwen3-0.6b.yaml) | [`sglang-qwen3-0-6b-snapshot`](sglang/capture/qwen3-0.6b-snapshotjob.yaml) | [`sglang-1gpu-restored`](sglang/restore/1-gpu.yaml) |
| [SGLang](sglang.md) | GLM 5.3 NVFP4 | 8 | [`sglang-glm-5-3-source`](sglang/capture/glm-5.3.yaml) | [`sglang-glm-5-3-snapshot`](sglang/capture/glm-5.3-snapshotjob.yaml) | [`sglang-8gpu-restored`](sglang/restore/8-gpu.yaml) |
| [SGLang](sglang.md) | DeepSeek V4.1 Flash | 8 | [`sglang-deepseek-v4-1-flash-source`](sglang/capture/deepseek-v4.1-flash.yaml) | [`sglang-deepseek-v4-1-flash-snapshot`](sglang/capture/deepseek-v4.1-flash-snapshotjob.yaml) | [`sglang-8gpu-restored`](sglang/restore/8-gpu.yaml) |
| [TensorRT-LLM](tensorrt-llm.md) | Qwen3 0.6B | 1 | [`tensorrt-llm-qwen3-0-6b-source`](tensorrt-llm/capture/qwen3-0.6b.yaml) | [`tensorrt-llm-qwen3-0-6b-snapshot`](tensorrt-llm/capture/qwen3-0.6b-snapshotjob.yaml) | [`tensorrt-llm-1gpu-restored`](tensorrt-llm/restore/1-gpu.yaml) |
| [TensorRT-LLM](tensorrt-llm.md) | GLM 5.3 NVFP4 | 8 | [`tensorrt-llm-glm-5-3-source`](tensorrt-llm/capture/glm-5.3.yaml) | [`tensorrt-llm-glm-5-3-snapshot`](tensorrt-llm/capture/glm-5.3-snapshotjob.yaml) | [`tensorrt-llm-8gpu-restored`](tensorrt-llm/restore/8-gpu.yaml) |

The names follow one pattern, with the dots of a model's name turned into
hyphens: `<framework>-<model>-source` for the source Deployment,
`<framework>-<model>-snapshot` for the `SnapshotJob` and the `PodSnapshot` it
produces, and `<framework>-<1|8>gpu-restored` for the restored Deployment.

The framework guides and the stages below use the Qwen3 0.6B recipe in their
commands. For another recipe, use its manifests and names from this table.

### Eight-GPU recipes

An eight-GPU recipe runs one model across all eight GPUs of a single node. Set up
the following once, before the first stage:

- **A node that fits the recipe.** All GPUs of the workload are on one node;
  multi-node is on the roadmap. The node has the GPU count, CPU, and memory the
  manifests request. The manifests select eight B200 GPUs. Change `nodeSelector`
  and the parallelism settings for other GPUs.
- **The agent image, for the source Deployments only.** A Deployment prepares
  its own Pod for multi-GPU capture with an init container that copies the
  multi-GPU libraries from the Snapshot agent image. Read the image from the
  installed agent, in the namespace Snapshot is installed in (`snapshot` in the
  README's install command), and substitute it into the Deployment manifest when
  you apply it.

  ```bash
  export SNAPSHOT_AGENT_IMAGE=$(kubectl get daemonset snapshot-agent \
    --namespace snapshot \
    --output jsonpath='{.spec.template.spec.containers[?(@.name=="agent")].image}')
  ```

  A `SnapshotJob` needs no substitution: the controller prepares its source Pod.
- **Time for the first run.** The model download dominates it. The manifests
  allow one hour for a Deployment to become ready and two for a `SnapshotJob`.
  On a slow link, raise `progressDeadlineSeconds` or `activeDeadlineSeconds`.

## 1. Make the workload snapshot-ready

Per inference framework, mount an entrypoint into the framework's runtime
image and deploy it:

- [vLLM](vllm.md)
- [SGLang](sglang.md)
- [TensorRT-LLM](tensorrt-llm.md)

## 2. Checkpoint

- [Checkpoint a replica](checkpoint.md)

## 3. Restore

- [Restore a replica](restore.md)

## Clean up and run again

The names above are fixed, so a recipe cannot run twice in one namespace until
the first run is deleted. A `SnapshotJob` fails with `PodSnapshotNameConflict` when
a `PodSnapshot` of its name already exists, which is what Option 1 and any earlier
`SnapshotJob` leave behind. Delete a run's resources with the restored replica
first, using your recipe's names from the table. The commands skip what the
other capture lane never created:

```bash
kubectl delete deployment vllm-1gpu-restored --namespace "$SNAPSHOT_NAMESPACE" --ignore-not-found
kubectl delete deployment vllm-qwen3-0-6b-source --namespace "$SNAPSHOT_NAMESPACE" --ignore-not-found
kubectl delete snapshotjob vllm-qwen3-0-6b-snapshot --namespace "$SNAPSHOT_NAMESPACE" --ignore-not-found
kubectl delete podsnapshot vllm-qwen3-0-6b-snapshot --namespace "$SNAPSHOT_NAMESPACE" --ignore-not-found
```

Deleting a `SnapshotJob` keeps its `PodSnapshot`, so a checkpoint outlives the job.
Deleting a `PodSnapshot` deletes its checkpoint from the checkpoint volume. Keep the
ConfigMap and the model cache for the next run, which saves the download. When you
are done with every recipe, and no Pod mounts it any more, delete the model cache
with `kubectl delete pvc model-cache`.

## If a step fails

Describe the resource that stalled, such as `kubectl describe podsnapshot <name>`,
`kubectl describe snapshotjob <name>` or `kubectl describe pod <pod>`, then see
[Troubleshooting](../operations/troubleshooting.md) for the agent logs and the
common causes.

See [Installation](../operations/install.md) for installing and uninstalling
Snapshot and the [API reference](../reference/api.md) for full resource detail.
