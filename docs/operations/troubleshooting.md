# Troubleshooting

Common issues when running Snapshot, and where to look.

## A checkpoint never becomes Ready

A `PodSnapshot` becomes Ready only after the `snapshot-agent` confirms the
checkpoint contents — writing the checkpoint is not enough on its own. Check the
status and the agent logs:

```bash
kubectl get podsnapshot <name> -n <ns>
kubectl logs daemonset/snapshot-agent -n <ns> --all-containers
```

A common cause is a replica manifest that uses the raw runtime image instead of a
[snapshot-ready image](../guides/README.md), or that omits mounts or secrets the
replica needs to start.

## Restore cannot find or mount checkpoint storage

Restore discovers checkpoint storage through the `snapshot-agent` DaemonSet, which
must be ready and must have the checkpoint PVC available:

```bash
kubectl rollout status daemonset/snapshot-agent -n <ns>
kubectl get pvc -n <ns>
```

## The agent or a restore pod will not start

The `snapshot-agent` runs privileged with `hostPID`, `hostIPC`, and `hostNetwork`.
If the namespace enforces a restrictive Pod Security level, the agent — or a
restore pod — can be rejected. See [Security](security.md).
