<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Naming conventions

Terms and naming patterns for the API, the chart, the CLI, and the docs. Use one
name per concept, and use it everywhere: field, value, docs, log line, event,
error message, and test. When a concept has two names, readers assume there are
two things.

## Terms

| Use | Means | Do not write |
| --- | --- | --- |
| checkpoint | The saved state of a pod, and the act of saving it | `snapshot` as a noun or verb, `dump`, `backup` |
| capture | The same operation seen from the source side: the capture step, the capture Deployment, the `Captured` condition | `capture` for the saved state |
| restore | Start a new pod from a checkpoint | `rehydrate`, `revive` |
| Snapshot | The product. Also the prefix of the kinds `PodSnapshot` and `SnapshotJob` | `snapshot` in lowercase for the product |
| checkpoint artifact | The files a checkpoint writes to the checkpoint store | `artifact` alone when another kind of artifact is in the sentence |
| checkpoint store | The backend that holds checkpoint artifacts. Today it is a PVC | `checkpoint storage`, `shared volume` |
| checkpoint volume | The shared `ReadWriteMany` PVC that is the checkpoint store today | |
| source Pod | The pod being checkpointed. Guides may call the serving instance a replica | `original pod`, `origin pod` |
| restore Pod | The new pod that a restore starts | |
| target container | The container being checkpointed (`targetContainers`) | `captured container`, `main container` |
| non-target container | Any other container in the source Pod | `helper`, `sidecar`, `writer`, `saver`, except as an example of what one does |
| node agent | The per-node DaemonSet (`snapshot-agent`) that drives CRIU and `cuda-checkpoint`. After the first use, "agent" is enough | `agent` for an AI assistant |
| PageBroker | The broker sidecar of the node agent | `pagebroker` or `page broker` in prose |
| baseline | The minimum versions every installation needs, listed in the README prerequisites | `general floor`, `default requirements` |

Add a row when a concept could be called two ways, or when one definition has to
hold across pages. A feature name does not need a row.

## Naming a new thing

| Surface | Pattern | Existing examples |
| --- | --- | --- |
| Kind | PascalCase noun | `PodSnapshot`, `SnapshotJob` |
| Spec or status field | lowerCamelCase. A reference to an object ends in `Ref`, a name in `Name`, a UID in `UID`, a timestamp in `At`, a duration in whole seconds in `Seconds`. A list is plural | `podRef`, `boundSnapshotContentName`, `sourceJobUID`, `startedAt`, `activeDeadlineSeconds`, `targetContainers` |
| Condition type | PascalCase adjective or past participle | `Ready`, `Running`, `Captured`, `Completed`, `Failed` |
| Condition or event reason | PascalCase, subject then outcome, no spaces | `DeadlineExceeded`, `PodSnapshotDeleted`, `CheckpointFailed` |
| Annotation or label | `nvidia.com/` plus kebab-case. The restore interface starts with `restore-`; the rest usually start with `snapshot-` | `nvidia.com/restore-from`, `nvidia.com/snapshot-job-uid`, `nvidia.com/snapshot-skip-compat-check` |
| Finalizer | `nvidia.com/` plus the object it protects and the action, in kebab-case | `nvidia.com/podsnapshotcontent-cleanup` |
| Pod condition type | `nvidia.com/` plus PascalCase | `nvidia.com/Restored` |
| Environment variable | `SNAPSHOT_` plus UPPER_SNAKE. `DYN_SNAPSHOT_` is a deprecated alias; do not add new ones | `SNAPSHOT_CONTROL_DIR` |
| Helm value | lowerCamelCase, nested under its component (`pageBroker`, `daemonset`, `storage`, `operator`, `config`). Name it like its siblings: `operator.*` durations are duration strings, `config.*` durations are `…Seconds`, sizes are `…Bytes` or a Kubernetes quantity | `operator.artifactCleanup.scanInterval: 10m`, `config.restore.restoreTimeoutSeconds`, `pageBroker.transferChunkBytes`, `pageBroker.staging.sizeLimit` |
| CLI flag | Kebab-case long option | `--manifest`, `--snapshot` |
| Object, volume, or file that Snapshot creates | Kebab-case | `snapshot-agent`, `snapshot-control`, `ready-for-snapshot` |
| Docs file | Kebab-case `.md`. A framework guide is `docs/guides/<framework>.md`, with its manifests in `docs/guides/<framework>/` | `tensorrt-llm.md` |

## Before you add a name

1. Search for the concept: `rg -n -i '<word>' README.md docs api charts operator agent`.
   If it has a name, use that name.
2. Name what the user controls or sees, not the mechanism. `CustomStorage`,
   `NIXL`, and `CRIU` are mechanisms; users choose a behavior.
3. Qualify a generic word such as `artifact`, `storage`, or `state`, or pick a
   specific one.
4. Use the same name in every place. If you rename something, rename it
   everywhere, and search the docs for the old name.
5. Give a feature one noun in all its names: the Helm value, annotation, event
   reason, and docs heading. Use the noun of the object it acts on, such as
   `PodSnapshot`.
