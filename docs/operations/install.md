# Installation

Snapshot installs as a single per-cluster Helm release: a control-plane operator
and a privileged node agent (DaemonSet) on GPU nodes. Install it in its own
namespace, and run GPU workloads in separate namespaces.

Review the [prerequisites](../../README.md#prerequisites) before installing.

## Install from a release

Find the latest version on the
[releases page](https://github.com/ai-dynamo/snapshot/releases), then install the
published chart, replacing `<VERSION>`:

```bash
helm install snapshot oci://ghcr.io/ai-dynamo/snapshot/snapshot \
  --version <VERSION> \
  --namespace snapshot --create-namespace
```

By default the chart provisions its own `ReadWriteMany` checkpoint volume, shared
by every checkpoint. See [Storage](storage.md) for the volume model and options,
including reusing an existing claim.

## Verify the installation

```bash
kubectl get pods --namespace snapshot
kubectl rollout status daemonset/snapshot-agent --namespace snapshot
```

The operator and the `snapshot-agent` DaemonSet become ready once the node agent
is running on each GPU node.

## Uninstall

```bash
helm uninstall snapshot --namespace snapshot
```

Chart-created checkpoint volumes are retained on uninstall, so checkpoints survive
removal — see [Storage](storage.md#retention).
