// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nri

import "github.com/ai-dynamo/snapshot/api/podcontract"

// groupDestination returns the destination container of a pod group's
// restore target, from its binding alone. The operator writes the binding,
// nvidia.com/restore-group-uid and a single-pair container map, in the patch
// that removes the target's scheduling gate, so the sandbox carries both from
// its creation and the plugin needs no API read. bound is false for a Pod
// without a binding. A bound Pod whose binding is invalid reports an error, and
// its containers are not created, so its own command never runs.
func groupDestination(annotations map[string]string) (destination string, bound bool, err error) {
	claim, bound, err := podcontract.RestoreGroupClaimOf(annotations)
	if !bound || err != nil {
		return "", bound, err
	}
	return claim.Mapping.Destination, true, nil
}
