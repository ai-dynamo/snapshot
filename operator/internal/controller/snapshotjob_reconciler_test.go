// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

func snapshotJobReconcilerScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = snapshotv1alpha1.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	_ = batchv1.AddToScheme(s)
	return s
}

func makeSnapshotJobReconciler(s *runtime.Scheme, objs ...client.Object) *SnapshotJobReconciler {
	return makeSnapshotJobReconcilerWithInterceptor(s, interceptor.Funcs{}, objs...)
}

// makeSnapshotJobReconcilerWithInterceptor builds a reconciler whose fake client routes calls
// through interceptor.Funcs, letting tests inject API errors on specific code paths.
func makeSnapshotJobReconcilerWithInterceptor(s *runtime.Scheme, funcs interceptor.Funcs, objs ...client.Object) *SnapshotJobReconciler {
	return &SnapshotJobReconciler{
		Client: fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
			WithStatusSubresource(&snapshotv1alpha1.SnapshotJob{}).
			WithInterceptorFuncs(funcs).Build(),
		Recorder: record.NewFakeRecorder(10),
	}
}

func reconcileRequest(sj *snapshotv1alpha1.SnapshotJob) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: sj.Namespace, Name: sj.Name}}
}

func getSourceJob(t *testing.T, cl client.Client, sj *snapshotv1alpha1.SnapshotJob) *batchv1.Job {
	t.Helper()
	job := &batchv1.Job{}
	err := cl.Get(context.Background(), client.ObjectKey{Namespace: sj.Namespace, Name: sj.Name}, job)
	require.NoError(t, err)
	return job
}

// sourcePodForJob builds a pod matching what the batch/v1 Job controller
// creates for job: batchv1.JobNameLabel plus a controller ownerRef, which
// findSourcePod requires (List by label, then confirm via IsControlledBy).
func sourcePodForJob(job *batchv1.Job) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      job.Name + "-abcde",
			Namespace: job.Namespace,
			Labels:    map[string]string{batchv1.JobNameLabel: job.Name},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1",
				Kind:       "Job",
				Name:       job.Name,
				UID:        job.UID,
				Controller: ptr.To(true),
			}},
		},
	}
}

func TestSnapshotJobReconcileFirstPass(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()
	funcs := interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if job, ok := obj.(*batchv1.Job); ok {
			job.UID = types.UID("source-job-uid")
		}
		return c.Create(ctx, obj, opts...)
	}}
	r := makeSnapshotJobReconcilerWithInterceptor(s, funcs, sj)

	_, err := r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)

	job := getSourceJob(t, r.Client, sj)
	assert.True(t, metav1.IsControlledBy(job, sj), "source Job must carry a controller ownerRef to the SnapshotJob")

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	cond := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionRunning)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, snapshotv1alpha1.ReasonPodPending, cond.Reason)
	assert.Nil(t, updated.Status.StartedAt)
	assert.Equal(t, types.UID("source-job-uid"), updated.Status.SourceJobUID)
}

func TestSnapshotJobReconcileRecoversAfterSourceJobUIDStatusWriteFailure(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()

	jobCreates := 0
	statusPatches := 0
	funcs := interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if job, ok := obj.(*batchv1.Job); ok {
				jobCreates++
				job.UID = types.UID("source-job-uid")
			}
			return c.Create(ctx, obj, opts...)
		},
		SubResourcePatch: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if subResourceName == "status" {
				statusPatches++
				if statusPatches == 1 {
					return errors.New("injected status write failure")
				}
			}
			return c.SubResource(subResourceName).Patch(ctx, obj, patch, opts...)
		},
	}
	r := makeSnapshotJobReconcilerWithInterceptor(s, funcs, sj)

	_, err := r.Reconcile(context.Background(), reconcileRequest(sj))
	require.Error(t, err)

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	assert.Empty(t, updated.Status.SourceJobUID, "the injected failed status patch must not appear persisted")

	_, err = r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	assert.Equal(t, types.UID("source-job-uid"), updated.Status.SourceJobUID)
	assert.Equal(t, 1, jobCreates, "the existing matching Job must be adopted, not recreated after the failed status write")
}

