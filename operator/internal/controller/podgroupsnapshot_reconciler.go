// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// +kubebuilder:rbac:groups=nvidia.com,resources=podgroupsnapshots,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=nvidia.com,resources=podgroupsnapshots/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=nvidia.com,resources=podgroupsnapshots/finalizers,verbs=update
// +kubebuilder:rbac:groups=nvidia.com,resources=podsnapshots,verbs=create;get;list;watch
// +kubebuilder:rbac:groups=nvidia.com,resources=podsnapshotcontents,verbs=get;list;watch
// +kubebuilder:rbac:groups=nvidia.com,resources=podgrouprestores,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

// PodGroupSnapshotReconciler reconciles a PodGroupSnapshot: it admits the
// group, creates one owned PodSnapshot per member, and aggregates what the
// members report into the group's status.
//
// The operator never fails a group while member work runs, except at the
// deadline and on deletion, both of which the coordinator observes itself.
// The coordinator does not read group status, so a failure only the operator
// knew about would leave every agent waiting until the deadline. The
// reconciler therefore refuses a bad group before it creates any member work,
// then creates all of it, and from then on only records what the members
// report: each agent reports on its member's PodSnapshotContent, and the
// PodSnapshot controller mirrors that to the PodSnapshot.
type PodGroupSnapshotReconciler struct {
	client.Client
	NonCacheReadClient client.Reader
	Recorder           record.EventRecorder
	// AgentNamespace and AgentSelector select the node agent Pods.
	// Admission waits until the agent on each member's node is Ready and
	// supports the group protocol.
	AgentNamespace string
	AgentSelector  labels.Selector
}

// podGroupRestoreSnapshotUIDField indexes PodGroupRestores by the UID of the
// PodGroupSnapshot they restore, which holds its finalizer while any of them
// is unfinished.
const podGroupRestoreSnapshotUIDField = "spec.snapshotRef.uid"

// Reconcile is a thin sequence: observe the members and, while no member work
// exists, run admission; derive the status; record it; then act on it, by
// creating the members' PodSnapshots or, for a group being deleted, releasing
// its finalizer. Recording comes first, so a Cancelled group is recorded
// before it disappears.
//
// Every irreversible decision is made from the API server. The reconciler
// observes through the cache first. When the status derived from that would
// make a change that can never be undone (podGroupSnapshotChangeIsIrreversible),
// it observes again through the API server and derives again from the
// persisted status, never from the first derived one, whose final phases would
// stick. It records only the second pass, and acts on that pass's observation
// and status. This keeps cache lag out of every final decision; it does not
// make the reads and the status write atomic, so a report that lands in
// between is the next reconcile's.
func (r *PodGroupSnapshotReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	group := &snapshotv1alpha1.PodGroupSnapshot{}
	if err := r.Get(ctx, req.NamespacedName, group); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	deleting := !group.DeletionTimestamp.IsZero()
	if deleting && !controllerutil.ContainsFinalizer(group, snapshotv1alpha1.PodGroupInUseFinalizer) {
		return ctrl.Result{}, nil
	}
	if !deleting {
		if added, err := addPodGroupFinalizer(ctx, r.Client, group); err != nil || added {
			return ctrl.Result{}, err
		}
	}

	now := time.Now()
	observed, err := r.observe(ctx, r.Client, group, now)
	if err != nil {
		return ctrl.Result{}, err
	}
	desired := derivePodGroupSnapshotStatus(&group.Spec, observed, group.Status, now)
	if podGroupSnapshotChangeIsIrreversible(observed, &group.Status, &desired, deleting) {
		if observed, err = r.observe(ctx, r.NonCacheReadClient, group, now); err != nil {
			return ctrl.Result{}, err
		}
		desired = derivePodGroupSnapshotStatus(&group.Spec, observed, group.Status, now)
	}
	if err := r.recordStatus(ctx, group, desired); err != nil {
		return ctrl.Result{}, err
	}

	if deleting {
		if podGroupSnapshotReleasable(observed, &group.Status) {
			// Garbage collection then deletes the members' PodSnapshots.
			return ctrl.Result{}, removePodGroupFinalizer(ctx, r.Client, group)
		}
	} else if err := r.createMemberPodSnapshots(ctx, group, observed, now); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: podGroupSnapshotRequeue(observed, &group.Status, now)}, nil
}

