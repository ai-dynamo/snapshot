// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	"github.com/ai-dynamo/snapshot/agent/internal/executor"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/api/compat"
	"github.com/ai-dynamo/snapshot/api/podcontract"
)

// fakeControlVolume stands in for the Pod's snapshot-control volume as the
// node sees it, keyed by "<node UID>/<container>/<file>".
type fakeControlVolume struct {
	mu        sync.Mutex
	files     map[string][]byte
	readErr   error
	writeErrs map[string]error // by file name
	writes    []string
}

func newFakeControlVolume(w *NodeController) *fakeControlVolume {
	v := &fakeControlVolume{files: map[string][]byte{}, writeErrs: map[string]error{}}
	w.readPodControlFileFn = v.read
	w.writePodControlSentinelFn = v.write
	w.removePodControlFileFn = v.remove
	return v
}

func (v *fakeControlVolume) remove(uid, container, name string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.files, uid+"/"+container+"/"+name)
	return nil
}

func (v *fakeControlVolume) read(uid, container, name string, maxBytes int64) ([]byte, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.readErr != nil {
		return nil, v.readErr
	}
	data, ok := v.files[uid+"/"+container+"/"+name]
	if !ok {
		return nil, fmt.Errorf("open %s: %w", name, os.ErrNotExist)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%s is larger than %d bytes", name, maxBytes)
	}
	return data, nil
}

func (v *fakeControlVolume) write(uid, container, name string, data []byte) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.writes = append(v.writes, uid+"/"+container+"/"+name)
	if err := v.writeErrs[name]; err != nil {
		return err
	}
	v.files[uid+"/"+container+"/"+name] = append([]byte(nil), data...)
	return nil
}

func (v *fakeControlVolume) put(t *testing.T, uid, container string, record any) {
	t.Helper()
	data, ok := record.([]byte)
	if !ok {
		var err error
		data, err = json.Marshal(record)
		require.NoError(t, err)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.files[uid+"/"+container+"/"+podcontract.RestoreAttemptsFile] = data
}

func (v *fakeControlVolume) record(t *testing.T, uid, container string) *restoreAttempts {
	t.Helper()
	v.mu.Lock()
	data, ok := v.files[uid+"/"+container+"/"+podcontract.RestoreAttemptsFile]
	v.mu.Unlock()
	require.True(t, ok, "no attempt record for %s/%s", uid, container)
	record, err := parseRestoreAttempts(data)
	require.NoError(t, err)
	return record
}

func TestParseRestoreAttempts(t *testing.T) {
	deadline := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		data    string
		want    *restoreAttempts
		wantErr string
	}{
		"a full record": {
			data: `{"limit":2,"attempt":1,"deadline":"2026-11-09T12:00:00Z","containerID":"ctr-abc"}`,
			want: &restoreAttempts{Limit: 2, Attempt: 1, Deadline: deadline, ContainerID: "ctr-abc"},
		},
		"no deadline": {data: `{"limit":0,"attempt":1,"containerID":"ctr-abc"}`, want: &restoreAttempts{Attempt: 1, ContainerID: "ctr-abc"}},
		"fields from a newer agent": {
			data: `{"limit":1,"attempt":2,"containerID":"ctr-abc","fromANewerAgent":{"x":1}}`,
			want: &restoreAttempts{Limit: 1, Attempt: 2, ContainerID: "ctr-abc"},
		},
		"not JSON":            {data: `limit=2`, wantErr: "invalid character"},
		"empty":               {data: ``, wantErr: "unexpected end of JSON input"},
		"attempt 0":           {data: `{"limit":2,"attempt":0,"containerID":"ctr-abc"}`, wantErr: "attempt 0 is not positive"},
		"a negative limit":    {data: `{"limit":-1,"attempt":1,"containerID":"ctr-abc"}`, wantErr: "retry limit -1 is negative"},
		"no container":        {data: `{"limit":2,"attempt":1}`, wantErr: "no container ID"},
		"a string attempt":    {data: `{"limit":2,"attempt":"1","containerID":"ctr-abc"}`, wantErr: "cannot unmarshal"},
		"a deadline not time": {data: `{"limit":2,"attempt":1,"deadline":"soon","containerID":"ctr-abc"}`, wantErr: "cannot parse"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := parseRestoreAttempts([]byte(tc.data))
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want.Limit, got.Limit)
			assert.Equal(t, tc.want.Attempt, got.Attempt)
			assert.True(t, tc.want.Deadline.Equal(got.Deadline), "deadline = %s, want %s", got.Deadline, tc.want.Deadline)
			assert.Equal(t, tc.want.ContainerID, got.ContainerID)
		})
	}
}

