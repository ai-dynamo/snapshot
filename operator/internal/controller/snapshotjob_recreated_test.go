// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// snapshotJobCreatedAgain returns a SnapshotJob that was created age ago under the name of a
// deleted SnapshotJob.
func snapshotJobCreatedAgain(age time.Duration) *snapshotv1alpha1.SnapshotJob {
	sj := minimalSnapshotJob()
	sj.CreationTimestamp = metav1.NewTime(time.Now().Add(-age))
	return sj
}

// sourceJobOfDeletedSnapshotJob returns the source Job of a SnapshotJob that had sj's name and is
// gone. The garbage collector removes that Job a moment after its SnapshotJob.
func sourceJobOfDeletedSnapshotJob(t *testing.T, sj *snapshotv1alpha1.SnapshotJob) *batchv1.Job {
	t.Helper()
	deleted := sj.DeepCopy()
	deleted.UID = types.UID("deleted-sj-uid")
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: sj.Name, Namespace: sj.Namespace, UID: types.UID("deleted-sj-job-uid")}}
	require.NoError(t, controllerutil.SetControllerReference(deleted, job, snapshotJobReconcilerScheme()))
	return job
}

func readSnapshotJob(t *testing.T, r *SnapshotJobReconciler, sj *snapshotv1alpha1.SnapshotJob) *snapshotv1alpha1.SnapshotJob {
	t.Helper()
	current := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, current))
	return current
}

func recordedEvents(t *testing.T, r *SnapshotJobReconciler) []string {
	t.Helper()
	recorder, ok := r.Recorder.(*record.FakeRecorder)
	require.True(t, ok)
	var events []string
	for {
		select {
		case event := <-recorder.Events:
			events = append(events, event)
		default:
			return events
		}
	}
}

// requireWaitingForPreviousJob checks what a user sees while the SnapshotJob waits: no failure,
// and a Running condition that names the wait and the Job that is waited for.
func requireWaitingForPreviousJob(t *testing.T, r *SnapshotJobReconciler, sj *snapshotv1alpha1.SnapshotJob, previous *batchv1.Job) *snapshotv1alpha1.SnapshotJob {
	t.Helper()
	waiting := readSnapshotJob(t, r, sj)
	assert.False(t, snapshotv1alpha1.IsSnapshotJobFailed(waiting), "a name that is being freed is not a conflict")
	running := meta.FindStatusCondition(waiting.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionRunning)
	require.NotNil(t, running, "the SnapshotJob must say that it waits")
	assert.Equal(t, metav1.ConditionFalse, running.Status)
	assert.Equal(t, snapshotv1alpha1.ReasonWaitingForPreviousJobDeletion, running.Reason)
	assert.Contains(t, running.Message, string(previous.UID))
	assert.Contains(t, running.Message, previousSourceJobDeletionWaitEnd(waiting), "the status says when the wait ends")
	assert.Empty(t, waiting.Status.SourceJobUID, "the Job of the deleted SnapshotJob is never recorded")
	assert.Empty(t, recordedEvents(t, r), "a wait is not a failure")
	return waiting
}

// A SnapshotJob that is deleted and created again under its name can find the source Job of the
// deleted one. Kubernetes deletes that Job, so the name is being freed: the new SnapshotJob says
// that it waits, and creates its own source Job when the old one is gone.
func TestSnapshotJobReconcileWaitsForTheSourceJobOfADeletedSnapshotJob(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	ctx := context.Background()
	sj := snapshotJobCreatedAgain(0)
	leftover := sourceJobOfDeletedSnapshotJob(t, sj)
	r := makeSnapshotJobReconciler(s, sj, leftover)

	result, err := r.Reconcile(ctx, reconcileRequest(sj))
	require.NoError(t, err)

	// Half of the wait is the margin for a slow test run. A backstop requeue would be far below it.
	assert.Greater(t, result.RequeueAfter, previousSourceJobDeletionWait/2, "the requeue ends the wait")
	assert.LessOrEqual(t, result.RequeueAfter, previousSourceJobDeletionWait)
	waiting := requireWaitingForPreviousJob(t, r, sj, leftover)
	assert.Equal(t, leftover.UID, getSourceJob(t, r.Client, sj).UID, "the Job of the deleted SnapshotJob is left to the garbage collector")

	_, err = r.Reconcile(ctx, reconcileRequest(sj))
	require.NoError(t, err)
	assert.Equal(t, waiting.ResourceVersion, readSnapshotJob(t, r, sj).ResourceVersion, "the wait is written once")

	require.NoError(t, r.Delete(ctx, leftover))
	_, err = r.Reconcile(ctx, reconcileRequest(sj))
	require.NoError(t, err)

	created := getSourceJob(t, r.Client, sj)
	assert.True(t, metav1.IsControlledBy(created, sj), "the new SnapshotJob creates its own source Job")
	started := readSnapshotJob(t, r, sj)
	assert.False(t, snapshotv1alpha1.IsSnapshotJobFailed(started))
	assert.Equal(t, created.UID, started.Status.SourceJobUID)
	running := meta.FindStatusCondition(started.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionRunning)
	require.NotNil(t, running)
	assert.Equal(t, snapshotv1alpha1.ReasonPodPending, running.Reason, "the wait reason ends with the wait")
}

