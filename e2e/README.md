# Snapshot E2E

This directory contains the Python helpers and pytest tests used to run Snapshot
end-to-end checks against a Kubernetes cluster.

## Requirements

- `uv`
- `kubectl` and `helm`
- For CI/vCluster mode: `vcluster`
- A GPU Kubernetes cluster with `RuntimeClass/nvidia`, GPU Operator `26.3.0+`,
  MIG disabled on the target GPU nodes, CUDA driver `580+`, and a storage class
  that can provision `ReadWriteMany` volumes

Set `SNAPSHOT_E2E_STORAGE_CLASS` when the cluster default cannot provision RWX
claims. The AKS workflow uses `azurefile-csi`; local runs default to the
cluster's default storage class.

## Modes

### CI Mode

The GitHub workflow creates a temporary vCluster, installs the Snapshot chart
there, runs the environment check, then runs the snapshot lifecycle tests.

The workflow resolves the latest published Snapshot operator/agent image tag and
passes it through `SNAPSHOT_E2E_SNAPSHOT_TAG`.

### Local Direct Mode

Use direct mode when `KUBECONFIG` already points at the cluster where Snapshot
should be installed and tested.

Direct mode uses the real cluster, so an existing Snapshot Helm release with the
same release name must either be reused or uninstalled first. The chart owns
cluster-scoped RBAC names such as `snapshot-operator`, and Helm cannot import
those from a release in another namespace.

```bash
export SNAPSHOT_E2E_MODE=direct
export SNAPSHOT_E2E_TEST_NAMESPACE=snapshot-e2e
export SNAPSHOT_E2E_SNAPSHOT_TAG=<published-snapshot-tag>
unset SNAPSHOT_E2E_TARGET_KUBECONFIG

uv run --project e2e python -m snapshot_e2e.infra.setup --phase host-preflight
uv run --project e2e python -m snapshot_e2e.infra.setup --phase snapshot-install
uv run --project e2e python -m snapshot_e2e.infra.setup --phase snapshot-ready

uv run --project e2e pytest e2e/tests -m environment -vv
uv run --project e2e pytest e2e/tests/test_snapshot_lifecycle.py -vv -s
uv run --project e2e pytest e2e/tests/test_snapshotjob.py -vv -s
```

When finished, uninstall the Snapshot release and delete the checkpoint PVC:

```bash
uv run --project e2e python -m snapshot_e2e.infra.setup --phase snapshot-uninstall
```

The uninstall phase leaves the namespace in place. Delete it explicitly if it is
only used for this e2e run.

### Local vCluster Mode

Use vCluster mode to reproduce the CI layout from your own kubeconfig. Keep the
host kubeconfig and generated vCluster kubeconfig separate: the host kubeconfig
is used to create/connect to the vCluster, and `SNAPSHOT_E2E_TARGET_KUBECONFIG`
is the generated kubeconfig used by pytest.

```bash
export SNAPSHOT_E2E_MODE=vcluster
export SNAPSHOT_E2E_HOST_KUBECONFIG="$HOME/.kube/config"
export KUBECONFIG="$SNAPSHOT_E2E_HOST_KUBECONFIG"
export SNAPSHOT_E2E_HOST_NAMESPACE=snapshot-e2e-manual-$(date +%s)
export SNAPSHOT_E2E_VCLUSTER_NAME="$SNAPSHOT_E2E_HOST_NAMESPACE"
export SNAPSHOT_E2E_TEST_NAMESPACE=snapshot-e2e
export SNAPSHOT_E2E_TARGET_KUBECONFIG="$(mktemp -t snapshot-e2e-kubeconfig.XXXXXX)"
export SNAPSHOT_E2E_SNAPSHOT_TAG=<published-snapshot-tag>

uv run --project e2e python -m snapshot_e2e.infra.setup --phase host-preflight
uv run --project e2e python -m snapshot_e2e.infra.setup --phase vcluster
uv run --project e2e python -m snapshot_e2e.infra.setup --phase snapshot-install
uv run --project e2e python -m snapshot_e2e.infra.setup --phase snapshot-ready

KUBECONFIG="$SNAPSHOT_E2E_TARGET_KUBECONFIG" kubectl get pods -n "$SNAPSHOT_E2E_TEST_NAMESPACE"

export KUBECONFIG="$SNAPSHOT_E2E_TARGET_KUBECONFIG"
uv run --project e2e pytest e2e/tests -m environment -vv
uv run --project e2e pytest e2e/tests/test_snapshot_lifecycle.py -vv -s
uv run --project e2e pytest e2e/tests/test_snapshotjob.py -vv -s
```