// observe reads, through reader, each member's PodSnapshot, its bound content
// and its source Pod's node, and, for a group being deleted, the
// PodGroupRestores that reference it, then runs admission if no member work
// exists yet. Once any member's PodSnapshot exists, admission never runs
// again, so an attempt with some members already created is never refused
// halfway. Reconcile passes the cache first, and the API server when the
// result would make an irreversible decision, such as a refusal or the
// release of the finalizer.
func (r *PodGroupSnapshotReconciler) observe(
	ctx context.Context,
	reader client.Reader,
	group *snapshotv1alpha1.PodGroupSnapshot,
	now time.Time,
) (podGroupSnapshotObservation, error) {
	observed := podGroupSnapshotObservation{
		members:  make([]podGroupMemberObservation, len(group.Spec.Members)),
		deleting: !group.DeletionTimestamp.IsZero(),
		deadline: group.CreationTimestamp.Add(time.Duration(group.Spec.DeadlineSeconds) * time.Second),
	}
	memberWorkExists := false
	previous := make(map[string]*snapshotv1alpha1.PodGroupSnapshotMemberStatus, len(group.Status.Members))
	for i := range group.Status.Members {
		m := &group.Status.Members[i]
		memberWorkExists = memberWorkExists || m.PodSnapshot != nil
		previous[m.ID] = m
	}
	for rank := range group.Spec.Members {
		member, err := r.observeMember(ctx, reader, group, rank, previous[group.Spec.Members[rank].ID])
		if err != nil {
			return podGroupSnapshotObservation{}, err
		}
		observed.members[rank] = member
		memberWorkExists = memberWorkExists || member.podSnapshot != nil
	}

	if observed.deleting {
		// The API server cannot select PodGroupRestores by
		// spec.snapshotRef.uid, which the CRD does not declare as a selectable
		// field, so the restores are listed and filtered here, in both passes.
		restores := &snapshotv1alpha1.PodGroupRestoreList{}
		if err := reader.List(ctx, restores, client.InNamespace(group.Namespace)); err != nil {
			return podGroupSnapshotObservation{}, fmt.Errorf("list the PodGroupRestores of PodGroupSnapshot %q: %w", group.Name, err)
		}
		for i := range restores.Items {
			if restores.Items[i].Spec.SnapshotRef.UID == group.UID {
				observed.restores = append(observed.restores, restores.Items[i])
			}
		}
	}

	if memberWorkExists || observed.deleting || !now.Before(observed.deadline) ||
		meta.IsStatusConditionTrue(group.Status.Conditions, snapshotv1alpha1.PodGroupConditionFailed) {
		return observed, nil
	}
	admission, err := r.admit(ctx, reader, group)
	if err != nil {
		return podGroupSnapshotObservation{}, err
	}
	observed.admission = admission
	return observed, nil
}

