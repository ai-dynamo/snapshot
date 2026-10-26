<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Naming conventions

Terms and naming patterns for the API, the chart, the CLI, and the docs. Use one
name per concept, and use it everywhere: field, value, docs, log line, event,
error message, and test. When a concept has two names, readers assume there are
two things.

## User-facing and internal names

Snapshot has two kinds of names. Keep them apart.

| | User-facing | Internal |
| --- | --- | --- |
| What it is | Anything that a user, workload author, integrator, or operator types, reads, or sees | Anything that only Snapshot's own code sees |
| Examples | Kinds, fields, and conditions; annotations, labels, and environment variables in the reference or the contracts; Helm values; CLI flags and output; events, metrics, log lines, and error messages; the docs | Go packages, types, and functions; messages between the operator, the node agent, and PageBroker; keys and files that only those components read; calls to CRIU and `cuda-checkpoint`; tests |
| Named after | The capability: what the user gets or controls | What is clearest to a maintainer, often the mechanism |

A user-facing name says what the feature does, in the user's words. It does not
use the name of the library, tool, driver call, protocol, or component that does
the work. Check a name with two questions:

- Could someone who has never read the code tell what it does?
- Would it still be right if the mechanism were replaced?

The multi-GPU opt-in shows the difference. It was
`nvidia.com/cuda-shared-memory-support`, which names the mechanism and not what
the user gets. It is now `nvidia.com/snapshot-multi-gpu`, and the Go constant
keeps the mechanism's name, `CuInterposeAnnotation` (#626). The key that users
write names the feature, and the code keeps its own name.

To keep the mechanism out of what users meet:

- Do not copy another tool's option names into a value, flag, annotation, or
  field. Decide what the user gets, and name that. The `config.criu.*` values
  and `snapshotctl checkpoint --cuda-checkpoint-wrap` follow the mechanism. Do
  not copy them.
- Group settings that change what Snapshot does by capability (`checkpoint`,
  `restore`, `storage`), not by the tool that implements them. Settings that
  shape how a component is deployed, such as images, resources, and tolerations,
  stay with the component (`operator`, `daemonset`).
- Add a setting that only tunes a mechanism, such as a buffer size or a tool
  option, only when a user can observe the difference. Keep it in its own
  section, and label it as tuning in the reference.
- In text, name a mechanism only where the reader must act on it, such as a
  binary that an image must contain. Lead with the capability.

An internal name follows the conventions of its package and language, and may
describe the mechanism when that is clearest. It must not reach users: translate
it into the feature's name in docs, messages, and events. A key, variable, or
file that only Snapshot's components read is not part of the API, so leave it
out of the reference.

## Terms

| Use | Means | Do not write |
| --- | --- | --- |
| Snapshot | The product. Also the prefix of the kinds `PodSnapshot` and `SnapshotJob` | `snapshot` in lowercase for the product |
| snapshot | The saved state of a pod: the artifact a checkpoint writes to the snapshot store | `checkpoint` as a noun, `checkpoint artifact`, `dump`, `backup` |
| checkpoint | Verb: take a snapshot of a pod. Also the `snapshotctl checkpoint` command | `snapshot` as a verb, `capture`, `dump` |
| restore | Verb: start a new pod from a snapshot | `rehydrate`, `revive` |
| capture | The mechanism inside a checkpoint. Use it in architecture pages and code, and in the existing `Captured` condition and capture Deployment. Elsewhere write checkpoint | `capture` for the act of checkpointing or for the snapshot |
| snapshot store | The backend that holds snapshots. Today it is a PVC | `checkpoint store`, `checkpoint storage`, `shared volume` |
| snapshot volume | The shared `ReadWriteMany` PVC that is the snapshot store today | `checkpoint volume` |
| source Pod | The pod being checkpointed. Guides may call the serving instance a replica | `original pod`, `origin pod` |
| restore Pod | The new pod that a restore starts | |
| target container | The container you checkpoint from (`targetContainers`). It is a checkpoint-side term; a restore runs in a destination container | `captured container`, `main container` |
| non-target container | Any other container in the source Pod | `helper`, `sidecar`, `writer`, `saver`, except as an example of what one does |
| destination container | The container in a restore Pod that you restore into. `nvidia.com/restore-container-map` maps the target container to one or more destination containers | `target container` or `restore target` for this side |
| node agent | The per-node DaemonSet (`snapshot-agent`) that drives CRIU and `cuda-checkpoint`. After the first use, "agent" is enough | `agent` for an AI assistant |
| PageBroker | The broker sidecar of the node agent | `pagebroker` or `page broker` in prose |
| baseline | The minimum versions every installation needs, in the baseline row of Compatibility | `general floor`, `default requirements` |

