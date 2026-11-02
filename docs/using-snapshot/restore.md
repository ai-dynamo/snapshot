# Restore a Pod

Restoring starts a new replica from a snapshot instead of cold-starting it. The
restored pods carry the `nvidia.com/restore-from` annotation, naming the
`PodSnapshot` to restore from; the node agent restores the checkpointed state into
the container during pod startup.

This is stage 3 of [Using Snapshot](README.md): restore a new pod
from the snapshot. It starts from the ready
`PodSnapshot` that [checkpoint](checkpoint.md) produced:

```bash
kubectl get podsnapshot --namespace "$WORKLOAD_NAMESPACE"
```

Run the commands from [Choose Your Recipe](README.md#choose-your-recipe) first, in
the same terminal. The commands on this page use the values they set.

## How the Restore Works

Snapshot replaces the container's command with its restore proxy, as the
[restore Pod contract](../reference/restore-pod-contract.md) describes, so the
application does not start from scratch. The proxy waits for the restore, then
follows the restored process: the container ends when the restored process
does. Readiness waits on the framework's post-restore sentinel —
`vllm-restore-ready` and its equivalents — not the `ready-for-snapshot` file the
source pod writes.

## Choose the Restore Profile

A restore profile is a Deployment manifest for the restored pod. Applying it
creates the restored pod.

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

## Set the Snapshot to Restore From

Every profile carries the annotation `nvidia.com/restore-from: ${SNAPSHOT_NAME}`,
a placeholder for the `PodSnapshot` to restore from.

Open your profile (`$RESTORE_MANIFEST`) and replace `${SNAPSHOT_NAME}` with your
recipe's snapshot name, then save the file. `echo "$SNAPSHOT_NAME"` prints it, for
example `vllm-qwen3-0-6b-snapshot`.

Whichever method you used, that is the
`SnapshotJob`'s name, which its `PodSnapshot` shares, or the name you gave the
`PodSnapshot` in
[Checkpoint, Option 1](checkpoint.md#option-1--podsnapshot-checkpoint-a-running-pod).

On nodes without NRI v0.10 or newer, there is no restore proxy. Before applying
the profile, edit it to change the container's command to
`["sleep", "infinity"]`, remove its `args`, and add a liveness probe on the
framework's API. See
[Nodes Without the Restore Proxy](../reference/restore-pod-contract.md#nodes-without-the-restore-proxy).

## Restore

Apply the profile you edited, then watch the rollout:

```bash
kubectl apply \
  --namespace "$WORKLOAD_NAMESPACE" \
  --filename "$RESTORE_MANIFEST"

kubectl rollout status \
  --namespace "$WORKLOAD_NAMESPACE" \
  deployment/"$RESTORED_NAME" \
  --timeout="$WAIT"
```

## Verify the Restored Pod

The node agent adds a `nvidia.com/Restored` condition to the pod once the restore
completes — watch it, along with pod readiness, to confirm:

```bash
kubectl get pods --namespace "$WORKLOAD_NAMESPACE" \
  --selector app="$RESTORED_NAME" \
  --output jsonpath='{.items[0].status.conditions[?(@.type=="nvidia.com/Restored")].status}'
```

It prints `True`. If the restore does not complete, see
[Troubleshoot Restores](../troubleshooting/restores.md).

In practice, higher-level systems create these restored Deployments rather than
applying them by hand. To generate restore pods programmatically — from a
controller, operator, or serving platform — implement the
[restore Pod contract](../reference/restore-pod-contract.md), which specifies the
required annotations, control volume, and `SNAPSHOT_CONTROL_DIR`, plus the
optional startup gate and seccomp profile a restored pod may carry.

## One Restore per Pod

A Pod gets one restore. The supported frameworks use a Deployment, which restarts containers,
so if the restored process exits, the container ends in `CrashLoopBackOff`
instead of starting again. Delete the Pod to restore a new replica.
