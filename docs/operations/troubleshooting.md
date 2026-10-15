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

A common cause is a replica manifest that does not follow the [framework
guides](../guides/README.md) -- missing the control volume, seccomp profile,
or readiness gate Snapshot relies on -- or that omits mounts or secrets the
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

## CRIU reports `Unable to create tun`

`Error (criu/tun.c:85): tun: Unable to create tun: No such file or directory`
comes from a nonfatal TUN capability probe and can appear on successful
restores. Workloads that do not use TUN devices do not need a `/dev/net/tun`
mount. For a failed restore, look past this line to the final CRIU errors and
the agent's `CRIU restore tail` diagnostics.
