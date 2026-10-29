// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"

	snapshottypes "github.com/ai-dynamo/snapshot/agent/internal/types"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// guardController wires both measurements, so a test can state the only two
// numbers the decision turns on.
func guardController(stagingPath string, total, minimum int64, measureErr error) *NodeController {
	return &NodeController{
		log:    logr.Discard(),
		config: &snapshottypes.AgentConfig{PageBroker: snapshottypes.PageBrokerSpec{StagingPath: stagingPath}},
		stagingCapacityFn: func(string) (int64, error) {
			return total, measureErr
		},
		minimumImageSizeFn: func(string, []int) (int64, error) {
			return minimum, measureErr
		},
	}
}

func TestCheckStagingCapacity(t *testing.T) {
	for _, tc := range []struct {
		name        string
		total       int64
		minimum     int64
		wantRefusal bool
	}{
		{name: "the image cannot fit the volume at all", total: 1 << 30, minimum: 4 << 30, wantRefusal: true},
		{name: "the image fits exactly", total: 4 << 30, minimum: 4 << 30},
		{name: "the image fits with room to spare", total: 8 << 30, minimum: 1 << 30},
		{name: "nothing resident yet", total: 8 << 30, minimum: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := guardController("/pagebroker/staging", tc.total, tc.minimum, nil)

			err := w.checkStagingCapacity(1)

			if !tc.wantRefusal {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err, "an image larger than the volume must be refused")
			assert.Contains(t, err.Error(), "short by", "the refusal must report the shortfall")
		})
	}
}

// The check is a guard, not a gate: it may only refuse what cannot fit, so
// every case it cannot measure has to let the capture through.
func TestCheckStagingCapacitySkipsWhatItCannotMeasure(t *testing.T) {
	for _, tc := range []struct {
		name        string
		stagingPath string
		measureErr  error
	}{
		{name: "no staging path configured", stagingPath: ""},
		{name: "staging cannot be measured", stagingPath: "/pagebroker/staging", measureErr: errors.New("statfs failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Numbers that would refuse, so only the unmeasurable path can pass this.
			w := guardController(tc.stagingPath, 1<<30, 4<<30, tc.measureErr)

			assert.NoError(t, w.checkStagingCapacity(1))
		})
	}
}

// Occupied staging is not the same as an image that cannot fit: another capture
// holding the volume must not fail this one terminally, because it frees up.
func TestCheckStagingCapacityIgnoresWhatOtherCapturesHold(t *testing.T) {
	// The volume holds 8GiB and this image needs 4GiB, so it fits — whatever
	// else is resident in staging right now.
	w := guardController("/pagebroker/staging", 8<<30, 4<<30, nil)

	assert.NoError(t, w.checkStagingCapacity(1))
}

// The refusal has to reach the work order, or the capture suspends a workload
// the node was never going to be able to stage.
func TestReconcileCaptureRecordsImageTooLarge(t *testing.T) {
	content := makeWorkOrder("content-a", "node-a", "a")
	pod := makeSourcePod()
	fc := &fakeCheckpointer{}
	w := makeNodeController(t, fc, content, pod)
	w.runtime = &fakeRuntime{resolveContainerPID: 7}
	w.config.PageBroker.StagingPath = "/pagebroker/staging"
	w.stagingCapacityFn = func(string) (int64, error) { return 1 << 30, nil }
	w.minimumImageSizeFn = func(string, []int) (int64, error) { return 4 << 30, nil }

	require.NoError(t, w.reconcileCapture(context.Background(), content.Name))

	got := getContent(t, w, content.Name)
	require.True(t, isContentFailed(got))
	assert.Equal(t, reasonImageTooLarge,
		meta.FindStatusCondition(got.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed).Reason)
	assert.False(t, fc.wasCalled(), "a capture refused for size must not suspend the workload")
}
