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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

func failedNonTargetAttempt() (*snapshotv1alpha1.SnapshotJob, *batchv1.Job, *corev1.Pod) {
	sj := &snapshotv1alpha1.SnapshotJob{
		ObjectMeta: metav1.ObjectMeta{Name: "checkpoint", Namespace: "test", UID: "attempt-uid"},
		Spec: snapshotv1alpha1.SnapshotJobSpec{NonTargetArtifacts: &snapshotv1alpha1.NonTargetArtifactPolicy{
			Containers: []string{"saver"}, OnFailure: snapshotv1alpha1.NonTargetArtifactCleanup,
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
				Annotations: map[string]string{podcontract.NonTargetArtifactContainersAnnotation: "saver"}},
			Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, Containers: []corev1.Container{
				{Name: "worker"}, {Name: "saver", Env: []corev1.EnvVar{
					{Name: podcontract.SnapshotJobUIDEnv, Value: string(sj.UID)},
					{Name: podcontract.NonTargetArtifactSubdirEnv, Value: "non-target-artifacts/attempt-uid"},
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

func writeNonTargetArtifact(t *testing.T, base, uid string) string {
	t.Helper()
	root := filepath.Join(base, podcontract.NonTargetArtifactsDirectory, uid)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "gms", "device-0"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "gms", "device-0", "manifest.json"), []byte("published"), 0o600))
	return root
}

func TestNonTargetArtifactSweepFailureOrdering(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*snapshotv1alpha1.SnapshotJob, *batchv1.Job, *corev1.Pod)
		keep   bool
	}{
		{"failed capture after non-target artifact publication", func(*snapshotv1alpha1.SnapshotJob, *batchv1.Job, *corev1.Pod) {}, false},
		{"omitted policy retains legacy managed bytes", func(sj *snapshotv1alpha1.SnapshotJob, _ *batchv1.Job, _ *corev1.Pod) {
			sj.Spec.NonTargetArtifacts = nil
		}, true},
		{"explicit retain protects bytes", func(sj *snapshotv1alpha1.SnapshotJob, _ *batchv1.Job, _ *corev1.Pod) {
			sj.Spec.NonTargetArtifacts.OnFailure = snapshotv1alpha1.NonTargetArtifactRetain
		}, true},
		{"unknown policy fails closed", func(sj *snapshotv1alpha1.SnapshotJob, _ *batchv1.Job, _ *corev1.Pod) {
			sj.Spec.NonTargetArtifacts.OnFailure = "FuturePolicy"
		}, true},
		{"cleanup without managed ownership retains bytes", func(sj *snapshotv1alpha1.SnapshotJob, _ *batchv1.Job, _ *corev1.Pod) {
			sj.Spec.NonTargetArtifacts.Containers = nil
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
			sj, job, pod := failedNonTargetAttempt()
			tc.mutate(sj, job, pod)
			base := t.TempDir()
			root := writeNonTargetArtifact(t, base, string(sj.UID))
			other := writeNonTargetArtifact(t, base, "different-generation")
			captureRoot := filepath.Join(base, "capture-content-uid")
			require.NoError(t, os.MkdirAll(captureRoot, 0o750))
			captureFile := filepath.Join(captureRoot, "capture-evidence")
			require.NoError(t, os.WriteFile(captureFile, []byte("capture bytes"), 0o600))
			q, _ := newTestQueue(t, base, sj, job, pod)
			backend, err := q.backend()
			require.NoError(t, err)
			require.NoError(t, q.processNonTargetArtifactSweep(context.Background(), backend, log.Log))
			if tc.keep {
				require.DirExists(t, root)
			} else {
				require.NoDirExists(t, root)
			}
			require.DirExists(t, other, "never delete a different/unknown attempt")
			captureBytes, err := os.ReadFile(captureFile)
			require.NoError(t, err)
			require.Equal(t, "capture bytes", string(captureBytes), "non-target artifact policy must not delete capture artifacts")
			require.NoError(t, q.processNonTargetArtifactSweep(context.Background(), backend, log.Log), "retry must be idempotent")
			current := &snapshotv1alpha1.SnapshotJob{}
			require.NoError(t, q.client.Get(context.Background(), client.ObjectKeyFromObject(sj), current))
			assert.Equal(t, sj.Status, current.Status, "failure details stay available")
		})
	}
}

func TestNonTargetArtifactSweepAfterDeadlinePodRemoval(t *testing.T) {
	for _, tc := range []struct {
		name      string
		condition batchv1.JobConditionType
	}{
		{name: "terminal failure without pod proof", condition: batchv1.JobFailed},
		{name: "failure target before pod termination", condition: batchv1.JobFailureTarget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sj, job, _ := failedNonTargetAttempt()
			job.Status.Conditions[0].Type = tc.condition
			base := t.TempDir()
			root := writeNonTargetArtifact(t, base, string(sj.UID))
			q, recorder := newTestQueue(t, base, sj, job) // Job controller removed its Pod.
			backend, err := q.backend()
			require.NoError(t, err)
			require.NoError(t, q.processNonTargetArtifactSweep(context.Background(), backend, log.Log))
			require.DirExists(t, root, "without an authoritative Pod record the writer may still be stopping")
			require.NoError(t, q.processNonTargetArtifactSweep(context.Background(), backend, log.Log))
			if tc.condition == batchv1.JobFailed {
				select {
				case event := <-recorder.Events:
					require.Contains(t, event, "NonTargetArtifactsRetained")
				default:
					t.Fatal("expected retention warning")
				}
			}
			select {
			case event := <-recorder.Events:
				t.Fatalf("unexpected repeated retention event: %s", event)
			default:
			}
		})
	}
}

