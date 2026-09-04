// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func exactRequest(count int64) resourcev1.DeviceRequest {
	return resourcev1.DeviceRequest{Name: "gpu", Exactly: &resourcev1.ExactDeviceRequest{
		DeviceClassName: "gpu.nvidia.com", AllocationMode: resourcev1.DeviceAllocationModeExactCount, Count: count,
	}}
}

func TestCountClaimDevices(t *testing.T) {
	count, known, err := countClaimDevices([]resourcev1.DeviceRequest{exactRequest(4), exactRequest(0)})
	require.NoError(t, err)
	assert.True(t, known)
	assert.Equal(t, 5, count, "an unset count is one device")

	_, known, err = countClaimDevices([]resourcev1.DeviceRequest{{Name: "all", Exactly: &resourcev1.ExactDeviceRequest{
		DeviceClassName: "gpu.nvidia.com", AllocationMode: resourcev1.DeviceAllocationModeAll,
	}}})
	require.NoError(t, err)
	assert.False(t, known, "\"All\" has no fixed size")

	count, known, err = countClaimDevices([]resourcev1.DeviceRequest{{Name: "either", FirstAvailable: []resourcev1.DeviceSubRequest{
		{Name: "big", DeviceClassName: "gpu.nvidia.com", AllocationMode: resourcev1.DeviceAllocationModeExactCount, Count: 8},
		{Name: "small", DeviceClassName: "gpu.nvidia.com", AllocationMode: resourcev1.DeviceAllocationModeExactCount, Count: 2},
	}}})
	require.NoError(t, err)
	assert.True(t, known)
	assert.Equal(t, 8, count, "a first-available list counts as its largest alternative")
}

func TestClaimDevicesResolvesTemplatesAndClaims(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	require.NoError(t, resourcev1.AddToScheme(s))
	template := &resourcev1.ResourceClaimTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu-template", Namespace: "inference"},
		Spec: resourcev1.ResourceClaimTemplateSpec{Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{exactRequest(4)}},
		}},
	}
	claim := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "one-gpu", Namespace: "inference"},
		Spec:       resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{exactRequest(1)}}},
	}
	r := makeSnapshotJobReconciler(s, template, claim)
	resolve := r.claimDevices(context.Background(), "inference")

	count, known, err := resolve(corev1.PodResourceClaim{Name: "gpus", ResourceClaimTemplateName: ptr.To("gpu-template")})
	require.NoError(t, err)
	assert.True(t, known)
	assert.Equal(t, 4, count)

	count, known, err = resolve(corev1.PodResourceClaim{Name: "gpu", ResourceClaimName: ptr.To("one-gpu")})
	require.NoError(t, err)
	assert.True(t, known)
	assert.Equal(t, 1, count)

	_, known, err = resolve(corev1.PodResourceClaim{Name: "gone", ResourceClaimTemplateName: ptr.To("missing")})
	require.NoError(t, err)
	assert.False(t, known, "a missing template is unknown, which wraps the target")

	broken := makeSnapshotJobReconcilerWithInterceptor(s, interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("apiserver unavailable")
		},
	})
	_, _, err = broken.claimDevices(context.Background(), "inference")(corev1.PodResourceClaim{Name: "gpus", ResourceClaimTemplateName: ptr.To("gpu-template")})
	require.Error(t, err)
	assert.True(t, errors.As(err, new(claimLookupError)), "API failures are retried, not terminal: %v", err)
}
