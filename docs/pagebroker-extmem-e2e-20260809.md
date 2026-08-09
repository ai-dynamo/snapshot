# PageBroker external-memory 120B restore: reproducible state

Date: 2026-08-09

This document preserves the exact state that produced a real PageBroker-backed
120B restore. It is an archive/runbook, not a claim that the changes are ready
to merge upstream.

## What passed

The fresh checkpoint `fresh-gpt120b-agentcuda-20260809t1545z` was restored on
the selected target with PageBroker enabled. This was the external-memory path:

- PageBroker observed `INIT=1`, `OPEN_IMAGE=602`, `GET_VMA=4776`,
  `GET_SHARED=307`, `WAIT_READY=1`, then `COMMIT=1`.
- CRIU core restore / resume: `6.607118031s`.
- CRIU restore wall: `6.613394948s`.
- Provider-side `WAIT_READY`: `4.763248s`. The separate Snapshot readiness
  control request took only `347.29us` because the eager fills had already
  completed when CRIU reached pre-resume.
- Agent-owned CUDA restore, after CRIU resumed: `11.973771109s`.
- The elapsed time from CRIU launch through CUDA completion was about 18.7 s.
  The current `Restore timing summary.restore.duration` is deliberately only
  the pre-CUDA CRIU portion (`6.662606894s`), so do not call that end-to-end.
- `/health` returned 200. After `POST /wake_up`, completions returned
  `restored`, and after the post-restore checkpoint returned `still alive`.
- A direct checkpoint of the restored target succeeded:
  `fresh-gpt120b-agentcuda-pagebroker-afterrestore-20260809t1653z`, bound to
  `podsnapshotcontent-c8a2aa73-5237-4adf-919c-f47d86de1d24` with
  `Ready=True:Captured` and `Failed=False:Captured`.

No CRIU CUDA plugin was loaded. CUDA restore was owned by the Snapshot agent
after CRIU resume. No `_mm_stream_ps` crash appeared.

## Why the earlier ~40 s result was not PageBroker

The stock CRIU image silently ignored the PageBroker socket. Its successful
restore log contained 310 `Read piov iovs` calls totaling 122.38 GiB between
0.185 s and 30.828 s. PageBroker had no provider requests in that run.

The first custom-CRIU image was also invalid: it had been compiled with host
objects requiring `GLIBC_2.38`; CRIU executes inside the Ubuntu 22.04 vLLM
mount namespace, so its loader failed before the first provider request. The
custom image must compile in Ubuntu 22.04 *and run `make clean` first* so stale
host-built objects cannot be reused.

## Git state to recover

Only these repositories are part of this archive:

| Repository | Archive branch | Base before this archive |
| --- | --- | --- |
| `/home/dfeigin/Work/checkpoints/snapshot` | `archive/pagebroker-extmem-e2e-20260809` | `d20e86743f1b94e055b3b61e626e443016723182` |
| `/home/dfeigin/Work/checkpoints/criu-upstream` | `archive/pagebroker-extmem-e2e-20260809` | `b7a28e147b3094145f8e79c1763930d235498c58` |

The separate `/home/dfeigin/Work/checkpoints/criu` checkout was not changed.

The Snapshot archive includes the agent-owned CUDA path, PageBroker manifest
and range materialization, inherited-FD/swrk plumbing, restore timing/log
preservation, PageBroker deferred-fill test support, CRDs/chart/controller
changes, and this document. The CRIU archive includes the external-memory
provider and additional agent-CUDA/profiling work present in the dirty source.
The deployed flow did not load the CRIU CUDA plugin; retain that distinction
when splitting this archive later.

Generated files deliberately not committed:

- `snapshot/nsrestore` (55 MiB local Go build output)
- `snapshot/agent/pagebroker/pagebroker-compat` (112 KiB generated binary)
- ignored CRIU object files and binaries

Regenerate them with the build commands below. All source files, protocol
definitions, tests, and the `disable-binfmt-sandbox.patch` are committed.

## CRIU changes that make the data path real

1. `images/extmem.proto` defines `INIT`, `OPEN_IMAGE`, `GET_VMA`,
   `GET_SHARED`, `WAIT_READY`, `COMMIT`, and `ABORT`.
2. `criu/extmem.c` resolves the inherited socket key
   `0-extmem-provider`, exchanges those messages, accepts memfds through
   `SCM_RIGHTS`, validates them, and owns readiness/commit/abort.
