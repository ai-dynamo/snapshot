# Restore a replica

Restoring starts a new replica from a snapshot instead of cold-starting it. The
restored pods carry the `nvidia.com/restore-from` annotation, naming the
`PodSnapshot` to restore from; the node agent restores the checkpointed state into
the container during pod startup.

This is stage 3 of the [usage guides](README.md). It starts from the ready
`PodSnapshot` that [checkpoint](checkpoint.md) produced:

```bash
kubectl get podsnapshot --namespace "$SNAPSHOT_NAMESPACE"
```

## 1. Choose the restore profile

The restored replica reuses the source's snapshot-ready pod spec. Each framework
ships two restore profiles under `restore/`. Choose the one that matches the
source's GPU count:

| Framework | 1 GPU | 8 GPUs |
|-----------|-------|--------|
| vLLM | [1-gpu.yaml](vllm/restore/1-gpu.yaml) | [8-gpu.yaml](vllm/restore/8-gpu.yaml) |
| SGLang | [1-gpu.yaml](sglang/restore/1-gpu.yaml) | [8-gpu.yaml](sglang/restore/8-gpu.yaml) |
| TensorRT-LLM | [1-gpu.yaml](tensorrt-llm/restore/1-gpu.yaml) | [8-gpu.yaml](tensorrt-llm/restore/8-gpu.yaml) |

A profile works for every model of its framework and GPU count, because the
model configuration comes from the checkpoint. It also works for captures made
with or without `SnapshotJob`. Keep the container `image` the same as the
source's.

## 2. Set the snapshot to restore from

Every profile carries the annotation `nvidia.com/restore-from: ${SNAPSHOT_NAME}`,
naming the `PodSnapshot` to restore from. Set the variable to your recipe's
snapshot name from the [recipe table](README.md#pick-a-recipe). Whichever method you
used, that is the `SnapshotJob`'s name, which its `PodSnapshot` shares, or the name
you gave the `PodSnapshot` in
[Checkpoint, Option 1](checkpoint.md#option-1--podsnapshot-checkpoint-a-running-replica).
If you named your `PodSnapshot` something else, use that name instead.

```bash
export SNAPSHOT_NAME=vllm-qwen3-0-6b-snapshot
```

## 3. Restore

Fill the name into the profile and apply it, then watch the rollout. The Deployment
is named `<framework>-1gpu-restored` or `<framework>-8gpu-restored`, as the
[recipe table](README.md#pick-a-recipe) lists:

```bash
envsubst '${SNAPSHOT_NAME}' < vllm/restore/1-gpu.yaml | \
  kubectl apply --namespace "$SNAPSHOT_NAMESPACE" --filename -

kubectl rollout status \
  --namespace "$SNAPSHOT_NAMESPACE" \
  deployment/vllm-1gpu-restored \
  --timeout=30m
```

Snapshot replaces the container's command with its restore proxy, as the
[restore Pod contract](../reference/restore-pod-contract.md) describes, so the
application does not start from scratch. The proxy waits for the restore, then
follows the restored process: the container ends when the restored process
does. Readiness waits on the framework's post-restore sentinel —
`vllm-restore-ready` and its equivalents — not the `ready-for-snapshot` file the
source pod writes.

On nodes without NRI v0.10 or newer, there is no restore proxy. Before applying
the profile, edit it to change the container's command to
`["sleep", "infinity"]`, remove its `args`, and add a liveness probe on the
framework's API. See
[Nodes without the restore proxy](../reference/restore-pod-contract.md#nodes-without-the-restore-proxy).

A Pod gets one restore. The guides use a Deployment, which restarts containers,
so if the restored process exits, the container ends in `CrashLoopBackOff`
instead of starting again. Delete the Pod to restore a new replica.

## 4. Verify the restored replica

The node agent adds a `nvidia.com/Restored` condition to the pod once the restore
completes — watch it, along with pod readiness, to confirm. Sending a request is a
good end-to-end check that the workload resumed correctly. Forward the API port of
your profile's Deployment, then send a prompt from a second terminal:

```bash
kubectl port-forward \
  --namespace "$SNAPSHOT_NAMESPACE" \
  deployment/vllm-1gpu-restored 8000:8000
```

```bash
curl --silent --request POST http://localhost:8000/generate \
  --header 'Content-Type: application/json' \
  --data '{"prompt": "What is the capital of Italy?"}'
```

The restored process resumed from the checkpointed state, skipping model loading
and warm-up, so it answers with generated text in a `text` field.

To run another recipe or repeat this one, first
[clean up](README.md#clean-up-and-run-again). If the restore does not complete, see
[Troubleshooting](../operations/troubleshooting.md).

In practice, higher-level systems create these restored Deployments rather than
applying them by hand. To generate restore pods programmatically — from a
controller, operator, or serving platform — implement the
[restore Pod contract](../reference/restore-pod-contract.md), which specifies the
required annotations, control volume, and `SNAPSHOT_CONTROL_DIR`, plus the
optional startup gate and seccomp profile a restored pod may carry.
