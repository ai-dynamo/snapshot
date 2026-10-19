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
2. **Get the example files.** The manifests and entrypoints live in this
   directory. Every command in the guides runs from it:

   ```bash
   git clone https://github.com/ai-dynamo/snapshot.git
   cd snapshot/docs/guides
   ```

3. **Pick the namespace** where the workload runs. The guides read it from
   `SNAPSHOT_NAMESPACE`:

   ```bash
   export SNAPSHOT_NAMESPACE=<namespace>
   kubectl get namespace "$SNAPSHOT_NAMESPACE"
   ```

## Pick a recipe

A recipe is one inference framework serving one model on a fixed number of GPUs.
Each recipe ships a source Deployment and a `SnapshotJob` for
[checkpointing](checkpoint.md), and its framework ships a restore profile for the
GPU count. Each name below links to its manifest, and the guides use these names
in their `kubectl` commands.

| Framework | Model | GPUs | Source Deployment | `SnapshotJob` | Restore Deployment |
|-----------|-------|------|-------------------|---------------|--------------------|
| [vLLM](vllm.md) | Qwen3 0.6B | 1 | [`vllm-source`](vllm/capture/qwen3-0.6b.yaml) | [`vllm-snapshot`](vllm/capture/qwen3-0.6b-snapshotjob.yaml) | [`vllm-restored`](vllm/restore/single-gpu.yaml) |
| [vLLM](vllm.md) | GLM 5.3 NVFP4 | 8 | [`vllm-glm-5-3-source`](vllm/capture/glm-5.3.yaml) | [`vllm-glm-5-3-snapshot`](vllm/capture/glm-5.3-snapshotjob.yaml) | [`vllm-8gpu-restored`](vllm/restore/8-gpu.yaml) |
| [vLLM](vllm.md) | DeepSeek V4.1 Flash | 8 | [`vllm-deepseek-v4.1-flash-source`](vllm/capture/deepseek-v4.1-flash.yaml) | [`vllm-deepseek-v4.1-flash-snapshot`](vllm/capture/deepseek-v4.1-flash-snapshotjob.yaml) | [`vllm-8gpu-restored`](vllm/restore/8-gpu.yaml) |
| [SGLang](sglang.md) | Qwen3 0.6B | 1 | [`sglang-source`](sglang/capture/qwen3-0.6b.yaml) | [`sglang-snapshot`](sglang/capture/qwen3-0.6b-snapshotjob.yaml) | [`sglang-restored`](sglang/restore/single-gpu.yaml) |
| [SGLang](sglang.md) | GLM 5.3 NVFP4 | 8 | [`sglang-glm-5-3-source`](sglang/capture/glm-5.3.yaml) | [`sglang-glm-5-3-snapshot`](sglang/capture/glm-5.3-snapshotjob.yaml) | [`sglang-8gpu-restored`](sglang/restore/8-gpu.yaml) |
| [SGLang](sglang.md) | DeepSeek V4.1 Flash | 8 | [`sglang-deepseek-v4.1-flash-source`](sglang/capture/deepseek-v4.1-flash.yaml) | [`sglang-deepseek-v4.1-flash-snapshot`](sglang/capture/deepseek-v4.1-flash-snapshotjob.yaml) | [`sglang-8gpu-restored`](sglang/restore/8-gpu.yaml) |
| [TensorRT-LLM](tensorrt-llm.md) | Qwen3 0.6B | 1 | [`tensorrt-llm-source`](tensorrt-llm/capture/qwen3-0.6b.yaml) | [`tensorrt-llm-snapshot`](tensorrt-llm/capture/qwen3-0.6b-snapshotjob.yaml) | [`tensorrt-llm-restored`](tensorrt-llm/restore/single-gpu.yaml) |
| [TensorRT-LLM](tensorrt-llm.md) | GLM 5.3 NVFP4 | 8 | [`tensorrt-llm-glm-5-3-source`](tensorrt-llm/capture/glm-5.3.yaml) | [`tensorrt-llm-glm-5-3-snapshot`](tensorrt-llm/capture/glm-5.3-snapshotjob.yaml) | [`tensorrt-llm-8gpu-restored`](tensorrt-llm/restore/8-gpu.yaml) |

The framework guides and the stages below use the Qwen3 0.6B recipe in their
commands. For another recipe, use its manifests and names from this table.

### Eight-GPU recipes

An eight-GPU recipe runs one model across all eight GPUs of a single node. Set
up the following once, before the first stage:

- **A node that fits the recipe.** All GPUs of the workload are on one node;
  multi-node is on the roadmap. The node has the GPU count, CPU, and memory the
  manifests request. The manifests select eight B200 GPUs. Change `nodeSelector`
  and the parallelism settings for other GPUs.
- **A PVC for the model files.** These recipes keep the model on a PVC named
  `snapshot-model-cache` instead of in the container's own filesystem, so the
  source and the restored Pods read the same files. An init container downloads
  the model onto it on first use. It is a plain Kubernetes volume, separate from
  the [checkpoint volume](../operations/storage.md) that Snapshot installs. It
  requests 2 TiB with `ReadWriteMany` access from the default storage class. Set
  `storageClassName` in [`model-cache-pvc.yaml`](model-cache-pvc.yaml) if the
  default does not support `ReadWriteMany`, then create it:

  ```bash
  kubectl apply \
    --namespace "$SNAPSHOT_NAMESPACE" \
    --filename model-cache-pvc.yaml
  ```

- **The agent image, for the source Deployments only.** A Deployment prepares
  its own Pod for multi-GPU capture with an init container that copies the
  multi-GPU libraries from the Snapshot agent image. Read the image from the
  installed agent, in the namespace Snapshot is installed in (`snapshot` in the
  README's install command), and substitute it into the Deployment manifest when
  you apply it. `envsubst` comes with `gettext-base` on Debian and Ubuntu.

  ```bash
  export SNAPSHOT_AGENT_IMAGE=$(kubectl get daemonset snapshot-agent \
    --namespace snapshot \
    --output jsonpath='{.spec.template.spec.containers[?(@.name=="agent")].image}')
  ```

  A `SnapshotJob` needs no substitution: the controller prepares its source Pod.

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

See [Installation](../operations/install.md) for installing and uninstalling
Snapshot and the [API reference](../reference/api.md) for full resource detail.