func TestSnapshotJobReconcileInvalidSpecIsTerminal(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()
	sj.Spec.PodSnapshotTemplate.TargetContainers = []string{"does-not-exist"}
	r := makeSnapshotJobReconciler(s, sj)

	_, err := r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err, "terminal validation must not be returned as an error — that would retry forever")

	jobs := &batchv1.JobList{}
	require.NoError(t, r.List(context.Background(), jobs))
	assert.Nil(t, getBatchJobByName(jobs, sj.Name), "no Job should be created for an invalid spec")

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	assert.True(t, snapshotv1alpha1.IsSnapshotJobFailed(updated))
	cond := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionFailed)
	require.NotNil(t, cond)
	assert.Equal(t, snapshotv1alpha1.ReasonInvalidSpec, cond.Reason)
	assert.NotNil(t, updated.Status.CompletedAt,
		"completedAt must be set on this terminal transition — IsSnapshotJobTerminal short-circuits every later reconcile, so this is the only chance")

	running := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionRunning)
	require.NotNil(t, running, "Running must not be entirely absent alongside a terminal Failed=True — missing is not the same as known False")
	assert.Equal(t, metav1.ConditionFalse, running.Status)
	assert.Equal(t, snapshotv1alpha1.ReasonPodPending, running.Reason)

	captured := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionCaptured)
	require.NotNil(t, captured, "Captured must not be entirely absent alongside a terminal Failed=True — missing is not the same as known False")
	assert.Equal(t, metav1.ConditionFalse, captured.Status)
	assert.Equal(t, snapshotv1alpha1.ReasonInvalidSpec, captured.Reason)

	completed := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionCompleted)
	require.NotNil(t, completed, "Completed must not be entirely absent alongside a terminal Failed=True — missing is not the same as known False")
	assert.Equal(t, metav1.ConditionFalse, completed.Status)
	assert.Equal(t, snapshotv1alpha1.ReasonInvalidSpec, completed.Reason)
}

func TestSnapshotJobReconcilePropagatesJobGetError(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()

	funcs := interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if _, ok := obj.(*batchv1.Job); ok {
			return errors.New("transient API error")
		}
		return c.Get(ctx, key, obj, opts...)
	}}
	r := makeSnapshotJobReconcilerWithInterceptor(s, funcs, sj)

	_, err := r.Reconcile(context.Background(), reconcileRequest(sj))
	require.Error(t, err, "a non-NotFound Get error must be returned so controller-runtime requeues with backoff")

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	assert.Empty(t, updated.Status.Conditions, "no status should be written on a retryable Get error")
}

// TestSnapshotJobReconcileAdoptsJobOnAlreadyExistsRace simulates the
// stale-informer-cache race: a prior reconcile's Create already landed the Job
// server-side, but this reconcile's own Get misses it (returns NotFound from a
// cache that hasn't caught up), so Create is attempted again and the server
// returns AlreadyExists. This must self-heal by adopting the existing Job, not
// surface as an error or recorded event.
func TestSnapshotJobReconcileAdoptsJobOnAlreadyExistsRace(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()
	sj.UID = types.UID("sj-uid")

	job, err := buildSourceJob(sj)
	require.NoError(t, err)
	require.NoError(t, controllerutil.SetControllerReference(sj, job, s))
	job.UID = types.UID("source-job-uid")

	getCalls := 0
	funcs := interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if _, ok := obj.(*batchv1.Job); ok {
			getCalls++
			if getCalls == 1 {
				return apierrors.NewNotFound(batchv1.Resource("jobs"), key.Name)
			}
		}
		return c.Get(ctx, key, obj, opts...)
	}}
	r := makeSnapshotJobReconcilerWithInterceptor(s, funcs, sj, job)

	_, err = r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err, "the AlreadyExists race must self-heal, not surface as an error")

	jobs := &batchv1.JobList{}
	require.NoError(t, r.List(context.Background(), jobs))
	assert.Len(t, jobs.Items, 1, "the existing Job must be adopted, not recreated")

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	cond := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionRunning)
	require.NotNil(t, cond, "adopting the existing Job on the AlreadyExists path must still observe it for Running")
	assert.Equal(t, job.UID, updated.Status.SourceJobUID)
}