func TestNonTargetArtifactRetentionWarningDistinguishesPermanentAndWaiting(t *testing.T) {
	for _, tc := range []struct {
		name        string
		mutate      func(*snapshotv1alpha1.SnapshotJob, *batchv1.Job, *corev1.Pod)
		omitJob     bool
		wantWarning bool
	}{
		{"deleted source Job", nil, true, true},
		{"different source Job incarnation", func(sj *snapshotv1alpha1.SnapshotJob, _ *batchv1.Job, _ *corev1.Pod) {
			sj.Status.SourceJobUID = "different-job"
		}, false, true},
		{"writer declaration mismatch", func(_ *snapshotv1alpha1.SnapshotJob, job *batchv1.Job, _ *corev1.Pod) {
			job.Spec.Template.Annotations = nil
		}, false, true},
		{"writer environment mismatch", func(_ *snapshotv1alpha1.SnapshotJob, job *batchv1.Job, _ *corev1.Pod) {
			job.Spec.Template.Spec.Containers[1].Env[0].Value = "different-attempt"
		}, false, true},
		{"source Job still running", func(_ *snapshotv1alpha1.SnapshotJob, job *batchv1.Job, _ *corev1.Pod) {
			job.Status.Conditions = nil
		}, false, false},
		{"source writer still running", func(_ *snapshotv1alpha1.SnapshotJob, _ *batchv1.Job, pod *corev1.Pod) {
			pod.Status.ContainerStatuses[1].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
		}, false, false},
		{"failed attempt has no source Job UID", func(sj *snapshotv1alpha1.SnapshotJob, _ *batchv1.Job, _ *corev1.Pod) {
			sj.Status.SourceJobUID = ""
		}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sj, job, pod := failedNonTargetAttempt()
			if tc.mutate != nil {
				tc.mutate(sj, job, pod)
			}
			base := t.TempDir()
			root := writeNonTargetArtifact(t, base, string(sj.UID))
			objects := []client.Object{sj, pod}
			if !tc.omitJob {
				objects = append(objects, job)
			}
			q, recorder := newTestQueue(t, base, objects...)
			backend, err := q.backend()
			require.NoError(t, err)
			for range 2 {
				require.NoError(t, q.processNonTargetArtifactSweep(context.Background(), backend, log.Log))
			}
			require.DirExists(t, root)
			if tc.wantWarning {
				select {
				case event := <-recorder.Events:
					require.Contains(t, event, "NonTargetArtifactsRetained")
				default:
					t.Fatal("expected retention warning")
				}
			}
			select {
			case event := <-recorder.Events:
				t.Fatalf("unexpected retention event: %s", event)
			default:
			}
		})
	}
}

func TestNonTargetSweepRunsAfterContentSweepReadFailure(t *testing.T) {
	sj, job, pod := failedNonTargetAttempt()
	base := t.TempDir()
	root := writeNonTargetArtifact(t, base, string(sj.UID))
	q, _ := newTestQueue(t, base, sj, job, pod)
	failedLists := 0
	q.apiReader = interceptor.NewClient(q.client.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, options ...client.ListOption) error {
			if _, metadata := list.(*metav1.PartialObjectMetadataList); metadata && failedLists < q.config.ListAttempts {
				failedLists++
				return apierrors.NewServiceUnavailable("content list unavailable")
			}
			return c.List(ctx, list, options...)
		},
	})
	q.EnqueueSweep()
	require.True(t, q.processNextItem(context.Background(), log.Log))
	require.DirExists(t, root, "the content sweep failed before the independent work item")
	require.True(t, q.processNextItem(context.Background(), log.Log))
	require.NoDirExists(t, root, "the non-target sweep has its own deadline and API attempt")
	require.Equal(t, q.config.ListAttempts, failedLists)
}