Add a row when a concept could be called two ways, or when one definition has to
hold across pages. A feature name does not need a row.

## Naming a user-facing thing

These patterns are for user-facing names. An internal name follows its package.

| Surface | Pattern | Existing examples |
| --- | --- | --- |
| Kind | PascalCase noun | `PodSnapshot`, `SnapshotJob` |
| Spec or status field | lowerCamelCase. A reference to an object ends in `Ref`, a name in `Name`, a UID in `UID`, a timestamp in `At`, a duration in whole seconds in `Seconds`. A list is plural | `podRef`, `boundSnapshotContentName`, `sourceJobUID`, `startedAt`, `activeDeadlineSeconds`, `targetContainers` |
| Condition type | PascalCase adjective or past participle | `Ready`, `Running`, `Captured`, `Completed`, `Failed` |
| Condition or event reason | PascalCase, subject then outcome, no spaces | `DeadlineExceeded`, `PodSnapshotDeleted`, `CheckpointFailed` |
| Annotation or label that Snapshot defines | `nvidia.com/` plus kebab-case. The restore interface starts with `restore-`; the rest usually start with `snapshot-` | `nvidia.com/restore-from`, `nvidia.com/snapshot-multi-gpu`, `nvidia.com/snapshot-skip-compat-check` |
| Finalizer | `nvidia.com/` plus the object it protects and the action, in kebab-case | `nvidia.com/podsnapshotcontent-cleanup` |
| Pod condition type | `nvidia.com/` plus PascalCase | `nvidia.com/Restored` |
| Environment variable | `SNAPSHOT_` plus UPPER_SNAKE. `DYN_SNAPSHOT_` is a deprecated alias; do not add new ones | `SNAPSHOT_CONTROL_DIR` |
| Helm value | lowerCamelCase. A setting that changes what Snapshot does is nested under its capability (`config.restore`, `storage`). A setting that shapes how a component is deployed is nested under the component (`operator`, `daemonset`). Name it like its siblings: `operator.*` durations are duration strings, `config.*` durations are `…Seconds`, sizes are `…Bytes` or a Kubernetes quantity | `config.restore.restoreTimeoutSeconds`, `config.restore.skipCompatCheck`, `operator.artifactCleanup.scanInterval: 10m`, `storage.pvc.size` |
| CLI flag | Kebab-case long option, named for the feature it turns on and not for the command it runs | `--manifest`, `--snapshot` |
| Object, volume, or file that Snapshot creates | Kebab-case | `snapshot-agent`, `snapshot-control`, `ready-for-snapshot` |
| Docs file | Kebab-case `.md`, except `README.md`, which indexes a directory or holds a SNEP. A framework guide is `docs/guides/<framework>.md`, with its manifests in `docs/guides/<framework>/` | `tensorrt-llm.md` |

## Before you add a name

1. Decide who sees it. If a user, workload author, integrator, or operator can
   type it, read it, or see it, it is user-facing, and the rules above apply.
2. Search for the concept: `rg -n -i '<word>' README.md docs api charts operator agent`.
   If it has a name, use that name.
3. Say in one plain sentence what the user gets, and take the name from that
   sentence. Leave out the tools, libraries, and components that do the work.
   `CustomStorage`, `NIXL`, and `CRIU` are mechanisms; users choose a behavior.
4. Qualify a generic word such as `artifact`, `storage`, `state`, or `support`,
   or pick a specific one.
5. Use the same name in every place. If you rename something, rename it
   everywhere, and search the docs for the old name.
6. Give a feature one noun in all its names: the Helm value, annotation, event
   reason, message, and docs heading. Use the noun of the object it acts on,
   such as `PodSnapshot`.
