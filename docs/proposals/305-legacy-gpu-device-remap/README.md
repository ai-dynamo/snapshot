<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# SEP-305: Restore UUID-mapped GPU device paths

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Proposal](#proposal)
  - [Limitations, Risks, and Mitigations](#limitations-risks-and-mitigations)
- [Design Details](#design-details)
  - [Monitoring](#monitoring)
  - [Test Plan](#test-plan)
  - [Graduation Criteria](#graduation-criteria)
- [Alternatives](#alternatives)
<!-- /toc -->

## Summary

Allow compatible cross-GPU restores with explicit legacy
`NVIDIA_VISIBLE_DEVICES` selections by resolving host indices to UUIDs and
preparing checkpoint-time device paths before CRIU restore.

Tracking issue: [#305](https://github.com/ai-dynamo/snapshot/issues/305).
Status: proposed; awaiting maintainer approval.

## Motivation

CRIU restores external NVIDIA device mounts before CUDA migration runs.
When source and destination GPUs have different physical device paths,
the source `/dev/nvidiaN` may not exist in the destination namespace.
CUDA UUID remapping alone cannot fix this earlier mount failure.

### Goals

- Resolve explicit index and full-UUID selections against host inventory.
- Persist UUID-to-device-path associations with checkpoint metadata.
- Apply mount aliases using the existing CUDA source/destination UUID pairing.
- Handle multiple devices, including overlapping paths and swaps.
- Preserve existing DRA and device-discovery paths when no explicit list exists.

### Non-Goals

- Support MIG identifiers or arbitrary renamed device nodes.
- Rewrite `CUDA_VISIBLE_DEVICES` or translate its application-level ordinals.
- Remove CUDA driver, hardware, or workload compatibility requirements.
- Resolve driver-level `CUDA_ERROR_NOT_SUPPORTED`.

## Proposal

The agent reads `NVIDIA_VISIBLE_DEVICES` from the workload OCI environment.
An explicit index/full-UUID list takes precedence over allocation discovery;
numeric indices are resolved on the host, not interpreted as container-local
CUDA indices. Absent, empty, `all`, `none`, and `void` values leave discovery
to the existing path. This fallback does not grant additional GPU access.
Unsupported lists and duplicate resolved UUIDs fail rather than silently
falling back.

The manifest records both the raw selection and resolved device paths.
Restore independently resolves the destination selection and prepares the
source paths using the existing CUDA UUID migration map.

`CUDA_VISIBLE_DEVICES` may further filter or reorder the application's CUDA
view, but is not the authority for physical host device paths. This proposal
does not interpret or rewrite it.

### Limitations, Risks, and Mitigations

- Device paths are validated as physical NVIDIA character devices using
  host UUID/minor inventory and the workload namespace.
- All alias sources are opened before any mount changes, so overlapping
  aliases cannot change a later source.
- Partial preparation rolls back in reverse order.
- Older manifests without device metadata retain conservative single-GPU
  inference; ambiguous multi-GPU mismatches fail.
- Multi-GPU mapping logic has unit coverage, but multi-GPU runtime validation
  remains a release qualification gap.
- Explicit environment values can be stale in mixed CDI/legacy configurations.
  Device-node validation catches missing or mismatched exposure; operators
  must keep explicit selections consistent with allocation.

## Design Details

1. Resolve explicit selections with host `nvidia-smi`.
2. Read UUID/minor associations from the NVIDIA procfs inventory.
3. Validate corresponding `/dev/nvidiaN` nodes in the workload namespace.
4. Store optional `nvidiaVisibleDevices` and `devicePaths` in the CUDA manifest.
5. Resolve destination devices and reuse the CUDA UUID migration pairing.
6. In the placeholder mount namespace, pin alias source nodes using `O_PATH`
   descriptors, then bind through `/proc/self/fd/<fd>`.
7. Run CRIU, then CUDA restore/unlock. Roll back preparation on failure;
   retain aliases after successful CRIU reconstruction.

No Kubernetes CRD changes are required. The new manifest fields are optional;
older agents do not gain remapping support merely by reading a new manifest.

### Monitoring

Use agent GPU-discovery and mount-alias logs to identify source and destination
UUIDs and paths. Restore errors distinguish discovery/preparation failures
from CRIU and CUDA failures. Existing restore timing summaries and Pod restore
conditions remain the operational interface; no new metrics are introduced.

### Test Plan

Implementation and validation are tracked in #305.

- Unit: index/UUID selection, malformed and duplicate selections, fallback
  values, physical device-node validation, overlapping paths, swaps, missing
  destinations, and legacy-manifest behavior.
- Local kind: Qwen3-0.6B real cross-GPU restore on two RTX 5880 Ada GPUs passed.
  A fresh capture with private NVIDIA mounts was required to avoid nested
  kind mount-propagation errors.
- A100 Kubernetes: different-UUID restore passed in 18.05 seconds through
  the existing device-discovery path.
- B200 Kubernetes/DRA: same-GPU restore passed in 3.03 seconds and a restore
  excluding the source UUID passed in 3.10 seconds.
- Every successful restored workload generated output.
- Remaining: multi-GPU workload migration, runtime overlap/swap tests, and
  workload filtering/reordering with `CUDA_VISIBLE_DEVICES`.

### Graduation Criteria

Before merging, obtain maintainer design approval, pass repository gates,
and review failure cleanup and allocation-selection semantics. Before claiming
general multi-GPU support, complete real multi-GPU migration and path-swap
validation. No broader driver compatibility claim follows from these tests.

## Alternatives

- Require CDI everywhere: avoids this legacy configuration but does not
  address users explicitly selecting GPUs through environment injection.
- Create aliases only when paths are missing: fails when a source path exists
  but refers to the wrong destination device, particularly during swaps.
- Infer mappings solely from path ordinals: confuses device identity with
  placement; UUID metadata supplies the missing association.
- Apply only CUDA UUID remapping: too late to repair CRIU mount reconstruction.
