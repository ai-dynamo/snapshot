# Snapshot Helm Chart

> Experimental feature. The agent runs as a privileged DaemonSet to perform
> CRIU checkpoint and restore operations.

This chart installs Snapshot: the operator, the node agent, the snapshot volume,
and the RBAC and seccomp profile they need.

For installation, configuration, upgrade, and the full list of Helm values, see
the [Snapshot documentation](../../docs/README.md).

- [Installation](../../docs/get-started/installation.md)
- [Helm Chart Values](../../docs/reference/helm-values.md)
- [Upgrade](../../docs/operate/upgrade.md)

## Notice and disclaimer

Installing this chart causes your cluster to retrieve container images that are
not distributed with the chart, including the `busybox` image used as a
short-lived init container to write the seccomp profile (overridable via
`daemonset.initContainer.image`).

> **NOTICE AND DISCLAIMER:** This software automatically retrieves, accesses or
> interacts with external materials. Those retrieved materials are not
> distributed with this software and are governed solely by separate terms,
> conditions and licenses. You are solely responsible for finding, reviewing and
> complying with all applicable terms, conditions, and licenses, and for
> verifying the security, integrity and suitability of any retrieved materials
> for your specific use case. This software is provided "AS IS", without
> warranty of any kind. The author makes no representations or warranties
> regarding any retrieved materials, and assumes no liability for any losses,
> damages, liabilities or legal consequences from your use or inability to use
> this software or any retrieved materials. Use this software and the retrieved
> materials at your own risk.

Materials this chart causes to be retrieved:

| Image | Default reference | License | Retrieved from |
|---|---|---|---|
| Snapshot operator | `ghcr.io/ai-dynamo/snapshot/operator` | Apache-2.0 (NVIDIA) | GHCR |
| Snapshot agent | `ghcr.io/ai-dynamo/snapshot/agent` | Apache-2.0 (NVIDIA) | GHCR |
| Snapshot PageBroker | `ghcr.io/ai-dynamo/snapshot/pagebroker` | Apache-2.0 (NVIDIA) | GHCR |
| busybox init container | `busybox:1.37.0` (digest-pinned) | GPL-2.0 | Docker Hub |

Third-party attribution and corresponding source for the NVIDIA images are
shipped inside those images under `/legal/`.
