# Installation

Snapshot installs as a single per-cluster Helm release: a control-plane operator
and a privileged node agent (DaemonSet) on GPU nodes. Install it in its own
namespace, and run GPU workloads in separate namespaces.

Review the [prerequisites](../../README.md#prerequisites) before installing.

## Install from a release

Install the published chart (see the
[releases page](https://github.com/ai-dynamo/snapshot/releases) for other versions):

```bash
helm install snapshot oci://ghcr.io/ai-dynamo/snapshot/snapshot \
  --version 0.1.0 \
  --namespace snapshot --create-namespace
```

By default the chart provisions its own `ReadWriteMany` checkpoint volume, shared
by every checkpoint. See [Storage](storage.md) for the volume model and options,
including reusing an existing claim.

Each agent pod runs two containers: the agent and the PageBroker sidecar, which
stages checkpoint data in memory and publishes it to the shared volume. The pod
requests 3 CPU and 3Gi of memory per GPU node by default, and its memory limits
decide the largest checkpoint it can take. See
[PageBroker staging](storage.md#pagebroker-staging) for how to size it.

## Verify the installation

```bash
kubectl get pods --namespace snapshot
kubectl rollout status daemonset/snapshot-agent --namespace snapshot
```

The operator and the `snapshot-agent` DaemonSet become ready once the node agent
and its PageBroker sidecar are running on each GPU node. Each agent pod reports
`2/2` containers ready.

## Uninstall

```bash
helm uninstall snapshot --namespace snapshot
```

Chart-created checkpoint volumes are retained on uninstall, so checkpoints survive
removal — see [Storage](storage.md#retention).