// observeMember reads, through reader, one member's PodSnapshot, adopting only
// one that is this group's own (see isMemberPodSnapshot), then its bound
// content and the source Pod's node. previous is the member's status entry, if
// any: an unfinished member whose pinned PodSnapshot reader finds gone, or
// replaced, is observed deleted.
func (r *PodGroupSnapshotReconciler) observeMember(
	ctx context.Context,
	reader client.Reader,
	group *snapshotv1alpha1.PodGroupSnapshot,
	rank int,
	previous *snapshotv1alpha1.PodGroupSnapshotMemberStatus,
) (podGroupMemberObservation, error) {
	var observed podGroupMemberObservation
	member := &group.Spec.Members[rank]

	snap := &snapshotv1alpha1.PodSnapshot{}
	key := client.ObjectKey{Namespace: group.Namespace, Name: podGroupMemberPodSnapshotName(group, member)}
	switch err := reader.Get(ctx, key, snap); {
	case apierrors.IsNotFound(err):
		snap = nil
	case err != nil:
		return observed, fmt.Errorf("get PodSnapshot %q of member %q: %w", key.Name, member.ID, err)
	case isMemberPodSnapshot(group, rank, snap):
		observed.podSnapshot = snap
	}
	observed.podSnapshotDeleted = previous != nil && previous.PodSnapshot != nil && !isCheckpointPhaseFinal(previous.Phase) &&
		(snap == nil || snap.UID != previous.PodSnapshot.UID)

	if contentName := boundContentName(observed.podSnapshot); contentName != "" {
		content := &snapshotv1alpha1.PodSnapshotContent{}
		switch err := reader.Get(ctx, client.ObjectKey{Name: contentName}, content); {
		case apierrors.IsNotFound(err):
		case err != nil:
			return observed, fmt.Errorf("get PodSnapshotContent %q of member %q: %w", contentName, member.ID, err)
		case verifyContentBacklink(observed.podSnapshot, content) == nil:
			observed.content = content
		}
	}

	pod := &corev1.Pod{}
	podKey := client.ObjectKey{Namespace: group.Namespace, Name: member.Source.PodRef.Name}
	switch err := reader.Get(ctx, podKey, pod); {
	case apierrors.IsNotFound(err):
	case err != nil:
		return observed, fmt.Errorf("get source Pod %q of member %q: %w", podKey.Name, member.ID, err)
	case pod.UID == member.Source.PodRef.UID:
		observed.nodeName = pod.Spec.NodeName
	}
	if observed.nodeName == "" && observed.content != nil {
		// A checkpoint ends its source Pod; the content keeps its node.
		observed.nodeName = observed.content.Spec.Source.NodeName
	}
	return observed, nil
}

// boundContentName returns the name of the content bound to snap, or "".
func boundContentName(snap *snapshotv1alpha1.PodSnapshot) string {
	if snap == nil {
		return ""
	}
	return ptr.Deref(snap.Status.BoundPodSnapshotContentName, "")
}

// createMemberPodSnapshots creates the PodSnapshot of every member that has
// none, once admission has passed or member work already exists, until the
// group fails or its deadline passes. A member whose pinned PodSnapshot is
// gone is never recreated: a member is never retried in place.
func (r *PodGroupSnapshotReconciler) createMemberPodSnapshots(
	ctx context.Context,
	group *snapshotv1alpha1.PodGroupSnapshot,
	observed podGroupSnapshotObservation,
	now time.Time,
) error {
	if meta.IsStatusConditionTrue(group.Status.Conditions, snapshotv1alpha1.PodGroupConditionFailed) || !now.Before(observed.deadline) {
		return nil
	}
	started := observed.admission.state == podGroupAdmissionPassed
	for _, m := range group.Status.Members {
		started = started || m.PodSnapshot != nil
	}
	if !started {
		return nil
	}
	// One member's failure does not hold back the others; the error requeues.
	var errs []error
	for rank, m := range group.Status.Members {
		if m.PodSnapshot != nil || observed.member(rank).podSnapshot != nil {
			continue
		}
		errs = append(errs, r.createMemberPodSnapshot(ctx, group, rank))
	}
	return errors.Join(errs...)
}

