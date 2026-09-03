// SPDX-FileCopyrightText: Copyright (c) 2025-2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"syscall"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	snapshottypes "github.com/ai-dynamo/snapshot/agent/internal/types"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// makeNodeControllerWithInterceptor mirrors makeNodeController but threads interceptor.Funcs so a
// test can inject API errors on specific code paths.
func makeNodeControllerWithInterceptor(t *testing.T, fc *fakeCheckpointer, funcs interceptor.Funcs, objs ...client.Object) *NodeController {
	t.Helper()
	s := contentScheme(t)
	idx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{podRefIndex: podRefIndexFunc})
	for _, o := range objs {
		if sc, ok := o.(*snapshotv1alpha1.PodSnapshotContent); ok {
			require.NoError(t, idx.Add(mustUnstructured(t, sc)))
		}
	}
	w := &NodeController{
		config:    &snapshottypes.AgentConfig{NodeName: "node-a", Storage: snapshottypes.StorageSpec{Type: "pvc", BasePath: t.TempDir()}},
		clientset: k8sfake.NewClientset(),
		client: crfake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
			WithStatusSubresource(&snapshotv1alpha1.PodSnapshotContent{}).
			WithInterceptorFuncs(funcs).Build(),
		runtime:        &fakeRuntime{},
		log:            logr.Discard(),
		holderID:       "snapshot-agent/test",
		inFlight:       make(map[string]struct{}),
		contentIndexer: idx,
		sendSignalFn:   snapshotruntime.SendSignalToPID,
	}
	w.checkpointFn = fc.fn
	return w
}

func TestUnknownCheckpointRecoveryRetriesStatusAfterExactTermination(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-abc", "node-a", "abc")
	pod := makeSourcePod()
	failedOnce := false
	funcs := interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if snapshotContent, ok := obj.(*snapshotv1alpha1.PodSnapshotContent); ok {
				condition := meta.FindStatusCondition(snapshotContent.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed)
				if condition != nil && condition.Reason == "CheckpointOutcomeUnknown" && !failedOnce {
					failedOnce = true
					return errors.New("status temporarily unavailable")
				}
			}
			return c.Status().Patch(ctx, obj, patch, opts...)
		},
	}
	w := makeNodeControllerWithInterceptor(t, &fakeCheckpointer{}, funcs, content, pod)
	w.runtime = &fakeRuntime{resolveContainerPID: 123}
	signalCount := 0
	w.sendSignalFn = func(logr.Logger, int, syscall.Signal, string) error {
		signalCount++
		return nil
	}
	foreignCaptureLease(t, w, content, true)

	err := w.reconcileSourcePod(context.Background(), pod)
	require.ErrorContains(t, err, "status temporarily unavailable")
	assert.Equal(t, 1, signalCount)

	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{ExitCode: 137},
	}
	require.NoError(t, w.reconcileSourcePod(context.Background(), pod))
	assert.Equal(t, 1, signalCount, "a terminated exact target must not be signaled again")
	condition := meta.FindStatusCondition(getContent(t, w, content.Name).Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed)
	require.NotNil(t, condition)
	assert.Equal(t, "CheckpointOutcomeUnknown", condition.Reason)
}

func TestReconcilePodSnapshotContent_ContentGetErrorReturns(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-x", "node-a", "x")
	pod := makeSourcePod()
	funcs := interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*snapshotv1alpha1.PodSnapshotContent); ok {
				return errors.New("apiserver unavailable")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}
	w := makeNodeControllerWithInterceptor(t, &fakeCheckpointer{}, funcs, content, pod)

	w.reconcilePodSnapshotContent(context.Background(), content.Name)

	// The gate could not read the work order, so it must not have promoted the pod.
	_, labeled := getPod(t, w, "inference", "worker-0").Labels[snapshotv1alpha1.CaptureEligibleLabel]
	assert.False(t, labeled)
}

func TestReconcilePodSnapshotContent_SourcePodGetErrorReturns(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-x", "node-a", "x")
	pod := makeSourcePod()
	funcs := interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.Pod); ok {
				return errors.New("apiserver unavailable")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}
	w := makeNodeControllerWithInterceptor(t, &fakeCheckpointer{}, funcs, content, pod)

	w.reconcilePodSnapshotContent(context.Background(), content.Name)

	// A transient pod-Get error must be retried, not written as a terminal failure.
	assert.Empty(t, getContent(t, w, content.Name).Status.Conditions)
}

