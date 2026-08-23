// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// errPodSnapshotNameConflict marks an existing PodSnapshot at the SnapshotJob's
// deterministic name that is not owned by this SnapshotJob — a terminal name
// collision, not a cache race.
var errPodSnapshotNameConflict = errors.New("existing PodSnapshot is not owned by this SnapshotJob")

const sourcePodRequeueBackstop = 2 * time.Second

// createPodSnapshotForSourceJob waits for the source Pod, then creates or
// classifies the deterministic PodSnapshot. A missing active source Pod gets a
// bounded backstop requeue; completed source work with no remaining Pod is a
// concrete missing-capture failure.
func (r *SnapshotJobReconciler) createPodSnapshotForSourceJob(ctx context.Context, sj *snapshotv1alpha1.SnapshotJob, job *batchv1.Job) (snapshotJobObservation, ctrl.Result, error) {
	observed := snapshotJobObservation{job: job}
	pod, err := findSourcePod(ctx, r.APIReader, job)
	if apierrors.IsNotFound(err) {
		observed.sourcePodMissing = true
		if classifySourceJobTerminal(job).state == sourceJobComplete {
			observed.failure = &snapshotJobFailure{
				reason: snapshotv1alpha1.ReasonSourceCompletedWithoutCapture,
				cause:  fmt.Errorf("source Job completed and its pod is gone before a PodSnapshot capture result was recorded"),
			}
			return observed, ctrl.Result{}, nil
		}
		return observed, ctrl.Result{RequeueAfter: sourcePodRequeueBackstop}, nil
	}
	if err != nil {
		return snapshotJobObservation{}, ctrl.Result{}, fmt.Errorf("find source pod for Job %q: %w", job.Name, err)
	}

	snap, err := r.createPodSnapshot(ctx, sj, pod)
	if errors.Is(err, errPodSnapshotNameConflict) {
		observed.failure = &snapshotJobFailure{reason: snapshotv1alpha1.ReasonPodSnapshotNameConflict, cause: err}
		return observed, ctrl.Result{}, nil
	}
	if err != nil {
		return snapshotJobObservation{}, ctrl.Result{}, err
	}

	observed.podSnapshot = snap
	return observed, ctrl.Result{}, nil
}

// findSourcePod returns the source Job's pod, or a NotFound error if the Job has
// not created it yet (callers use apierrors.IsNotFound to decide how to proceed).
// This is a read triggered by a Job status change, not a pod watch — the
// controller does not watch pods (design: "SnapshotJob observes the Job, not the
// Pod, for failure status").
func findSourcePod(ctx context.Context, reader client.Reader, job *batchv1.Job) (*corev1.Pod, error) {
	var pods corev1.PodList
	if err := reader.List(ctx, &pods,
		client.InNamespace(job.Namespace),
		client.MatchingLabels{batchv1.JobNameLabel: job.Name},
	); err != nil {
		return nil, err
	}
	var controlled []*corev1.Pod
	for i := range pods.Items {
		if metav1.IsControlledBy(&pods.Items[i], job) {
			controlled = append(controlled, &pods.Items[i])
		}
	}
	switch len(controlled) {
	case 0:
		return nil, apierrors.NewNotFound(corev1.Resource("pods"), job.Name)
	case 1:
		return controlled[0], nil
	default:
		return nil, fmt.Errorf("source Job %q controls %d pods; expected exactly one", job.Name, len(controlled))
	}
}

// findOwnedPodSnapshot returns the PodSnapshot at this SnapshotJob's deterministic
// namespace/name and verifies that it belongs to this SnapshotJob incarnation.
// Differently named objects are unrelated even if they carry the same owner
// labels; a same-name object with mismatched ownership is a terminal conflict.
func (r *SnapshotJobReconciler) findOwnedPodSnapshot(ctx context.Context, sj *snapshotv1alpha1.SnapshotJob) (*snapshotv1alpha1.PodSnapshot, error) {
	return readOwnedPodSnapshot(ctx, r.Client, sj)
}

func (r *SnapshotJobReconciler) readAuthoritativeOwnedPodSnapshot(ctx context.Context, sj *snapshotv1alpha1.SnapshotJob) (*snapshotv1alpha1.PodSnapshot, error) {
	snap, err := readOwnedPodSnapshot(ctx, r.APIReader, sj)
	if err != nil {
		return nil, fmt.Errorf("authoritatively read recorded PodSnapshot %q: %w", sj.Name, err)
	}
	return snap, nil
}