// TestSnapshotJobReconcileFailsForeignJobOnAlreadyExistsRace is the same
// race as above, but the Job that already exists is not ours.
func TestSnapshotJobReconcileFailsForeignJobOnAlreadyExistsRace(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()
	sj.UID = types.UID("sj-uid")

	foreignJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: sj.Name, Namespace: sj.Namespace},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers:    []corev1.Container{{Name: "other", Image: "test:latest"}},
				},
			},
		},
	}

	getCalls := 0
	funcs := interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if _, ok := obj.(*batchv1.Job); ok {
			getCalls++
			if getCalls == 1 {
				return apierrors.NewNotFound(batchv1.Resource("jobs"), key.Name)
			}
		}
		return c.Get(ctx, key, obj, opts...)
	}}
	r := makeSnapshotJobReconcilerWithInterceptor(s, funcs, sj, foreignJob)

	_, err := r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	assert.True(t, snapshotv1alpha1.IsSnapshotJobFailed(updated),
		"a foreign Job hit via the AlreadyExists race must be classified the same as one found by the initial Get")
	cond := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionFailed)
	require.NotNil(t, cond)
	assert.Equal(t, snapshotv1alpha1.ReasonJobNameConflict, cond.Reason)
	captured := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionCaptured)
	require.NotNil(t, captured, "Captured must not be entirely absent alongside a terminal Failed=True")
	assert.Equal(t, metav1.ConditionFalse, captured.Status)
	completed := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionCompleted)
	require.NotNil(t, completed, "Completed must not be entirely absent alongside a terminal Failed=True")
	assert.Equal(t, metav1.ConditionFalse, completed.Status)
	running := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionRunning)
	require.NotNil(t, running, "Running must not be entirely absent alongside a terminal Failed=True")
	assert.Equal(t, metav1.ConditionFalse, running.Status)

	got := &batchv1.Job{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, got))
	assert.Empty(t, got.OwnerReferences, "the foreign Job must be left untouched")
	assertJobNameConflictEventRecorded(t, r)
}

func TestSnapshotJobReconcileAdoptsOwnedJob(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()
	sj.UID = types.UID("sj-uid")

	job, err := buildSourceJob(sj)
	require.NoError(t, err)
	require.NoError(t, controllerutil.SetControllerReference(sj, job, s))
	job.UID = types.UID("source-job-uid")

	r := makeSnapshotJobReconciler(s, sj, job)

	_, err = r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)

	jobs := &batchv1.JobList{}
	require.NoError(t, r.List(context.Background(), jobs))
	assert.Len(t, jobs.Items, 1, "an already-owned Job must not be recreated")

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	assert.Equal(t, job.UID, updated.Status.SourceJobUID)
}

func TestSnapshotJobReconcileRejectsOwnedJobWithoutImmutableUIDStamp(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()

	job, err := buildSourceJob(sj)
	require.NoError(t, err)
	require.NoError(t, controllerutil.SetControllerReference(sj, job, s))
	delete(job.Spec.Template.Labels, snapshotv1alpha1.SnapshotJobOwnerUIDLabel)
	r := makeSnapshotJobReconciler(s, sj, job)

	_, err = r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	failed := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionFailed)
	require.NotNil(t, failed)
	assert.Equal(t, snapshotv1alpha1.ReasonJobNameConflict, failed.Reason)

	jobs := &batchv1.JobList{}
	require.NoError(t, r.List(context.Background(), jobs))
	assert.Len(t, jobs.Items, 1, "the mismatched Job must be preserved for inspection")
}

func TestSnapshotJobReconcileRejectsSameNameJobWithDifferentUID(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()
	sj.Status.SourceJobUID = types.UID("original-job-uid")

	replacement, err := buildSourceJob(sj)
	require.NoError(t, err)
	require.NoError(t, controllerutil.SetControllerReference(sj, replacement, s))
	replacement.UID = types.UID("replacement-job-uid")
	r := makeSnapshotJobReconciler(s, sj, replacement)

	_, err = r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	failed := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionFailed)
	require.NotNil(t, failed)
	assert.Equal(t, snapshotv1alpha1.ReasonJobNameConflict, failed.Reason)
	assert.Equal(t, types.UID("original-job-uid"), updated.Status.SourceJobUID,
		"the replacement UID must never overwrite the persisted source Job identity")

	snaps := &snapshotv1alpha1.PodSnapshotList{}
	require.NoError(t, r.List(context.Background(), snaps))
	assert.Empty(t, snaps.Items, "a replacement Job must be rejected before capture resources are created")
}