// createMemberPodSnapshot creates one member's PodSnapshot. On AlreadyExists,
// a PodSnapshot that is this group's own is the cache lagging, and the next
// reconcile adopts it. Anything else holds the member's name since admission
// checked it; the member then waits for the deadline, because the operator
// never refuses a group whose member work exists.
func (r *PodGroupSnapshotReconciler) createMemberPodSnapshot(ctx context.Context, group *snapshotv1alpha1.PodGroupSnapshot, rank int) error {
	desired := buildMemberPodSnapshot(group, rank)
	err := r.Create(ctx, desired)
	if err == nil {
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		r.Recorder.Event(group, corev1.EventTypeWarning, "PodSnapshotCreateFailed", err.Error())
		return fmt.Errorf("create PodSnapshot %q: %w", desired.Name, err)
	}
	existing := &snapshotv1alpha1.PodSnapshot{}
	if getErr := r.Get(ctx, client.ObjectKeyFromObject(desired), existing); getErr != nil {
		return fmt.Errorf("PodSnapshot %q already exists but cannot be read yet, requeueing: %w", desired.Name, getErr)
	}
	if !isMemberPodSnapshot(group, rank, existing) {
		r.Recorder.Eventf(group, corev1.EventTypeWarning, snapshotv1alpha1.ReasonPodSnapshotNameConflict,
			"member %q cannot be checkpointed: PodSnapshot %q belongs to something else",
			group.Spec.Members[rank].ID, desired.Name)
	}
	return nil
}

// podGroupMemberPodSnapshotName is the name of a member's PodSnapshot:
// <group>-<member id>. The group's name and the member's ID are each at most
// 63 characters, so the name is always valid.
func podGroupMemberPodSnapshotName(group *snapshotv1alpha1.PodGroupSnapshot, member *snapshotv1alpha1.PodGroupSnapshotMember) string {
	return group.Name + "-" + member.ID
}

// buildMemberPodSnapshot builds the PodSnapshot of the member at rank. It
// takes the labels, annotations and container settings from the member's
// template; the controller owns the rest: the name, a controller owner
// reference to the group, the group labels, and spec.source from the member.
func buildMemberPodSnapshot(group *snapshotv1alpha1.PodGroupSnapshot, rank int) *snapshotv1alpha1.PodSnapshot {
	member := &group.Spec.Members[rank]
	var podLabels, annotations map[string]string
	var containers []snapshotv1alpha1.ContainerSnapshotConfig
	if template := member.PodSnapshotTemplate; template != nil {
		containers = template.Containers
		if template.Metadata != nil {
			podLabels = maps.Clone(template.Metadata.Labels)
			annotations = maps.Clone(template.Metadata.Annotations)
		}
	}
	if podLabels == nil {
		podLabels = make(map[string]string, 2)
	}
	podLabels[snapshotv1alpha1.PodGroupLabel] = group.Name
	podLabels[snapshotv1alpha1.PodGroupMemberLabel] = member.ID
	return newPodSnapshot(
		metav1.ObjectMeta{
			Name:        podGroupMemberPodSnapshotName(group, member),
			Namespace:   group.Namespace,
			Labels:      podLabels,
			Annotations: annotations,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(group, snapshotv1alpha1.GroupVersion.WithKind(snapshotv1alpha1.KindPodGroupSnapshot)),
			},
		},
		member.Source.PodRef,
		containers,
	)
}

// isMemberPodSnapshot reports whether snap is the PodSnapshot of the member at
// rank: controlled by this incarnation of the group, with the member's source.
// Only such a PodSnapshot is adopted; anything else at the member's name is a
// collision.
func isMemberPodSnapshot(group *snapshotv1alpha1.PodGroupSnapshot, rank int, snap *snapshotv1alpha1.PodSnapshot) bool {
	return metav1.IsControlledBy(snap, group) && podSnapshotHasExpectedIdentity(snap, buildMemberPodSnapshot(group, rank))
}

// recordStatus patches the group's status when it changed, then emits an event
// for each transition (see patchPodGroupStatus). On success group holds the
// patched object.
func (r *PodGroupSnapshotReconciler) recordStatus(ctx context.Context, group *snapshotv1alpha1.PodGroupSnapshot, desired snapshotv1alpha1.PodGroupSnapshotStatus) error {
	if apiequality.Semantic.DeepEqual(group.Status, desired) {
		return nil
	}
	original := group.DeepCopy()
	group.Status = desired
	return patchPodGroupStatus(ctx, r.Client, r.Recorder, original, group, original.Status.Conditions, desired.Conditions)
}

