// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"github.com/ai-dynamo/snapshot/agent/internal/executor"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/api/compat"
	"github.com/ai-dynamo/snapshot/api/podcontract"
)

// putAttempts keeps record in the agent's memory, as it holds a destination's
// record once an attempt started.
func putAttempts(w *NodeController, uid ktypes.UID, container string, record podcontract.ContainerRetryStatus) {
	w.restoreAttemptRecords.Store(restoreContainerKey(uid, container), record)
}

// savedAttempts returns the destination's record in the agent's memory, nil
// when it has none.
func savedAttempts(w *NodeController, uid ktypes.UID, container string) *podcontract.ContainerRetryStatus {
	value, ok := w.restoreAttemptRecords.Load(restoreContainerKey(uid, container))
	if !ok {
		return nil
	}
	record := value.(podcontract.ContainerRetryStatus)
	return &record
}

// leaveRetryStatus sets the retry status annotation an earlier agent left on
// the Pod.
func leaveRetryStatus(t *testing.T, pod *corev1.Pod, status podcontract.RestoreRetryStatus) {
	t.Helper()
	value, err := podcontract.FormatRestoreRetryStatus(status)
	require.NoError(t, err)
	pod.Annotations[podcontract.RestoreRetryStatusAnnotation] = value
}

// attemptHarness runs one restore attempt of the Pod's "main" container.
type attemptHarness struct {
	w   *NodeController
	pod *corev1.Pod
	// containerID is the destination container a pass finds.
	containerID string
	// recovering is whether a pass finds the restore in progress, as every
	// pass after the first does.
	recovering bool
	// now is the agent's clock. It starts at the real time, since an
	// attempt's context ends by the real clock at the record's deadline.
	now      time.Time
	restores int
	deadline time.Time
	// seen is the record as the attempt saw it when it started.
	seen  *podcontract.ContainerRetryStatus
	calls []string
}

func newAttemptHarness(t *testing.T, annotations map[string]string, restoreErr error) (*attemptHarness, func() error) {
	t.Helper()
	pod := restorePod(annotations)
	w := makeTestController(t, pod)
	w.config.Restore.TimeoutSeconds = 60
	w.runtime = &fakeRuntime{resolveContainerPID: 4242}
	h := &attemptHarness{w: w, pod: pod, containerID: "ctr-abc", now: time.Now().UTC().Truncate(time.Second)}
	w.nowFn = func() time.Time { return h.now }
	w.restoreFn = func(ctx context.Context, _ snapshotruntime.Runtime, _ logr.Logger, _ executor.RestoreRequest, _ executor.RestoreMounter) (executor.RestoreResult, error) {
		h.restores++
		h.deadline, _ = ctx.Deadline()
		h.seen = savedAttempts(w, pod.UID, "main")
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
		return w.runRestore(context.Background(), pod, &restorePlan{artifact: artifact}, "main", h.containerID, h.now, h.recovering)
	}
}

func retryLimit(limit string) map[string]string {
	return map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a", podcontract.RestoreRetryLimitAnnotation: limit}
}

func TestRunRestoreSavesTheFirstAttemptBeforeRunningIt(t *testing.T) {
	for name, tc := range map[string]struct {
		annotation     string
		agentDefault   *int
		wantRetryLimit int
	}{
		"the agent's default":              {agentDefault: ptr.To(3), wantRetryLimit: 3},
		"the Pod's limit over the agent's": {annotation: "2", agentDefault: ptr.To(3), wantRetryLimit: 2},
	} {
		t.Run(name, func(t *testing.T) {
			annotations := map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"}
			if tc.annotation != "" {
				annotations[podcontract.RestoreRetryLimitAnnotation] = tc.annotation
			}
			h, run := newAttemptHarness(t, annotations, nil)
			h.w.config.Restore.RetryLimit = tc.agentDefault

			require.NoError(t, run())

			want := podcontract.ContainerRetryStatus{RetryLimit: tc.wantRetryLimit, Attempt: 1, Deadline: h.now.Add(time.Minute), ContainerID: "ctr-abc", PodUID: h.pod.UID}
			require.NotNil(t, h.seen, "the record must be saved before the attempt runs")
			assert.Equal(t, want, *h.seen)
			assert.True(t, want.Deadline.Equal(h.deadline), "the attempt's context ends at %s, want the record's %s", h.deadline, want.Deadline)
			assert.Equal(t, 1, h.w.retryStatusQueue.Len(), "the attempt did not queue the retry status")
		})
	}
}

