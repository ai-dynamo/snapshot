// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/ai-dynamo/snapshot/agent/internal/executor"
	"k8s.io/apimachinery/pkg/api/meta"

	snapshottypes "github.com/ai-dynamo/snapshot/agent/internal/types"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// guardController wires both measurements, so a test can state the only two
// numbers the decision turns on. Each measurement fails on its own, because
// a shared error would never reach the second one.
func guardController(stagingPath string, total, minimum int64, capacityErr, sizeErr error) *NodeController {
	return &NodeController{
		log:    logr.Discard(),
		config: &snapshottypes.AgentConfig{PageBroker: snapshottypes.PageBrokerSpec{StagingPath: stagingPath}},
		stagingCapacityFn: func(string) (int64, error) {
			return total, capacityErr
		},
		minimumImageSizeFn: func(string, []int) (int64, error) {
			return minimum, sizeErr
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
			w := guardController("/pagebroker/staging", tc.total, tc.minimum, nil, nil)

			err := w.checkStagingCapacity(context.Background(), 1)

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
		capacityErr error
		sizeErr     error
	}{
		{name: "no staging path configured", stagingPath: ""},
		{name: "the volume cannot be measured", stagingPath: "/pagebroker/staging", capacityErr: errors.New("statfs failed")},
		{name: "the image cannot be sized", stagingPath: "/pagebroker/staging", sizeErr: errors.New("read /proc: no such process")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Numbers that would refuse, so only the unmeasurable path can pass this.
			w := guardController(tc.stagingPath, 1<<30, 4<<30, tc.capacityErr, tc.sizeErr)

			assert.NoError(t, w.checkStagingCapacity(context.Background(), 1))
		})
	}
}

// Occupied staging is not the same as an image that cannot fit: another capture
// holding the volume must not fail this one terminally, because it frees up.
func TestCheckStagingCapacityIgnoresWhatOtherCapturesHold(t *testing.T) {
	// The volume holds 8GiB and this image needs 4GiB, so it fits — whatever
	// else is resident in staging right now.
	w := guardController("/pagebroker/staging", 8<<30, 4<<30, nil, nil)

	assert.NoError(t, w.checkStagingCapacity(context.Background(), 1))
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

// The preflight only ever establishes "too big", never "fits", so a capture it
// let through can still run out of room. Only a measurement says which
// condition that was, and the answer decides whether retrying is worth anything.
func TestRefineFullStaging(t *testing.T) {
	staging := t.TempDir()
	dumpDir := filepath.Join(staging, "checkpoint", "tx-abc")
	require.NoError(t, os.MkdirAll(dumpDir, 0o755))
	enospc := &executor.DumpSpaceError{Dir: dumpDir,
		Err: errors.Join(errors.New("CRIU dump failed: Unable to splice data"), unix.ENOSPC)}
	for _, tc := range []struct {
		name          string
		stagingPath   string
		err           error
		total         int64
		minimum       int64
		capacityErr   error
		sizeErr       error
		wantReason    string
		wantRetryable bool
		wantShortfall bool
	}{
		{
			name: "an image larger than the volume can never fit", stagingPath: staging, err: enospc,
			total: 1 << 30, minimum: 4 << 30, wantReason: reasonImageTooLarge, wantShortfall: true,
		},
		{
			name: "an image that fits means the volume was merely occupied", stagingPath: staging, err: enospc,
			total: 8 << 30, minimum: 1 << 30, wantReason: reasonStagingUnavailable, wantRetryable: true,
		},
		{
			name: "a failure that is not ENOSPC is left alone", stagingPath: staging, err: errors.New("criu exploded"),
			total: 1 << 30, minimum: 4 << 30, wantReason: reasonCheckpointFailed,
		},
		{
			name: "a dump that named no directory is left alone", err: &executor.DumpSpaceError{Dir: "",
				Err: errors.Join(errors.New("CRIU dump failed"), unix.ENOSPC)},
			total: 1 << 30, minimum: 4 << 30, wantReason: reasonCheckpointFailed,
		},
		{
			name: "an unmeasurable volume is reported unrefined", stagingPath: staging, err: enospc,
			capacityErr: errors.New("statfs"), wantReason: reasonCheckpointFailed,
		},
		{
			name: "an unsizeable image is reported unrefined", stagingPath: staging, err: enospc,
			sizeErr: errors.New("no proc"), wantReason: reasonCheckpointFailed,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := guardController(tc.stagingPath, tc.total, tc.minimum, tc.capacityErr, tc.sizeErr)
			var measured string
			capacity := w.stagingCapacityFn
			w.stagingCapacityFn = func(path string) (int64, error) { measured = path; return capacity(path) }
			got, gotErr := w.refineFullStaging(context.Background(),
				captureFailure{reason: reasonCheckpointFailed}, tc.err, os.Getpid())

			assert.Equal(t, tc.wantReason, got.reason)
			assert.Equal(t, tc.wantRetryable, got.retryable, "retryable")
			require.ErrorIs(t, gotErr, tc.err, "the cause must stay reachable")
			if tc.wantShortfall {
				assert.ErrorContains(t, gotErr, "short by")
			} else {
				assert.NotContains(t, gotErr.Error(), "short by")
			}
			if tc.wantReason != reasonCheckpointFailed {
				assert.Equal(t, dumpDir, measured,
					"the volume the dump named must be the one measured")
			}
		})
	}
}