3. `prepare_extmem_vmas()` in `criu/mem.c` asks the provider for every
   applicable anonymous-private VMA, marks it `VMA_EXT_PROVIDER`, and avoids
   treating it as ordinary anonymous restore input.
4. The normal private-page restore path skips pages for provider-backed VMAs;
   this is the change that removes stock CRIU's `Read piov iovs` page reads.
5. `open_vmas()` maps the provider memfd. `criu/shmem.c` and
   `criu/memfd.c` take the same path for shared-memory / memfd objects.
6. `criu/image.c` lets the provider open ordinary CRIU images. CRIU calls
   `extmem_wait_ready()` just before pre-resume and commits or aborts at the
   end of restore.

The Snapshot side passes one connected socket to CRIU under that exact key;
it does not use a special protobuf RPC field, fd-256 relocation, or fd padding.
The `criu swrk 3` transport reserves fd 3, and inherited resources follow it.

## Immutable image used for the successful run

Registry tag:

```text
nvcr.io/nvidian/dynamo-dev/agent:agent-pagebroker-extmem-ubuntu22-clean-20260809t1650z
```

Immutable OCI index and amd64 manifest:

```text
index: sha256:0588fa333aabf9f91f81087a9212e5c5fad31f43c9f76c9b168529350a8a643f
amd64: sha256:978e818cdd4637cb6403095970dc9ffc340b80d0463f830e4d05ac68423f0b50
```

Build it from the Snapshot checkout, with the archived CRIU checkout adjacent:

```bash
cd /home/dfeigin/Work/checkpoints/snapshot
docker buildx build --platform linux/amd64 --push \
  --build-context api=./api \
  --build-context criu-local=../criu-upstream \
  --target agent-extmem \
  -t nvcr.io/nvidian/dynamo-dev/agent:agent-pagebroker-extmem-ubuntu22-clean-20260809t1650z \
  -f agent/Dockerfile agent/
```

`agent-extmem` is intentionally a local diagnostic target. The normal `agent`
target remains on its configured upstream CRIU. The added local builder runs:

```Dockerfile
FROM ubuntu:22.04 AS criu-local-builder
COPY --from=criu-local . /tmp/criu
RUN cd /tmp/criu && make clean && make -j$(nproc) \
    && make DESTDIR=/criu-local-install install-criu install-lib
```

Verify the compatibility property before deployment:

```bash
docker run --rm --entrypoint sh \
  nvcr.io/nvidian/dynamo-dev/agent@sha256:978e818cdd4637cb6403095970dc9ffc340b80d0463f830e4d05ac68423f0b50 \
  -c 'objdump -T /usr/local/sbin/criu | grep GLIBC_2.38 && exit 1 || true; criu --version'
```

## Cluster state at archive time

Always use this context explicitly:

```text
nv-prd-dgxc.teleport.sh-dynamo-nscale-dev-cluster
```

Namespace: `runai-test`.

| Role | Pod / node | State |
| --- | --- | --- |
| live pinned checkpoint source | `vllm-test-frmhp` on `cluster-0967a26d-pool-14bee067-prctr-xmhbj` | Running; never restarted or moved |
| source agent | `snapshot-standalone-cp-agent-8s2q5` on `...-xmhbj` | left running on its existing agent image |
| PageBroker restore target | `fresh-gpt120b-agentcuda-pagebroker-restore` on `cluster-0967a26d-pool-14bee067-prctr-hx78c` | Running/Ready |
| restore-node agent | `snapshot-standalone-cp-agent-66jxd` on `...-hx78c` | rotated to the immutable amd64 image above |

DaemonSet `snapshot-standalone-cp-agent` uses `OnDelete`. Its template was set
to the amd64 digest for both `agent` and `pagebroker`; only the restore-node
DaemonSet pod was deleted, so no other agent was rotated. To reproduce that
scope, do not use a rolling-update strategy:

```bash
CTX=nv-prd-dgxc.teleport.sh-dynamo-nscale-dev-cluster
NS=runai-test
IMG=nvcr.io/nvidian/dynamo-dev/agent@sha256:978e818cdd4637cb6403095970dc9ffc340b80d0463f830e4d05ac68423f0b50

kubectl --context="$CTX" -n "$NS" set image daemonset/snapshot-standalone-cp-agent \
  agent="$IMG" pagebroker="$IMG"
kubectl --context="$CTX" -n "$NS" get pods -o wide
# Identify only the agent pod on ...-hx78c, then delete that exact pod.
kubectl --context="$CTX" -n "$NS" delete pod snapshot-standalone-cp-agent-66jxd
```

The fresh source checkpoint is pinned to this source UID and container:

```text
Pod:        vllm-test-frmhp
UID:        67cece0a-3c84-4854-ad8c-69bde4dadc6f
Container:  vllm
Node:       cluster-0967a26d-pool-14bee067-prctr-xmhbj
Checkpoint: fresh-gpt120b-agentcuda-20260809t1545z
Content:    podsnapshotcontent-aeeb312b-5e56-41e9-904e-5c61ec2dcb41
```

For a new checkpoint, allocate a new name and re-read the live UID; never copy
the UID above after recreating the source pod.

```yaml
apiVersion: nvidia.com/v1alpha1
kind: PodSnapshot
metadata:
  name: <new-checkpoint-id>
  namespace: runai-test
  labels:
    nvidia.com/snapshot-checkpoint-id: <new-checkpoint-id>
spec:
  source:
    podRef:
      name: vllm-test-frmhp
      uid: <live-uid-from-kubectl>
      containers: [vllm]
```

## Restore and validation sequence

1. Create a fresh restore target pinned to `...-hx78c`, using the placeholder
   image `nvcr.io/nvidian/dynamo-dev/snapshot-placeholder@sha256:f4fd614340087d2451b95fae14603a7c97d08bcc1221b6aba9d0193f6bd4799c`.
   It needs the `snapshot-pvc-big`, `shared-model-cache`, `/dev/shm` tmpfs
   (16 GiB), `/snapshot-control` emptyDir, `runtimeClassName: nvidia`, one GPU,
   and the normal block-iouring seccomp profile.
2. Set these restore annotations:

   ```text
   nvidia.com/snapshot-is-restore-target: "true"                 # label
   nvidia.com/snapshot-checkpoint-id: <checkpoint-id>             # label
   nvidia.com/snapshot-artifact-version: "1"
   nvidia.com/snapshot-checkpoint-location: /checkpoints/<checkpoint-id>
   nvidia.com/snapshot-checkpoint-storage-type: pvc
   nvidia.com/snapshot-target-containers: vllm
   nvidia.com/snapshot-pagebroker: "true"
   ```

3. Keep `PAGEBROKER_DEFER_FILL` unset for the successful eager run. The
   manifest was `images=1206`, `host_memory_objects=2695`,
   `resident_bytes=136922976256`, `tasks=2694`, and `workers=32`.
4. Inspect only the relevant evidence:

   ```bash
   kubectl --context="$CTX" -n "$NS" logs <restore-node-agent> -c agent --since=10m | \
     rg 'Prepared CRIU|PageBroker host memory|Restoring CUDA|Restore timing'
   kubectl --context="$CTX" -n "$NS" logs <restore-node-agent> -c pagebroker --since=10m | \
     rg 'provider request|WAIT_READY|COMMIT|provider requests|provider timing'
   ```

   Required proof is exactly one CUDA owner in Snapshot logs, real provider
   requests including `GET_VMA` and `GET_SHARED`, `WAIT_READY`, `COMMIT`, no
   CRIU CUDA-plugin activity, and no `_mm_stream_ps` crash.

5. Validate service and inference. The restored vLLM is intentionally in sleep
   mode, so wake it before the first inference:

   ```bash
   kubectl --context="$CTX" -n "$NS" port-forward pod/<restore-pod> 18000:8000
   curl --fail http://127.0.0.1:18000/health
   curl --fail http://127.0.0.1:18000/v1/models
   curl --fail -X POST http://127.0.0.1:18000/wake_up
   curl --fail http://127.0.0.1:18000/is_sleeping
   curl --fail -H 'Content-Type: application/json' \
     -d '{"model":"openai/gpt-oss-120b","prompt":"Reply with exactly: restored","max_tokens":4,"temperature":0}' \
     http://127.0.0.1:18000/v1/completions
   ```

6. Create a direct `PodSnapshot` against the currently restored target to test
   checkpoint-after-restore. Read the target UID immediately before applying;
   a same-named recreated pod has a different UID.

   ```yaml
   apiVersion: nvidia.com/v1alpha1
   kind: PodSnapshot
   metadata:
     name: <new-afterrestore-checkpoint-id>
     namespace: runai-test
     labels:
       nvidia.com/snapshot-checkpoint-id: <new-afterrestore-checkpoint-id>
   spec:
     source:
       podRef:
         name: <restore-pod>
         uid: <live-restore-pod-uid>
         containers: [vllm]
   ```

   Require `Ready=True:Captured`, `Failed=False:Captured`, then repeat health
   and inference. Do not infer checkpoint-after-restore success from a merely
   Running restore pod.