// The wait has an end. A source Job of a deleted SnapshotJob that is still there when the wait is
// over is held by something, and the new SnapshotJob fails with the documented name conflict.
func TestSnapshotJobReconcileFailsWhenTheSourceJobOfADeletedSnapshotJobStays(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	ctx := context.Background()

	tests := []struct {
		name string
		hold func(job *batchv1.Job)
	}{
		{name: "Kubernetes did not delete the Job", hold: func(*batchv1.Job) {}},
		{name: "the Job is being deleted and a finalizer keeps it", hold: func(job *batchv1.Job) {
			job.Finalizers = []string{"example.com/hold"}
			job.DeletionTimestamp = ptr.To(metav1.Now())
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			left := time.Minute
			sj := snapshotJobCreatedAgain(previousSourceJobDeletionWait - left)
			held := sourceJobOfDeletedSnapshotJob(t, sj)
			test.hold(held)
			r := makeSnapshotJobReconciler(s, sj, held)

			result, err := r.Reconcile(ctx, reconcileRequest(sj))
			require.NoError(t, err)
			assert.Greater(t, result.RequeueAfter, left/2, "the requeue ends the wait")
			assert.LessOrEqual(t, result.RequeueAfter, left)
			requireWaitingForPreviousJob(t, r, sj, held)

			sj = snapshotJobCreatedAgain(previousSourceJobDeletionWait + time.Second)
			r = makeSnapshotJobReconciler(s, sj, held)

			result, err = r.Reconcile(ctx, reconcileRequest(sj))
			require.NoError(t, err)
			assert.Zero(t, result.RequeueAfter, "a failed SnapshotJob is not reconciled again")
			failed := meta.FindStatusCondition(readSnapshotJob(t, r, sj).Status.Conditions, snapshotv1alpha1.SnapshotJobConditionFailed)
			require.NotNil(t, failed)
			assert.Equal(t, metav1.ConditionTrue, failed.Status)
			assert.Equal(t, snapshotv1alpha1.ReasonJobNameConflict, failed.Reason)
			assert.Contains(t, failed.Message, string(held.UID))
			assert.Contains(t, failed.Message, "was not deleted within "+previousSourceJobDeletionWait.String())
			events := recordedEvents(t, r)
			require.Len(t, events, 1)
			assert.Contains(t, events[0], snapshotv1alpha1.ReasonJobNameConflict)

			untouched := getSourceJob(t, r.Client, sj)
			assert.Equal(t, held.UID, untouched.UID, "the Job is never deleted")
			assert.Equal(t, held.OwnerReferences, untouched.OwnerReferences, "the Job is never adopted")
			assert.Equal(t, held.Finalizers, untouched.Finalizers)
		})
	}
}

// The wait counts from the creation of the SnapshotJob, so that it needs no status write and an
// operator restart does not start it again.
func TestPreviousSourceJobDeletionWaitLeft(t *testing.T) {
	created := time.Date(2026, time.November, 27, 6, 0, 0, 0, time.UTC)
	sj := minimalSnapshotJob()
	sj.CreationTimestamp = metav1.NewTime(created)

	tests := []struct {
		name string
		now  time.Time
		want time.Duration
	}{
		{name: "at the creation", now: created, want: previousSourceJobDeletionWait},
		{name: "one second before the end", now: created.Add(previousSourceJobDeletionWait - time.Second), want: time.Second},
		{name: "at the end", now: created.Add(previousSourceJobDeletionWait), want: 0},
		{name: "after the end", now: created.Add(time.Hour), want: 0},
		{name: "the operator clock is behind the API server", now: created.Add(-5 * time.Second), want: previousSourceJobDeletionWait + 5*time.Second},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, previousSourceJobDeletionWaitLeft(sj, test.now))
		})
	}

	assert.Zero(t, previousSourceJobDeletionWaitLeft(minimalSnapshotJob(), created), "no creation time means no wait")
}