// podGroupSnapshotRequeue is when to look at a group again without an event,
// by the rule both kinds share (see podGroupRequeue).
func podGroupSnapshotRequeue(observed podGroupSnapshotObservation, status *snapshotv1alpha1.PodGroupSnapshotStatus, now time.Time) time.Duration {
	return podGroupRequeue(observed.admission, observed.deadline, now, podGroupSnapshotMemberViews(status.Members))
}

// SetupWithManager watches the group, the PodSnapshots it owns, every
// PodSnapshotContent, because the agent's Started condition lives on the
// content, and every PodGroupRestore, which may hold the group's finalizer: a
// content maps to its PodSnapshot, and from there to the group that controls
// it, and a restore maps to the group it restores.
func (r *PodGroupSnapshotReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.NonCacheReadClient == nil {
		return errors.New("pod group snapshot reconciler requires a non-cache read client")
	}
	if err := validatePodGroupAgents(r.AgentNamespace, r.AgentSelector); err != nil {
		return err
	}
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &snapshotv1alpha1.PodGroupRestore{},
		podGroupRestoreSnapshotUIDField, podGroupRestoreSnapshotUID); err != nil {
		return fmt.Errorf("index PodGroupRestores by PodGroupSnapshot UID: %w", err)
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&snapshotv1alpha1.PodGroupSnapshot{}).
		Owns(&snapshotv1alpha1.PodSnapshot{}).
		Watches(
			&snapshotv1alpha1.PodSnapshotContent{},
			handler.EnqueueRequestsFromMapFunc(r.podSnapshotContentToPodGroupSnapshot),
		).
		Watches(
			&snapshotv1alpha1.PodGroupRestore{},
			handler.EnqueueRequestsFromMapFunc(podGroupRestoreToPodGroupSnapshot),
		).
		Complete(r)
}

// podGroupRestoreSnapshotUID is the podGroupRestoreSnapshotUIDField index's
// value of a PodGroupRestore.
func podGroupRestoreSnapshotUID(obj client.Object) []string {
	restore, ok := obj.(*snapshotv1alpha1.PodGroupRestore)
	if !ok {
		return nil
	}
	return []string{string(restore.Spec.SnapshotRef.UID)}
}

// podGroupRestoreToPodGroupSnapshot maps a PodGroupRestore to the
// PodGroupSnapshot it restores, so a snapshot being deleted drops its
// finalizer as soon as its last unfinished restore ends.
func podGroupRestoreToPodGroupSnapshot(_ context.Context, obj client.Object) []reconcile.Request {
	restore, ok := obj.(*snapshotv1alpha1.PodGroupRestore)
	if !ok {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: restore.Namespace, Name: restore.Spec.SnapshotRef.Name}}}
}

// podSnapshotContentToPodGroupSnapshot maps a PodSnapshotContent to the group
// that controls its PodSnapshot, if any.
func (r *PodGroupSnapshotReconciler) podSnapshotContentToPodGroupSnapshot(ctx context.Context, obj client.Object) []reconcile.Request {
	ref, err := podSnapshotRefFromContentObj(obj)
	if err != nil {
		log.FromContext(ctx).Error(err, "Failed to map PodSnapshotContent to PodGroupSnapshot")
		return nil
	}
	if ref.Name == "" {
		return nil
	}
	snap := &snapshotv1alpha1.PodSnapshot{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, snap); err != nil {
		if !apierrors.IsNotFound(err) {
			log.FromContext(ctx).Error(err, "Failed to get PodSnapshot for PodSnapshotContent", "podSnapshot", ref.Name)
		}
		return nil
	}
	owner := metav1.GetControllerOf(snap)
	if owner == nil || !isPodGroupSnapshotOwner(owner) {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: snap.Namespace, Name: owner.Name}}}
}

// isPodGroupSnapshotOwner reports whether an owner reference names a
// PodGroupSnapshot.
func isPodGroupSnapshotOwner(owner *metav1.OwnerReference) bool {
	return owner.APIVersion == snapshotv1alpha1.GroupVersion.String() && owner.Kind == snapshotv1alpha1.KindPodGroupSnapshot
}
