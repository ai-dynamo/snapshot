// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// errPodSnapshotNameConflict marks an existing PodSnapshot at the SnapshotJob's
// deterministic name that is not owned by this SnapshotJob — a terminal name
// collision, not a cache race.
var errPodSnapshotNameConflict = errors.New("existing PodSnapshot is not owned by this SnapshotJob")

// findSourcePod returns the source Job's pod, or a NotFound error if the Job has
// not created it yet (callers use apierrors.IsNotFound to decide how to proceed).
// This is a read triggered by a Job status change, not a pod watch — the
// controller does not watch pods (design: "SnapshotJob observes the Job, not the
// Pod, for failure status").
func findSourcePod(ctx context.Context, c client.Client, job *batchv1.Job) (*corev1.Pod, error) {
	var pods corev1.PodList
	if err := c.List(ctx, &pods,
		client.InNamespace(job.Namespace),
		client.MatchingLabels{batchv1.JobNameLabel: job.Name},
	); err != nil {
		return nil, err
	}
	for i := range pods.Items {
		if metav1.IsControlledBy(&pods.Items[i], job) {
			return &pods.Items[i], nil
		}
	}
	return nil, apierrors.NewNotFound(corev1.Resource("pods"), job.Name)
}

// findOwnedPodSnapshot returns this SnapshotJob's PodSnapshot, located by
// SnapshotJobOwnerLabel. It returns a NotFound error when none exists. The
// produced PodSnapshot carries no ownerReference (artifacts must outlive the
// SnapshotJob — see buildPodSnapshot), so the label match from List *is* the
// ownership check; there is no IsControlledBy to additionally verify. More than
// one match is a controller invariant violation (deterministic naming makes it
// impossible under normal operation), so it emits a warning and returns a
// non-terminal error to requeue rather than silently picking one.
func (r *SnapshotJobReconciler) findOwnedPodSnapshot(ctx context.Context, sj *snapshotv1alpha1.SnapshotJob) (*snapshotv1alpha1.PodSnapshot, error) {
	var snaps snapshotv1alpha1.PodSnapshotList
	if err := r.List(ctx, &snaps,
		client.InNamespace(sj.Namespace),
		client.MatchingLabels{snapshotv1alpha1.SnapshotJobOwnerLabel: sj.Name},
	); err != nil {
		return nil, err
	}
	switch len(snaps.Items) {
	case 0:
		return nil, apierrors.NewNotFound(
			schema.GroupResource{Group: snapshotv1alpha1.GroupVersion.Group, Resource: "podsnapshots"},
			sj.Name,
		)
	case 1:
		return &snaps.Items[0], nil
	default:
		err := fmt.Errorf("multiple PodSnapshots owned by SnapshotJob %q; expected at most one", sj.Name)
		r.Recorder.Event(sj, corev1.EventTypeWarning, "PodSnapshotLookupAmbiguous", err.Error())
		return nil, err
	}
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
	if len(targetContainers) == 0 {
		return nil, fmt.Errorf("spec.podSnapshotTemplate.targetContainers is empty")
	}
	return &snapshotv1alpha1.PodSnapshot{
		TypeMeta: metav1.TypeMeta{
			APIVersion: snapshotv1alpha1.GroupVersion.String(),
			Kind:       "PodSnapshot",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      sj.Name,
			Namespace: sj.Namespace,
			Labels: map[string]string{
				snapshotv1alpha1.SnapshotJobOwnerLabel: sj.Name,
			},
		},
		Spec: snapshotv1alpha1.PodSnapshotSpec{
			Source: snapshotv1alpha1.PodSnapshotSource{
				PodRef: snapshotv1alpha1.PodReference{Name: pod.Name, UID: pod.UID, Containers: targetContainers},
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
			return r.classifyExistingPodSnapshot(ctx, sj, snap.Name, err)
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
func (r *SnapshotJobReconciler) classifyExistingPodSnapshot(ctx context.Context, sj *snapshotv1alpha1.SnapshotJob, name string, createErr error) (*snapshotv1alpha1.PodSnapshot, error) {
	existing := &snapshotv1alpha1.PodSnapshot{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: sj.Namespace, Name: name}, existing); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("PodSnapshot %q already exists but is not yet in cache, requeueing: %w", name, createErr)
		}
		return nil, fmt.Errorf("get existing PodSnapshot %q: %w", name, err)
	}
	if existing.Labels[snapshotv1alpha1.SnapshotJobOwnerLabel] != sj.Name {
		return nil, fmt.Errorf("%w: PodSnapshot %q", errPodSnapshotNameConflict, name)
	}
	return existing, nil
}

// mapPodSnapshotToSnapshotJob maps a PodSnapshot (including a delete-event
// tombstone) back to the SnapshotJob that owns it via SnapshotJobOwnerLabel. It
// MUST unwrap cache.DeletedFinalStateUnknown so a PodSnapshot delete still
// re-enqueues its SnapshotJob, mirroring podSnapshotRefFromContentObj's tombstone
// handling for PodSnapshotContent.
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
// namespace/name from a PodSnapshot's SnapshotJobOwnerLabel, unwrapping a
// cache.DeletedFinalStateUnknown tombstone first. It errors when the object is
// not a PodSnapshot (a malformed watch event, not a control-flow skip).
func snapshotJobOwnerFromPodSnapshotObj(obj any) (types.NamespacedName, error) {
	if tombstone, isTombstone := obj.(cache.DeletedFinalStateUnknown); isTombstone {
		obj = tombstone.Obj
	}
	snap, ok := obj.(*snapshotv1alpha1.PodSnapshot)
	if !ok {
		return types.NamespacedName{}, fmt.Errorf("expected *PodSnapshot, got %T", obj)
	}
	return types.NamespacedName{Namespace: snap.Namespace, Name: snap.Labels[snapshotv1alpha1.SnapshotJobOwnerLabel]}, nil
}
