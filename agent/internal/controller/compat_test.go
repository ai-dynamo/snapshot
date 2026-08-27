// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"runtime"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/ai-dynamo/snapshot/agent/internal/executor"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/ai-dynamo/snapshot/api/compat"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// A checkpoint whose manifest cannot be read is not incompatible, it is broken.
// The restore path reads the manifest again and reports that; refusing here
// would report the wrong outcome and hide the real error.
func TestPreflightCompatibilityAllowsUnreadableManifest(t *testing.T) {
	r := newGatedRestore(t, compat.Mismatch{Check: "kernel-version"})
	path := writeTestArtifact(t, r.controller.config.Storage.BasePath, "no-manifest-here", nil)

	err := r.controller.preflightCompatibility(r.pod, &restoreArtifact{
		ContainerName: gatedRestoreContainer,
		Path:          path,
	})

	require.NoError(t, err)
	assert.Empty(t, r.comparison.calls, "comparison ran without a manifest")
}

func TestPreflightCompatibilityComparesRecordedFacts(t *testing.T) {
	r := newGatedRestore(t)
	path := writeTestArtifact(t, r.controller.config.Storage.BasePath, "mounted-content", &types.CheckpointManifest{
		Artifact: types.ArtifactManifest{ContentUID: "mounted-content", ContainerName: gatedRestoreContainer},
		CRIUDump: types.CRIUDumpManifest{
			ExtMnt: map[string]string{
				"/model-cache": "/model-cache",
				"/etc/hosts":   "/etc/hosts",
			},
		},
	})

	err := r.controller.preflightCompatibility(r.pod, &restoreArtifact{
		ContainerName: gatedRestoreContainer,
		Path:          path,
	})

	require.NoError(t, err)
	require.Len(t, r.comparison.calls, 1)
	assert.Equal(t, compat.GatePreflight, r.comparison.calls[0].gate)
	assert.Equal(t, []string{"/etc/hosts", "/model-cache"}, r.comparison.calls[0].source.ExternalizedMounts)
}

// The gate compares the checkpoint against where it would be restored, so the
// target side has to describe this node and this pod rather than stay empty.
func TestPreflightCompatibilityDescribesTheRestoreTarget(t *testing.T) {
	r := newGatedRestore(t)
	r.controller.config.HostKernelVersion = "5.15.0-1071-aws"
	r.pod.Spec.Containers[0].Image = "nvcr.io/nvidia/tritonserver:24.09-py3"
	r.pod.Status.ContainerStatuses[0].ImageID = "sha256:deadbeef"

	require.NoError(t, r.controller.preflightCompatibility(r.pod, r.artifact))

	require.Len(t, r.comparison.calls, 1)
	assert.Equal(t, compat.Facts{
		CPUArch:       runtime.GOARCH,
		KernelVersion: "5.15.0-1071-aws",
		Image:         "nvcr.io/nvidia/tritonserver:24.09-py3",
		ImageID:       "sha256:deadbeef",
	}, r.comparison.calls[0].target)
}

// The refusal event is its own reason, so alerting that pages on restore
// failures does not fire on a restore that was never attempted.
func TestRefusalEmitsOneIncompatibleEventAtBothGates(t *testing.T) {
	mismatch := compat.Mismatch{Check: "cpu-arch", Source: "amd64", Target: "arm64"}
	wantMessage := "cpu-arch: source amd64, target arm64"

	assertOneEvent := func(t *testing.T, r *gatedRestore) {
		t.Helper()
		events := r.events(t, restoreIncompatibleReason)
		require.Len(t, events, 1)
		assert.Equal(t, wantMessage, events[0].Message)
		assert.Equal(t, corev1.EventTypeWarning, events[0].Type)
		assert.Empty(t, r.events(t, restoreFailedReason), "refusal also reported a restore failure")
	}

	t.Run("preflight gate", func(t *testing.T) {
		r := newGatedRestore(t, mismatch)

		r.reconcile(t)

		assertOneEvent(t, r)
		assert.Empty(t, r.events(t, restoreRequestedReason), "refused restore still announced a request")
	})

	t.Run("inspect gate", func(t *testing.T) {
		r := newGatedRestore(t)
		r.controller.restoreFn = refuseWith(mismatch)

		r.runRestore(t)

		assertOneEvent(t, r)
	})
}

