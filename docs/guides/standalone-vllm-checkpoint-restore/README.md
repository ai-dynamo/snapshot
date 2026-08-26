<!--
SPDX-FileCopyrightText: 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Checkpoint and restore vLLM with Snapshot

This guide shows how to quiesce a vLLM engine, capture its Kubernetes pod with
Snapshot, and resume inference in a restored pod.

Snapshot must already be installed in the cluster. The guide only covers the
vLLM workload and the Kubernetes resources needed for capture and restore.

Companion examples:

- [snapshot_lifecycle.py](snapshot_lifecycle.py)
- [Source pod fields](vllm-source-pod-fields.yaml)
- [PodSnapshot](vllm-snapshot.yaml)
- [Restore pod](vllm-restore.yaml)

The YAML files are reference templates. The commands below resolve runtime
values such as the source pod UID and node name.

## Prerequisites

- An x86_64 Kubernetes cluster with an NVIDIA GPU node.
- Snapshot installed, including the operator, node agent, and
  `PodSnapshot` and `PodSnapshotContent` CRDs.
- A vLLM pod manifest that you can modify and redeploy.
- `kubectl` access to create pods and `PodSnapshot` resources.
- The same immutable workload image for capture and restore.
- The source GPU node available for restore. This guide restores to the same
  node.

The currently tested configuration uses NVIDIA driver 580 or newer, MIG
disabled, and a workload image compatible with the Snapshot restore utilities.

Use the namespace from the current `kubectl` context, or set `NAMESPACE`
beforehand to override it:

```bash
NAMESPACE="${NAMESPACE:-$(kubectl config view --minify --output 'jsonpath={..namespace}')}"
NAMESPACE="${NAMESPACE:-default}"
kubectl get pods --namespace "$NAMESPACE"
```

Select the vLLM pod. For a pod with sidecars, set `CONTAINER` beforehand to the
vLLM container name; otherwise the first container is used. All other values
are derived:

```bash
SOURCE_POD="<vllm-pod-name>"
CONTAINER="${CONTAINER:-$(kubectl get pod "$SOURCE_POD" \
  --namespace "$NAMESPACE" \
  --output jsonpath='{.spec.containers[0].name}')}"
RESTORE_POD="${SOURCE_POD}-restore"
VLLM_IMAGE="$(kubectl get pod "$SOURCE_POD" \
  --namespace "$NAMESPACE" \
  --output "jsonpath={.status.containerStatuses[?(@.name=='${CONTAINER}')].imageID}")"
VLLM_IMAGE="${VLLM_IMAGE#*://}"
```

## 1. Configure the vLLM lifecycle

vLLM must stop generation and enter sleep mode before capture. After restore,
it must wake up before accepting generation requests.

This command reads the startup command of the selected container's main
process. It does not change the pod. The output identifies which vLLM interface
the pod uses:

```bash
kubectl exec "$SOURCE_POD" \
  --namespace "$NAMESPACE" \
  --container "$CONTAINER" \
  -- sh -c 'tr "\000" " " < /proc/1/cmdline; echo'
```

