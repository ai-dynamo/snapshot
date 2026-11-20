// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package podcontract defines Snapshot's workload Pod contract and provides
// helpers to build, validate, and interpret restore Pods.
//
// A standalone restore Pod names its PodSnapshot in RestoreFromAnnotation;
// Build and Validate shape and check it. A group restore target, one member of
// a PodGroupRestore, goes through four states instead:
//
//  1. Gated. The owner creates the Pod with RestoreGroupSchedulingGate,
//     through BuildGroupTarget, and with no restore annotation. The gate keeps
//     it unscheduled, so nothing has run in it.
//  2. Claimed. The operator claims it for one PodGroupRestore in a single
//     patch conditional on its resourceVersion, built from ClaimGroupTarget:
//     the patch writes RestoreGroupUIDAnnotation and a single-pair
//     RestoreContainerMapAnnotation, and removes the gate. Only the operator
//     removes the gate and writes the claim.
//  3. Scheduled. The Pod takes a node, and the NRI plugin reads the claim from
//     the sandbox's annotations with RestoreGroupClaimOf, without an API read.
//  4. Pinned. The operator records the Pod's UID, container IDs and address in
//     the PodGroupRestore's status, and the node agents act on those pins.
//
// A claimed Pod is single-use: it is never claimed again, so its binding names
// the only attempt that may act or report on it. An owner replaces a Pod whose
// attempt failed.
package podcontract