// The pod carries its own verdict on the condition every other restore outcome
// is published on, so a refusal is visible to anything that reads pods and is
// told apart from a failure by its reason alone.
func TestRefusalPublishesTheRestoredConditionAtBothGates(t *testing.T) {
	mismatch := compat.Mismatch{Check: "gpu-model", Source: "Tesla T4", Target: "NVIDIA A100-SXM4-40GB"}
	wantMessage := "gpu-model: source Tesla T4, target NVIDIA A100-SXM4-40GB"

	assertPublished := func(t *testing.T, r *gatedRestore) {
		t.Helper()
		condition := r.condition(t)
		assert.Equal(t, corev1.PodConditionType(snapshotv1alpha1.RestoredCondition), condition.Type)
		assert.Equal(t, corev1.ConditionFalse, condition.Status)
		assert.Equal(t, restoreIncompatibleReason, condition.Reason)
		// The same sentence the log line and the event carry, so a reader who
		// starts from the pod does not get a different answer.
		assert.Equal(t, wantMessage, condition.Message)
		assert.Equal(t, wantMessage, r.refusalLog(t)["reason"])
	}

	t.Run("preflight gate", func(t *testing.T) {
		r := newGatedRestore(t, mismatch)

		r.reconcile(t)

		assertPublished(t, r)
	})

	t.Run("inspect gate", func(t *testing.T) {
		r := newGatedRestore(t)
		r.controller.restoreFn = refuseWith(mismatch)

		r.runRestore(t)

		assertPublished(t, r)
	})
}

