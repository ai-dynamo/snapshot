# Snapshot E2E

This directory contains the Python helpers and pytest tests used to run Snapshot
end-to-end checks against a Kubernetes cluster.

## Requirements

- `uv`
- `kubectl` and `helm`
- For CI/vCluster mode: `vcluster`
- A GPU Kubernetes cluster with `RuntimeClass/nvidia`, GPU Operator `26.3.0+`,
  MIG disabled on the target GPU nodes, and CUDA driver `580+`

## CI Mode

The GitHub workflow creates a temporary vCluster, installs the Snapshot chart
there, runs the environment check, then runs the snapshot lifecycle tests.

The workflow resolves the latest published Snapshot operator/agent image tag and
passes it through `SNAPSHOT_E2E_SNAPSHOT_TAG`.

## Restore Verification

The success tests use a small long-running source process instead of a one-shot
command. Before taking the snapshot, the test waits until the source has appended
at least a few `tick N` lines to `/tmp/tick.log`. After restore, it checks that
the restored tick value is at least the pre-snapshot value, then waits and checks
that the counter continues to increase.

The GPU variant uses the same counter logic and also mirrors the next tick value
inside CUDA device memory. The restored process reads the CUDA value on each loop
and exits if it no longer matches the restored CPU-side counter, so a successful
post-restore counter advance also exercises CUDA memory restore.

## Local Direct Mode

Use direct mode when `KUBECONFIG` already points at the cluster where Snapshot
should be installed and tested.

```bash
export SNAPSHOT_E2E_MODE=direct
export SNAPSHOT_E2E_TEST_NAMESPACE=snapshot-e2e
export SNAPSHOT_E2E_SNAPSHOT_TAG=<published-snapshot-tag>

uv run --project e2e python -m snapshot_e2e.infra.setup --phase host-preflight
uv run --project e2e python -m snapshot_e2e.infra.setup --phase snapshot-install
uv run --project e2e python -m snapshot_e2e.infra.setup --phase snapshot-ready

uv run --project e2e pytest e2e/tests -m environment -vv
uv run --project e2e pytest e2e/tests/test_snapshot_lifecycle.py -vv -s
```

## Local vCluster Mode

Use vCluster mode to reproduce the CI layout from your own kubeconfig.

```bash
export SNAPSHOT_E2E_MODE=vcluster
export SNAPSHOT_E2E_HOST_NAMESPACE=snapshot-e2e-manual-$(date +%s)
export SNAPSHOT_E2E_VCLUSTER_NAME="$SNAPSHOT_E2E_HOST_NAMESPACE"
export SNAPSHOT_E2E_TEST_NAMESPACE=snapshot-e2e
export SNAPSHOT_E2E_TARGET_KUBECONFIG="$PWD/.kubeconfig-snapshot-e2e"
export SNAPSHOT_E2E_SNAPSHOT_TAG=<published-snapshot-tag>

uv run --project e2e python -m snapshot_e2e.infra.setup --phase host-preflight
uv run --project e2e python -m snapshot_e2e.infra.setup --phase vcluster
uv run --project e2e python -m snapshot_e2e.infra.setup --phase snapshot-install
uv run --project e2e python -m snapshot_e2e.infra.setup --phase snapshot-ready

export KUBECONFIG="$SNAPSHOT_E2E_TARGET_KUBECONFIG"
uv run --project e2e pytest e2e/tests -m environment -vv
uv run --project e2e pytest e2e/tests/test_snapshot_lifecycle.py -vv -s
```