If the generated target kubeconfig is missing or points at the host context,
regenerate it from the host kubeconfig:

```bash
export KUBECONFIG="$SNAPSHOT_E2E_HOST_KUBECONFIG"
kubectl port-forward \
  -n "$SNAPSHOT_E2E_HOST_NAMESPACE" \
  "svc/$SNAPSHOT_E2E_VCLUSTER_NAME" \
  8443:443 \
  > .snapshot-e2e-vcluster-port-forward.log 2>&1 &

vcluster connect "$SNAPSHOT_E2E_VCLUSTER_NAME" \
  --namespace "$SNAPSHOT_E2E_HOST_NAMESPACE" \
  --server https://127.0.0.1:8443 \
  --print > "$SNAPSHOT_E2E_TARGET_KUBECONFIG"
chmod 0600 "$SNAPSHOT_E2E_TARGET_KUBECONFIG"
```

When finished with a local vCluster run, clean up explicitly:

```bash
uv run --project e2e python -m snapshot_e2e.infra.setup --phase snapshot-uninstall

export KUBECONFIG="$SNAPSHOT_E2E_HOST_KUBECONFIG"
helm uninstall vcluster-hpm -n "$SNAPSHOT_E2E_HOST_NAMESPACE" --ignore-not-found
vcluster delete "$SNAPSHOT_E2E_VCLUSTER_NAME" -n "$SNAPSHOT_E2E_HOST_NAMESPACE"
kubectl delete namespace "$SNAPSHOT_E2E_HOST_NAMESPACE" --ignore-not-found
rm -f "$SNAPSHOT_E2E_TARGET_KUBECONFIG"
```

## Restore Verification

The success tests prove restore with explicit source and restore state tokens.
The source pod starts with a source token and stores it in three places:

- CPU memory, as the worker process' in-memory token.
- Filesystem state, in `/tmp/e2e-state/file-token`.
- GPU memory, for GPU tests, in a small CUDA device allocation.

The restore pod starts with a different restore token. After restore completes,
the test verifies that the restored process and files report the source token,
not the restore token. The worker also appends periodic observations to
`/tmp/e2e-state/observations.log`; the observation count is only a liveness
check that the restored process continues running after restore.

## Framework Tests

`tests/test_frameworks.py` checkpoints and restores each framework guide
workload (`vllm`, `sglang`, `tensorrt-llm`) with the guide's own program and
manifests: source pod Ready (`ready-for-snapshot`, written only after a
pre-capture generation) → `PodSnapshot` → restore pod pinned to the source node
→ `nvidia.com/Restored=RestoreSucceeded` → `<framework>-restore-ready` →
`POST /generate` answers → the placeholder never loaded a model itself.

```bash
# one framework (CI runs one per matrix job); omit the variable for all three
SNAPSHOT_E2E_FRAMEWORK=vllm \
  uv run --project e2e pytest e2e/tests/test_frameworks.py -vv -s

# test a different image instead of the guide's own pinned image
SNAPSHOT_E2E_FRAMEWORK=vllm SNAPSHOT_E2E_FRAMEWORK_IMAGE=<registry>/vllm-snapshot:dev \
  uv run --project e2e pytest e2e/tests/test_frameworks.py -vv -s
```

Model weights come from one of two places:

- **Shared model cache** (CI): set `SNAPSHOT_E2E_MODEL_CACHE_SERVER` and
  `SNAPSHOT_E2E_MODEL_CACHE_PATH` to an NFS export holding a Hugging Face cache
  in `HF_HOME` layout (`hub/models--<org>--<model>/...`). The test creates a
  static `PersistentVolume`/`PersistentVolumeClaim` (`SNAPSHOT_E2E_MODEL_CACHE_PVC`,
  default `model-cache`), mounts it at `/models` on every framework container,
  sets `HF_HOME=/models` and `HF_HUB_OFFLINE=1`, and drops the guide's download
  init container. In vCluster mode the setup enables `sync.toHost.persistentVolumes`
  so the NFS mount options reach the node. The model must already be in the cache.
- **Guide download** (default without the variables): the guide's own plumbing
  runs unchanged. SGLang's init container downloads into its PVC, which the test
  creates from the guide manifest if missing (with `SNAPSHOT_E2E_STORAGE_CLASS`
  when set) and leaves in place; vLLM and TensorRT-LLM download in-process.
  This needs working DNS and egress from the pods. A partial or stale SGLang
  cache (for example after a killed run) is reset by deleting that PVC; the
  next run recreates and refills it.