// attemptHarness runs one restore attempt against a fake control volume. The
// runtime names the Pod "host-uid" on the node, as a vCluster would.
type attemptHarness struct {
	w      *NodeController
	pod    *corev1.Pod
	volume *fakeControlVolume
	// containerID is the destination container a pass finds.
	containerID string
	// now is the agent's clock. It starts at the real time, since an
	// attempt's context ends by the real clock at the record's deadline.
	now      time.Time
	restores int
	deadline time.Time
	// seen is the record as the attempt saw it when it started.
	seen  *restoreAttempts
	calls []string
}

func newAttemptHarness(t *testing.T, annotations map[string]string, restoreErr error) (*attemptHarness, func() error) {
	t.Helper()
	pod := restorePod(annotations)
	w := makeTestController(t, pod)
	w.config.Restore.TimeoutSeconds = 60
	w.runtime = &fakeRuntime{resolveContainerPID: 4242, podUID: "host-uid"}
	h := &attemptHarness{w: w, pod: pod, volume: newFakeControlVolume(w), containerID: "ctr-abc", now: time.Now().UTC().Truncate(time.Second)}
	w.nowFn = func() time.Time { return h.now }
	w.restoreFn = func(ctx context.Context, _ snapshotruntime.Runtime, _ logr.Logger, _ executor.RestoreRequest, _ executor.RestoreMounter) (executor.RestoreResult, error) {
		h.restores++
		h.deadline, _ = ctx.Deadline()
		h.seen = h.volume.record(t, "host-uid", "main")
		return executor.RestoreResult{PlaceholderHostPID: 4242, RestoredPID: 77}, restoreErr
	}
	w.writeControlSentinelFn = func(_ int, name string, _ []byte) error {
		h.calls = append(h.calls, "write "+name)
		return nil
	}
	w.sendSignalFn = func(logr.Logger, int, syscall.Signal, string) error {
		h.calls = append(h.calls, "kill")
		return nil
	}
	artifact := &restoreArtifact{SnapshotName: "snapshot-a", ContentUID: "content-uid", SourceContainerName: "main"}
	return h, func() error {
		return w.runRestore(context.Background(), pod, &restorePlan{artifact: artifact}, "main", h.containerID, h.now, false)
	}
}

func TestRunRestoreSavesTheFirstAttemptBeforeRunningIt(t *testing.T) {
	for name, tc := range map[string]struct {
		annotation     string
		agentDefault   *int
		wantRetryLimit int
	}{
		"neither sets a limit":                  {wantRetryLimit: 0},
		"the agent's default":                   {agentDefault: ptr.To(3), wantRetryLimit: 3},
		"the Pod's limit over the agent's":      {annotation: "2", agentDefault: ptr.To(3), wantRetryLimit: 2},
		"the Pod's explicit 0 over the agent's": {annotation: "0", agentDefault: ptr.To(3), wantRetryLimit: 0},
	} {
		t.Run(name, func(t *testing.T) {
			annotations := map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"}
			if tc.annotation != "" {
				annotations[podcontract.RestoreRetryLimitAnnotation] = tc.annotation
			}
			h, run := newAttemptHarness(t, annotations, nil)
			h.w.config.Restore.RetryLimit = tc.agentDefault

			require.NoError(t, run())

			want := &restoreAttempts{Limit: tc.wantRetryLimit, Attempt: 1, Deadline: h.now.Add(time.Minute), ContainerID: "ctr-abc"}
			require.NotNil(t, h.seen, "the record must be saved before the attempt runs")
			assert.Equal(t, want.Limit, h.seen.Limit)
			assert.Equal(t, want.Attempt, h.seen.Attempt)
			assert.Equal(t, want.ContainerID, h.seen.ContainerID)
			assert.True(t, want.Deadline.Equal(h.seen.Deadline), "deadline = %s, want %s", h.seen.Deadline, want.Deadline)
			assert.True(t, want.Deadline.Equal(h.deadline), "the attempt's context ends at %s, want the record's %s", h.deadline, want.Deadline)
			assert.Equal(t, []string{"host-uid/main/" + podcontract.RestoreAttemptsFile}, h.volume.writes)
		})
	}
}

