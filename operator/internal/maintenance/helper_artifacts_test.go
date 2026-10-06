// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package maintenance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/ai-dynamo/snapshot/api/podcontract"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

func failedHelperAttempt() (*snapshotv1alpha1.SnapshotJob, *batchv1.Job, *corev1.Pod) {
	sj := &snapshotv1alpha1.SnapshotJob{
		ObjectMeta: metav1.ObjectMeta{Name: "checkpoint", Namespace: "test", UID: "attempt-uid"},
		Spec: snapshotv1alpha1.SnapshotJobSpec{OnFailurePolicy: snapshotv1alpha1.SnapshotJobOnFailureCleanupHelpers, PodTemplate: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{podcontract.HelperArtifactContainersAnnotation: "saver"}},
		}},
		Status: snapshotv1alpha1.SnapshotJobStatus{SourceJobUID: "job-uid", Conditions: []metav1.Condition{{
			Type: snapshotv1alpha1.SnapshotJobConditionFailed, Status: metav1.ConditionTrue, Reason: "CaptureFailed",
		}}},
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: sj.Name, Namespace: sj.Namespace, UID: sj.Status.SourceJobUID,
			OwnerReferences: []metav1.OwnerReference{{APIVersion: snapshotv1alpha1.GroupVersion.String(), Kind: "SnapshotJob", Name: sj.Name, UID: sj.UID, Controller: ptr.To(true)}}},
		Spec: batchv1.JobSpec{BackoffLimit: ptr.To(int32(0)), Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{snapshotv1alpha1.SnapshotJobOwnerUIDLabel: string(sj.UID)},
				Annotations: map[string]string{podcontract.HelperArtifactContainersAnnotation: "saver"}},
			Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, Containers: []corev1.Container{
				{Name: "worker"}, {Name: "saver", Env: []corev1.EnvVar{
					{Name: podcontract.SnapshotJobUIDEnv, Value: string(sj.UID)},
					{Name: podcontract.HelperArtifactSubdirEnv, Value: "helper-artifacts/attempt-uid"},
				}},
			}},
		}},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "source", Namespace: sj.Namespace, UID: "pod-uid",
			Labels:          job.Spec.Template.Labels,
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: ptr.To(true)}}},
		Spec: job.Spec.Template.Spec,
		Status: corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{
			{Name: "worker", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137}}},
			{Name: "saver", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
		}},
	}
	return sj, job, pod
}

func writeHelperArtifact(t *testing.T, base, uid string) string {
	t.Helper()
	root := filepath.Join(base, podcontract.HelperArtifactsDirectory, uid)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "gms", "device-0"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "gms", "device-0", "manifest.json"), []byte("published"), 0o600))
	return root
}

