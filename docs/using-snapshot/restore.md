# Restore a Pod

Restoring starts a new replica from a snapshot instead of cold-starting it. The
restored pods carry the `nvidia.com/restore-from` annotation, which names the
`PodSnapshot` to restore from. During pod startup, the node agent restores the
checkpointed state into the container.

## Before You Start

- You need a Ready `PodSnapshot`, which [Checkpoint a Pod](checkpoint.md) produces
  with either method. Check that it exists:

  ```bash
  kubectl get podsnapshot --namespace "$WORKLOAD_NAMESPACE"
  ```

- Run the commands from [Choose Your Recipe](README.md#choose-your-recipe) in the
  same terminal. The commands on this page use the values they set.

## How the Restore Works

Snapshot replaces the container's command with its restore proxy, as the
[Restore Pod Contract](../reference/restore-pod-contract.md) describes, so the
application does not start from scratch. The proxy waits for the restore, then
follows the restored process. The container ends when the restored process does.

The restored pod also becomes Ready differently from the source pod. The source pod
is Ready when the workload writes the `ready-for-snapshot` file. The restored pod is
Ready when the framework writes its post-restore file, such as `vllm-restore-ready`
for vLLM. Each framework page lists its own under After the Restore:
[vLLM](supported-frameworks/vllm.md#after-the-restore),
[SGLang](supported-frameworks/sglang.md#after-the-restore) and
[TensorRT-LLM](supported-frameworks/tensorrt-llm.md#after-the-restore).

## Choose the Restore Profile

A restore profile is a Deployment manifest that creates the restored pod. It keeps
the source pod's image, resources, node selection, volumes and security settings. It
leaves out what only the source pod needs: the init containers (the model download
and, for eight GPUs, the copy of Snapshot's multi-GPU libraries), the container
arguments and engine settings, and the multi-GPU launcher and annotation.

Each framework ships two profiles under `restore/`, one for each GPU count. Your
recipe already chose one: `$RESTORE_MANIFEST`, set in
[Set the Names](README.md#set-the-names) from your `FRAMEWORK` and `GPUS`. The table
lists all of them:

| Framework | 1 GPU | 8 GPUs |
|-----------|-------|--------|
| vLLM | [1-gpu.yaml](vllm/restore/1-gpu.yaml) | [8-gpu.yaml](vllm/restore/8-gpu.yaml) |
| SGLang | [1-gpu.yaml](sglang/restore/1-gpu.yaml) | [8-gpu.yaml](sglang/restore/8-gpu.yaml) |
| TensorRT-LLM | [1-gpu.yaml](tensorrt-llm/restore/1-gpu.yaml) | [8-gpu.yaml](tensorrt-llm/restore/8-gpu.yaml) |

A profile works for every model of its framework and GPU count, because the
model configuration comes from the snapshot. It also works after either
checkpoint method, `PodSnapshot` or `SnapshotJob`. Keep the container `image` the
same as the source's.

## Set the Snapshot to Restore From

Every profile carries the annotation `nvidia.com/restore-from: ${SNAPSHOT_NAME}`,
a placeholder for the `PodSnapshot` to restore from.

Open your profile (`$RESTORE_MANIFEST`) and replace `${SNAPSHOT_NAME}` with your
recipe's snapshot name, then save the file. `echo "$SNAPSHOT_NAME"` prints it, for
example `vllm-qwen3-0-6b-snapshot`.

`$SNAPSHOT_NAME` is the name of the `PodSnapshot` you made when you checkpointed,
whichever method you used:

- **`SnapshotJob`:** the `PodSnapshot` has the same name as the job
- **`PodSnapshot`:** it is the name you gave it in
  [Checkpoint, Option 1](checkpoint.md#option-1---podsnapshot-checkpoint-a-running-pod)

### Nodes Without the Restore Proxy

On nodes without NRI v0.10 or newer, there is no restore proxy. Before applying the
profile, edit it:

- Change the container's command to `["sleep", "infinity"]`
- Remove its `args`
- Add a liveness probe on the framework's API

For details, see the
[Restore Pod Contract](../reference/restore-pod-contract.md#nodes-without-the-restore-proxy).

## Restore

1. Apply the profile you edited:

   ```bash
   kubectl apply \
     --namespace "$WORKLOAD_NAMESPACE" \
     --filename "$RESTORE_MANIFEST"
   ```

2. Wait for the rollout to finish:

   ```bash
   kubectl rollout status \
     --namespace "$WORKLOAD_NAMESPACE" \
     deployment/"$RESTORED_NAME" \
     --timeout="$WAIT"
   ```

## Verify the Restored Pod

The node agent adds a `nvidia.com/Restored` condition to the pod once the restore
completes. Watch it, along with pod readiness, to confirm:

```bash
kubectl get pods --namespace "$WORKLOAD_NAMESPACE" \
  --selector app="$RESTORED_NAME" \
  --output jsonpath='{.items[0].status.conditions[?(@.type=="nvidia.com/Restored")].status}'
```

It prints `True`. If the restore does not complete, see
[Troubleshoot Restores](../troubleshooting/restores.md).

## One Restore per Pod

A pod gets one restore. The supported frameworks use a Deployment, which restarts
containers, so if the restored process exits, the container ends in
`CrashLoopBackOff` instead of starting again. Delete the pod to restore a new
replica.

## Generate Restore Pods Programmatically

In practice, higher-level systems create these restored Deployments rather than
applying them by hand. To generate restore pods from a controller, operator or
serving platform, implement the
[Restore Pod Contract](../reference/restore-pod-contract.md). It specifies the
required annotations, control volume and `SNAPSHOT_CONTROL_DIR`, plus the optional
startup gate and seccomp profile a restored pod may carry.
