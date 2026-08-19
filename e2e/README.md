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
