<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Troubleshoot the Agent

Use this page when the node agent or a component it runs fails. It covers an
agent or restore pod that will not start, a GPU that fails with
`target GPU is unavailable`, PageBroker that does not start after an S3 settings
change, and CRIU reporting `Unable to create tun`.

## The Agent or a Restore Pod Will Not Start

The `snapshot-agent` runs privileged with `hostPID`, `hostIPC`, and `hostNetwork`.
If the namespace enforces a restrictive Pod Security level, the agent or a
restore pod can be rejected. See [Allow the Privileged Node Agent](../get-started/installation.md#allow-the-privileged-node-agent).

When using AWS EFS, make sure a reused static PV references a real EFS file
system ID and has mount targets reachable from every GPU node. A placeholder
such as `fs-xxxx` can bind a PVC but the snapshot agent will not start because
the node cannot mount it.

## A GPU Checkpoint or Restore Fails with `target GPU is unavailable`

When the driver supports CustomStorage, PageBroker sets up every GPU on the
node when it starts. A GPU that fails setup, for example a faulted GPU or one
with too little free memory, is skipped, and work on the node's other GPUs
continues. Only checkpoints and restores that use the skipped GPU fail. The
PageBroker log names it:

```bash
kubectl logs <agent-pod> -n <ns> -c pagebroker | grep '^skip GPU'
```

Once the GPU is healthy and has free memory again, restart the agent pod so
PageBroker sets it up.

## PageBroker Does Not Start After an S3 Settings Change

After a change to `storage.s3.addressing` or the CA bundle, a PageBroker pod can
start before the operator has rewritten its configuration. PageBroker refuses to
start while its environment and its configuration disagree, and the pod
recovers after the operator's configuration catches up and PageBroker restarts.
PageBroker also refuses to start on a key it does not know in
`pageBroker.transferEngine.modelStreamer`, so a misspelled key shows up as a
failing pod, not as a rendering error. Read the PageBroker container log for the
rejected setting.

## CRIU Reports `Unable to create tun`

`Error (criu/tun.c:85): tun: Unable to create tun: No such file or directory`
comes from a nonfatal TUN capability probe and can appear on successful
restores. Workloads that do not use TUN devices do not need a `/dev/net/tun`
mount. For a failed restore, look past this line to the final CRIU errors and
the agent's `CRIU restore tail` diagnostics.
