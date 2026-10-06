<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Checkpoint CUDA shared memory

CUDA shared-memory support preserves supported CUDA allocations shared between
processes on one node, including POSIX-exported VMM, synchronous memory IPC,
HOST_NUMA allocations, and multicast objects. Snapshot uses its internal
cuInterpose libraries to save and reconstruct these resources.

The [multi-GPU examples](#multi-gpu-models) enable this support. The small
single-GPU [vLLM](vllm.md), [SGLang](sglang.md), and
[TensorRT-LLM](tensorrt-llm.md) examples use native CUDA checkpoint and restore.
Use matching Snapshot agent and operator builds for shared-memory support.
The engine images remain unmodified.

## Ordinary Pods and Deployments

The multi-GPU source manifests perform three steps:

1. Set the Pod annotation `nvidia.com/cuda-shared-memory-support: "enabled"`.
2. Copy both libraries and `cuinterpose-launch` from the Snapshot agent image
   into an `emptyDir` mounted at `/tmp/snapshot-cuda`.
3. Run the engine through the launcher, which prepends the shim to the
   runtime-resolved `LD_PRELOAD` and executes the existing command.

Set `SNAPSHOT_AGENT_IMAGE` to the same immutable image reference used by the
capture and restore agents. For example, read the agent container's image from
the installed DaemonSet, substituting its namespace and name:

```bash
export SNAPSHOT_AGENT_IMAGE="$(kubectl --namespace <snapshot-namespace> \
  get daemonset <snapshot-agent> \
  -o jsonpath='{.spec.template.spec.containers[?(@.name=="agent")].image}')"
```

Use an image pinned by digest. If the DaemonSet uses a mutable tag, pin the
installation and installer to the same resolved digest first. A tag can otherwise
give the installer different library bytes from the running agent.

The source manifest contains `${SNAPSHOT_AGENT_IMAGE}`. Render only that
variable before applying it. `envsubst` is supplied by GNU gettext (`gettext-base`
on Debian and Ubuntu):

```bash
: "${SNAPSHOT_AGENT_IMAGE:?Set the matching Snapshot agent image first}"
envsubst '${SNAPSHOT_AGENT_IMAGE}' < deployment-glm-5.3.yaml | \
  kubectl apply --namespace "$SNAPSHOT_NAMESPACE" --filename -
```

Adding the annotation to an already running Pod cannot activate the shim. Create
a new source Pod with the installer and launcher in place. The restore manifest
keeps its inert `sleep infinity` command. The restore agent mounts the matching
libraries before restoring the checkpointed process.

## SnapshotJob

For a SnapshotJob, the operator installs the libraries and wraps the source
command automatically. Put the annotation under `spec.podTemplate.metadata`
and keep an explicit engine command:

```yaml
spec:
  podTemplate:
    metadata:
      annotations:
        nvidia.com/cuda-shared-memory-support: "enabled"
    spec:
      containers:
        - name: main
          command: ["python3", "/snapshot-app/app.py"]
```

This is a fragment to combine with the rest of your workload template. When
adapting the Deployment examples, remove their manual `snapshot-cuda-install`
init container, `snapshot-cuda` volume and mount, and launcher command prefix.
The operator owns those additions for SnapshotJobs. Keep the application
ConfigMap, cache, control volume, environment, and resource requirements.

The annotation accepts `enabled` or `disabled` after trimming whitespace.
Values are case-sensitive. Other values are rejected. Omitting the annotation
leaves a source that has no active shim on the native CUDA checkpoint path.

## Engine settings

| Setting | Recipe behavior | Reason |
| --- | --- | --- |
| SGLang `NCCL_CUMEM_ENABLE`, `NCCL_NVLS_ENABLE` | Leave unset | Let the engine choose its CUDA allocation and multicast paths. Engine options and runtime probes still determine which paths are selected. |
| TensorRT-LLM `TLLM_NCCL_SYMMETRIC_ZERO_COPY` | Leave unset | Allow the engine's default selection. Availability and successful registration still determine whether registered windows are used. |
| SGLang `NCCL_IB_DISABLE=1` | Keep | RDMA connections and NIC registrations are outside CUDA shared-memory reconstruction. |
| SGLang RAS and PyTorch monitoring/timeout settings | Keep | These concern background services and checkpoint pauses. |
| TensorRT-LLM `OMPI_MCA_pml=ob1`, `OMPI_MCA_btl=tcp,self` | Keep | Keep CPU MPI traffic on the tested TCP transport. |
| vLLM multiprocessing `spawn`, pause/sleep, SGLang pause/release | Keep | Process startup, quiescence, and memory offload are separate concerns. |
| CUDA graphs and custom all-reduce | Use engine defaults | The examples do not add flags that disable them. |

Removing an override does not prove a feature ran. Engines can select a fallback
because of their version, GPU topology, or runtime probes. In particular, NCCL
NVLS and an engine's own multicast implementation are different paths.

## Multi-GPU models

The single-GPU manifests remain small Qwen3 examples. The following separate
manifest pairs each use all eight GPUs on one B200 node. They use the same
`app.py` and ConfigMap as their engine's single-GPU example:

| Model | Engine | GPUs and parallelism | Context | Manifest pair |
| --- | --- | --- | --- | --- |
| GLM 5.3 NVFP4 | vLLM | 8, TP8/EP8 | 128K | [Source](vllm/deployment-glm-5.3.yaml), [restore](vllm/restore-deployment-glm-5.3.yaml) |
| GLM 5.3 NVFP4 | SGLang | 8, TP8/EP8 | 128K | [Source](sglang/deployment-glm-5.3.yaml), [restore](sglang/restore-deployment-glm-5.3.yaml) |
| GLM 5.3 NVFP4 | TensorRT-LLM | 8, TP8/EP8 | 128K | [Source](tensorrt-llm/deployment-glm-5.3.yaml), [restore](tensorrt-llm/restore-deployment-glm-5.3.yaml) |
| DeepSeek V4.1 Flash | vLLM | 8, TP8/EP8 | 128K | [Source](vllm/deployment-deepseek-v4.1-flash.yaml), [restore](vllm/restore-deployment-deepseek-v4.1-flash.yaml) |
| DeepSeek V4.1 Flash | SGLang | 8, TP8/EP8 | 128K | [Source](sglang/deployment-deepseek-v4.1-flash.yaml), [restore](sglang/restore-deployment-deepseek-v4.1-flash.yaml) |

These configurations keep prefill and decode in the same engine and do not
use serving-time KV offloading. The existing checkpoint pause and memory-release
steps still apply. GLM limits concurrency to 32 and DeepSeek to 64. These are
bounded example settings, not measured throughput optima or full-context stress
test results.

The GLM pairs pin [RadixArk/GLM-5.3-NVFP4](https://huggingface.co/RadixArk/GLM-5.3-NVFP4)
and the DeepSeek pairs pin
[deepseek-ai/DeepSeek-V4.1-Flash](https://huggingface.co/deepseek-ai/DeepSeek-V4.1-Flash).
Each source and restore pair uses the same model revision and engine image.
The [SGLang GLM guide](https://github.com/sgl-project/sglang/blob/v0.5.21/docs/cookbook/autoregressive/GLM/GLM-5.3.mdx)
labels this GLM quantized checkpoint experimental. Its
[B200 NVFP4 profile](https://github.com/sgl-project/sglang/blob/v0.5.21/docs/src/snippets/configs/zai-org/glm-5.3.jsx)
is marked unverified upstream.
The vLLM and SGLang examples use releases 0.31.0 and 0.5.21. DeepSeek follows
the [vLLM V4.1 recipe](https://github.com/vllm-project/recipes/blob/main/models/deepseek-ai/DeepSeek-V4.1-Flash.yaml)
and [SGLang B200 profile](https://github.com/sgl-project/sglang/blob/v0.5.21/docs/src/snippets/configs/deepseek-ai/deepseek-v4_1.jsx),
adapted here to TP8/EP8 with context bounded to 128K. Engram stays on the GPUs.
The author checkpoint
combines FP8 dense weights with four-bit routed experts, so the recipes let
the engine detect its quantization format.

Model-specific settings are JSON objects in `VLLM_ENGINE_ARGS`,
`SGLANG_ENGINE_ARGS`, or `TRTLLM_ENGINE_ARGS`. Keys are Python constructor
arguments, not CLI flags. They override the small example's defaults. The
GLM vLLM and SGLang profiles use the model's native MTP head to propose five tokens.
SGLang calls this path `EAGLE`. DeepSeek V4.1 uses its bundled DSpark head with
five speculative tokens. Neither profile downloads an external draft model.
These are baseline configurations, not claims that speculation improves every
workload or concurrency level. The TensorRT-LLM profile
omits MTP, following the pinned release's
[GLM NVFP4 guide](https://github.com/NVIDIA/TensorRT-LLM/blob/v1.3.0rc24/docs/source/deployment-guide/deployment-guide-for-glm-5-on-trtllm.md#b200-nvfp4-config).

Create the [shared model-cache PVC](model-cache-pvc.yaml) in the workload
namespace, using a ReadWriteMany storage class available in your cluster.
The large examples request 2 TiB of cache space and mount it at `/hf-cache`.
You can use an existing PVC containing a Hugging Face cache instead by changing
`claimName` in both manifests. The download init container reuses cached files
and downloads missing files for the exact model revision. Both source and restore
also mount a 64 GiB memory-backed `/dev/shm`.

For example, from the repository root, after creating the `vllm-app` ConfigMap
and setting `SNAPSHOT_AGENT_IMAGE`:

```bash
kubectl apply --namespace "$SNAPSHOT_NAMESPACE" \
  --filename docs/guides/model-cache-pvc.yaml

envsubst '${SNAPSHOT_AGENT_IMAGE}' \
  < docs/guides/vllm/deployment-deepseek-v4.1-flash.yaml | \
  kubectl apply --namespace "$SNAPSHOT_NAMESPACE" --filename -

kubectl rollout status --namespace "$SNAPSHOT_NAMESPACE" \
  deployment/vllm-deepseek-v4.1-flash-source --timeout=60m
```

[Checkpoint](checkpoint.md) the resulting source Pod. Set the restore
manifest's `nvidia.com/restore-from` to that PodSnapshot name, then follow the
[restore guide](restore.md) with the matching multi-GPU restore manifest.
Restore needs the same GPU count and enough CPU memory for the captured state.
The vLLM and SGLang large-model pairs request 1 TiB of host memory for the
checkpoint-time weight backup and process state. This is separate from
serving-time offloading.
The TensorRT-LLM GLM recipe requests 1 TiB of host memory without a hard memory
limit. It retains GPU state during capture, so the host copy can exceed 1 TiB.
Use a node with enough available RAM and measure peak memory before setting a
hard limit for your workload.
On a node with only enough GPUs for one copy, remove the source after the
checkpoint is ready before creating the restore Pod.

A populated model cache avoids repeated weight downloads. Compilation caches
can also be mounted on persistent storage, but reuse them only with compatible
engine, CUDA and GPU versions. Record cache use when comparing startup times.

External compilation caches need extra care when reusing a checkpoint. On NFS,
a mapped temporary JIT library can depend on a remap link that CRIU removes
during restore. Keeping the checkpoint directory alone does not preserve that
external file for another restore. Keep compiler caches in the captured
container filesystem unless your external-cache setup preserves every
referenced file for each restore.

## Gotchas

Use the declared GPU count and tensor parallelism in the selected multi-GPU
manifest pair. Allocate compatible GPUs at restore.

- Finish requests and all CUDA work before announcing `ready-for-snapshot`.
  Shared-memory support does not pause the application for you. Shared HOST_NUMA
  memory must also have no CPU writers during capture.
- Keep all sharing processes in the captured group, with the shim loaded from
  startup. Do not share CUDA allocations with an external process.
- Keep the library files unchanged. Restore checks both library hashes even
  when compatibility checks are skipped. Recreate a checkpoint after changing
  the shim bundle.
- Supported sharing is within one node. Moving the whole captured Pod to a
  compatible node is different from checkpointing a model distributed across
  nodes. FABRIC handles and RDMA are outside this recipe's scope.
- Converted CUDA allocations use VMM backing rounded to the GPU's allocation
  granularity. Many small allocations can increase startup cost and memory use.
- Reuse model and compilation caches when testing, but always perform a fresh
  capture of the configuration being qualified. Verify a new inference request
  after restore, not only Pod readiness.