// PageBroker releases a failed transaction's directory before the failure is
// classified, so measuring the path the dump named would statfs something that
// is no longer there and report the failure unrefined.
func TestRefineFullStagingMeasuresASurvivingAncestor(t *testing.T) {
	root := t.TempDir()
	gone := filepath.Join(root, "artifacts", "uid", "containers", "main.pagebroker-tx-abc")

	w := guardController(root, 8<<30, 1<<30, nil, nil)
	var measured string
	w.stagingCapacityFn = func(path string) (int64, error) {
		measured = path
		if _, err := os.Stat(path); err != nil {
			return 0, err
		}
		return 8 << 30, nil
	}

	got, _ := w.refineFullStaging(context.Background(), captureFailure{reason: reasonCheckpointFailed},
		&executor.DumpSpaceError{Dir: gone, Err: errors.Join(errors.New("CRIU dump failed"), unix.ENOSPC)}, os.Getpid())

	assert.Equal(t, root, measured, "the nearest surviving ancestor must be measured")
	assert.Equal(t, reasonStagingUnavailable, got.reason)
	assert.True(t, got.retryable)
}

// Repeating a dump is otherwise decided by probing the workload, which answers
// whether another dump can run — not whether it could end any differently.
func TestDumpWorthRepeating(t *testing.T) {
	dumpDir := t.TempDir()
	enospc := &executor.DumpSpaceError{Dir: dumpDir,
		Err: errors.Join(errors.New("CRIU dump failed"), unix.ENOSPC)}

	for _, tc := range []struct {
		name    string
		err     error
		minimum int64
		want    bool
	}{
		{name: "an image that can never fit is not repeated", err: enospc, minimum: 16 << 30},
		{name: "an image that fits is repeated", err: enospc, minimum: 1 << 30, want: true},
		{name: "any other failure is left to the probe", err: errors.New("criu exploded"), minimum: 16 << 30, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := guardController(dumpDir, 8<<30, tc.minimum, nil, nil)
			got, reason := w.dumpWorthRepeating(context.Background(), os.Getpid())(tc.err)
			assert.Equal(t, tc.want, got)
			if !tc.want {
				assert.NotEmpty(t, reason, "a refusal must say why")
			}
		})
	}
}

// Staging frees up between attempts; the artifact store holds committed
// checkpoints and does not. A dump that filled the store is not contention, so
// retrying it on this node cannot help.
func TestRefineFullStagingSeparatesTheStoreFromStaging(t *testing.T) {
	staging := t.TempDir()
	store := t.TempDir()
	full := func(dir string) error {
		return &executor.DumpSpaceError{Dir: dir,
			Err: errors.Join(errors.New("CRIU dump failed"), unix.ENOSPC)}
	}

	w := guardController(staging, 8<<30, 1<<30, nil, nil)
	inStaging, _ := w.refineFullStaging(context.Background(),
		captureFailure{reason: reasonCheckpointFailed}, full(staging), os.Getpid())
	assert.Equal(t, reasonStagingUnavailable, inStaging.reason)
	assert.True(t, inStaging.retryable, "staging frees up, so the retry is worth taking")

	inStore, _ := w.refineFullStaging(context.Background(),
		captureFailure{reason: reasonCheckpointFailed}, full(store), os.Getpid())
	assert.Equal(t, reasonStoreExhausted, inStore.reason)
	assert.False(t, inStore.retryable, "no retry on this node frees room in the store")

	// An agent with no staging volume dumps straight to the store.
	noStaging := guardController("", 8<<30, 1<<30, nil, nil)
	got, _ := noStaging.refineFullStaging(context.Background(),
		captureFailure{reason: reasonCheckpointFailed}, full(store), os.Getpid())
	assert.Equal(t, reasonStoreExhausted, got.reason)
}

// Walking past the staging root would measure an unrelated filesystem, and
// eventually the agent's own rootfs, reporting a capacity that was never the
// dump's.
func TestStagingAncestorStopsAtTheStagingRoot(t *testing.T) {
	staging := t.TempDir()
	nested := filepath.Join(staging, "checkpoint", "tx-abc")
	require.NoError(t, os.MkdirAll(nested, 0o755))

	got, ok := stagingAncestor(filepath.Join(nested, "gone", "deeper"), staging)
	require.True(t, ok)
	assert.Equal(t, nested, got, "the nearest surviving ancestor inside staging")

	// Staging itself gone: there is nothing left that was the dump's volume.
	_, ok = stagingAncestor(filepath.Join("/no-such-staging", "checkpoint", "tx"), "/no-such-staging")
	assert.False(t, ok, "the walk must not escape to an unrelated filesystem")
}

// A dump that terminated the tree leaves nothing to size, and a zero minimum
// would read as "the image fits".
func TestRefineFullStagingDoesNotSizeATerminatedSource(t *testing.T) {
	staging := t.TempDir()
	w := guardController(staging, 1<<30, 0, nil, nil)
	sized := false
	w.minimumImageSizeFn = func(string, []int) (int64, error) { sized = true; return 0, nil }

	got, _ := w.refineFullStaging(context.Background(), captureFailure{reason: reasonCheckpointFailed},
		&executor.DumpSpaceError{Dir: staging, Err: errors.Join(errors.New("CRIU dump failed"), unix.ENOSPC)},
		// A PID whose tree is gone, as it is once the dump has terminated it.
		999999)

	assert.False(t, sized, "a tree that is gone must not be sized")
	assert.Equal(t, reasonCheckpointFailed, got.reason, "the failure is reported unrefined")
}
