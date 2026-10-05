<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Checkpoint CUDA shared memory

CUDA shared-memory support preserves supported CUDA allocations shared between
processes on one node, including POSIX-exported VMM, synchronous memory IPC,
HOST_NUMA allocations, and multicast objects. Snapshot uses its internal
cuInterpose libraries to save and reconstruct these resources.

The [vLLM](vllm.md), [SGLang](sglang.md), and
[TensorRT-LLM](tensorrt-llm.md) examples enable this support. Use matching
Snapshot agent and operator builds that include the feature. The engine images
remain unmodified.

## Ordinary Pods and Deployments

The source manifests perform three steps:

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
envsubst '${SNAPSHOT_AGENT_IMAGE}' < deployment.yaml | \
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
| SGLang `NCCL_CUMEM_ENABLE`, `NCCL_NVLS_ENABLE` | Leave unset | Let the engine choose its CUDA allocation and multicast paths. SGLang 0.5.17 still defaults these off unless its corresponding options are enabled. |
| TensorRT-LLM `TLLM_NCCL_SYMMETRIC_ZERO_COPY` | Leave unset | Allow the engine's default selection. Availability and successful registration still determine whether registered windows are used. |
| SGLang `NCCL_IB_DISABLE=1` | Keep | RDMA connections and NIC registrations are outside CUDA shared-memory reconstruction. |
| SGLang RAS and PyTorch monitoring/timeout settings | Keep | These concern background services and checkpoint pauses. |
| TensorRT-LLM `OMPI_MCA_pml=ob1`, `OMPI_MCA_btl=tcp,self` | Keep | Keep CPU MPI traffic on the tested TCP transport. |
| vLLM multiprocessing `spawn`, pause/sleep, SGLang pause/release | Keep | Process startup, quiescence, and memory offload are separate concerns. |
| CUDA graphs and custom all-reduce | Use engine defaults | The examples do not add flags that disable them. |

Removing an override does not prove a feature ran. Engines can select a fallback
because of their version, GPU topology, or runtime probes. In particular, NCCL
NVLS and an engine's own multicast implementation are different paths.

## Multiple GPUs and gotchas

The examples default to one GPU. For two GPUs on one node, set
`SNAPSHOT_TENSOR_PARALLEL_SIZE` to `"2"` and the main container's
`resources.limits.nvidia.com/gpu` to `"2"` in both source and restore manifests.
Use a model whose attention heads and engine implementation support that
parallelism. Allocate compatible GPUs at restore.

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