func TestHelperArtifactSweepFailureOrdering(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*snapshotv1alpha1.SnapshotJob, *batchv1.Job, *corev1.Pod)
		keep   bool
	}{
		{"failed capture after helper publication", func(*snapshotv1alpha1.SnapshotJob, *batchv1.Job, *corev1.Pod) {}, false},
		{"omitted policy retains legacy managed bytes", func(sj *snapshotv1alpha1.SnapshotJob, _ *batchv1.Job, _ *corev1.Pod) {
			sj.Spec.OnFailurePolicy = ""
		}, true},
		{"explicit retain protects bytes", func(sj *snapshotv1alpha1.SnapshotJob, _ *batchv1.Job, _ *corev1.Pod) {
			sj.Spec.OnFailurePolicy = snapshotv1alpha1.SnapshotJobOnFailureRetain
		}, true},
		{"unknown policy fails closed", func(sj *snapshotv1alpha1.SnapshotJob, _ *batchv1.Job, _ *corev1.Pod) {
			sj.Spec.OnFailurePolicy = "FuturePolicy"
		}, true},
		{"cleanup without managed ownership retains bytes", func(sj *snapshotv1alpha1.SnapshotJob, _ *batchv1.Job, _ *corev1.Pod) {
			sj.Spec.PodTemplate.Annotations = nil
		}, true},
		{"completed wins over contradictory failure", func(sj *snapshotv1alpha1.SnapshotJob, _ *batchv1.Job, _ *corev1.Pod) {
			sj.Status.Conditions = append(sj.Status.Conditions, metav1.Condition{Type: snapshotv1alpha1.SnapshotJobConditionCompleted, Status: metav1.ConditionTrue})
		}, true},
		{"completed protects bytes", func(sj *snapshotv1alpha1.SnapshotJob, _ *batchv1.Job, _ *corev1.Pod) {
			sj.Status.Conditions = []metav1.Condition{{Type: snapshotv1alpha1.SnapshotJobConditionCompleted, Status: metav1.ConditionTrue}}
		}, true},
		{"pending protects bytes", func(sj *snapshotv1alpha1.SnapshotJob, _ *batchv1.Job, _ *corev1.Pod) { sj.Status.Conditions = nil }, true},
		{"saver still publishing", func(_ *snapshotv1alpha1.SnapshotJob, _ *batchv1.Job, pod *corev1.Pod) {
			pod.Status.ContainerStatuses[1].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
		}, true},
		{"saver waiting", func(_ *snapshotv1alpha1.SnapshotJob, _ *batchv1.Job, pod *corev1.Pod) {
			pod.Status.ContainerStatuses[1].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{}}
		}, true},
		{"pod phase nonterminal", func(_ *snapshotv1alpha1.SnapshotJob, _ *batchv1.Job, pod *corev1.Pod) {
			pod.Status.Phase = corev1.PodRunning
		}, true},
		{"sidecar can restart", func(_ *snapshotv1alpha1.SnapshotJob, _ *batchv1.Job, pod *corev1.Pod) {
			pod.Spec.InitContainers = []corev1.Container{{Name: "sidecar", RestartPolicy: ptr.To(corev1.ContainerRestartPolicyAlways)}}
		}, true},
		{"job incarnation mismatch", func(sj *snapshotv1alpha1.SnapshotJob, _ *batchv1.Job, _ *corev1.Pod) {
			sj.Status.SourceJobUID = "different-job"
		}, true},
		{"missing durable job identity", func(sj *snapshotv1alpha1.SnapshotJob, _ *batchv1.Job, _ *corev1.Pod) { sj.Status.SourceJobUID = "" }, true},
		{"only failure target", func(_ *snapshotv1alpha1.SnapshotJob, job *batchv1.Job, _ *corev1.Pod) {
			job.Status.Conditions[0].Type = batchv1.JobFailureTarget
		}, true},
		{"legacy job without optin", func(_ *snapshotv1alpha1.SnapshotJob, job *batchv1.Job, _ *corev1.Pod) {
			job.Spec.Template.Annotations = nil
		}, true},
		{"different env generation", func(_ *snapshotv1alpha1.SnapshotJob, job *batchv1.Job, _ *corev1.Pod) {
			job.Spec.Template.Spec.Containers[1].Env[0].Value = "different-attempt"
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sj, job, pod := failedHelperAttempt()
			tc.mutate(sj, job, pod)
			base := t.TempDir()
			root := writeHelperArtifact(t, base, string(sj.UID))
			other := writeHelperArtifact(t, base, "different-generation")
			captureRoot := filepath.Join(base, "capture-content-uid")
			require.NoError(t, os.MkdirAll(captureRoot, 0o750))
			captureFile := filepath.Join(captureRoot, "capture-evidence")
			require.NoError(t, os.WriteFile(captureFile, []byte("capture bytes"), 0o600))
			q, _ := newTestQueue(t, base, sj, job, pod)
			backend, err := q.backend()
			require.NoError(t, err)
			require.NoError(t, q.processHelperSweep(context.Background(), backend, log.Log))
			if tc.keep {
				require.DirExists(t, root)
			} else {
				require.NoDirExists(t, root)
			}
			require.DirExists(t, other, "never delete a different/unknown attempt")
			captureBytes, err := os.ReadFile(captureFile)
			require.NoError(t, err)
			require.Equal(t, "capture bytes", string(captureBytes), "helper policy must not delete capture artifacts")
			require.NoError(t, q.processHelperSweep(context.Background(), backend, log.Log), "retry must be idempotent")
			current := &snapshotv1alpha1.SnapshotJob{}
			require.NoError(t, q.client.Get(context.Background(), client.ObjectKeyFromObject(sj), current))
			assert.Equal(t, sj.Status, current.Status, "failure details stay available")
		})
	}
}