func TestReconcilePodSnapshotContent_LabelErrorLeavesPodUnlabeled(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-x", "node-a", "x")
	pod := makeSourcePod()
	funcs := interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			return errors.New("patch rejected")
		},
	}
	w := makeNodeControllerWithInterceptor(t, &fakeCheckpointer{}, funcs, content, pod)

	w.reconcilePodSnapshotContent(context.Background(), content.Name)

	// Validation passed but the promotion patch failed: logged best-effort, pod stays unlabeled.
	_, labeled := getPod(t, w, "inference", "worker-0").Labels[snapshotv1alpha1.CaptureEligibleLabel]
	assert.False(t, labeled)
}

func TestReconcileSourcePod_ContentGetErrorReturns(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-x", "node-a", "x")
	pod := makeSourcePod()
	pod.Labels[snapshotv1alpha1.CaptureEligibleLabel] = "true"
	fc := &fakeCheckpointer{}
	funcs := interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*snapshotv1alpha1.PodSnapshotContent); ok {
				return errors.New("apiserver unavailable")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}
	w := makeNodeControllerWithInterceptor(t, fc, funcs, content, pod)

	require.Error(t, w.reconcileSourcePod(context.Background(), pod))

	assert.False(t, fc.called, "a content Get error must abort before the dump")
}

func TestLabelCaptureEligible_AlreadyLabeledNoOp(t *testing.T) {
	pod := makeSourcePod()
	pod.Labels[snapshotv1alpha1.CaptureEligibleLabel] = "true"
	funcs := interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			return errors.New("patch must not be called")
		},
	}
	w := makeNodeControllerWithInterceptor(t, &fakeCheckpointer{}, funcs, pod)

	// Already labeled → early return, no patch (so the erroring interceptor is never hit).
	require.NoError(t, w.labelCaptureEligible(context.Background(), pod))
}

func TestLabelCaptureEligible_PatchErrorReturned(t *testing.T) {
	pod := makeSourcePod()
	funcs := interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			return errors.New("patch rejected")
		},
	}
	w := makeNodeControllerWithInterceptor(t, &fakeCheckpointer{}, funcs, pod)

	err := w.labelCaptureEligible(context.Background(), pod)
	require.Error(t, err)
}

func TestRemoveCaptureEligibleLabel_AbsentNoOp(t *testing.T) {
	pod := makeSourcePod() // no CaptureEligibleLabel
	funcs := interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			return errors.New("patch must not be called")
		},
	}
	w := makeNodeControllerWithInterceptor(t, &fakeCheckpointer{}, funcs, pod)

	// No label → early return, no patch attempted (best-effort, void); must not panic.
	w.removeCaptureEligibleLabel(context.Background(), pod)
	_, labeled := getPod(t, w, "inference", "worker-0").Labels[snapshotv1alpha1.CaptureEligibleLabel]
	assert.False(t, labeled)
}

func TestRemoveCaptureEligibleLabel_PatchErrorLeavesLabel(t *testing.T) {
	pod := makeSourcePod()
	pod.Labels[snapshotv1alpha1.CaptureEligibleLabel] = "true"
	funcs := interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			return errors.New("patch rejected")
		},
	}
	w := makeNodeControllerWithInterceptor(t, &fakeCheckpointer{}, funcs, pod)

	// Patch fails → logged best-effort; the stored pod keeps the label.
	w.removeCaptureEligibleLabel(context.Background(), pod)
	_, labeled := getPod(t, w, "inference", "worker-0").Labels[snapshotv1alpha1.CaptureEligibleLabel]
	assert.True(t, labeled)
}

func TestSetSnapshotContentSucceeded_StatusPatchErrorReturnsError(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-x", "node-a", "x")
	funcs := interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			return errors.New("status patch rejected")
		},
	}
	w := makeNodeControllerWithInterceptor(t, &fakeCheckpointer{}, funcs, content)

	err := w.setSnapshotContentSucceeded(context.Background(), content)

	require.Error(t, err)
	assert.Nil(t, meta.FindStatusCondition(getContent(t, w, content.Name).Status.Conditions, snapshotv1alpha1.PodSnapshotConditionReady))
}