// The wait writes a status, so the cached SnapshotJob can be older than that write when the old
// Job goes. The source Job is then created only from a current SnapshotJob, and a reconcile that
// starts from the older copy creates and writes nothing.
func TestSnapshotJobReconcileStartsAfterTheWaitOnlyFromACurrentSnapshotJob(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	ctx := context.Background()
	sj := snapshotJobCreatedAgain(0)
	leftover := sourceJobOfDeletedSnapshotJob(t, sj)

	// The API server: the wait is written there.
	reads := 0
	api := snapshotJobAPIReader(s, &reads, sj.DeepCopy(), leftover.DeepCopy())
	onAPIServer := &SnapshotJobReconciler{Client: api, NonCacheReadClient: api, Recorder: record.NewFakeRecorder(10)}
	_, err := onAPIServer.Reconcile(ctx, reconcileRequest(sj))
	require.NoError(t, err)
	requireWaitingForPreviousJob(t, onAPIServer, sj, leftover)

	// The cache: the old Job is gone, and the SnapshotJob is the copy from before that write.
	r := makeSnapshotJobReconciler(s, sj)
	r.NonCacheReadClient = api

	result, err := r.Reconcile(ctx, reconcileRequest(sj))
	require.NoError(t, err)
	assert.Equal(t, snapshotJobCacheBehindRequeue, result.RequeueAfter)
	assert.Zero(t, countSourceJobs(t, r.Client), "a stale SnapshotJob must not start the workload")
	assert.Empty(t, readSnapshotJob(t, r, sj).Status.Conditions, "nothing is written from a stale SnapshotJob")

	// The cache caught up.
	current := readSnapshotJob(t, onAPIServer, sj)
	r = makeSnapshotJobReconciler(s, current)
	r.NonCacheReadClient = api

	_, err = r.Reconcile(ctx, reconcileRequest(sj))
	require.NoError(t, err)
	created := getSourceJob(t, r.Client, sj)
	assert.True(t, metav1.IsControlledBy(created, sj))
	assert.Equal(t, created.UID, readSnapshotJob(t, r, sj).Status.SourceJobUID)
}

// Only the Job of a previous SnapshotJob of this name is waited for. Every other Job at the name
// is a conflict at once, as before, also for a SnapshotJob that was created a moment ago.
func TestSnapshotJobReconcileKeepsTheNameConflictForOtherJobs(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	ctx := context.Background()

	tests := []struct {
		name   string
		change func(sj *snapshotv1alpha1.SnapshotJob, job *batchv1.Job)
	}{
		{name: "the Job has no controller", change: func(_ *snapshotv1alpha1.SnapshotJob, job *batchv1.Job) {
			job.OwnerReferences = nil
		}},
		{name: "the owner reference is not a controller reference", change: func(_ *snapshotv1alpha1.SnapshotJob, job *batchv1.Job) {
			job.OwnerReferences[0].Controller = ptr.To(false)
		}},
		{name: "the controller is another kind of this API group", change: func(_ *snapshotv1alpha1.SnapshotJob, job *batchv1.Job) {
			job.OwnerReferences[0].Kind = "PodSnapshot"
		}},
		{name: "the controller is a SnapshotJob kind of another API group", change: func(_ *snapshotv1alpha1.SnapshotJob, job *batchv1.Job) {
			job.OwnerReferences[0].APIVersion = "example.com/v1alpha1"
		}},
		{name: "the controller is a SnapshotJob of another name", change: func(_ *snapshotv1alpha1.SnapshotJob, job *batchv1.Job) {
			job.OwnerReferences[0].Name = "another-snapshotjob"
		}},
		{name: "this SnapshotJob recorded its own source Job", change: func(sj *snapshotv1alpha1.SnapshotJob, _ *batchv1.Job) {
			sj.Status.SourceJobUID = types.UID("recorded-job-uid")
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sj := snapshotJobCreatedAgain(0)
			job := sourceJobOfDeletedSnapshotJob(t, sj)
			require.Equal(t, ptr.To(true), job.OwnerReferences[0].Controller)
			test.change(sj, job)
			r := makeSnapshotJobReconciler(s, sj, job)

			result, err := r.Reconcile(ctx, reconcileRequest(sj))
			require.NoError(t, err)
			assert.Zero(t, result.RequeueAfter)

			failed := meta.FindStatusCondition(readSnapshotJob(t, r, sj).Status.Conditions, snapshotv1alpha1.SnapshotJobConditionFailed)
			require.NotNil(t, failed)
			assert.Equal(t, metav1.ConditionTrue, failed.Status)
			assert.Equal(t, snapshotv1alpha1.ReasonJobNameConflict, failed.Reason)
			assert.Equal(t, `an object not controlled by this SnapshotJob already holds the name "warm-worker"`, failed.Message)
			assert.Equal(t, job.UID, getSourceJob(t, r.Client, sj).UID)
		})
	}
}
