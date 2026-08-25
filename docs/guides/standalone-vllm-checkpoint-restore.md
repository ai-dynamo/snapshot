<!--
SPDX-FileCopyrightText: 2026 NVIDIA CORPORATION & AFFILIATES
SPDX-License-Identifier: Apache-2.0
-->

# Checkpoint and restore vLLM with Snapshot

This guide shows how to quiesce a vLLM engine, capture its Kubernetes pod with
Snapshot, and resume inference in a restored pod.

Snapshot must already be installed in the cluster. The guide only covers the
vLLM workload and the Kubernetes resources needed for capture and restore.

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
CHECKPOINT_ID="vllm-$(kubectl get pod "$SOURCE_POD" \
  --namespace "$NAMESPACE" \
  --output jsonpath='{.metadata.uid}' |
  cut -c1-8)"
VLLM_IMAGE="$(kubectl get pod "$SOURCE_POD" \
  --namespace "$NAMESPACE" \
  --output "jsonpath={.status.containerStatuses[?(@.name=='${CONTAINER}')].imageID}")"
VLLM_IMAGE="${VLLM_IMAGE#*://}"
```

## 1. Add the vLLM lifecycle

vLLM must stop generation and enter sleep mode before capture. After restore,
it must wake up before accepting generation requests.

Sleep mode is opt-in; vLLM does not enable it by default. Enable it through the
Python API or server option below.

### Python API

Create the engine with sleep mode enabled:

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

When the application is ready to be captured, run this lifecycle:

```python
import asyncio
from pathlib import Path

async def checkpoint_lifecycle(engine):
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
            await engine.wake_up()
            await engine.resume_generation()
            control_dir.joinpath("vllm-restore-ready").write_text(
                "ready\n",
                encoding="utf-8",
            )
            return True
        await asyncio.sleep(1)
```

Exit the source process when the function returns `False`. When it returns
`True`, the process was restored and can resume serving requests.

### vLLM server HTTP API

If the pod runs `vllm serve`, enable the administrative endpoints:

```bash
VLLM_SERVER_DEV_MODE=1 vllm serve <model> --enable-sleep-mode
```

Both settings are required: `--enable-sleep-mode` enables the engine feature,
and `VLLM_SERVER_DEV_MODE=1` exposes its administrative endpoints. Do not expose
these endpoints outside a trusted network.

Confirm that the endpoint is available. An active server initially returns
`false`:

```bash
kubectl exec "$SOURCE_POD" \
  --namespace "$NAMESPACE" \
  --container "$CONTAINER" \
  -- curl -fsS "http://127.0.0.1:8000/is_sleeping"
```

Quiesce the server:

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

The second state check must return `true`.

The Python and HTTP options execute the same lifecycle. Use only the option
that matches how the vLLM process is started.

## 2. Prepare the source pod

Add the following fields to the existing vLLM pod template. The labels identify
the checkpoint, the annotation selects the vLLM container, and the control
volume carries the lifecycle files.

```yaml
metadata:
  labels:
    nvidia.com/snapshot-is-checkpoint-source: "true"
    nvidia.com/snapshot-checkpoint-id: <checkpoint-id>
  annotations:
    nvidia.com/snapshot-target-containers: <vllm-container-name>
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

Redeploy the pod, run the quiesce calls from step 1, and wait for the readiness
probe:

```bash
kubectl wait \
  --for=condition=Ready \
  "pod/$SOURCE_POD" \
  --namespace "$NAMESPACE" \
  --timeout=20m
```

## 3. Capture the pod

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
  labels:
    nvidia.com/snapshot-checkpoint-id: ${CHECKPOINT_ID}
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

## 4. Create the restore pod

The restore pod supplies the target container, GPU, and mounts. Snapshot
replaces its inert process with the captured vLLM process.

Use the same image and workload settings as the source pod. This minimal
manifest restores to the source node:

```bash
cat >/tmp/vllm-restore.yaml <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${RESTORE_POD}
  namespace: ${NAMESPACE}
  labels:
    nvidia.com/snapshot-checkpoint-id: ${CHECKPOINT_ID}
    nvidia.com/snapshot-is-restore-target: "true"
  annotations:
    nvidia.com/snapshot-target-containers: ${CONTAINER}
    nvidia.com/snapshot-artifact-version: "1"
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

## 5. Resume vLLM

Wait until Snapshot has restored the process:

```bash
until kubectl exec "$RESTORE_POD" \
  --namespace "$NAMESPACE" \
  --container "$CONTAINER" \
  -- test -f /snapshot-control/restore-complete
do
  sleep 2
done
```

The Python lifecycle from step 1 wakes and resumes the engine automatically.

For a `vllm serve` process, run the HTTP calls:

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
  --output json |
  jq -r --arg container "$CONTAINER" \
    '.metadata.annotations["nvidia.com/snapshot-restore-status." + $container]'
```

The status must be `completed`. Send a normal inference request to the restored
vLLM process to complete the validation.

## Clean up

```bash
kubectl delete pod "$SOURCE_POD" "$RESTORE_POD" \
  --namespace "$NAMESPACE" \
  --ignore-not-found

kubectl delete "podsnapshot/${SOURCE_POD}-snapshot" \
  --namespace "$NAMESPACE" \
  --ignore-not-found
```

Deleting the `PodSnapshot` removes its API record. Retained checkpoint data is
managed by the Snapshot installation's storage policy.

For the vLLM lifecycle APIs, see
[Sleep Mode](https://docs.vllm.ai/en/v0.27.1/features/sleep_mode/) and
[Online Serving](https://docs.vllm.ai/en/v0.27.1/serving/online_serving/).