func TestNonTargetArtifactSweepResumesPagedMetadataAfterDeadline(t *testing.T) {
	sj, job, pod := failedNonTargetAttempt()
	earlySJ, earlyJob, earlyPod := indexedNonTargetAttempt(1)
	base := t.TempDir()
	root := writeNonTargetArtifact(t, base, string(sj.UID))
	earlyRoot := writeNonTargetArtifact(t, base, string(earlySJ.UID))
	q, _ := newTestQueue(t, base, sj, job, pod, earlySJ, earlyJob, earlyPod)
	q.config.ListAttempts = 1
	pageCalls := make([]string, 0, 6)
	failLastPage := true
	q.apiReader = interceptor.NewClient(q.client.(client.WithWatch), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, options ...client.ListOption) error {
			metadata, ok := list.(*metav1.PartialObjectMetadataList)
			if !ok {
				return c.List(ctx, list, options...)
			}
			listOptions := &client.ListOptions{}
			listOptions.ApplyOptions(options)
			pageCalls = append(pageCalls, listOptions.Continue)
			page := 0
			if listOptions.Continue != "" {
				_, err := fmt.Sscanf(listOptions.Continue, "page-%d", &page)
				require.NoError(t, err)
			}
			if page == nonTargetJobPagesPerSweep && failLastPage {
				failLastPage = false
				return context.DeadlineExceeded
			}
			metadata.ResourceVersion = "consistent-snapshot"
			if page < nonTargetJobPagesPerSweep {
				metadata.Items = make([]metav1.PartialObjectMetadata, nonTargetJobListPageLimit)
				for i := range metadata.Items {
					metadata.Items[i].ObjectMeta = metav1.ObjectMeta{
						Namespace: sj.Namespace, Name: fmt.Sprintf("unrelated-%d-%d", page, i),
						UID: types.UID(fmt.Sprintf("unrelated-uid-%d-%d", page, i)),
					}
				}
				if page == 0 {
					metadata.Items[0].ObjectMeta = metav1.ObjectMeta{
						Namespace: earlySJ.Namespace, Name: earlySJ.Name, UID: earlySJ.UID,
					}
				}
				metadata.Continue = fmt.Sprintf("page-%d", page+1)
			} else {
				metadata.Items = []metav1.PartialObjectMetadata{{ObjectMeta: metav1.ObjectMeta{
					Namespace: sj.Namespace, Name: sj.Name, UID: sj.UID,
				}}}
			}
			return nil
		},
	})
	backend, err := q.backend()
	require.NoError(t, err)

	require.NoError(t, q.processNonTargetArtifactSweep(context.Background(), backend, log.Log))
	require.DirExists(t, root, "partial metadata lists cannot authorize deletion")
	require.DirExists(t, earlyRoot, "even an early match waits for the whole consistent list")
	require.Equal(t, "page-4", q.nonTargetScan.continueToken)
	require.Len(t, pageCalls, nonTargetJobPagesPerSweep)

	require.ErrorIs(t, q.processNonTargetArtifactSweep(context.Background(), backend, log.Log), context.DeadlineExceeded)
	require.DirExists(t, root)
	require.Equal(t, "page-4", q.nonTargetScan.continueToken, "a failed page must preserve the last successful continuation")

	require.NoError(t, q.processNonTargetArtifactSweep(context.Background(), backend, log.Log))
	require.NoDirExists(t, root, "the next sweep must reach the candidate beyond 2,000 metadata entries")
	require.NoDirExists(t, earlyRoot, "an earlier match must survive the deadline and be cleaned after list completion")
	require.Nil(t, q.nonTargetScan)
	require.Equal(t, []string{"", "page-1", "page-2", "page-3", "page-4", "page-4"}, pageCalls)
}

func TestNonTargetArtifactSweepRetainsDeletedAttemptsAndUnsafeRoots(t *testing.T) {
	base := t.TempDir()
	orphan := writeNonTargetArtifact(t, base, "deleted-completed-attempt")
	sj, job, pod := failedNonTargetAttempt()
	external := t.TempDir()
	root := filepath.Join(base, podcontract.NonTargetArtifactsDirectory, string(sj.UID))
	require.NoError(t, os.Symlink(external, root))
	q, _ := newTestQueue(t, base, sj, job, pod)
	backend, err := q.backend()
	require.NoError(t, err)
	require.NoError(t, q.processNonTargetArtifactSweep(context.Background(), backend, log.Log))
	require.DirExists(t, external)
	require.DirExists(t, orphan, "a deleted attempt is not proof of failure")
}