// The escape hatches: with either one set, neither gate runs, so a checkpoint
// the policy table would turn down is still attempted.
func TestSkipCompatCheckTurnsOffTheGates(t *testing.T) {
	mismatch := compat.Mismatch{Check: "cpu-arch", Source: "amd64", Target: "arm64"}

	// Lets the restore start and end quickly, since the point here is only
	// whether the gate let it through.
	stopEarly := func(r *gatedRestore) {
		r.controller.restoreFn = func(context.Context, snapshotruntime.Runtime, logr.Logger, executor.RestoreRequest, executor.RestoreMounter) (int, error) {
			return 0, errors.New("test restore stopped")
		}
	}

	t.Run("the pod annotation turns it off", func(t *testing.T) {
		r := newGatedRestore(t, mismatch)
		r.pod.Annotations[snapshotv1alpha1.SkipCompatCheckAnnotation] = "true"
		stopEarly(r)

		r.reconcile(t)

		assert.Empty(t, r.comparison.calls, "skipped gate compared anyway")
		assert.Empty(t, r.events(t, restoreIncompatibleReason), "skipped gate refused the restore")
	})

	// A node with the gate off skips every restore it handles, whether or not
	// the pod asked for it.
	t.Run("the node config turns it off for an unannotated pod", func(t *testing.T) {
		r := newGatedRestore(t, mismatch)
		r.controller.config.Restore.SkipCompatCheck = true
		stopEarly(r)

		r.reconcile(t)

		assert.Empty(t, r.comparison.calls, "skipped gate compared anyway")
		assert.Empty(t, r.events(t, restoreIncompatibleReason), "skipped gate refused the restore")
	})

	// Gate B is inside the executor, past the point where either switch can be
	// read again, so the decision travels with the request. Without it, a
	// skipped restore would still be refused a few steps later.
	t.Run("the decision travels to the second gate", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			set  func(*gatedRestore)
			want bool
		}{
			{name: "checked", set: func(*gatedRestore) {}},
			{
				name: "skipped by pod",
				set: func(r *gatedRestore) {
					r.pod.Annotations[snapshotv1alpha1.SkipCompatCheckAnnotation] = "true"
				},
				want: true,
			},
			{
				name: "skipped by node",
				set:  func(r *gatedRestore) { r.controller.config.Restore.SkipCompatCheck = true },
				want: true,
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				r := newGatedRestore(t)
				tc.set(r)
				var requested executor.RestoreRequest
				r.controller.restoreFn = func(_ context.Context, _ snapshotruntime.Runtime, _ logr.Logger, req executor.RestoreRequest, _ executor.RestoreMounter) (int, error) {
					requested = req
					return 0, errors.New("test restore stopped")
				}

				r.reconcile(t)

				assert.Equal(t, tc.want, requested.SkipCompatCheck)
			})
		}
	})

	// The node switch is read per restore, not once at startup, which is what
	// makes flipping the ConfigMap enough to be heard.
	t.Run("the node config is re-read for every restore", func(t *testing.T) {
		r := newGatedRestore(t, mismatch)
		reads := 0
		r.controller.skipCompatCheckFn = func() bool {
			reads++
			return reads > 1
		}
		stopEarly(r)

		r.reconcile(t)
		require.Len(t, r.comparison.calls, 1, "gate did not run while the switch was off")

		r.pod.Status.Conditions = nil
		r.controller.handledRestores.Delete(string(r.pod.UID))
		r.reconcile(t)

		assert.Len(t, r.comparison.calls, 1, "gate ran after the switch was flipped on")
		assert.Equal(t, 2, reads)
	})

	// The annotation has to reach a pod the gate already turned down, or the
	// only way out of a wrong refusal is deleting and recreating the pod.
	t.Run("it reopens a pod that was already refused", func(t *testing.T) {
		r := newGatedRestore(t, mismatch)
		stopEarly(r)

		r.reconcile(t)
		require.Len(t, r.events(t, restoreIncompatibleReason), 1, "the gate did not refuse the restore")

		r.pod.Status.Conditions = append(r.pod.Status.Conditions, corev1.PodCondition{
			Type:    corev1.PodConditionType(snapshotv1alpha1.RestoredCondition),
			Status:  corev1.ConditionFalse,
			Reason:  restoreIncompatibleReason,
			Message: mismatch.Reason(),
		})
		r.pod.Annotations[snapshotv1alpha1.SkipCompatCheckAnnotation] = "true"
		r.comparison.calls = nil

		r.reconcile(t)

		assert.Empty(t, r.comparison.calls, "the reopened restore was compared anyway")
		assert.Len(t, r.events(t, restoreIncompatibleReason), 1, "the reopened restore was refused again")
	})
}

// Both gates log the same sentence for the same refusal, and each names the gate
// it came from, so an operator greps one field and learns how far the restore got
// before the node turned it down.
func TestRefusalIsLoggedWithTheSameReasonAtBothGates(t *testing.T) {
	mismatch := compat.Mismatch{Check: "memory-limit", Source: "32Gi", Target: "1Gi"}
	wantReason := "memory-limit: source 32Gi, target 1Gi"

	t.Run("preflight gate", func(t *testing.T) {
		r := newGatedRestore(t, mismatch)

		r.reconcile(t)

		refusal := r.refusalLog(t)
		assert.Equal(t, wantReason, refusal["reason"])
		assert.Equal(t, string(compat.GatePreflight), refusal["gate"])
		// The refusal names the pod it belongs to, so a reader does not have to
		// correlate on time.
		assert.Equal(t, "inference/restore-worker", refusal["pod"])
	})

	t.Run("inspect gate", func(t *testing.T) {
		r := newGatedRestore(t)
		r.controller.restoreFn = refuseWith(mismatch)

		r.runRestore(t)

		refusal := r.refusalLog(t)
		assert.Equal(t, wantReason, refusal["reason"])
		assert.Equal(t, string(compat.GateInspect), refusal["gate"])
	})
}