func TestSnapshotJobReconcileObservesTerminalPodSnapshotFromAlreadyExistsRace(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()
	sj.UID = types.UID("sj-uid")
	job, err := buildSourceJob(sj)
	require.NoError(t, err)
	require.NoError(t, controllerutil.SetControllerReference(sj, job, s))
	pod := sourcePodForJob(job)
	snap, err := buildPodSnapshot(sj, pod)
	require.NoError(t, err)
	meta.SetStatusCondition(&snap.Status.Conditions, metav1.Condition{
		Type: snapshotv1alpha1.PodSnapshotConditionReady, Status: metav1.ConditionTrue, Reason: "Captured",
	})

	listCalls := 0
	funcs := interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if snaps, ok := list.(*snapshotv1alpha1.PodSnapshotList); ok {
			listCalls++
			if listCalls == 1 {
				snaps.Items = nil // informer cache misses the already-created object
				return nil
			}
		}
		return c.List(ctx, list, opts...)
	}}
	r := makeSnapshotJobReconcilerWithInterceptor(s, funcs, sj, job, pod, snap)

	_, err = r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	captured := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionCaptured)
	require.NotNil(t, captured)
	assert.Equal(t, metav1.ConditionTrue, captured.Status)
	assert.Equal(t, snapshotv1alpha1.ReasonCaptureCompleted, captured.Reason)
}

func TestSnapshotJobReconcileRunningTransitionsOnJobReady(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()
	sj.UID = types.UID("sj-uid")

	job, err := buildSourceJob(sj)
	require.NoError(t, err)
	require.NoError(t, controllerutil.SetControllerReference(sj, job, s))
	job.Status.Ready = ptr.To(int32(1))

	// Running is only set in the observe phase, reached once a PodSnapshot
	// already exists for this SnapshotJob — pre-seed both the source pod (so
	// PodSnapshot creation, tested separately, isn't what's under test here)
	// and an already-owned PodSnapshot to land straight in observe().
	pod := sourcePodForJob(job)
	snap, err := buildPodSnapshot(sj, pod)
	require.NoError(t, err)

	r := makeSnapshotJobReconciler(s, sj, job, pod, snap)

	_, err = r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	cond := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionRunning)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, snapshotv1alpha1.ReasonPodReady, cond.Reason)
	require.NotNil(t, updated.Status.StartedAt)
	startedAt := *updated.Status.StartedAt

	// Reconciling again while still ready must not rewrite startedAt.
	_, err = r.Reconcile(context.Background(), reconcileRequest(updated))
	require.NoError(t, err)
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	assert.Equal(t, startedAt, *updated.Status.StartedAt)

	// The pod going unready again (e.g. a container restart) must flip Running
	// back to False, but must not touch the already-recorded startedAt.
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, job))
	job.Status.Ready = ptr.To(int32(0))
	require.NoError(t, r.Status().Update(context.Background(), job))

	_, err = r.Reconcile(context.Background(), reconcileRequest(updated))
	require.NoError(t, err)
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	cond = meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionRunning)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, snapshotv1alpha1.ReasonPodPending, cond.Reason)
	assert.Equal(t, startedAt, *updated.Status.StartedAt)
}

// TestSnapshotJobReconcileSetsStartedAtWhenRunningAlreadyPersisted covers a case
// setCondition's return value alone can't catch: Running=True/PodReady is
// already persisted (e.g. status was pre-loaded, or a prior write raced) while
// StartedAt is still nil. setCondition would then report no change, and without
// explicitly OR-ing in the StartedAt assignment, that timestamp would be
// silently dropped on every reconcile from then on.
func TestSnapshotJobReconcileSetsStartedAtWhenRunningAlreadyPersisted(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()
	sj.UID = types.UID("sj-uid")
	meta.SetStatusCondition(&sj.Status.Conditions, metav1.Condition{
		Type: snapshotv1alpha1.SnapshotJobConditionRunning, Status: metav1.ConditionTrue,
		Reason: snapshotv1alpha1.ReasonPodReady, Message: "source pod is ready",
	})
	require.Nil(t, sj.Status.StartedAt, "precondition: StartedAt must start nil")

	job, err := buildSourceJob(sj)
	require.NoError(t, err)
	require.NoError(t, controllerutil.SetControllerReference(sj, job, s))
	job.Status.Ready = ptr.To(int32(1))
	pod := sourcePodForJob(job)
	snap, err := buildPodSnapshot(sj, pod)
	require.NoError(t, err)

	r := makeSnapshotJobReconciler(s, sj, job, pod, snap)

	_, err = r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	require.NotNil(t, updated.Status.StartedAt,
		"StartedAt must be persisted even though Running=True/PodReady was already set and setCondition reports no change")
}

