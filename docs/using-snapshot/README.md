# Using Snapshot

Snapshot checkpoints a running inference replica, so a new replica restores from
the snapshot in seconds instead of loading the model from scratch. This section
takes you through the flow once, end to end. At the end, a restored replica is up
and ready without cold-starting.

## The Flow

| Phase | What you do | Page |
|-------|-------------|------|
| Prepare the workload | Create the ConfigMap with the framework's `app.py` and deploy the source replica | [vLLM](supported-frameworks/vllm.md), [SGLang](supported-frameworks/sglang.md), [TensorRT-LLM](supported-frameworks/tensorrt-llm.md) |
| Checkpoint a running pod | Create a snapshot of the source replica, with a `PodSnapshot` or a `SnapshotJob` | [Checkpoint a Pod](checkpoint.md) |
| Restore a new pod from the snapshot | Start a new replica from the snapshot | [Restore a Pod](restore.md) |

## What This Page Covers

This page is the setup for the whole flow. You do it once, before you prepare the workload:

- [Before You Start](#before-you-start) - Install Snapshot, download the example
  files and pick the namespace.
- [Model Cache](#model-cache) - Create the volume that holds the model files.
- [Choose Your Recipe](#choose-your-recipe) - Pick the framework, model and GPU
  count, and set the values the commands on the other pages use.

The commands are the same for every inference framework (vLLM, SGLang and
TensorRT-LLM) and every recipe. The recipe you choose here sets the values they use.

> [!NOTE]
> This section uses `kubectl` to show the resources and the flow. In production, a
> controller or platform creates and watches these resources through the Kubernetes
> API as part of its own control loop. The `kubectl` commands here let you run each
> step yourself and see what happens.

## Before You Start

1. **Install Snapshot.** Follow [Prerequisites](../get-started/prerequisites.md)
   and [Installation](../get-started/installation.md).
2. **Download the example files.** This section uses ready-made Kubernetes
   manifests and small entrypoint programs. They all live in the `docs/using-snapshot`
   folder of the Snapshot repository. Clone it and open that folder: every
   command in this section is written to run from there.

   ```bash
   git clone https://github.com/ai-dynamo/snapshot.git
   cd snapshot/docs/using-snapshot
   ```

   The folder holds these files for each recipe:

   | File | What it is |
   |---|---|
   | `model-cache-pvc.yaml` | The model cache PVC, shared by all recipes. |
   | `<framework>/app.py` | The program that prepares the framework for checkpoint and resumes it after restore. |
   | `<framework>/capture/<model>.yaml` | The source Deployment, for a `PodSnapshot`. |
   | `<framework>/capture/<model>-snapshotjob.yaml` | The `SnapshotJob`. |
   | `<framework>/restore/1-gpu.yaml`, `8-gpu.yaml` | The restore Deployments, by GPU count. |

3. **Pick the namespace** where the workload runs, creating it if it does not
   exist. Use a different namespace from the one Snapshot is installed in
   (`snapshot` in the [Installation](../get-started/installation.md) command).
   The commands read it from `WORKLOAD_NAMESPACE`:

   ```bash
   export WORKLOAD_NAMESPACE=<namespace>
   kubectl get namespace "$WORKLOAD_NAMESPACE" || kubectl create namespace "$WORKLOAD_NAMESPACE"
   ```

4. **Create the model cache.** Every recipe reads its model from it, as the
   next section describes.

### Model Cache

Every recipe reads its model from a model cache: a PVC named `model-cache`,
mounted at `/hf-cache`, so that the source and the restored Pods read the same
files. An init container downloads the model from Hugging Face into it on first
use, so the pods need network access to `huggingface.co`. The first run downloads
about 1.5 GB for Qwen3 0.6B, 465 GB for GLM 5.3 NVFP4 and 510 GB for DeepSeek
V4.1 Flash, and later runs reuse the files. The model cache is a plain Kubernetes
volume, separate from the [snapshot volume](../get-started/installation.md#snapshot-volume) that
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

## Choose Your Recipe

A recipe is one inference framework serving one model on a fixed number of GPUs.
Choose one row from the table. The commands on the other pages use it.

| Recipe | FRAMEWORK | MODEL | GPUS |
|--------|-------------|---------|--------|
| vLLM, Qwen3 0.6B | `vllm` | `qwen3-0.6b` | `1` |
| vLLM, GLM 5.3 NVFP4 | `vllm` | `glm-5.3` | `8` |
| vLLM, DeepSeek V4.1 Flash | `vllm` | `deepseek-v4.1-flash` | `8` |
| SGLang, Qwen3 0.6B | `sglang` | `qwen3-0.6b` | `1` |
| SGLang, GLM 5.3 NVFP4 | `sglang` | `glm-5.3` | `8` |
| SGLang, DeepSeek V4.1 Flash | `sglang` | `deepseek-v4.1-flash` | `8` |
| TensorRT-LLM, Qwen3 0.6B | `tensorrt-llm` | `qwen3-0.6b` | `1` |
| TensorRT-LLM, GLM 5.3 NVFP4 | `tensorrt-llm` | `glm-5.3` | `8` |

### Set the Recipe Values

Set the three values from your row. The example is the first row:

```bash
export FRAMEWORK=vllm
export MODEL=qwen3-0.6b
export GPUS=1
```

### Set the Names

Paste this block as it is. It works out the files and resource names for your
recipe from those three values, so nothing in it needs editing:

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

### Eight-GPU Recipes

A multi-GPU recipe runs one model across the GPUs of a single node.

> [!NOTE]
> Multi-node is not supported yet.

Before you use an eight-GPU recipe:

- **GPUs** - The manifests select eight B200 GPUs
  (`nodeSelector: nvidia.com/gpu.product: NVIDIA-B200`). For other GPUs, change
  `nodeSelector` and the parallelism settings described under Eight-GPU Recipes
  on each framework page:
  [vLLM](supported-frameworks/vllm.md#eight-gpu-recipes),
  [SGLang](supported-frameworks/sglang.md#eight-gpu-recipes),
  [TensorRT-LLM](supported-frameworks/tensorrt-llm.md#eight-gpu-recipes).
- **Driver** - Multi-GPU restore needs a minimum CUDA user-mode driver version.
  See [Prerequisites](../get-started/prerequisites.md#multi-gpu-workloads).
- **Agent image** - If you checkpoint with a `PodSnapshot`, the source manifest
  needs the image of your installed Snapshot agent: an init container copies
  Snapshot's multi-GPU libraries from it. You set it when you deploy the source
  replica, under Deploy the Source Pod on the page for your framework
  ([vLLM](supported-frameworks/vllm.md#deploy-the-source-pod),
  [SGLang](supported-frameworks/sglang.md#deploy-the-source-pod),
  [TensorRT-LLM](supported-frameworks/tensorrt-llm.md#deploy-the-source-pod)). A
  `SnapshotJob` creates the source replica itself, so you can skip this.

To get the agent image, read it from the `snapshot-agent` DaemonSet in the
namespace where you installed Snapshot (`snapshot` in the
[Installation](../get-started/installation.md) command):

```bash
export SNAPSHOT_AGENT_IMAGE=$(kubectl get daemonset snapshot-agent \
  --namespace snapshot \
  --output jsonpath='{.spec.template.spec.containers[?(@.name=="agent")].image}')
```

## Next Steps

- [vLLM](supported-frameworks/vllm.md), [SGLang](supported-frameworks/sglang.md) or
  [TensorRT-LLM](supported-frameworks/tensorrt-llm.md) - Prepare the workload for
  your framework
- [Checkpoint a Pod](checkpoint.md) - Create a snapshot of the source replica
- [Restore a Pod](restore.md) - Start a new pod from the snapshot