func TestSetSnapshotContentSucceeded_ConflictReturnsError(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-x", "node-a", "x")
	funcs := interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			return conflictErr()
		},
	}
	w := makeNodeControllerWithInterceptor(t, &fakeCheckpointer{}, funcs, content)

	err := w.setSnapshotContentSucceeded(context.Background(), content)

	require.Error(t, err)
	assert.True(t, apierrors.IsConflict(err))
	assert.Nil(t, meta.FindStatusCondition(getContent(t, w, content.Name).Status.Conditions, snapshotv1alpha1.PodSnapshotConditionReady))
}

func TestRunCheckpoint_ReadyPatchErrorLeavesNotReady(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-x", "node-a", "x")
	funcs := interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			return errors.New("status patch rejected")
		},
	}
	w := makeNodeControllerWithInterceptor(t, &fakeCheckpointer{}, funcs, content)
	pod := &corev1.Pod{}
	leaseKey := client.ObjectKey{Namespace: "inference", Name: "checkpoint-lease-x"}
	artifactPath := w.config.Storage.BasePath

	w.runCheckpoint(context.Background(), content, pod, "main", "abc123", 7, "x", artifactPath, leaseKey, "x")

	// The artifact resume path retries the Ready write later; nothing is written now.
	assert.Nil(t, meta.FindStatusCondition(
		getContent(t, w, content.Name).Status.Conditions,
		snapshotv1alpha1.PodSnapshotConditionReady,
	))
}

// failedBeforeReadyInterceptor rejects Ready patches with a conflict, simulating a Failed
// condition that landed first (its optimistic lock wins).
func failedBeforeReadyInterceptor() interceptor.Funcs {
	return interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			sc, ok := obj.(*snapshotv1alpha1.PodSnapshotContent)
			if ok {
				if cond := meta.FindStatusCondition(sc.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionReady); cond != nil && cond.Status == metav1.ConditionTrue {
					return conflictErr()
				}
			}
			return c.Status().Patch(ctx, obj, patch, opts...)
		},
	}
}

func TestMarkCheckpointReady_FailedBeforeReadyIsSticky(t *testing.T) {
	stored := makeWorkOrder("podsnapshotcontent-x", "node-a", "x")
	meta.SetStatusCondition(&stored.Status.Conditions, metav1.Condition{
		Type:    snapshotv1alpha1.PodSnapshotConditionFailed,
		Status:  metav1.ConditionTrue,
		Reason:  "SourcePodGone",
		Message: "source pod is gone",
	})
	w := makeNodeControllerWithInterceptor(t, &fakeCheckpointer{}, failedBeforeReadyInterceptor(), stored)
	stale := makeWorkOrder("podsnapshotcontent-x", "node-a", "x")

	err := w.markCheckpointReady(context.Background(), stale)

	require.NoError(t, err, "a sticky Failed condition is an accepted outcome, not a retryable error")
	got := getContent(t, w, stored.Name)
	assert.Nil(t, meta.FindStatusCondition(got.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionReady))
	failed := meta.FindStatusCondition(got.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed)
	require.NotNil(t, failed)
	assert.Equal(t, "SourcePodGone", failed.Reason)
}

func TestRunCheckpoint_FailedBeforeReadyDoesNotKill(t *testing.T) {
	stored := makeWorkOrder("podsnapshotcontent-x", "node-a", "x")
	meta.SetStatusCondition(&stored.Status.Conditions, metav1.Condition{
		Type:    snapshotv1alpha1.PodSnapshotConditionFailed,
		Status:  metav1.ConditionTrue,
		Reason:  "SourcePodGone",
		Message: "source pod is gone",
	})
	w := makeNodeControllerWithInterceptor(t, &fakeCheckpointer{}, failedBeforeReadyInterceptor(), stored)
	stale := makeWorkOrder("podsnapshotcontent-x", "node-a", "x")
	pod := &corev1.Pod{}
	leaseKey := client.ObjectKey{Namespace: "inference", Name: "checkpoint-lease-x"}
	artifactPath := w.config.Storage.BasePath
	_, target := startKillableTarget(t)

	w.runCheckpoint(context.Background(), stale, pod, "main", "abc123", target.Process.Pid, "x", artifactPath, leaseKey, "x")

	// The dump itself terminates the source; a raced Failed condition must not
	// trigger an extra kill of an unrelated PID.
	require.NoError(t, target.Process.Signal(syscall.Signal(0)), "target must not be signalled on the Failed-before-Ready path")
	got := getContent(t, w, stored.Name)
	assert.Nil(t, meta.FindStatusCondition(got.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionReady))
	failed := meta.FindStatusCondition(got.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed)
	require.NotNil(t, failed)
	assert.Equal(t, "SourcePodGone", failed.Reason)
}