`tests/test_framework_manifests.py` pins the guide manifests, and the cache
rewrite, to the restore-pod contract without a cluster.

### Framework benchmark results

Every selected framework test prints a benchmark summary and writes one
versioned JSON result. CI uploads the JSON as a 30-day artifact even when the
functional test fails. Set `SNAPSHOT_E2E_BENCHMARK_DIR` to choose the local
output directory; without it, results go under the system temporary directory.

The required durations use these stable boundaries:

- `checkpoint.duration`: immediately before the `PodSnapshot` create request
  until both `PodSnapshot` and its bound `PodSnapshotContent` report Ready.
- `restore.to_traffic.duration`: the Snapshot agent's timestamp for the
  `RestoreRequested` event until the framework writes its restore-ready
  sentinel, which happens after its API is listening and its first
  post-restore generation completes.
- `restore.pod_create_to_traffic.duration`: immediately before restore pod
  creation until the same traffic-ready sentinel. This secondary measurement
  includes scheduling and target discovery.
- `test.total.duration` (`Full E2E test` in the console): entry into the test
  body through restored inference verification and the no-cold-start
  assertion. Failure diagnostics, fixture cleanup, and post-test benchmark
  metadata collection are excluded.

The same result separates system work from test-observation overhead with the
agent's structured durations. `checkpoint.agent.duration` and
`restore.agent.duration` are the agent totals; every reported agent phase is
also emitted as `<operation>.<phase>.duration`, including
`checkpoint.criu_dump.duration` and `restore.criu_restore.duration`.
`restore.agent_complete_to_traffic.duration` shows framework wake-up time after
the agent completes. Restore success and the traffic sentinel are watched in
one one-second polling loop. The sentinel check execs into the restore pod, so
it starts only after the `nvidia.com/Restored` condition reports
`RestoreSucceeded`: the agent restores the checkpointed process tree with its
original PIDs, and an exec session in that PID namespace during the restore
could collide with one of them. The one-second poll bounds the delay this adds.

`source.image_pull.duration` and `restore.image_pull.duration` come from the
kubelet's `Pulled` events. The companion `*_including_wait.duration` includes
kubelet queueing. A content-addressed image already present on the node is
recorded as a zero-second cache hit in `environment.imagePulls`, rather than as
a network pull.

Most durations use the test runner's monotonic clock. The restore start is
backdated once from the Kubernetes event timestamp to remove event-observation
delay, and the agent-to-traffic gap compares the agent log timestamp with the
observed traffic-ready timestamp. Both cross the boundary between the cluster's
clock and the runner's clock. The agent stamps its events with a microsecond
`eventTime`, and the `restore.requested` event in the result records
`observationDelaySeconds`, the signed gap between the agent's timestamp and the
runner's observation; a negative value means the cluster clock is ahead of the
runner, and the console summary warns when the gap is negative or larger than
a few seconds. The result also keeps the underlying events, test outcome,
source revision, framework image tag and resolved digest, model, cache mode,
and storage metadata (CSI provisioner, storage class, requested size, bound
capacity, access modes, and volume mode). Source and restore GPU model, UUID,
driver, and node are recorded separately because restore may receive a
different physical GPU; the node's `nvidia.com/gpu.product` label is recorded
too and stands in for the model when `nvidia-smi` is unavailable. Missing
boundaries remain explicit incomplete measurements and are never serialized as
zero.

Outcomes are `passed`, `failed`, `timed_out`, `skipped`, and
`infrastructure_failed`. A lifecycle wait that exhausts its budget, or a pytest
run interrupted by the workflow's timeout, is `timed_out`; other assertion
failures are `failed`; a run that never produced a pytest call report is
`infrastructure_failed`. The durable `error.message` holds only the final
exception line. Full tracebacks stay in the pytest log and the short-lived
diagnostics artifact because results are retained outside the cluster.

`snapshot_e2e.benchmark.BenchmarkSession` is intentionally independent of the
framework schema: another E2E suite can name its own case, events, and duration
measurements while producing the same result envelope.

### Benchmark history and comparison

After the matrix finishes, one CPU-only job downloads all selected framework
results and compares every completed measurement with the previous compatible
successful run and the median of the previous seven. The comparison, including
GPU, storage, image-cache state, commit, outcome, and workflow links, is written
to the GitHub Actions step summary and uploaded as another 30-day artifact.
Failed and timed-out results remain visible but do not contribute values to a
baseline. A missing or invalid matrix artifact becomes an explicit
`infrastructure_failed` result instead of disappearing from history. The
aggregation reads the artifacts of every attempt of the workflow run and keeps
the newest attempt per framework, so "Re-run failed jobs" does not turn the
frameworks that already passed into missing artifacts. A synthesized result
takes its Snapshot tag from a sibling framework result of the same run when the
aggregation job itself does not know it.