// Without retries the restore keeps no record: it reads no retry status,
// writes none, and every pass gets a full timeout.
func TestRunRestoreKeepsNoRecordWithoutRetries(t *testing.T) {
	for name, tc := range map[string]struct {
		annotation   string
		agentDefault *int
		restoreErr   error
	}{
		"neither sets a limit":                  {},
		"the Pod's explicit 0 over the agent's": {annotation: "0", agentDefault: ptr.To(3)},
		"a refusal": {
			restoreErr: compat.NewIncompatibleError(compat.GateInspect, []compat.Mismatch{{Check: "memory-limit", Source: "32Gi", Target: "1Gi"}}),
		},
	} {
		t.Run(name, func(t *testing.T) {
			annotations := map[string]string{
				podcontract.RestoreFromAnnotation:        "snapshot-a",
				podcontract.RestoreRetryStatusAnnotation: "not the retry status",
			}
			if tc.annotation != "" {
				annotations[podcontract.RestoreRetryLimitAnnotation] = tc.annotation
			}
			h, run := newAttemptHarness(t, annotations, tc.restoreErr)
			h.w.config.Restore.RetryLimit = tc.agentDefault

			assert.ErrorIs(t, run(), tc.restoreErr)

			assert.Equal(t, 1, h.restores)
			assert.True(t, h.now.Add(time.Minute).Equal(h.deadline), "the attempt's context ends at %s, want a full timeout", h.deadline)
			assert.Nil(t, savedAttempts(h.w, h.pod.UID, "main"))
			assert.Zero(t, h.w.retryStatusQueue.Len())
		})
	}
}

// An agent that restarted during an attempt repeats it, as today, but the
// retry status it left keeps the first attempt's limit and deadline.
func TestRunRestoreRepeatsAnInterruptedAttemptWithinItsDeadline(t *testing.T) {
	h, run := newAttemptHarness(t, retryLimit("5"), nil)
	first := h.now.Add(-50 * time.Second).Add(time.Minute)
	leaveRetryStatus(t, h.pod, podcontract.RestoreRetryStatus{
		"main": {Attempt: 1, RetryLimit: 2, Deadline: first, ContainerID: "ctr-abc", PodUID: h.pod.UID},
	})

	require.NoError(t, run())

	require.Equal(t, 1, h.restores)
	assert.True(t, first.Equal(h.deadline), "the attempt's context ends at %s, want the first attempt's %s", h.deadline, first)
	assert.Equal(t, 2, h.seen.RetryLimit, "the limit is resolved once")
	assert.Empty(t, h.events(restoreAttemptFailedReason))
	assert.Equal(t, 1, h.seen.Attempt)
	assert.True(t, first.Equal(h.seen.Deadline))
}

// Annotations copied from another Pod, such as from a Pod manifest, carry
// that Pod's attempts, not this one's.
func TestRunRestoreIgnoresTheRetryStatusOfAnotherPod(t *testing.T) {
	h, run := newAttemptHarness(t, retryLimit("2"), nil)
	leaveRetryStatus(t, h.pod, podcontract.RestoreRetryStatus{
		"main": {Attempt: 3, RetryLimit: 2, Deadline: h.now.Add(-time.Hour), ContainerID: "ctr-abc", PodUID: "another-pod-uid"},
	})

	require.NoError(t, run())

	require.Equal(t, 1, h.restores)
	assert.Equal(t, podcontract.ContainerRetryStatus{RetryLimit: 2, Attempt: 1, Deadline: h.now.Add(time.Minute), ContainerID: "ctr-abc", PodUID: h.pod.UID}, *h.seen)
}

