<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Reading a Failure Message

Every checkpoint and restore failure names the step it came from, in the form
`while <step>: <cause>`. The step tells you which part of the flow stopped; the
cause is what the tool at that step reported. Step names say which component
was running, CRIU, PageBroker, nsenter, because that is whose log holds the
detail and what to search for. The sections below are organised by step.

Several steps name the placeholder: the restore pod's container, which waits
with no workload of its own until the agent puts the checkpointed process into
it. The [Restore Pod Contract](../reference/restore-pod-contract.md) describes
how it is set up.

For a restore, the message is on the pod's `nvidia.com/Restored` condition and
on a `RestoreWorkerFailed` event, one entry per destination container:

```bash
kubectl get pod <restore-pod> -n <ns> -o jsonpath='{.status.conditions[?(@.type=="nvidia.com/Restored")].message}'
kubectl get events -n <ns> --field-selector involvedObject.name=<restore-pod>
```

For a checkpoint, the message is on the `PodSnapshotContent` `Failed` condition
with reason `CheckpointFailed`:

```bash
kubectl get podsnapshotcontent <name> -o jsonpath='{.status.conditions[?(@.type=="Failed")].message}'
```

The agent on the node where the pod ran logs the same failure with a
structured `step` key, so one filter shows every failure at a given step. The
agent runs in the Snapshot release namespace, one pod per node, so find that
node's pod first rather than reading the DaemonSet, which picks an arbitrary
pod:

```bash
node=$(kubectl get pod <restore-pod> -n <ns> -o jsonpath='{.spec.nodeName}')
agent=$(kubectl get pods -n <snapshot-ns> -l app.kubernetes.io/component=snapshot-agent \
  --field-selector spec.nodeName=$node -o name)
kubectl logs $agent -n <snapshot-ns> -c agent | grep '"step"'
```

For a checkpoint, take the node from the source pod instead. The PageBroker
commands below assume the same `$agent`.

Raise `daemonset.snapshotLogLevel` in the chart values when you need more than
the error lines; it sets the level for the agent and for nsrestore. A restore
that failed inside the placeholder also leaves nsrestore's stderr and the key
lines of CRIU's `restore.log` in the agent log.
