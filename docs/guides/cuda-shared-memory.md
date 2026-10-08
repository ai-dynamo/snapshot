<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Run a multi-GPU example

These examples enable CUDA shared-memory checkpointing on one node with eight
B200 GPUs. The single-GPU [vLLM](vllm.md), [SGLang](sglang.md), and
[TensorRT-LLM](tensorrt-llm.md) examples do not need this option.

## Prerequisites

- [Install Snapshot](../operations/install.md) with matching agent and operator builds.
- Have eight B200 GPUs and the CPU and memory requested by the selected manifests.
- Have a ReadWriteMany storage class for the 2 TiB [model cache](model-cache-pvc.yaml),
  or set `claimName` in the chosen capture and restore manifests to an existing
  Hugging Face cache PVC.
  Set the cache PVC's `storageClassName` if that class is not the default.
- Create the 100 GiB [compiler cache](compiler-cache-pvc.yaml) on RWX storage.
  See [compiler-cache requirements](compiler-cache.md) for cache retention and fixed paths.
- For Deployment capture, install `envsubst` (`gettext-base` on Debian and Ubuntu).

## Multi-GPU models

The large models below are mixture-of-experts (MoE) models. They use TEP8:
tensor parallelism across eight GPUs for attention and expert parallelism across
the same eight GPUs for MoE layers. vLLM sets `tensor_parallel_size=8` and
`enable_expert_parallel=true`, SGLang sets both parallel sizes to 8, and
TensorRT-LLM sets `tensor_parallel_size=8` and `moe_expert_parallel_size=8`.
The dense Qwen3-0.6B recipes remain single-GPU.

Choose a Deployment or SnapshotJob for capture. Each engine shares one
`restore/8-gpu.yaml`, `app.py`, and ConfigMap across its models.

| Model | Engine | Deployment | SnapshotJob | Restore |
| --- | --- | --- | --- | --- |
| GLM 5.3 NVFP4 | vLLM | [Manifest](vllm/capture/glm-5.3.yaml) | [Manifest](vllm/capture/glm-5.3-snapshotjob.yaml) | [Manifest](vllm/restore/8-gpu.yaml) |
| GLM 5.3 NVFP4 | SGLang | [Manifest](sglang/capture/glm-5.3.yaml) | [Manifest](sglang/capture/glm-5.3-snapshotjob.yaml) | [Manifest](sglang/restore/8-gpu.yaml) |
| GLM 5.3 NVFP4 | TensorRT-LLM | [Manifest](tensorrt-llm/capture/glm-5.3.yaml) | [Manifest](tensorrt-llm/capture/glm-5.3-snapshotjob.yaml) | [Manifest](tensorrt-llm/restore/8-gpu.yaml) |
| DeepSeek V4.1 Flash | vLLM | [Manifest](vllm/capture/deepseek-v4.1-flash.yaml) | [Manifest](vllm/capture/deepseek-v4.1-flash-snapshotjob.yaml) | [Manifest](vllm/restore/8-gpu.yaml) |
| DeepSeek V4.1 Flash | SGLang | [Manifest](sglang/capture/deepseek-v4.1-flash.yaml) | [Manifest](sglang/capture/deepseek-v4.1-flash-snapshotjob.yaml) | [Manifest](sglang/restore/8-gpu.yaml) |

## Prepare the workload

Run from the repository root. This example uses vLLM. For another engine, use
its `app.py`, ConfigMap name, and manifests from the table.

```bash
export SNAPSHOT_NAMESPACE=<namespace>

kubectl apply --namespace "$SNAPSHOT_NAMESPACE" \
  --filename docs/guides/model-cache-pvc.yaml

kubectl apply --namespace "$SNAPSHOT_NAMESPACE" \
  --filename docs/guides/compiler-cache-pvc.yaml

kubectl create configmap vllm-app --namespace "$SNAPSHOT_NAMESPACE" \
  --from-file=app.py=docs/guides/vllm/app.py \
  --dry-run=client -o yaml | kubectl apply --filename -
```

Choose one of the following capture paths.

## Capture without SnapshotJob

Set `SNAPSHOT_AGENT_IMAGE` to the digest-pinned image used by the installed
Snapshot agent. The Deployment installs the libraries and launches the workload.

```bash
export SNAPSHOT_AGENT_IMAGE=<registry>/snapshot/agent@sha256:<digest>

envsubst '${SNAPSHOT_AGENT_IMAGE}' < docs/guides/vllm/capture/glm-5.3.yaml | \
  kubectl apply --namespace "$SNAPSHOT_NAMESPACE" --filename -

kubectl rollout status --namespace "$SNAPSHOT_NAMESPACE" \
  deployment/vllm-glm-5-3-source --timeout=60m

kubectl get pods --namespace "$SNAPSHOT_NAMESPACE" \
  --selector app=vllm-glm-5-3-source
```

[Create a PodSnapshot](checkpoint.md#option-1--podsnapshot-checkpoint-a-running-replica)
for the ready source Pod. After the checkpoint is ready, delete the source
Deployment if you need its GPUs for restore.

## Capture with SnapshotJob

The operator installs the libraries, waits for the workload to become ready,
creates the checkpoint, and removes the source Pod.

```bash
kubectl apply --namespace "$SNAPSHOT_NAMESPACE" \
  --filename docs/guides/vllm/capture/glm-5.3-snapshotjob.yaml

kubectl wait --namespace "$SNAPSHOT_NAMESPACE" \
  --for=condition=Completed snapshotjob/vllm-glm-5-3-snapshot --timeout=120m
```

## Restore

In your engine's `restore/8-gpu.yaml`, replace `<snapshot-name>` with the ready
PodSnapshot name. For the vLLM GLM SnapshotJob above, set:

```yaml
nvidia.com/restore-from: vllm-glm-5-3-snapshot
```

Keep the same model-cache PVC populated by capture, then apply the manifest:

```bash
kubectl apply --namespace "$SNAPSHOT_NAMESPACE" \
  --filename docs/guides/vllm/restore/8-gpu.yaml

kubectl rollout status --namespace "$SNAPSHOT_NAMESPACE" \
  deployment/vllm-8gpu-restored --timeout=30m
```

See the [restore guide](restore.md) to verify the restored replica.
