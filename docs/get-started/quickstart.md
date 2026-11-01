<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Quickstart: Checkpoint and Restore One Pod

This quickstart checkpoints a vLLM replica that serves Qwen3 0.6B on one GPU,
then starts a new replica from the snapshot. The replica runs the stock vLLM
image, plus a small program, `app.py`, mounted from a ConfigMap. The program
prepares vLLM for the checkpoint and resumes it after the restore. It uses the
smallest recipe from [Choose Your Recipe](../guides/README.md#choose-your-recipe):
vLLM, Qwen3 0.6B, 1 GPU.

Before you start, install Snapshot. See [Installation](installation.md).

## 1. Set Up

Download the example files. They live in the `docs/guides` folder of the
Snapshot repository. Clone it and open that folder: every command in this
quickstart runs from there.

```bash
git clone https://github.com/ai-dynamo/snapshot.git
cd snapshot/docs/guides
```

Pick the namespace for the workload, and create it if it does not exist:

```bash
export WORKLOAD_NAMESPACE=<namespace>
kubectl get namespace "$WORKLOAD_NAMESPACE" || kubectl create namespace "$WORKLOAD_NAMESPACE"
```

Create the model cache, a `ReadWriteMany` PVC that both replicas read the model
from. The first run downloads about 1.5 GB from Hugging Face, so the pods need
access to `huggingface.co`. If your default storage class does not support
`ReadWriteMany`, add `storageClassName` to `model-cache-pvc.yaml` first.

```bash
kubectl apply --namespace "$WORKLOAD_NAMESPACE" --filename model-cache-pvc.yaml
```

## 2. Prepare the Workload

Create the ConfigMap that holds `app.py`:

```bash
kubectl create configmap vllm-app \
  --namespace "$WORKLOAD_NAMESPACE" \
  --from-file=app.py=vllm/app.py
```

## 3. Checkpoint

Create a `SnapshotJob`. Snapshot starts the replica, waits until it is ready,
checkpoints it, and removes it:

```bash
kubectl apply --namespace "$WORKLOAD_NAMESPACE" --filename vllm/capture/qwen3-0.6b-snapshotjob.yaml

kubectl wait --namespace "$WORKLOAD_NAMESPACE" \
  --for=condition=Completed snapshotjob/vllm-qwen3-0-6b-snapshot \
  --timeout=30m
```

## 4. Restore

The snapshot is a `PodSnapshot` named `vllm-qwen3-0-6b-snapshot`. Start a new
replica from the snapshot. Its pod carries the `nvidia.com/restore-from`
annotation that names the snapshot:

```bash
sed 's/${SNAPSHOT_NAME}/vllm-qwen3-0-6b-snapshot/' vllm/restore/1-gpu.yaml \
  | kubectl apply --namespace "$WORKLOAD_NAMESPACE" --filename -

kubectl rollout status --namespace "$WORKLOAD_NAMESPACE" \
  deployment/vllm-1gpu-restored --timeout=30m
```

Confirm that the replica was restored, not started from scratch. The command
prints `True`:

```bash
kubectl get pods --namespace "$WORKLOAD_NAMESPACE" \
  --selector app=vllm-1gpu-restored \
  --output jsonpath='{.items[0].status.conditions[?(@.type=="nvidia.com/Restored")].status}'
```

## Next Steps

- Try other models and frameworks in [Checkpoint and Restore](../guides/README.md).
- Use your own framework or image: see
  [Use Your Own Framework or Image](../integrate/use-your-own-framework.md).