func readOwnedPodSnapshot(ctx context.Context, reader client.Reader, sj *snapshotv1alpha1.SnapshotJob) (*snapshotv1alpha1.PodSnapshot, error) {
	snap := &snapshotv1alpha1.PodSnapshot{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: sj.Namespace, Name: sj.Name}, snap); err != nil {
		return nil, err
	}
	if err := validatePodSnapshotOwnership(sj, snap); err != nil {
		return nil, err
	}
	return snap, nil
}

// validatePodSnapshotForAdoption closes the create/status-write crash window.
// Before the child UID has been persisted, mutable owner labels are insufficient:
// the immutable source identity must also match the Pod controlled by this Job.
func (r *SnapshotJobReconciler) validatePodSnapshotForAdoption(ctx context.Context, sj *snapshotv1alpha1.SnapshotJob, job *batchv1.Job, snap *snapshotv1alpha1.PodSnapshot) (*snapshotJobFailure, error) {
	pod, err := findSourcePod(ctx, r.APIReader, job)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return &snapshotJobFailure{
				reason: snapshotv1alpha1.ReasonPodSnapshotNameConflict,
				cause: fmt.Errorf("cannot safely adopt PodSnapshot %q because its source Pod is no longer available",
					snap.Name),
			}, nil
		}
		return nil, fmt.Errorf("find source Pod before adopting PodSnapshot %q: %w", snap.Name, err)
	}

	desired, err := buildPodSnapshot(sj, pod)
	if err != nil {
		return &snapshotJobFailure{reason: snapshotv1alpha1.ReasonInvalidSpec, cause: err}, nil
	}
	if !podSnapshotHasExpectedIdentity(snap, desired) {
		return &snapshotJobFailure{
			reason: snapshotv1alpha1.ReasonPodSnapshotNameConflict,
			cause: fmt.Errorf("existing PodSnapshot %q does not carry the immutable source identity expected for this SnapshotJob",
				snap.Name),
		}, nil
	}
	return nil, nil
}

// buildPodSnapshot constructs the desired PodSnapshot for a SnapshotJob's source
// pod. The name is the SnapshotJob's own name (bounded by the same DNS-1123
// validation already applied to the source Job); SnapshotJobOwnerLabel is the
// lookup key. The source pod's UID is pinned so PodSnapshotReconciler rejects a
// same-named recreation instead of capturing the wrong workload.
//
// Deliberately no ownerReference: SnapshotJob does not own PodSnapshot or
// PodSnapshotContent — artifacts must outlive the SnapshotJob, and a controller
// ownerRef would make Kubernetes GC delete this artifact along with its owner.
func buildPodSnapshot(sj *snapshotv1alpha1.SnapshotJob, pod *corev1.Pod) (*snapshotv1alpha1.PodSnapshot, error) {
	targetContainers := sj.Spec.PodSnapshotTemplate.TargetContainers
	if len(targetContainers) != 1 {
		return nil, fmt.Errorf("spec.podSnapshotTemplate.targetContainers must have exactly one entry, got %d", len(targetContainers))
	}
	// Copy: the produced PodSnapshot must not share a backing array with the
	// SnapshotJob's own spec slice.
	containers := append([]string(nil), targetContainers...)
	return &snapshotv1alpha1.PodSnapshot{
		TypeMeta: metav1.TypeMeta{
			APIVersion: snapshotv1alpha1.GroupVersion.String(),
			Kind:       "PodSnapshot",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      sj.Name,
			Namespace: sj.Namespace,
			Labels: map[string]string{
				snapshotv1alpha1.SnapshotJobOwnerLabel:    sj.Name,
				snapshotv1alpha1.SnapshotJobOwnerUIDLabel: string(sj.UID),
			},
		},
		Spec: snapshotv1alpha1.PodSnapshotSpec{
			Source: snapshotv1alpha1.PodSnapshotSource{
				PodRef: snapshotv1alpha1.PodReference{Name: pod.Name, UID: pod.UID, Containers: containers},
			},
		},
	}, nil
}

// createPodSnapshot creates this SnapshotJob's PodSnapshot. The caller has
// confirmed via findOwnedPodSnapshot that none exists, so this is a pure create.
// On AlreadyExists the object at the deterministic name is classified: cache lag
// (ours) is adopted; a foreign owner is terminal.
func (r *SnapshotJobReconciler) createPodSnapshot(ctx context.Context, sj *snapshotv1alpha1.SnapshotJob, pod *corev1.Pod) (*snapshotv1alpha1.PodSnapshot, error) {
	snap, err := buildPodSnapshot(sj, pod)
	if err != nil {
		return nil, err
	}
	if err := r.Create(ctx, snap); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return r.classifyExistingPodSnapshot(ctx, sj, snap, err)
		}
		r.Recorder.Event(sj, corev1.EventTypeWarning, "PodSnapshotCreateFailed", err.Error())
		return nil, fmt.Errorf("create PodSnapshot %q: %w", snap.Name, err)
	}
	return snap, nil
}

