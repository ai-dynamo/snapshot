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
can appear on successful restores. It comes from a TUN capability probe and
is a red herring for workloads that do not use TUN devices. The framework
recipes do not require a `/dev/net/tun` hostPath mount to silence this message.
Fresh vLLM 0.31 / Qwen3-0.6B capture, restore, and inference have been verified
without the device at TP1 and TP2, with CUDA shared-memory support enabled for
TP2 on two B200 GPUs.

For a failed restore, inspect the final CRIU error lines and the agent's
`CRIU restore tail` diagnostics. CRIU's RPC error can contain its first logged
error, including this nonfatal probe, rather than the failure that terminated
restore. A later worker-exit error is relevant failure context, though its
underlying cause may require inspecting the preceding messages.

Workloads that actually use TUN still need their devices configured. When
restoring an existing checkpoint, preserve its required mount layout. Removing
a mount from an already captured workload is a separate change from capturing
and restoring the recipes without it.
