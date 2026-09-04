// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ai-dynamo/snapshot/api/podcontract"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// claimLookupError marks a failed ResourceClaim or ResourceClaimTemplate read.
// The reconcile is retried rather than the SnapshotJob failed.
type claimLookupError struct{ err error }

func (e claimLookupError) Error() string { return e.err.Error() }
func (e claimLookupError) Unwrap() error { return e.err }

// claimDevices sizes the pod template's DRA claims through the API, so the
// launch-job rule (more than one GPU) covers DRA-allocated GPUs as well as the
// device plugin's nvidia.com/gpu. A claim or template that does not exist
// counts as unknown, which wraps the target; a read error is retried.
func (r *SnapshotJobReconciler) claimDevices(ctx context.Context, namespace string) podcontract.ClaimDevices {
	return func(claim corev1.PodResourceClaim) (int, bool, error) {
		var requests []resourcev1.DeviceRequest
		switch {
		case claim.ResourceClaimTemplateName != nil:
			template := &resourcev1.ResourceClaimTemplate{}
			key := client.ObjectKey{Namespace: namespace, Name: *claim.ResourceClaimTemplateName}
			if err := r.Get(ctx, key, template); err != nil {
				if apierrors.IsNotFound(err) {
					return 0, false, nil
				}
				return 0, false, claimLookupError{fmt.Errorf("read ResourceClaimTemplate %q: %w", key.Name, err)}
			}
			requests = template.Spec.Spec.Devices.Requests
		case claim.ResourceClaimName != nil:
			resourceClaim := &resourcev1.ResourceClaim{}
			key := client.ObjectKey{Namespace: namespace, Name: *claim.ResourceClaimName}
			if err := r.Get(ctx, key, resourceClaim); err != nil {
				if apierrors.IsNotFound(err) {
					return 0, false, nil
				}
				return 0, false, claimLookupError{fmt.Errorf("read ResourceClaim %q: %w", key.Name, err)}
			}
			requests = resourceClaim.Spec.Devices.Requests
		default:
			return 0, false, nil
		}
		return countClaimDevices(requests)
	}
}

// countClaimDevices sums the devices a claim asks for. An "All" allocation has
// no fixed size, so the total is unknown; a first-available list counts as its
// largest alternative.
func countClaimDevices(requests []resourcev1.DeviceRequest) (int, bool, error) {
	total := 0
	for _, request := range requests {
		switch {
		case request.Exactly != nil:
			if request.Exactly.AllocationMode == resourcev1.DeviceAllocationModeAll {
				return 0, false, nil
			}
			total += countOrOne(request.Exactly.Count)
		case len(request.FirstAvailable) > 0:
			largest := 0
			for _, sub := range request.FirstAvailable {
				if sub.AllocationMode == resourcev1.DeviceAllocationModeAll {
					return 0, false, nil
				}
				largest = max(largest, countOrOne(sub.Count))
			}
			total += largest
		}
	}
	return total, true, nil
}

// countOrOne applies the API default: an unset count means one device.
func countOrOne(count int64) int {
	if count <= 0 {
		return 1
	}
	return int(count)
}

// buildSourceJob constructs the desired source Job with this reconciler's tool
// delivery and DRA claim sizing.
func (r *SnapshotJobReconciler) buildSourceJob(ctx context.Context, sj *snapshotv1alpha1.SnapshotJob) (*batchv1.Job, []string, error) {
	return buildShapedSourceJob(sj, r.CUDATools, r.claimDevices(ctx, sj.Namespace))
}