### Exact successful restore-target manifest

The name, checkpoint ID, node, and Pod UID are deliberately run-specific.
For a replay, use a new pod name/checkpoint ID and retain the rest unless the
cluster's PVC names or seccomp profile changed.

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: fresh-gpt120b-agentcuda-pagebroker-restore
  namespace: runai-test
  labels:
    app: vllm-fresh-gpt120b-agentcuda
    nvidia.com/snapshot-is-restore-target: "true"
    nvidia.com/snapshot-checkpoint-id: fresh-gpt120b-agentcuda-20260809t1545z
  annotations:
    nvidia.com/snapshot-artifact-version: "1"
    nvidia.com/snapshot-checkpoint-location: /checkpoints/fresh-gpt120b-agentcuda-20260809t1545z
    nvidia.com/snapshot-checkpoint-storage-type: pvc
    nvidia.com/snapshot-target-containers: vllm
    nvidia.com/snapshot-pagebroker: "true"
spec:
  restartPolicy: Never
  runtimeClassName: nvidia
  nodeName: cluster-0967a26d-pool-14bee067-prctr-hx78c
  terminationGracePeriodSeconds: 0
  tolerations:
  - key: nvidia.com/gpu
    operator: Exists
    effect: NoSchedule
  imagePullSecrets:
  - name: ngc-secret
  securityContext:
    seccompProfile:
      type: Localhost
      localhostProfile: profiles/block-iouring.json
  containers:
  - name: vllm
    image: nvcr.io/nvidian/dynamo-dev/snapshot-placeholder@sha256:f4fd614340087d2451b95fae14603a7c97d08bcc1221b6aba9d0193f6bd4799c
    imagePullPolicy: Always
    command: ["sleep", "infinity"]
    env:
    - name: HOME
      value: /tmp
    - name: HF_HOME
      value: /tmp/hf_home
    - name: HF_HUB_CACHE
      value: /tmp/hf_home/hub
    - name: VLLM_CACHE_ROOT
      value: /tmp/hf_home/vllm-cache
    - name: DYN_SNAPSHOT_CONTROL_DIR
      value: /snapshot-control
    readinessProbe:
      httpGet: {path: /health, port: 8000}
      initialDelaySeconds: 1
      periodSeconds: 2
      failureThreshold: 900
    startupProbe:
      exec: {command: ["cat", "/snapshot-control/restore-complete"]}
      periodSeconds: 1
      failureThreshold: 1800
    resources:
      limits: {cpu: "10", nvidia.com/gpu: "1"}
      requests: {cpu: "10", nvidia.com/gpu: "1"}
    volumeMounts:
    - {name: model-cache, mountPath: /tmp/hf_home}
    - {name: checkpoint-storage, mountPath: /checkpoints}
    - {name: devshm, mountPath: /dev/shm}
    - {name: snapshot-control, mountPath: /snapshot-control}
  volumes:
  - name: model-cache
    persistentVolumeClaim: {claimName: shared-model-cache}
  - name: checkpoint-storage
    persistentVolumeClaim: {claimName: snapshot-pvc-big}
  - name: devshm
    emptyDir: {medium: Memory, sizeLimit: 16Gi}
  - name: snapshot-control
    emptyDir: {}
```

## Local validation performed before the live run

```bash
cd /home/dfeigin/Work/checkpoints/snapshot/agent/pagebroker
make clean test

cd /home/dfeigin/Work/checkpoints/snapshot
go test ./agent/internal/criu ./agent/internal/executor ./agent/internal/pagebroker
git diff --check
```

The successful image build above also compiled the archived CRIU source from a
clean Ubuntu 22.04 build tree. Do not claim a different image or local build as
the successful 120B E2E result without repeating the cluster checks.

## Known scope and caveats

- This is an agent-owned CUDA restore path. CRIU CUDA plugin code may exist in
  the archived CRIU worktree, but it is not configured or loaded at runtime.
- PageBroker's reported `GET_VMA` and `GET_SHARED` byte totals are requested
  mapping lengths, not bytes read from storage. The manifest resident-byte
  total is the meaningful host-memory data amount.
- The restored target was allowed to run before CUDA restore completed; CUDA
  restore then unlocks the restored CUDA process tree.
- The DaemonSet template now names the custom image. Because strategy is
  `OnDelete`, another node changes only if its pod is explicitly deleted.
- Preserve CRIU failure logs before retrying: a failed old restore log can be
  stale when a new CRIU process dies before creating a new work-directory log.