Only the scheduled workflow on `main` can publish. Pull-request mirror,
ordinary branch, and manually dispatched runs use a separate read-only job and
cannot mutate durable history. Both jobs check the history branch out through
the `.github/actions/fetch-benchmark-history` composite action, which uses the
job's own token so it works for private repositories and GitHub Enterprise
hosts. The publisher serializes updates, commits all frameworks from one
workflow in one commit, and bootstraps the data-only orphan branch
`e2e-benchmark-history` on its first successful invocation.

After a pull-request mirror or manual run completes, the trusted dashboard
workflow validates its comparison artifact and publishes a 14-day preview.
Pull requests use the stable path `/previews/pr-<number>/`; other manual runs
use `/previews/run-<workflow-run-id>/`. Each preview combines that run with a
read-only copy of nightly history. Generated preview indexes are isolated on
the `e2e-benchmark-previews` branch and never enter `e2e-benchmark-history`.
The dashboard deployment summary links to the resulting Pages URL.

Raw results are the source of truth. They are stored under:

```text
results/v1/<suite>/<case>/<test>/<year>/<month>/<day>/<run-id>-<attempt>.json
```

Derived monthly indexes live at `index/v1/<year>-<month>.ndjson`; the small
`index/manifest.json` lists chunks newest-first. Every index line carries the
raw path, the result, and a `comparisonKey`: the canonical JSON of the
comparison dimensions below, so readers group compatible results by that
string instead of re-deriving the rules. Identity is the GitHub run ID,
attempt, suite, case, and test, so publishing the same attempt again recognizes
the immutable record instead of duplicating it.

Compatible baselines have the same suite, case, test, schema and benchmark
versions, measurement unit, GPU models, framework image (the resolved digest
when the test recorded one, otherwise the tag), model, storage configuration,
model-cache mode, image-cache state, and GPU-monitoring mode. GPU UUID, node,
Snapshot commit, and Snapshot image tag stay diagnostic: they do not split the
baseline. Future suites can add stable suite-specific values under
`environment.comparisonDimensions`.

The monthly indexes and manifest can be reconstructed from a checked-out
history branch without modifying any raw result:

```bash
PYTHONPATH=e2e python3 -m snapshot_e2e.benchmark_history rebuild \
  --history-dir /path/to/e2e-benchmark-history
```

After inspecting the diff, commit and push the rebuilt `index/` directory. If
the history branch is lost, restore the raw `results/` tree from a backup or
retained workflow artifacts and run the same rebuild command; indexes alone
cannot reconstruct the raw records.

### Benchmark dashboard

The static dashboard source lives under `dashboard/`. It graphs the latest 90
days by default, exposes suite, case, GPU, outcome, channel, and measurement
filters, and compares completed points with the previous compatible run and
seven-run median. Failed and timed-out measurements remain visible as explicit
gaps instead of zero values.

The `E2E Benchmark Dashboard` workflow builds the TypeScript application from
`main` and combines it with the data-only `e2e-benchmark-history` branch plus
any unexpired pre-merge previews. A completed framework run triggers a new
Pages deployment without merging generated benchmark data into `main`. See
[`dashboard/README.md`](../dashboard/README.md) for local development and the
one-time GitHub Pages configuration.

## Framework Images

The framework e2e workloads are the programs and manifests under
`manifests/frameworks/<framework>/` (`vllm`, `sglang`, `tensorrt-llm`), owned
by the e2e suite -- these are not the `docs/guides/` examples, which still
document a build-and-push image flow and are updated separately. Each
framework runs the upstream image unmodified -- the exact image reference is
`spec.template.spec.containers[0].image` in that framework's own
`deployment.yaml` -- with `app.py` mounted from a ConfigMap (`kubectl create
configmap <framework>-app --from-file=app.py -n
"${SNAPSHOT_E2E_TEST_NAMESPACE:-snapshot-e2e}"`) rather than baked into a
Snapshot-built image. There is nothing under `manifests/frameworks/<framework>/`
for Snapshot to build, push, or keep available; `frameworks.framework_image()`
reads the image straight from that `deployment.yaml`, and
`framework_workloads.app_configmap()` builds the ConfigMap from the same
`app.py`.

Point `SNAPSHOT_E2E_FRAMEWORK_IMAGE` at a different image to test an
unpublished change (a fork of `vllm/vllm-openai`, for example) without
editing `deployment.yaml`.