// An agent that restarted during an attempt repeats it, as today, but the
// record keeps the first attempt's limit and deadline.
func TestRunRestoreRepeatsAnInterruptedAttemptWithinItsDeadline(t *testing.T) {
	h, run := newAttemptHarness(t, map[string]string{
		podcontract.RestoreFromAnnotation:       "snapshot-a",
		podcontract.RestoreRetryLimitAnnotation: "5",
	}, nil)
	first := h.now.Add(-50 * time.Second).Add(time.Minute)
	h.volume.put(t, "host-uid", "main", restoreAttempts{Limit: 2, Attempt: 1, Deadline: first, ContainerID: "ctr-abc"})

	require.NoError(t, run())

	require.Equal(t, 1, h.restores)
	assert.True(t, first.Equal(h.deadline), "the attempt's context ends at %s, want the first attempt's %s", h.deadline, first)
	assert.Equal(t, 2, h.seen.Limit, "the limit is resolved once")
	assert.Equal(t, 1, h.seen.Attempt)
	assert.True(t, first.Equal(h.seen.Deadline))
}

func TestRunRestoreEndsWhenItsDeadlinePassedBeforeTheAttempt(t *testing.T) {
	for name, before := range map[string]time.Duration{
		"passed":      time.Second,
		"passing now": 0,
	} {
		t.Run(name, func(t *testing.T) {
			h, run := newAttemptHarness(t, map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"}, nil)
			h.volume.put(t, "host-uid", "main", restoreAttempts{Limit: 2, Attempt: 1, Deadline: h.now.Add(-before), ContainerID: "ctr-abc"})

			err := run()

			require.ErrorContains(t, err, "restore timed out at")
			assert.Equal(t, 0, h.restores)
			assert.Equal(t, []string{"write " + podcontract.RestoreFailedFile, "kill"}, h.calls)
		})
	}
}

// A skip request reopens a refused restore, which then starts over: the
// refusal was no attempt, and its deadline has often passed by then.
func TestRunRestoreStartsOverAfterARefusal(t *testing.T) {
	for name, limit := range map[string]string{"without retries": "0", "with retries": "2"} {
		t.Run(name, func(t *testing.T) {
			h, run := newAttemptHarness(t, map[string]string{
				podcontract.RestoreFromAnnotation:       "snapshot-a",
				podcontract.RestoreRetryLimitAnnotation: limit,
			}, nil)
			restore := h.w.restoreFn
			h.w.restoreFn = func(ctx context.Context, rt snapshotruntime.Runtime, log logr.Logger, req executor.RestoreRequest, mounter executor.RestoreMounter) (executor.RestoreResult, error) {
				_, _ = restore(ctx, rt, log, req, mounter)
				return executor.RestoreResult{}, compat.NewIncompatibleError(compat.GateInspect, []compat.Mismatch{{Check: "memory-limit", Source: "32Gi", Target: "1Gi"}})
			}

			var incompatible *compat.IncompatibleError
			require.ErrorAs(t, run(), &incompatible)
			assert.Empty(t, h.calls, "a refusal wrote restore-failed or killed the container")
			assert.NotContains(t, h.volume.files, "host-uid/main/"+podcontract.RestoreAttemptsFile, "the refused attempt is still recorded")

			h.w.restoreFn = restore
			h.now = h.now.Add(10 * time.Minute)
			require.NoError(t, run())

			assert.Equal(t, 2, h.restores)
			assert.Equal(t, 1, h.seen.Attempt)
			assert.True(t, h.now.Add(time.Minute).Equal(h.seen.Deadline), "the reopened restore kept the refused attempt's deadline")
		})
	}
}

// The Pod can rewrite its record. One the agent cannot count from ends the
// restore instead of granting attempts.
func TestRunRestoreEndsOnARecordItCannotUse(t *testing.T) {
	for name, tc := range map[string]struct {
		record  []byte
		readErr error
		want    string
	}{
		"not JSON":         {record: []byte("attempt=1"), want: "restore attempt record \"restore-attempts\" is invalid"},
		"attempt 0":        {record: []byte(`{"limit":2,"attempt":0,"containerID":"ctr-abc"}`), want: "attempt 0 is not positive"},
		"a negative limit": {record: []byte(`{"limit":-5,"attempt":1,"containerID":"ctr-abc"}`), want: "retry limit -5 is negative"},
		"a link":           {readErr: fmt.Errorf("open restore-attempts: %w", syscall.ELOOP), want: "read restore attempt record"},
		"too large":        {record: make([]byte, restoreAttemptsMaxBytes+1), want: "larger than"},
	} {
		t.Run(name, func(t *testing.T) {
			h, run := newAttemptHarness(t, map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"}, nil)
			if tc.record != nil {
				h.volume.put(t, "host-uid", "main", tc.record)
			}
			h.volume.readErr = tc.readErr

			err := run()

			require.ErrorContains(t, err, tc.want)
			assert.Equal(t, 0, h.restores)
			assert.Equal(t, []string{"write " + podcontract.RestoreFailedFile, "kill"}, h.calls)
		})
	}
}

func TestRunRestoreEndsWhenTheAttemptCannotBeSaved(t *testing.T) {
	h, run := newAttemptHarness(t, map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"}, nil)
	h.volume.writeErrs[podcontract.RestoreAttemptsFile] = syscall.ENOSPC

	err := run()

	require.ErrorIs(t, err, syscall.ENOSPC)
	require.ErrorContains(t, err, "save restore attempt record")
	assert.Equal(t, 0, h.restores)
	assert.Equal(t, []string{"write " + podcontract.RestoreFailedFile, "kill"}, h.calls)
}

// The record goes under the Pod's own UID when the runtime cannot name the
// one on the node, as the restore-failed marker does.
func TestRunRestoreKeepsTheRecordUnderThePodsUIDWithoutTheNodes(t *testing.T) {
	h, run := newAttemptHarness(t, map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"}, errors.New("criu restore failed"))
	h.w.runtime = &fakeRuntime{resolveContainerPID: 4242}
	h.w.restoreFn = func(context.Context, snapshotruntime.Runtime, logr.Logger, executor.RestoreRequest, executor.RestoreMounter) (executor.RestoreResult, error) {
		return executor.RestoreResult{}, errors.New("criu restore failed")
	}

	require.Error(t, run())

	assert.Equal(t, []string{"restore-pod-uid/main/" + podcontract.RestoreAttemptsFile}, h.volume.writes)
}

func TestRecoveredPostRestoreKeepsTheRecordsDeadline(t *testing.T) {
	for name, tc := range map[string]struct {
		record     bool
		wantWithin time.Duration
	}{
		"with a record":    {record: true, wantWithin: 2 * time.Second},
		"without a record": {wantWithin: time.Minute},
	} {
		t.Run(name, func(t *testing.T) {
			h := newPostRestoreHarness(t, httpPostRestoreSpec())
			recoveringPod(h)
			h.w.runtime = &fakeRuntime{resolveContainerPID: 4242, podUID: "host-uid"}
			volume := newFakeControlVolume(h.w)
			deadline := time.Now().Add(2 * time.Second)
			if tc.record {
				volume.put(t, "host-uid", "main", restoreAttempts{Limit: 1, Attempt: 1, Deadline: deadline, ContainerID: "ctr-abc"})
			}
			h.httpResults = []int{0}
			var got time.Time
			h.w.postRestoreWaitFn = func(ctx context.Context, _ time.Duration) error {
				got, _ = ctx.Deadline()
				return context.DeadlineExceeded
			}

			require.Error(t, h.run(true))

			assert.Equal(t, 0, h.restores)
			assert.WithinDuration(t, time.Now().Add(tc.wantWithin), got, time.Second)
			if tc.record {
				assert.True(t, deadline.Equal(got), "the action ends at %s, want the record's %s", got, deadline)
			}
		})
	}
}