func TestRunRestoreEndsWhenItsDeadlinePassedBeforeTheAttempt(t *testing.T) {
	for name, before := range map[string]time.Duration{
		"passed":      time.Second,
		"passing now": 0,
	} {
		t.Run(name, func(t *testing.T) {
			h, run := newAttemptHarness(t, retryLimit("2"), nil)
			putAttempts(h.w, h.pod.UID, "main", podcontract.ContainerRetryStatus{RetryLimit: 2, Attempt: 1, Deadline: h.now.Add(-before), ContainerID: "ctr-abc"})

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
	h, run := newAttemptHarness(t, retryLimit("2"), nil)
	restore := h.w.restoreFn
	h.w.restoreFn = func(ctx context.Context, rt snapshotruntime.Runtime, log logr.Logger, req executor.RestoreRequest, mounter executor.RestoreMounter) (executor.RestoreResult, error) {
		_, _ = restore(ctx, rt, log, req, mounter)
		return executor.RestoreResult{}, compat.NewIncompatibleError(compat.GateInspect, []compat.Mismatch{{Check: "memory-limit", Source: "32Gi", Target: "1Gi"}})
	}

	var incompatible *compat.IncompatibleError
	require.ErrorAs(t, run(), &incompatible)
	assert.Empty(t, h.calls, "a refusal wrote restore-failed or killed the container")
	assert.Nil(t, savedAttempts(h.w, h.pod.UID, "main"), "the refused attempt is still recorded")

	h.w.restoreFn = restore
	h.now = h.now.Add(10 * time.Minute)
	require.NoError(t, run())

	assert.Equal(t, 2, h.restores)
	assert.Equal(t, 1, h.seen.Attempt)
	assert.True(t, h.now.Add(time.Minute).Equal(h.seen.Deadline), "the reopened restore kept the refused attempt's deadline")
}

// Anyone who can update the Pod can rewrite its retry status. One the agent
// cannot count from ends the restore instead of granting attempts.
func TestRunRestoreEndsOnARetryStatusItCannotUse(t *testing.T) {
	for name, tc := range map[string]struct {
		value string
		want  string
	}{
		"not JSON":         {value: "attempt=1", want: "parse " + podcontract.RestoreRetryStatusAnnotation},
		"attempt 0":        {value: `{"main":{"attempt":0,"retryLimit":2}}`, want: "has attempt 0"},
		"a negative limit": {value: `{"main":{"attempt":1,"retryLimit":-5}}`, want: "retry limit -5"},
		"no container ID": {
			value: `{"main":{"attempt":1,"retryLimit":2,"podUID":"restore-pod-uid"}}`,
			want:  `entry for container "main" has no container ID`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			h, run := newAttemptHarness(t, retryLimit("2"), nil)
			h.pod.Annotations[podcontract.RestoreRetryStatusAnnotation] = tc.value

			err := run()

			require.ErrorContains(t, err, tc.want)
			assert.Equal(t, 0, h.restores)
			assert.Equal(t, []string{"write " + podcontract.RestoreFailedFile, "kill"}, h.calls)
		})
	}
}

func newAttemptOperation(t *testing.T, annotations map[string]string) *restoreOperation {
	t.Helper()
	pod := restorePod(annotations)
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	w := makeTestController(t, pod)
	artifact := &restoreArtifact{SnapshotName: "snapshot-a", ContentUID: "content-uid", SourceContainerName: "main"}
	return w.newRestoreOperation(pod, &restorePlan{artifact: artifact}, "main", "ctr-abc", time.Now())
}

func TestRetryLimit(t *testing.T) {
	for name, tc := range map[string]struct {
		annotation   string
		agentDefault *int
		want         int
		wantErr      string
	}{
		"neither sets a limit":                  {want: 0},
		"the agent's default":                   {agentDefault: ptr.To(3), want: 3},
		"the Pod's limit over the agent's":      {annotation: "2", agentDefault: ptr.To(3), want: 2},
		"the Pod's explicit 0 over the agent's": {annotation: "0", agentDefault: ptr.To(3), want: 0},
		"a Pod limit that is not a number":      {annotation: "two", agentDefault: ptr.To(3), wantErr: "must be a non-negative integer"},
		"a negative Pod limit":                  {annotation: "-1", wantErr: "must be a non-negative integer"},
	} {
		t.Run(name, func(t *testing.T) {
			op := newAttemptOperation(t, map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"})
			if tc.annotation != "" {
				op.pod.Annotations[podcontract.RestoreRetryLimitAnnotation] = tc.annotation
			}
			op.controller.config.Restore.RetryLimit = tc.agentDefault

			got, err := op.retryLimit()

			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestLoadAttempts(t *testing.T) {
	inMemory := podcontract.ContainerRetryStatus{RetryLimit: 2, Attempt: 2, ContainerID: "ctr-abc", PodUID: "restore-pod-uid"}
	left := podcontract.ContainerRetryStatus{Attempt: 1, RetryLimit: 3, ContainerID: "ctr-old", PodUID: "restore-pod-uid"}
	for name, tc := range map[string]struct {
		memory    *podcontract.ContainerRetryStatus
		status    podcontract.RestoreRetryStatus
		want      *podcontract.ContainerRetryStatus
		wantFound bool
	}{
		"no record":                  {},
		"a record in memory":         {memory: &inMemory, want: &inMemory, wantFound: true},
		"a record the Pod carries":   {status: podcontract.RestoreRetryStatus{"main": left}, want: &left, wantFound: true},
		"memory over the Pod":        {memory: &inMemory, status: podcontract.RestoreRetryStatus{"main": left}, want: &inMemory, wantFound: true},
		"another container's record": {status: podcontract.RestoreRetryStatus{"sidecar": left}},
	} {
		t.Run(name, func(t *testing.T) {
			op := newAttemptOperation(t, nil)
			if tc.memory != nil {
				putAttempts(op.controller, op.pod.UID, "main", *tc.memory)
			}
			if tc.status != nil {
				leaveRetryStatus(t, op.pod, tc.status)
			}

			got, found, err := op.loadAttempts()

			require.NoError(t, err)
			assert.Equal(t, tc.wantFound, found)
			assert.Equal(t, tc.want, got)
		})
	}
}

// The agent reads a Pod's retry status once, the first time it loads one of
// the Pod's records, and copies every entry, so its own writes keep the
// entries of destinations it has not restored since.
func TestLoadAttemptsReadsThePodOnce(t *testing.T) {
	op := newAttemptOperation(t, nil)
	leaveRetryStatus(t, op.pod, podcontract.RestoreRetryStatus{
		"main":    {Attempt: 2, RetryLimit: 3, ContainerID: "ctr-main", PodUID: op.pod.UID},
		"sidecar": {Attempt: 4, RetryLimit: 3, ContainerID: "ctr-sidecar", PodUID: op.pod.UID},
	})

	_, found, err := op.loadAttempts()
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, &podcontract.ContainerRetryStatus{Attempt: 4, RetryLimit: 3, ContainerID: "ctr-sidecar", PodUID: op.pod.UID}, savedAttempts(op.controller, op.pod.UID, "sidecar"))

	op.pod.Annotations[podcontract.RestoreRetryStatusAnnotation] = "not the retry status"
	got, found, err := op.loadAttempts()
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, &podcontract.ContainerRetryStatus{Attempt: 2, RetryLimit: 3, ContainerID: "ctr-main", PodUID: op.pod.UID}, got)
}

func TestStartAttempt(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		annotation string
		timeout    int
		record     *podcontract.ContainerRetryStatus
		want       *podcontract.ContainerRetryStatus
		wantErr    string
	}{
		"the first attempt": {
			annotation: "2", timeout: 60,
			want: &podcontract.ContainerRetryStatus{RetryLimit: 2, Attempt: 1, Deadline: now.Add(time.Minute), ContainerID: "ctr-abc", PodUID: "restore-pod-uid"},
		},
		"the first attempt without a timeout": {
			annotation: "2",
			want:       &podcontract.ContainerRetryStatus{RetryLimit: 2, Attempt: 1, ContainerID: "ctr-abc", PodUID: "restore-pod-uid"},
		},
		"an interrupted attempt keeps its limit and deadline": {
			annotation: "5", timeout: 60,
			record: &podcontract.ContainerRetryStatus{RetryLimit: 1, Attempt: 2, Deadline: now.Add(30 * time.Second), ContainerID: "ctr-old"},
			want:   &podcontract.ContainerRetryStatus{RetryLimit: 1, Attempt: 2, Deadline: now.Add(30 * time.Second), ContainerID: "ctr-abc"},
		},
		"an interrupted attempt without a timeout": {
			annotation: "1", timeout: 60,
			record: &podcontract.ContainerRetryStatus{RetryLimit: 1, Attempt: 2, ContainerID: "ctr-old"},
			want:   &podcontract.ContainerRetryStatus{RetryLimit: 1, Attempt: 2, ContainerID: "ctr-abc"},
		},
		"the deadline passed": {
			annotation: "1", timeout: 60,
			record:  &podcontract.ContainerRetryStatus{RetryLimit: 1, Attempt: 2, Deadline: now.Add(-time.Second), ContainerID: "ctr-old"},
			wantErr: "restore timed out at 2026-11-09T11:59:59Z, before attempt 2 could start",
		},
		"the deadline is now": {
			annotation: "1", timeout: 60,
			record:  &podcontract.ContainerRetryStatus{RetryLimit: 1, Attempt: 2, Deadline: now, ContainerID: "ctr-old"},
			wantErr: "restore timed out at",
		},
		"an invalid Pod limit": {annotation: "two", timeout: 60, wantErr: "must be a non-negative integer"},
	} {
		t.Run(name, func(t *testing.T) {
			op := newAttemptOperation(t, map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"})
			if tc.annotation != "" {
				op.pod.Annotations[podcontract.RestoreRetryLimitAnnotation] = tc.annotation
			}
			op.controller.config.Restore.TimeoutSeconds = tc.timeout
			op.controller.nowFn = func() time.Time { return now }
			if tc.record != nil {
				putAttempts(op.controller, op.pod.UID, "main", *tc.record)
			}

			got, err := op.startAttempt(context.Background())

			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				assert.Zero(t, op.controller.retryStatusQueue.Len(), "a refused attempt was saved")
				assert.Equal(t, tc.record, savedAttempts(op.controller, op.pod.UID, "main"), "a refused attempt rewrote the record")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.want, savedAttempts(op.controller, op.pod.UID, "main"))
			assert.Equal(t, 1, op.controller.retryStatusQueue.Len())
		})
	}
}

func TestForgetAttempts(t *testing.T) {
	op := newAttemptOperation(t, nil)
	putAttempts(op.controller, op.pod.UID, "main", podcontract.ContainerRetryStatus{RetryLimit: 1, Attempt: 1, ContainerID: "ctr-abc"})
	putAttempts(op.controller, op.pod.UID, "sidecar", podcontract.ContainerRetryStatus{RetryLimit: 1, Attempt: 1, ContainerID: "ctr-sidecar"})

	op.forgetAttempts()

	assert.Nil(t, savedAttempts(op.controller, op.pod.UID, "main"))
	assert.NotNil(t, savedAttempts(op.controller, op.pod.UID, "sidecar"), "another destination's record went too")
	assert.Equal(t, 1, op.controller.retryStatusQueue.Len(), "the Pod's retry status still names the forgotten attempt")
}