func TestNonTargetArtifactSweepFailsClosedOnAuthoritativePodRead(t *testing.T) {
	sj, job, pod := failedNonTargetAttempt()
	base := t.TempDir()
	root := writeNonTargetArtifact(t, base, string(sj.UID))
	q, _ := newTestQueue(t, base, sj, job, pod)
	q.apiReader = interceptor.NewClient(q.client.(client.WithWatch), interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, options ...client.ListOption) error {
		if _, pods := list.(*corev1.PodList); pods {
			return assert.AnError
		}
		return c.List(ctx, list, options...)
	}})
	backend, err := q.backend()
	require.NoError(t, err)
	require.Error(t, q.processNonTargetArtifactSweep(context.Background(), backend, log.Log))
	require.DirExists(t, root)
}

func TestNonTargetArtifactSweepBoundsBlockedAttemptsAndReachesLaterCandidates(t *testing.T) {
	base := t.TempDir()
	var objects []client.Object
	var roots []string
	for i := 0; i < 3; i++ {
		sj, job, pod := indexedNonTargetAttempt(i)
		if i < 2 {
			pod.Status.Phase = corev1.PodRunning
		}
		objects = append(objects, sj, job, pod)
		roots = append(roots, writeNonTargetArtifact(t, base, string(sj.UID)))
	}
	q, _ := newTestQueue(t, base, objects...)
	q.config.BatchSize = 1
	backend, err := q.backend()
	require.NoError(t, err)
	require.NoError(t, q.processNonTargetArtifactSweep(context.Background(), backend, log.Log))
	require.DirExists(t, roots[0])
	require.DirExists(t, roots[1])
	require.DirExists(t, roots[2], "one blocked attempt consumes this sweep's API budget")
	require.NoError(t, q.processNonTargetArtifactSweep(context.Background(), backend, log.Log))
	require.DirExists(t, roots[2])
	require.NoError(t, q.processNonTargetArtifactSweep(context.Background(), backend, log.Log))
	require.NoDirExists(t, roots[2], "the cursor must reach later attempts")
}

func indexedNonTargetAttempt(i int) (*snapshotv1alpha1.SnapshotJob, *batchv1.Job, *corev1.Pod) {
	sj, job, pod := failedNonTargetAttempt()
	sj.Name = fmt.Sprintf("checkpoint-%d", i)
	sj.UID = types.UID(fmt.Sprintf("attempt-%d", i))
	sj.Status.SourceJobUID = types.UID(fmt.Sprintf("job-%d", i))
	job.Name, job.UID = sj.Name, sj.Status.SourceJobUID
	job.OwnerReferences[0].Name, job.OwnerReferences[0].UID = sj.Name, sj.UID
	job.Spec.Template.Labels[snapshotv1alpha1.SnapshotJobOwnerUIDLabel] = string(sj.UID)
	job.Spec.Template.Spec.Containers[1].Env[0].Value = string(sj.UID)
	job.Spec.Template.Spec.Containers[1].Env[1].Value = "non-target-artifacts/" + string(sj.UID)
	pod.Name = fmt.Sprintf("source-%d", i)
	pod.OwnerReferences[0].Name, pod.OwnerReferences[0].UID = job.Name, job.UID
	return sj, job, pod
}

type refusingNonTargetArtifactBackend struct {
	NonTargetArtifactBackend
	refusedUID string
}

func (b refusingNonTargetArtifactBackend) DeleteNonTargetArtifacts(ctx context.Context, uid string) error {
	if uid == b.refusedUID {
		return assert.AnError
	}
	return b.NonTargetArtifactBackend.DeleteNonTargetArtifacts(ctx, uid)
}

func TestNonTargetArtifactSweepStorageErrorDoesNotStarveLaterSweeps(t *testing.T) {
	base := t.TempDir()
	var objects []client.Object
	var roots []string
	for i := 0; i < 2; i++ {
		sj, job, pod := indexedNonTargetAttempt(i)
		objects = append(objects, sj, job, pod)
		roots = append(roots, writeNonTargetArtifact(t, base, string(sj.UID)))
	}
	q, _ := newTestQueue(t, base, objects...)
	q.config.BatchSize = 1
	backend, err := q.backend()
	require.NoError(t, err)
	nonTargetBackend, ok := backend.(NonTargetArtifactBackend)
	require.True(t, ok)
	require.Error(t, q.processNonTargetArtifactSweep(context.Background(), refusingNonTargetArtifactBackend{NonTargetArtifactBackend: nonTargetBackend, refusedUID: "attempt-0"}, log.Log))
	require.DirExists(t, roots[0])
	require.DirExists(t, roots[1], "the failed deletion consumes this sweep's API budget")
	require.NoError(t, q.processNonTargetArtifactSweep(context.Background(), backend, log.Log))
	require.NoDirExists(t, roots[1], "a later sweep reaches the next attempt")
}