// A refusal from the second gate is not a CRIU failure, so it neither reports
// one nor kills the placeholder: killing it would restart the container straight
// back into the same answer.
func TestRunRestoreTreatsIncompatibleAsTerminal(t *testing.T) {
	r := newGatedRestore(t)
	rt := &fakeRuntime{}
	r.controller.runtime = rt
	sentinels := 0
	r.controller.writeControlSentinelFn = func(int, string) error {
		sentinels++
		return nil
	}
	r.controller.restoreFn = refuseWith(compat.Mismatch{Check: "cpu-arch", Source: "amd64", Target: "arm64"})

	requeue := r.runRestore(t)

	assert.False(t, requeue, "a refusal asked to be driven again")
	assert.Empty(t, r.events(t, restoreFailedReason), "refusal reported itself as a restore failure")
	assert.Zero(t, sentinels, "refusal released the workload")
	assert.Empty(t, rt.resolvedContainerIDs, "refusal reached the placeholder kill path")
}

// The gate runs in preflight, before the restore is entered at all, so a refusal
// leaves no in-flight entry and no restore worker behind.
func TestReconcileRestorePodRefusesBeforeEnteringRestore(t *testing.T) {
	r := newGatedRestore(t, compat.Mismatch{Check: "memory-limit", Source: "32Gi", Target: "1Gi"})
	r.controller.restoreFn = func(context.Context, snapshotruntime.Runtime, logr.Logger, executor.RestoreRequest, executor.RestoreMounter) (int, error) {
		t.Error("a refused restore was entered")
		return 0, nil
	}

	r.reconcile(t)

	assert.Len(t, r.comparison.calls, 1)
	assert.Empty(t, r.events(t, restoreRequestedReason), "refused restore still announced a request")
	assert.Empty(t, r.controller.inFlight, "refused restore claimed an attempt")
}

func refuseWith(mismatches ...compat.Mismatch) func(context.Context, snapshotruntime.Runtime, logr.Logger, executor.RestoreRequest, executor.RestoreMounter) (int, error) {
	return func(context.Context, snapshotruntime.Runtime, logr.Logger, executor.RestoreRequest, executor.RestoreMounter) (int, error) {
		return 0, compat.NewIncompatibleError(compat.GateInspect, mismatches)
	}
}

// The facts recorded at capture describe one container, so a multi-container pod
// must not contribute another container's image or limits.
func TestPodFactsReadTheTargetContainer(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{
				Name:  "sidecar",
				Image: "busybox:1.36",
				Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("256Mi"),
				}},
			},
			{
				Name:  "main",
				Image: "nvcr.io/nvidia/tritonserver:24.09-py3",
				Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("4"),
					corev1.ResourceMemory: resource.MustParse("16Gi"),
				}},
			},
		}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
			{Name: "sidecar", ImageID: "sha256:sidecar"},
			{Name: "main", ImageID: "docker-pullable://nvcr.io/nvidia/tritonserver@sha256:deadbeef"},
		}},
	}

	assert.Equal(t, compat.Facts{
		Image:       "nvcr.io/nvidia/tritonserver:24.09-py3",
		ImageID:     "docker-pullable://nvcr.io/nvidia/tritonserver@sha256:deadbeef",
		CPULimit:    "4",
		MemoryLimit: "16Gi",
	}, podFacts(pod, "main"))
}

// A fact the pod does not carry stays unknown. An unlimited container is not a
// container limited to zero, and a status the kubelet has not published yet is
// not an image ID of "".
func TestPodFactsLeaveWhatThePodDoesNotSayUnknown(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:  "main",
			Image: "busybox:1.36",
			Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("16Gi"),
			}},
		}}},
	}

	assert.Equal(t, compat.Facts{Image: "busybox:1.36", MemoryLimit: "16Gi"}, podFacts(pod, "main"))
	assert.Equal(t, compat.Facts{}, podFacts(pod, "absent"), "a container not in the pod")
	assert.Equal(t, compat.Facts{}, podFacts(nil, "main"), "no pod at all")
}