func TestMarkCheckpointReady_AlreadyReadyNoOp(t *testing.T) {
	stored := makeWorkOrder("podsnapshotcontent-x", "node-a", "x")
	meta.SetStatusCondition(&stored.Status.Conditions, metav1.Condition{
		Type:    snapshotv1alpha1.PodSnapshotConditionReady,
		Status:  metav1.ConditionTrue,
		Reason:  "Captured",
		Message: "Checkpoint captured and verified",
	})
	w := makeNodeControllerWithInterceptor(t, &fakeCheckpointer{}, failedBeforeReadyInterceptor(), stored)
	stale := makeWorkOrder("podsnapshotcontent-x", "node-a", "x")

	err := w.markCheckpointReady(context.Background(), stale)

	require.NoError(t, err, "another holder's Ready write is success, not a conflict to escalate")
}

func TestMarkCheckpointReady_SecondConflictObservesFailed(t *testing.T) {
	stored := makeWorkOrder("podsnapshotcontent-x", "node-a", "x")
	readyPatches := 0
	funcs := interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := c.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if sc, ok := obj.(*snapshotv1alpha1.PodSnapshotContent); ok && readyPatches >= 2 {
				meta.SetStatusCondition(&sc.Status.Conditions, metav1.Condition{
					Type:    snapshotv1alpha1.PodSnapshotConditionFailed,
					Status:  metav1.ConditionTrue,
					Reason:  "SourcePodGone",
					Message: "source pod is gone",
				})
			}
			return nil
		},
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			sc, ok := obj.(*snapshotv1alpha1.PodSnapshotContent)
			if ok {
				if cond := meta.FindStatusCondition(sc.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionReady); cond != nil && cond.Status == metav1.ConditionTrue {
					readyPatches++
					return conflictErr()
				}
			}
			return c.Status().Patch(ctx, obj, patch, opts...)
		},
	}
	w := makeNodeControllerWithInterceptor(t, &fakeCheckpointer{}, funcs, stored)

	err := w.markCheckpointReady(context.Background(), makeWorkOrder("podsnapshotcontent-x", "node-a", "x"))

	require.NoError(t, err)
	assert.Equal(t, 2, readyPatches, "Failed must be observed on the second Ready conflict")
}

func TestSetSnapshotContentFailed_StatusPatchErrorReturnsError(t *testing.T) {
	content := makeWorkOrder("podsnapshotcontent-x", "node-a", "x")
	funcs := interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			return errors.New("status patch rejected")
		},
	}
	w := makeNodeControllerWithInterceptor(t, &fakeCheckpointer{}, funcs, content)

	err := w.setSnapshotContentFailed(context.Background(), content, "SomeReason", errors.New("boom"))

	require.Error(t, err)
	assert.Nil(t, meta.FindStatusCondition(getContent(t, w, content.Name).Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed))
}

// conflictErr returns a 409 Conflict error for the status subresource.
func conflictErr() error {
	return apierrors.NewConflict(schema.GroupResource{Resource: "podsnapshotcontents"}, "podsnapshotcontent-x", errors.New("resource version conflict"))
}

func TestSetSnapshotContentFailed_ConflictReturnsError(t *testing.T) {
	// Patch returns Conflict — optimistic lock rejected; error propagates to caller.
	content := makeWorkOrder("podsnapshotcontent-x", "node-a", "x")
	funcs := interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			return conflictErr()
		},
	}
	w := makeNodeControllerWithInterceptor(t, &fakeCheckpointer{}, funcs, content)

	err := w.setSnapshotContentFailed(context.Background(), content, "CheckpointFailed", errors.New("dump error"))

	require.Error(t, err)
	// The store is unchanged (no status written through the intercepted client).
	got := getContent(t, w, content.Name)
	assert.Nil(t, meta.FindStatusCondition(got.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed))
}