- If the output contains `vllm serve` or `vllm.entrypoints`, use the
  [HTTP API](#vllm-server-http-api).
- If the output names a Python program that your team maintains, use the
  [Python API](#python-api).
- If the output is a shell wrapper or remains unclear, check the workload
  manifest or ask the image owner which program starts vLLM.

### Python API

Use this option only when your team can edit the Python program shown by the
command above. This is a source-code change; do not paste the Python into a
terminal or edit files inside the running pod.

In the application source repository, find the Python entrypoint that creates
the `AsyncLLM` engine and enable sleep mode:

```python
engine_args = AsyncEngineArgs(
    model=model,
    enable_sleep_mode=True,
)
engine = AsyncLLM.from_engine_args(
    engine_args,
    usage_context=UsageContext.LLM_CLASS,
)
```

`enable_sleep_mode=True` enables the feature. The application writes
`ready-for-snapshot` only after `await engine.sleep()` succeeds, so capture
cannot start if sleep mode is unavailable.

`snapshot_lifecycle.py` does not create the snapshot. It adapts vLLM to
Snapshot's file-based lifecycle: make the engine safe to capture, wait for a
capture or restore result, and make a restored engine ready to serve.

Create the file next to the application entrypoint. Add the capture phase:

```python
import asyncio
from pathlib import Path

async def quiesce_for_snapshot(engine):
    control_dir = Path("/snapshot-control")

    await engine.pause_generation()
    await engine.sleep()
    control_dir.joinpath("ready-for-snapshot").write_text(
        "ready\n",
        encoding="utf-8",
    )

    while True:
        if control_dir.joinpath("snapshot-complete").exists():
            return False
        if control_dir.joinpath("restore-complete").exists():
            return True
        await asyncio.sleep(1)
```

The capture calls run in this order:

- `pause_generation()` stops new generation work from changing engine state.
- `sleep()` uses vLLM's default level 1 sleep: model weights move to CPU memory
  and the KV cache is discarded, releasing most GPU memory.
- `ready-for-snapshot` tells the pod readiness probe that both calls completed.
  Snapshot does not begin capture before the pod is ready.
- The loop keeps the process alive at the captured execution point.
  `snapshot-complete` identifies the original source process;
  `restore-complete` identifies the restored process.
- `asyncio.sleep(1)` prevents the wait loop from continuously using CPU.

Add the restore phase to the same file:

```python
from pathlib import Path

async def resume_after_restore(engine):
    await engine.wake_up()
    await engine.resume_generation()
    Path("/snapshot-control/vllm-restore-ready").write_text(
        "ready\n",
        encoding="utf-8",
    )
```

The restore calls reverse the vLLM-specific preparation:

- `wake_up()` returns the sleeping model weights to GPU memory.
- `resume_generation()` allows the scheduler to process generation requests.
- `vllm-restore-ready` tells the restored pod's readiness probe that vLLM can
  serve requests again.

Import the functions in the existing entrypoint and call them immediately
after engine initialization and warm-up:

```python
from snapshot_lifecycle import quiesce_for_snapshot, resume_after_restore

restored = await quiesce_for_snapshot(engine)
if not restored:
    return
await resume_after_restore(engine)
```

The source process waits in `quiesce_for_snapshot()` and exits after
`snapshot-complete`. The restored process resumes from that same wait loop,
sees `restore-complete`, and runs `resume_after_restore()`. The application's
existing request-serving code should follow this block, so only the restored
process reaches it.

Build a new workload image containing these source changes and use that image
when [preparing the source pod](#2-prepare-the-source-pod). The code runs when
the container starts the application. The readiness check in
[step 3](#3-quiesce-vllm) confirms when it can be captured.

### vLLM server HTTP API

If the pod runs `vllm serve`, enable the administrative endpoints:

```bash
VLLM_SERVER_DEV_MODE=1 vllm serve <model> --enable-sleep-mode
```

Both settings are required: `--enable-sleep-mode` enables the engine feature,
and `VLLM_SERVER_DEV_MODE=1` exposes its administrative endpoints. Do not expose
these endpoints outside a trusted network. Apply these settings to the pod
startup command when [preparing the source pod](#2-prepare-the-source-pod).

The [Python](#python-api) and [HTTP](#vllm-server-http-api) options execute the
same lifecycle. Use only the option that matches how the vLLM process is
started.

## 2. Prepare the source pod

Add the following fields to the existing vLLM pod template. The label identifies
the pod as a capture source, and the control volume carries the lifecycle files.

```yaml
metadata:
  labels:
    nvidia.com/snapshot-is-checkpoint-source: "true"
spec:
  runtimeClassName: nvidia
  securityContext:
    seccompProfile:
      type: Localhost
      localhostProfile: profiles/block-iouring.json
  containers:
  - name: <vllm-container-name>
    env:
    - name: SNAPSHOT_CONTROL_DIR
      value: /snapshot-control
    readinessProbe:
      exec:
        command:
        - cat
        - /snapshot-control/ready-for-snapshot
      periodSeconds: 1
      failureThreshold: 1200
    volumeMounts:
    - name: snapshot-control
      mountPath: /snapshot-control
    - name: tun
      mountPath: /dev/net/tun
  volumes:
  - name: snapshot-control
    emptyDir: {}
  - name: tun
    hostPath:
      path: /dev/net/tun
      type: CharDevice
```

Redeploy the pod with the lifecycle configuration from step 1 and the pod
fields above. Wait until its container is running:

```bash
kubectl wait \
  --for=jsonpath='{.status.phase}'=Running \
  "pod/$SOURCE_POD" \
  --namespace "$NAMESPACE" \
  --timeout=20m
```

## 3. Quiesce vLLM

For the [Python API](#python-api), the application calls
`quiesce_for_snapshot()` after initialization and writes the readiness file
without another command.

For the [HTTP API](#vllm-server-http-api), wait for the server endpoint:

```bash
until kubectl exec "$SOURCE_POD" \
  --namespace "$NAMESPACE" \
  --container "$CONTAINER" \
  -- curl -fsS "http://127.0.0.1:8000/is_sleeping"
do
  sleep 2
done
```

An active server returns `false`. Pause generation, enter sleep mode, confirm
the new state, and write the readiness file:

```bash
kubectl exec "$SOURCE_POD" \
  --namespace "$NAMESPACE" \
  --container "$CONTAINER" \
  -- curl -fsS -X POST "http://127.0.0.1:8000/pause"

kubectl exec "$SOURCE_POD" \
  --namespace "$NAMESPACE" \
  --container "$CONTAINER" \
  -- curl -fsS -X POST "http://127.0.0.1:8000/sleep?level=1"

kubectl exec "$SOURCE_POD" \
  --namespace "$NAMESPACE" \
  --container "$CONTAINER" \
  -- curl -fsS "http://127.0.0.1:8000/is_sleeping"

kubectl exec "$SOURCE_POD" \
  --namespace "$NAMESPACE" \
  --container "$CONTAINER" \
  -- sh -c 'printf "ready\n" > /snapshot-control/ready-for-snapshot'
```

The state check must return `true`. For either API, wait for the pod readiness
probe and confirm the file:

```bash
kubectl wait \
  --for=condition=Ready \
  "pod/$SOURCE_POD" \
  --namespace "$NAMESPACE" \
  --timeout=20m

kubectl exec "$SOURCE_POD" \
  --namespace "$NAMESPACE" \
  --container "$CONTAINER" \
  -- cat /snapshot-control/ready-for-snapshot
```

The final command must print `ready`. The pod is now ready for capture.

## 4. Capture the pod

A `PodSnapshot` is the capture request. Snapshot creates the corresponding
`PodSnapshotContent`, which records the captured artifact. Do not create the
`PodSnapshotContent` yourself.

Create the capture request using the live pod UID:

```bash
cat >/tmp/vllm-snapshot.yaml <<EOF
apiVersion: nvidia.com/v1alpha1
kind: PodSnapshot
metadata:
  name: ${SOURCE_POD}-snapshot
  namespace: ${NAMESPACE}
spec:
  source:
    podRef:
      name: ${SOURCE_POD}
      uid: $(kubectl get pod "$SOURCE_POD" \
        --namespace "$NAMESPACE" \
        --output jsonpath='{.metadata.uid}')
      containers:
      - ${CONTAINER}
EOF

kubectl apply -f /tmp/vllm-snapshot.yaml
```

Wait for capture to finish:

```bash
kubectl wait \
  --for=condition=Ready \
  "podsnapshot/${SOURCE_POD}-snapshot" \
  --namespace "$NAMESPACE" \
  --timeout=20m
```

Confirm that the request is ready and has bound content:

```bash
kubectl get "podsnapshot/${SOURCE_POD}-snapshot" \
  --namespace "$NAMESPACE" \
  --output jsonpath='{.status.conditions[?(@.type=="Ready")].status}{"\n"}{.status.boundSnapshotContentName}{"\n"}'
```

The command must print `True` followed by a
`podsnapshotcontent-<identifier>` name.

## 5. Create the restore pod

The restore pod supplies the target container, GPU, and mounts. Snapshot
replaces its inert process with the captured vLLM process.

Use the same image and workload settings as the source pod. This minimal
manifest restores to the source node. The `nvidia.com/restore-from` annotation
references the `PodSnapshot`:

```bash
cat >/tmp/vllm-restore.yaml <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${RESTORE_POD}
  namespace: ${NAMESPACE}
  annotations:
    nvidia.com/restore-from: ${SOURCE_POD}-snapshot
spec:
  restartPolicy: Never
  runtimeClassName: nvidia
  nodeName: $(kubectl get pod "$SOURCE_POD" \
    --namespace "$NAMESPACE" \
    --output jsonpath='{.spec.nodeName}')
  securityContext:
    seccompProfile:
      type: Localhost
      localhostProfile: profiles/block-iouring.json
  containers:
  - name: ${CONTAINER}
    image: ${VLLM_IMAGE}
    imagePullPolicy: IfNotPresent
    command: ["/bin/bash", "-lc", "sleep infinity"]
    env:
    - name: SNAPSHOT_CONTROL_DIR
      value: /snapshot-control
    resources:
      limits:
        nvidia.com/gpu: "1"
    startupProbe:
      exec:
        command:
        - cat
        - /snapshot-control/restore-complete
      periodSeconds: 1
      failureThreshold: 1200
    readinessProbe:
      exec:
        command:
        - cat
        - /snapshot-control/vllm-restore-ready
      periodSeconds: 1
      failureThreshold: 1200
    volumeMounts:
    - name: snapshot-control
      mountPath: /snapshot-control
    - name: tun
      mountPath: /dev/net/tun
  volumes:
  - name: snapshot-control
    emptyDir: {}
  - name: tun
    hostPath:
      path: /dev/net/tun
      type: CharDevice
EOF
```

Create the manifest before deleting the source pod so its node name is still
available. Then release the source GPU and create the restore target:

```bash
kubectl delete pod "$SOURCE_POD" \
  --namespace "$NAMESPACE" \
  --wait=true

kubectl apply -f /tmp/vllm-restore.yaml
```

## 6. Resume vLLM

Wait until Snapshot has restored the process:

```bash
kubectl wait \
  --for=condition=snapshot/Restored=True \
  "pod/$RESTORE_POD" \
  --namespace "$NAMESPACE" \
  --timeout=20m

until kubectl exec "$RESTORE_POD" \
  --namespace "$NAMESPACE" \
  --container "$CONTAINER" \
  -- test -f /snapshot-control/restore-complete
do
  sleep 2
done
```

### Resume with the Python API

The restored Python process continues from `quiesce_for_snapshot()` and calls
`resume_after_restore()` automatically. That function wakes the engine, resumes
generation, and writes `vllm-restore-ready`. No additional command is needed.

### Resume with the HTTP API

For a restored `vllm serve` process, wake the engine, resume generation, and
write the readiness file:

```bash
kubectl exec "$RESTORE_POD" \
  --namespace "$NAMESPACE" \
  --container "$CONTAINER" \
  -- curl -fsS -X POST "http://127.0.0.1:8000/wake_up"

kubectl exec "$RESTORE_POD" \
  --namespace "$NAMESPACE" \
  --container "$CONTAINER" \
  -- curl -fsS -X POST "http://127.0.0.1:8000/resume"

kubectl exec "$RESTORE_POD" \
  --namespace "$NAMESPACE" \
  --container "$CONTAINER" \
  -- sh -c 'printf "ready\n" > /snapshot-control/vllm-restore-ready'
```

Wait for the restored pod:

```bash
kubectl wait \
  --for=condition=Ready \
  "pod/$RESTORE_POD" \
  --namespace "$NAMESPACE" \
  --timeout=20m
```

Confirm the restore status:

```bash
kubectl get pod "$RESTORE_POD" \
  --namespace "$NAMESPACE" \
  --output jsonpath='{range .status.conditions[?(@.type=="snapshot/Restored")]}{.status}{"\t"}{.reason}{"\n"}{end}'
```

The command must print `True` and `RestoreSucceeded`. Send a normal inference
request to the restored vLLM process to complete the validation.

For the vLLM lifecycle APIs, see
[Sleep Mode](https://docs.vllm.ai/en/v0.27.1/features/sleep_mode/) and
[Online Serving](https://docs.vllm.ai/en/v0.27.1/serving/online_serving/).
