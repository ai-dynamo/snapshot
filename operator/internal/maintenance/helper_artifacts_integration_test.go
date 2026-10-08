// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package maintenance_test

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ai-dynamo/snapshot/api/podcontract"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/ai-dynamo/snapshot/operator/internal/controller"
	"github.com/ai-dynamo/snapshot/operator/internal/maintenance"
	"github.com/ai-dynamo/snapshot/operator/internal/maintenance/backends"
	operatortypes "github.com/ai-dynamo/snapshot/operator/internal/types"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// Exercise the real aggregate reconciler and a newly started maintenance
// runnable, modeling a crash after Failed was persisted but before rollback.
func TestPublishedHelperArtifactCaptureFailureAndMaintenanceRestart(t *testing.T) {
	for _, tc := range []struct {
		name    string
		policy  snapshotv1alpha1.SnapshotJobOnFailurePolicy
		cleanup bool
	}{
		{"omitted retains", "", false},
		{"explicit retain", snapshotv1alpha1.SnapshotJobOnFailureRetain, false},
		{"explicit cleanup", snapshotv1alpha1.SnapshotJobOnFailureCleanupHelpers, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testPublishedHelperFailureAndRestart(t, tc.policy, tc.cleanup)
		})
	}
}

func testPublishedHelperFailureAndRestart(t *testing.T, policy snapshotv1alpha1.SnapshotJobOnFailurePolicy, cleanup bool) {
	t.Helper()
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, snapshotv1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, batchv1.AddToScheme(scheme))
	sj := &snapshotv1alpha1.SnapshotJob{
		ObjectMeta: metav1.ObjectMeta{Name: "checkpoint", Namespace: "test", UID: "attempt-uid"},
		Spec: snapshotv1alpha1.SnapshotJobSpec{
			OnFailurePolicy: policy,
			PodTemplate: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{podcontract.HelperArtifactContainersAnnotation: "saver"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "worker"}, {Name: "saver"}}},
			},
			PodSnapshotTemplate: snapshotv1alpha1.PodSnapshotTemplate{TargetContainers: []string{"worker"}},
		},
	}
	var podReads atomic.Int32
	var sweepReads atomic.Int32
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sj).
		WithStatusSubresource(&snapshotv1alpha1.SnapshotJob{}, &snapshotv1alpha1.PodSnapshot{}, &batchv1.Job{}, &corev1.Pod{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				switch obj.(type) {
				case *batchv1.Job:
					obj.SetUID("job-uid")
				case *snapshotv1alpha1.PodSnapshot:
					obj.SetUID("snapshot-uid")
				}
				return c.Create(ctx, obj, opts...)
			},
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*snapshotv1alpha1.SnapshotJobList); ok {
					sweepReads.Add(1)
				}
				if _, ok := list.(*corev1.PodList); ok {
					podReads.Add(1)
				}
				if metadata, ok := list.(*metav1.PartialObjectMetadataList); ok {
					// Fake clients lack metadata-only server list resourceVersions.
					metadata.ResourceVersion = "1"
					return nil
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()
	recorder := record.NewFakeRecorder(20)
	r := &controller.SnapshotJobReconciler{Client: cl, NonCacheReadClient: cl, Recorder: recorder}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sj)}
	_, err := r.Reconcile(ctx, req)
	require.NoError(t, err)
	job := &batchv1.Job{}
	require.NoError(t, cl.Get(ctx, req.NamespacedName, job))
	labels := maps.Clone(job.Spec.Template.Labels)
	labels[batchv1.JobNameLabel] = job.Name
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "source", Namespace: sj.Namespace, UID: "pod-uid", Labels: labels,
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: ptr.To(true)}}},
		Spec: job.Spec.Template.Spec,
	}
	require.NoError(t, cl.Create(ctx, pod))
	_, err = r.Reconcile(ctx, req) // creates and records the PodSnapshot
	require.NoError(t, err)
	base := t.TempDir()
	root := filepath.Join(base, podcontract.HelperArtifactsDirectory, string(sj.UID))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "gms", "device-0"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "gms", "device-0", "manifest.json"), []byte("published"), 0o600))

	snap := &snapshotv1alpha1.PodSnapshot{}
	require.NoError(t, cl.Get(ctx, req.NamespacedName, snap))
	snap.Status.Conditions = []metav1.Condition{{Type: snapshotv1alpha1.PodSnapshotConditionFailed,
		Status: metav1.ConditionTrue, Reason: "CRIUDumpFailed", Message: "capture failed after helper published"}}
	require.NoError(t, cl.Status().Update(ctx, snap))
	_, err = r.Reconcile(ctx, req) // persist aggregate Failed before rollback
	require.NoError(t, err)
	failed := &snapshotv1alpha1.SnapshotJob{}
	require.NoError(t, cl.Get(ctx, req.NamespacedName, failed))
	require.True(t, snapshotv1alpha1.IsSnapshotJobFailed(failed))
	require.DirExists(t, root)

	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
	require.NoError(t, cl.Status().Update(ctx, job))
	pod.Status.Phase = corev1.PodRunning
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{
		{Name: "worker", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137}}},
		{Name: "saver", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
	}
	require.NoError(t, cl.Status().Update(ctx, pod))
	completed := &snapshotv1alpha1.SnapshotJob{
		ObjectMeta: metav1.ObjectMeta{Name: "completed", Namespace: sj.Namespace, UID: "completed-attempt"},
	}
	require.NoError(t, cl.Create(ctx, completed))
	completed.Status.Conditions = []metav1.Condition{{Type: snapshotv1alpha1.SnapshotJobConditionCompleted, Status: metav1.ConditionTrue}}
	require.NoError(t, cl.Status().Update(ctx, completed))
	otherRoot := filepath.Join(base, podcontract.HelperArtifactsDirectory, string(completed.UID))
	require.NoError(t, os.MkdirAll(otherRoot, 0o750))
	q, err := maintenance.NewQueue(cl, cl, recorder, operatortypes.ArtifactCleanupConfig{
		BasePath: base, ScanInterval: 20 * time.Millisecond, BatchSize: 10, ListAttempts: 3, Workers: 1, BackendType: backends.NamePVC,
	})
	require.NoError(t, err)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	finished := make(chan error, 1)
	readsBeforeRestart := podReads.Load()
	sweepsBeforeRestart := sweepReads.Load()
	go func() { finished <- q.Start(runCtx) }()
	require.Eventually(t, func() bool { return sweepReads.Load() > sweepsBeforeRestart }, 5*time.Second, 10*time.Millisecond,
		"maintenance must scan the failed attempt after a restart")
	if cleanup {
		require.Eventually(t, func() bool { return podReads.Load() > readsBeforeRestart }, 5*time.Second, 10*time.Millisecond,
			"explicit cleanup must inspect the retained writer")
	}
	require.DirExists(t, root, "Failed and JobFailed must not delete a helper that is still writing")
	pod.Status.Phase = corev1.PodFailed
	pod.Status.ContainerStatuses[1].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}
	require.NoError(t, cl.Status().Update(ctx, pod))
	if cleanup {
		require.Eventually(t, func() bool { _, err := os.Lstat(root); return os.IsNotExist(err) }, 5*time.Second, 10*time.Millisecond,
			"periodic scan must reclaim without a fresh reconcile callback")
	} else {
		sweepsAfterStop := sweepReads.Load()
		require.Eventually(t, func() bool { return sweepReads.Load() > sweepsAfterStop }, 5*time.Second, 10*time.Millisecond,
			"periodic retention must be exercised after all writers stop")
		require.DirExists(t, root, "omitted/Retain policy preserves stopped writers' diagnostic bytes")
	}
	q.EnqueueSweep()
	require.DirExists(t, otherRoot, "completed attempt must remain available")
	cancel()
	require.NoError(t, <-finished)
	require.NoError(t, cl.Get(ctx, req.NamespacedName, job), "source Job preserved for debugging")
	require.NoError(t, cl.Get(ctx, req.NamespacedName, snap), "capture failure evidence preserved")
	require.NoError(t, cl.Get(ctx, req.NamespacedName, failed))
	require.True(t, snapshotv1alpha1.IsSnapshotJobFailed(failed))
	var reclaimed bool
	for len(recorder.Events) > 0 {
		if strings.Contains(<-recorder.Events, "HelperArtifactsReclaimed") {
			reclaimed = true
		}
	}
	require.Equal(t, cleanup, reclaimed, "only explicit cleanup emits reclamation evidence")
}