func TestHelperSweepRetainsDeletedAttemptsAndUnsafeRoots(t *testing.T) {
	base := t.TempDir()
	orphan := writeHelperArtifact(t, base, "deleted-completed-attempt")
	sj, job, pod := failedHelperAttempt()
	external := t.TempDir()
	root := filepath.Join(base, podcontract.HelperArtifactsDirectory, string(sj.UID))
	require.NoError(t, os.Symlink(external, root))
	q, _ := newTestQueue(t, base, sj, job, pod)
	backend, err := q.backend()
	require.NoError(t, err)
	require.NoError(t, q.processHelperSweep(context.Background(), backend, log.Log))
	require.DirExists(t, external)
	require.DirExists(t, orphan, "a deleted attempt is not proof of failure")
}

func TestHelperSweepFailsClosedOnAuthoritativePodRead(t *testing.T) {
	sj, job, pod := failedHelperAttempt()
	base := t.TempDir()
	root := writeHelperArtifact(t, base, string(sj.UID))
	q, _ := newTestQueue(t, base, sj, job, pod)
	q.apiReader = interceptor.NewClient(q.client.(client.WithWatch), interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, options ...client.ListOption) error {
		if _, pods := list.(*corev1.PodList); pods {
			return assert.AnError
		}
		return c.List(ctx, list, options...)
	}})
	backend, err := q.backend()
	require.NoError(t, err)
	require.Error(t, q.processHelperSweep(context.Background(), backend, log.Log))
	require.DirExists(t, root)
}

func TestHelperSweepBlockedAttemptsDoNotStarveDeletionBatch(t *testing.T) {
	base := t.TempDir()
	var objects []client.Object
	var roots []string
	for i := 0; i < 3; i++ {
		sj, job, pod := indexedHelperAttempt(i)
		if i < 2 {
			pod.Status.Phase = corev1.PodRunning
		}
		objects = append(objects, sj, job, pod)
		roots = append(roots, writeHelperArtifact(t, base, string(sj.UID)))
	}
	q, _ := newTestQueue(t, base, objects...)
	q.config.BatchSize = 1
	backend, err := q.backend()
	require.NoError(t, err)
	require.NoError(t, q.processHelperSweep(context.Background(), backend, log.Log))
	require.DirExists(t, roots[0])
	require.DirExists(t, roots[1])
	require.NoDirExists(t, roots[2], "blocked attempts must not consume the safe deletion budget")
}

func indexedHelperAttempt(i int) (*snapshotv1alpha1.SnapshotJob, *batchv1.Job, *corev1.Pod) {
	sj, job, pod := failedHelperAttempt()
	sj.Name = fmt.Sprintf("checkpoint-%d", i)
	sj.UID = types.UID(fmt.Sprintf("attempt-%d", i))
	sj.Status.SourceJobUID = types.UID(fmt.Sprintf("job-%d", i))
	job.Name, job.UID = sj.Name, sj.Status.SourceJobUID
	job.OwnerReferences[0].Name, job.OwnerReferences[0].UID = sj.Name, sj.UID
	job.Spec.Template.Labels[snapshotv1alpha1.SnapshotJobOwnerUIDLabel] = string(sj.UID)
	job.Spec.Template.Spec.Containers[1].Env[0].Value = string(sj.UID)
	job.Spec.Template.Spec.Containers[1].Env[1].Value = "helper-artifacts/" + string(sj.UID)
	pod.Name = fmt.Sprintf("source-%d", i)
	pod.OwnerReferences[0].Name, pod.OwnerReferences[0].UID = job.Name, job.UID
	return sj, job, pod
}

type refusingHelperBackend struct {
	Backend
	refusedUID string
}

func (b refusingHelperBackend) DeleteHelpers(ctx context.Context, uid string) error {
	if uid == b.refusedUID {
		return assert.AnError
	}
	return b.Backend.DeleteHelpers(ctx, uid)
}

func TestHelperSweepStorageErrorDoesNotStarveDeletionBatch(t *testing.T) {
	base := t.TempDir()
	var objects []client.Object
	var roots []string
	for i := 0; i < 2; i++ {
		sj, job, pod := indexedHelperAttempt(i)
		objects = append(objects, sj, job, pod)
		roots = append(roots, writeHelperArtifact(t, base, string(sj.UID)))
	}
	q, _ := newTestQueue(t, base, objects...)
	q.config.BatchSize = 1
	backend, err := q.backend()
	require.NoError(t, err)
	require.Error(t, q.processHelperSweep(context.Background(), refusingHelperBackend{Backend: backend, refusedUID: "attempt-0"}, log.Log))
	require.DirExists(t, roots[0])
	require.NoDirExists(t, roots[1], "a permanent storage failure must not consume the reclamation budget")
}