// classifyExistingPodSnapshot resolves what holds the SnapshotJob's deterministic
// PodSnapshot name after a Create AlreadyExists. Cache lag (the object is ours
// but the informer hasn't synced) is harmless: return the existing object so the
// caller can observe it without an extra reconcile. A foreign owner is a
// permanent name collision: return errPodSnapshotNameConflict (terminal). A
// re-read NotFound means the cache is still behind: surface the original
// AlreadyExists so the caller requeues.
func (r *SnapshotJobReconciler) classifyExistingPodSnapshot(ctx context.Context, sj *snapshotv1alpha1.SnapshotJob, desired *snapshotv1alpha1.PodSnapshot, createErr error) (*snapshotv1alpha1.PodSnapshot, error) {
	existing := &snapshotv1alpha1.PodSnapshot{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(desired), existing); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("PodSnapshot %q already exists but is not yet in cache, requeueing: %w", desired.Name, createErr)
		}
		return nil, fmt.Errorf("get existing PodSnapshot %q: %w", desired.Name, err)
	}
	if err := validatePodSnapshotOwnership(sj, existing); err != nil {
		return nil, err
	}
	if !podSnapshotHasExpectedIdentity(existing, desired) {
		return nil, fmt.Errorf("%w: PodSnapshot %q does not match the expected source identity",
			errPodSnapshotNameConflict, desired.Name)
	}
	return existing, nil
}

func validatePodSnapshotOwnership(sj *snapshotv1alpha1.SnapshotJob, snap *snapshotv1alpha1.PodSnapshot) error {
	if !podSnapshotBelongsToSnapshotJob(snap, sj) {
		return fmt.Errorf("%w: PodSnapshot %q belongs to a different SnapshotJob incarnation",
			errPodSnapshotNameConflict, snap.Name)
	}
	if expectedUID := sj.Status.PodSnapshotUID; expectedUID != "" && snap.UID != expectedUID {
		return fmt.Errorf("%w: PodSnapshot %q was replaced: found uid %q, expected %q",
			errPodSnapshotNameConflict, snap.Name, snap.UID, expectedUID)
	}
	return nil
}

func podSnapshotHasExpectedIdentity(actual, desired *snapshotv1alpha1.PodSnapshot) bool {
	actualSource := actual.Spec.Source.PodRef
	desiredSource := desired.Spec.Source.PodRef
	if actualSource.Name != desiredSource.Name || actualSource.UID != desiredSource.UID ||
		len(actualSource.Containers) != len(desiredSource.Containers) {
		return false
	}
	for i := range actualSource.Containers {
		if actualSource.Containers[i] != desiredSource.Containers[i] {
			return false
		}
	}
	return true
}

func podSnapshotBelongsToSnapshotJob(snap *snapshotv1alpha1.PodSnapshot, sj *snapshotv1alpha1.SnapshotJob) bool {
	return snap.Labels[snapshotv1alpha1.SnapshotJobOwnerLabel] == sj.Name &&
		snap.Labels[snapshotv1alpha1.SnapshotJobOwnerUIDLabel] == string(sj.UID)
}

// mapPodSnapshotToSnapshotJob maps the already-unwrapped client.Object back to
// its SnapshotJob via SnapshotJobOwnerLabel.
func mapPodSnapshotToSnapshotJob(ctx context.Context, obj client.Object) []reconcile.Request {
	ref, err := snapshotJobOwnerFromPodSnapshotObj(obj)
	if err != nil {
		log.FromContext(ctx).Error(err, "Failed to map PodSnapshot to SnapshotJob")
		return nil
	}
	if ref.Name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: ref}}
}

// snapshotJobOwnerFromPodSnapshotObj extracts the owning SnapshotJob's
// namespace/name. controller-runtime unwraps delete tombstones before invoking
// EnqueueRequestsFromMapFunc.
func snapshotJobOwnerFromPodSnapshotObj(obj client.Object) (types.NamespacedName, error) {
	snap, ok := obj.(*snapshotv1alpha1.PodSnapshot)
	if !ok {
		return types.NamespacedName{}, fmt.Errorf("expected *PodSnapshot, got %T", obj)
	}
	return types.NamespacedName{Namespace: snap.Namespace, Name: snap.Labels[snapshotv1alpha1.SnapshotJobOwnerLabel]}, nil
}
