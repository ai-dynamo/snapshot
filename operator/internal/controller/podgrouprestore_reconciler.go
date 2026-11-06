// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/ai-dynamo/snapshot/api/podcontract"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// +kubebuilder:rbac:groups=nvidia.com,resources=podgrouprestores,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=nvidia.com,resources=podgrouprestores/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=nvidia.com,resources=podgrouprestores/finalizers,verbs=update
// +kubebuilder:rbac:groups=nvidia.com,resources=podgroupsnapshots,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

// PodGroupRestoreReconciler reconciles a PodGroupRestore: it admits the
// restore, pins every member to its restore Pod, and aggregates what the
// members report into the restore's status.
//
// As for a PodGroupSnapshot, the operator never fails a restore while member
// work runs, except at the deadline and on deletion. Member work starts with
// the pins, since each agent waits for its member's pins before it restores.
// The reconciler therefore decides every refusal before it pins any member,
// pins every member in one status patch, and from then on only records what
// the members report: each agent reports on its restore Pod's conditions,
// naming its attempt in nvidia.com/restore-group-report-uid, and the status
// reads only this restore's reports.
type PodGroupRestoreReconciler struct {
	client.Client
	NonCacheReadClient client.Reader
	Recorder           record.EventRecorder
	// AgentNamespace and AgentSelector select the node agent Pods.
	// Admission waits until the agent on each target's node is Ready and
	// supports the group protocol.
	AgentNamespace string
	AgentSelector  labels.Selector
}

// Reconcile is a thin sequence: observe the members and, while no member is
// pinned, run admission; derive the status, which pins every member once
// admission passes; record it; then, for a restore being deleted, release its
// finalizer once it has finished. Recording comes first, so a Cancelled
// restore is recorded before it disappears.
func (r *PodGroupRestoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	restore := &snapshotv1alpha1.PodGroupRestore{}
	if err := r.Get(ctx, req.NamespacedName, restore); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	deleting := !restore.DeletionTimestamp.IsZero()
	if deleting && !controllerutil.ContainsFinalizer(restore, snapshotv1alpha1.PodGroupInUseFinalizer) {
		return ctrl.Result{}, nil
	}
	if !deleting {
		if added, err := addPodGroupFinalizer(ctx, r.Client, restore); err != nil || added {
			return ctrl.Result{}, err
		}
	}

	now := time.Now()
	observed, err := r.observe(ctx, restore, now)
	if err != nil {
		return ctrl.Result{}, err
	}
	desired := derivePodGroupRestoreStatus(&restore.Spec, observed, restore.Status, now)
	if err := r.recordStatus(ctx, restore, desired); err != nil {
		return ctrl.Result{}, err
	}

	if deleting && podGroupRestoreFinished(&restore.Status) {
		return ctrl.Result{}, removePodGroupFinalizer(ctx, r.Client, restore)
	}
	return ctrl.Result{RequeueAfter: podGroupRestoreRequeue(observed, &restore.Status, now)}, nil
}

// observe reads the PodGroupSnapshot's ranks and each pinned member's report,
// then runs admission while no member is pinned. Every member is pinned in the
// same status patch, so once any is, admission never runs again, and a
// restore whose members wait for their pins is never refused.
func (r *PodGroupRestoreReconciler) observe(
	ctx context.Context,
	restore *snapshotv1alpha1.PodGroupRestore,
	now time.Time,
) (podGroupRestoreObservation, error) {
	observed := podGroupRestoreObservation{
		members:  make([]podGroupRestoreMemberObservation, len(restore.Spec.Targets)),
		deleting: !restore.DeletionTimestamp.IsZero(),
		deadline: restore.CreationTimestamp.Add(time.Duration(restore.Spec.DeadlineSeconds) * time.Second),
	}
	snapshot := &snapshotv1alpha1.PodGroupSnapshot{}
	switch err := r.Get(ctx, client.ObjectKey{Namespace: restore.Namespace, Name: restore.Spec.SnapshotRef.Name}, snapshot); {
	case apierrors.IsNotFound(err):
	case err != nil:
		return podGroupRestoreObservation{}, fmt.Errorf("get PodGroupSnapshot %q: %w", restore.Spec.SnapshotRef.Name, err)
	case snapshot.UID == restore.Spec.SnapshotRef.UID:
		observed.ranks = make(map[string]int, len(snapshot.Spec.Members))
		for rank, member := range snapshot.Spec.Members {
			observed.ranks[member.ID] = rank
		}
	}

	pinned := make(map[string]*snapshotv1alpha1.PodGroupRestoreMemberStatus, len(restore.Status.Members))
	for i := range restore.Status.Members {
		if m := &restore.Status.Members[i]; m.Pod != nil {
			pinned[m.ID] = m
		}
	}
	for i, target := range restore.Spec.Targets {
		if m := pinned[target.ID]; m != nil {
			member, err := r.observeMember(ctx, restore, m)
			if err != nil {
				return podGroupRestoreObservation{}, err
			}
			observed.members[i] = member
		}
	}

	if len(pinned) > 0 || observed.deleting || !now.Before(observed.deadline) ||
		meta.IsStatusConditionTrue(restore.Status.Conditions, snapshotv1alpha1.PodGroupConditionFailed) {
		return observed, nil
	}
	admission, pins, err := r.admit(ctx, r.Client, restore)
	if err == nil && (admission.state == podGroupAdmissionRefused || admission.state == podGroupAdmissionPassed) {
		// Refusing and pinning are both final: pins start the members' work.
		// So confirm either against the API server, and record what it shows.
		// The cache may lag behind a new target Pod, a binding, a snapshot that
		// became Ready, or another restore's pin of the same Pod.
		admission, pins, err = r.admit(ctx, r.NonCacheReadClient, restore)
	}
	if err != nil {
		return podGroupRestoreObservation{}, err
	}
	observed.admission, observed.pins = admission, pins
	return observed, nil
}