func TestSnapshotJobReconcileFailsForeignJob(t *testing.T) {
	s := snapshotJobReconcilerScheme()
	sj := minimalSnapshotJob()
	sj.UID = types.UID("sj-uid")

	foreignJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: sj.Name, Namespace: sj.Namespace},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers:    []corev1.Container{{Name: "other", Image: "test:latest"}},
				},
			},
		},
	}
	r := makeSnapshotJobReconciler(s, sj, foreignJob)

	_, err := r.Reconcile(context.Background(), reconcileRequest(sj))
	require.NoError(t, err)

	updated := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, updated))
	assert.True(t, snapshotv1alpha1.IsSnapshotJobFailed(updated))
	cond := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionFailed)
	require.NotNil(t, cond)
	assert.Equal(t, snapshotv1alpha1.ReasonJobNameConflict, cond.Reason)
	captured := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionCaptured)
	require.NotNil(t, captured, "Captured must not be entirely absent alongside a terminal Failed=True")
	assert.Equal(t, metav1.ConditionFalse, captured.Status)
	completed := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionCompleted)
	require.NotNil(t, completed, "Completed must not be entirely absent alongside a terminal Failed=True")
	assert.Equal(t, metav1.ConditionFalse, completed.Status)
	running := meta.FindStatusCondition(updated.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionRunning)
	require.NotNil(t, running, "Running must not be entirely absent alongside a terminal Failed=True")
	assert.Equal(t, metav1.ConditionFalse, running.Status)
	require.NotNil(t, updated.Status.CompletedAt)

	got := &batchv1.Job{}
	require.NoError(t, r.Get(context.Background(), reconcileRequest(sj).NamespacedName, got))
	assert.Empty(t, got.OwnerReferences, "the foreign Job must be left untouched, not adopted")
	assertJobNameConflictEventRecorded(t, r)
}

// assertJobNameConflictEventRecorded confirms a JobNameConflict event was
// emitted — the only operator-visible signal for a foreign Job name conflict
// until the completion gate (a later phase) adds a terminal status write.
func assertJobNameConflictEventRecorded(t *testing.T, r *SnapshotJobReconciler) {
	t.Helper()
	fr, ok := r.Recorder.(*record.FakeRecorder)
	require.True(t, ok)
	select {
	case e := <-fr.Events:
		assert.Contains(t, e, snapshotv1alpha1.ReasonJobNameConflict)
	default:
		t.Fatal("expected a JobNameConflict event to be recorded")
	}
}

func TestSnapshotJobReconcileSkipsTerminalAndDeleted(t *testing.T) {
	t.Run("terminal SnapshotJob is not reconciled", func(t *testing.T) {
		s := snapshotJobReconcilerScheme()
		sj := minimalSnapshotJob()
		meta.SetStatusCondition(&sj.Status.Conditions, metav1.Condition{
			Type: snapshotv1alpha1.SnapshotJobConditionFailed, Status: metav1.ConditionTrue, Reason: snapshotv1alpha1.ReasonJobFailed,
		})
		r := makeSnapshotJobReconciler(s, sj)

		_, err := r.Reconcile(context.Background(), reconcileRequest(sj))
		require.NoError(t, err)

		jobs := &batchv1.JobList{}
		require.NoError(t, r.List(context.Background(), jobs))
		assert.Nil(t, getBatchJobByName(jobs, sj.Name), "a terminal SnapshotJob must not create a Job")
	})

	t.Run("SnapshotJob with a deletion timestamp is not reconciled", func(t *testing.T) {
		s := snapshotJobReconcilerScheme()
		sj := minimalSnapshotJob()
		now := metav1.Now()
		sj.DeletionTimestamp = &now
		sj.Finalizers = []string{"nvidia.com/placeholder"} // fake client rejects a bare delete-in-flight object without one
		r := makeSnapshotJobReconciler(s, sj)

		_, err := r.Reconcile(context.Background(), reconcileRequest(sj))
		require.NoError(t, err)

		jobs := &batchv1.JobList{}
		require.NoError(t, r.List(context.Background(), jobs))
		assert.Nil(t, getBatchJobByName(jobs, sj.Name))
	})

	t.Run("SnapshotJob not found is a no-op", func(t *testing.T) {
		s := snapshotJobReconcilerScheme()
		r := makeSnapshotJobReconciler(s)

		_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "inference", Name: "gone"}})
		require.NoError(t, err)
	})
}
