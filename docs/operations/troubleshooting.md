# Troubleshooting

Common issues when running Snapshot, and where to look.

## A checkpoint never becomes Ready

A `PodSnapshot` becomes Ready only after the `snapshot-agent` confirms the
checkpoint contents — a completed capture is not enough on its own. Check the
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

## CUDA CustomStorage

### A `posix` checkpoint is rejected before capture

Snapshot deliberately fails before locking the CUDA target when the helper does not advertise both the CUDA 13.4 CustomStorage driver API and the NIXL POSIX transfer adapter. Check the helper startup event:

```bash
kubectl logs -n snapshot \
  -l app.kubernetes.io/instance=snapshot,app.kubernetes.io/component=snapshot-agent \
  -c cuda-checkpoint-helper --prefix=true --tail=-1 --max-log-requests=100 \
  | grep cuda_checkpoint_daemon_ready
```

If `custom_storage_driver_api_available` is `false`, install a driver that exports the CUDA 13.4 CustomStorage API. A CUDA 13.x-compatible driver does not necessarily provide new CUDA 13.4 APIs. If `custom_storage_transfer_backend_available` is `false`, use a Snapshot agent image that includes the NIXL POSIX adapter.

Snapshot also rejects `posix` creation when the workload uses more than one GPU. The initial implementation supports one or more CUDA-owning processes on one GPU.

### A retained `posix` checkpoint no longer restores

Restore follows the checkpoint manifest, not the current `storageMode` setting. Changing new checkpoint creation back to `legacy` does not convert existing `posix` artifacts. Keep a CustomStorage-capable driver and Snapshot image available until those artifacts are retired.

### The helper times out or is unhealthy after an operation

Check both agent and helper logs for the same target PID. An absent helper response after a state-changing CUDA request is an unknown outcome and is not replayed automatically. Do not resume the workload based only on storage cleanup. See [Configure CUDA CustomStorage with NIXL POSIX](../guides/custom-storage.md#configuration-values) before changing the helper deadline, restore timeout, or pinned-buffer settings.

<!-- TODO(eng): expand with real failure modes and messages (CRIU / cuda-checkpoint errors, RWX PVC access, driver/runtime mismatches). The Dynamo snapshot doc's troubleshooting section is a good source. -->