// observeMember reads a pinned member's Pod from the cache and, when it is the
// pinned Pod, takes this restore's report from it (podGroupRestoreReportOf), so
// the status reads neither another Pod nor another attempt's conditions. When
// the cache does not hold the pinned Pod while the member is unfinished, the
// API server confirms whether it is gone: failing the member is final, so the
// cache alone is not enough.
func (r *PodGroupRestoreReconciler) observeMember(
	ctx context.Context,
	restore *snapshotv1alpha1.PodGroupRestore,
	m *snapshotv1alpha1.PodGroupRestoreMemberStatus,
) (podGroupRestoreMemberObservation, error) {
	var observed podGroupRestoreMemberObservation
	key := client.ObjectKey{Namespace: restore.Namespace, Name: m.Pod.Name}
	pod := &corev1.Pod{}
	switch err := r.Get(ctx, key, pod); {
	case apierrors.IsNotFound(err):
	case err != nil:
		return observed, fmt.Errorf("get Pod %q of member %q: %w", key.Name, m.ID, err)
	case pod.UID == m.Pod.UID:
		observed.report = podGroupRestoreReportOf(pod, restore.UID)
		return observed, nil
	}
	if isRestorePhaseFinal(m.Phase) {
		return observed, nil
	}
	switch err := r.NonCacheReadClient.Get(ctx, key, pod); {
	case apierrors.IsNotFound(err):
		observed.podDeleted = true
	case err != nil:
		return observed, fmt.Errorf("confirm Pod %q is gone: %w", key.Name, err)
	default:
		observed.podDeleted = pod.UID != m.Pod.UID
	}
	return observed, nil
}

// recordStatus patches the restore's status when it changed, then emits an
// event for each transition (see patchPodGroupStatus). On success restore
// holds the patched object.
func (r *PodGroupRestoreReconciler) recordStatus(
	ctx context.Context,
	restore *snapshotv1alpha1.PodGroupRestore,
	desired snapshotv1alpha1.PodGroupRestoreStatus,
) error {
	if apiequality.Semantic.DeepEqual(restore.Status, desired) {
		return nil
	}
	original := restore.DeepCopy()
	restore.Status = desired
	return patchPodGroupStatus(ctx, r.Client, r.Recorder, original, restore, original.Status.Conditions, desired.Conditions)
}

// podGroupRestoreRequeue is when to look at a restore again without an event,
// by the rule both kinds share (see podGroupRequeue). A target Pod's events
// wake a restore that waits for it.
func podGroupRestoreRequeue(observed podGroupRestoreObservation, status *snapshotv1alpha1.PodGroupRestoreStatus, now time.Time) time.Duration {
	return podGroupRequeue(observed.admission, observed.deadline, now, podGroupRestoreMemberViews(status.Members, observed.ranks))
}

// SetupWithManager watches the restore and every Pod that carries
// nvidia.com/restore-group, mapped to the restore it names: target Pods
// appear, get bound, scheduled, their addresses and containers, and carry the
// agents' reports.
func (r *PodGroupRestoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.NonCacheReadClient == nil {
		return errors.New("pod group restore reconciler requires a non-cache read client")
	}
	if err := validatePodGroupAgents(r.AgentNamespace, r.AgentSelector); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&snapshotv1alpha1.PodGroupRestore{}).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(restoreGroupPodToPodGroupRestore)).
		Complete(r)
}

// restoreGroupPodToPodGroupRestore maps a Pod to the PodGroupRestore that its
// nvidia.com/restore-group annotation names, in the Pod's namespace.
func restoreGroupPodToPodGroupRestore(_ context.Context, obj client.Object) []reconcile.Request {
	name := obj.GetAnnotations()[podcontract.RestoreGroupAnnotation]
	if name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: name}}}
}
