// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// The parts of reconciling that both kinds of pod group share: the in-use
// finalizer, the status patch and its events, the requeue rule, and the node
// agent gate of admission. Each reconciler keeps its own short Reconcile
// sequence: observe, derive, record, act.

const (
	// podGroupProtocolVersion is the group protocol version this operator
	// speaks. Admission requires the agent on every member's node to list it.
	podGroupProtocolVersion = "v1"

	// podGroupAdmissionRequeue is how often admission looks again while it
	// waits for something that no watch reports: an agent, or a source Pod to
	// be scheduled. It polls instead of watching them: mapping a Pod event back
	// to the groups that wait on its node would need an index of waiting
	// groups by node, and the deadline bounds the wait anyway.
	podGroupAdmissionRequeue = 10 * time.Second
)

// addPodGroupFinalizer adds the in-use finalizer to a group when absent,
// reporting added=true when it updated the group; the resulting watch event
// reconciles it again.
func addPodGroupFinalizer(ctx context.Context, c client.Client, group client.Object) (bool, error) {
	if controllerutil.ContainsFinalizer(group, snapshotv1alpha1.PodGroupInUseFinalizer) {
		return false, nil
	}
	controllerutil.AddFinalizer(group, snapshotv1alpha1.PodGroupInUseFinalizer)
	if err := c.Update(ctx, group); err != nil {
		return false, fmt.Errorf("add finalizer %s: %w", snapshotv1alpha1.PodGroupInUseFinalizer, err)
	}
	return true, nil
}

// removePodGroupFinalizer removes the in-use finalizer from a group being
// deleted, once its kind's rules release it.
func removePodGroupFinalizer(ctx context.Context, c client.Client, group client.Object) error {
	controllerutil.RemoveFinalizer(group, snapshotv1alpha1.PodGroupInUseFinalizer)
	if err := c.Update(ctx, group); err != nil {
		return fmt.Errorf("remove finalizer %s: %w", snapshotv1alpha1.PodGroupInUseFinalizer, err)
	}
	return nil
}

// patchPodGroupStatus patches the status of group, which holds its new status,
// from original, then records an event for each transition from the
// conditions previous to next. The patch carries the resourceVersion it was
// derived from, so a status derived from a stale cache is refused instead of
// rewriting a Failed reason recorded since; the conflict requeues. On success
// group holds the patched object.
func patchPodGroupStatus(
	ctx context.Context,
	c client.Client,
	recorder record.EventRecorder,
	original, group client.Object,
	previous, next []metav1.Condition,
) error {
	if err := c.Status().Patch(ctx, group, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("patch the status of %q: %w", group.GetName(), err)
	}
	for _, e := range podGroupTransitions(previous, next) {
		recorder.Event(group, e.eventType, e.reason, e.message)
	}
	return nil
}

// podGroupRequeue is when to look at a group again without an event, given its
// members' views from the status just recorded. A group whose members are all
// final has ended, Ready or Failed, and one past its deadline needs nothing
// more. Otherwise it polls while admission waits for something that no watch
// reports, and looks again at the deadline, since the deadline is no event
// either.
func podGroupRequeue(admission podGroupAdmission, deadline, now time.Time, members []podGroupMemberView) time.Duration {
	untilDeadline := deadline.Sub(now)
	unfinished := false
	for _, m := range members {
		unfinished = unfinished || (!m.done && !m.failed)
	}
	if !unfinished || untilDeadline <= 0 {
		return 0
	}
	if admission.state == podGroupAdmissionWaiting && !isWatchedPodGroupAdmissionWait(admission.reason) {
		return min(podGroupAdmissionRequeue, untilDeadline)
	}
	return untilDeadline
}

// isWatchedPodGroupAdmissionWait reports whether a watch reports the end of an
// admission wait, so admission need not poll for it:
//
//   - PreviousGroupCleanup: the PodGroupSnapshot controller owns its members'
//     PodSnapshots, and the owner handler maps an owned object's events by its
//     owner's kind and name, not its UID, so the deletion of an earlier
//     group's PodSnapshot reconciles the group of the same name;
//   - TargetPodPending and TargetPodUnscheduled: the PodGroupRestore
//     controller maps the events of every Pod to the restores whose targets
//     name it, through an index on spec.targets[].podRef.name.
//
// Admission polls for every other wait: AgentNotReady and SourcePodUnscheduled.
func isWatchedPodGroupAdmissionWait(reason string) bool {
	return reason == snapshotv1alpha1.PodGroupReasonPreviousGroupCleanup ||
		reason == snapshotv1alpha1.PodGroupReasonTargetPodPending ||
		reason == snapshotv1alpha1.PodGroupReasonTargetPodUnscheduled
}

// validatePodGroupAgents refuses a reconciler set up without the node agent
// Pods that admission checks.
func validatePodGroupAgents(namespace string, selector labels.Selector) error {
	if namespace == "" || selector == nil || selector.Empty() {
		return errors.New("pod group reconcilers require the agent namespace and a non-empty agent selector")
	}
	return nil
}

// waitForPodGroupAgents returns admission's AgentNotReady wait, naming in node
// order each node whose node agent is not ready for groups and why, or nil
// once every agent is. The agent Pods are those in namespace that selector
// matches. An agent is ready when its Pod is Ready and its
// PodGroupProtocolsAnnotation lists podGroupProtocolVersion. Every agent Pod on
// a node must be ready, so an agent being replaced by an older one never
// passes.
func waitForPodGroupAgents(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	selector labels.Selector,
	nodes map[string]struct{},
) (*podGroupAdmission, error) {
	agents := &corev1.PodList{}
	if err := reader.List(ctx, agents,
		client.InNamespace(namespace),
		client.MatchingLabelsSelector{Selector: selector},
	); err != nil {
		return nil, fmt.Errorf("list node agent Pods: %w", err)
	}
	agentsByNode := make(map[string][]*corev1.Pod, len(agents.Items))
	for i := range agents.Items {
		agent := &agents.Items[i]
		agentsByNode[agent.Spec.NodeName] = append(agentsByNode[agent.Spec.NodeName], agent)
	}

	var blocked []string
	for _, node := range slices.Sorted(maps.Keys(nodes)) {
		if why := agentNotReadyForGroups(agentsByNode[node]); why != "" {
			blocked = append(blocked, fmt.Sprintf("node %q: %s", node, why))
		}
	}
	if len(blocked) == 0 {
		return nil, nil
	}
	return &podGroupAdmission{podGroupAdmissionWaiting, snapshotv1alpha1.PodGroupReasonAgentNotReady,
		fmt.Sprintf("waiting for the node agent on %d node(s): %s", len(blocked), strings.Join(blocked, "; "))}, nil
}

// agentNotReadyForGroups says why the agent Pods of one node cannot serve a
// group member, or returns "".
func agentNotReadyForGroups(agents []*corev1.Pod) string {
	if len(agents) == 0 {
		return "no node agent Pod runs on it"
	}
	for _, agent := range agents {
		protocols, advertised := agent.Annotations[snapshotv1alpha1.PodGroupProtocolsAnnotation]
		switch {
		case !isPodReady(agent):
			return fmt.Sprintf("agent Pod %q is not Ready", agent.Name)
		case !advertised:
			return fmt.Sprintf("agent Pod %q does not support pod groups", agent.Name)
		case !slices.Contains(splitProtocols(protocols), podGroupProtocolVersion):
			return fmt.Sprintf("agent Pod %q supports group protocols %q, not %s", agent.Name, protocols, podGroupProtocolVersion)
		}
	}
	return ""
}

// splitProtocols splits a PodGroupProtocolsAnnotation value, such as "v1,v2".
func splitProtocols(value string) []string {
	protocols := strings.Split(value, ",")
	for i := range protocols {
		protocols[i] = strings.TrimSpace(protocols[i])
	}
	return protocols
}

// The field indexes on PodGroupRestores, which the PodGroupRestore controller
// reads to map the events of the objects it watches to restores.
const (
	// podGroupRestoreTargetPodField indexes PodGroupRestores by the names of
	// their target Pods.
	podGroupRestoreTargetPodField = "spec.targets.podRef.name"
	// podGroupRestoreSnapshotUIDField indexes PodGroupRestores by the UID of
	// the PodGroupSnapshot they restore. The API server cannot select
	// PodGroupRestores by it, since the CRD does not declare it a selectable
	// field, so only the cache answers it.
	podGroupRestoreSnapshotUIDField = "spec.snapshotRef.uid"
)

// IndexPodGroupRestores registers the field indexes on PodGroupRestores that
// the pod-group controllers read: podGroupRestoreTargetPodField and
// podGroupRestoreSnapshotUIDField. A field can be indexed only once, so main
// calls it once, before it sets up either controller.
func IndexPodGroupRestores(ctx context.Context, indexer client.FieldIndexer) error {
	if err := indexer.IndexField(ctx, &snapshotv1alpha1.PodGroupRestore{},
		podGroupRestoreTargetPodField, podGroupRestoreTargetPods); err != nil {
		return fmt.Errorf("index PodGroupRestores by target Pod: %w", err)
	}
	if err := indexer.IndexField(ctx, &snapshotv1alpha1.PodGroupRestore{},
		podGroupRestoreSnapshotUIDField, podGroupRestoreSnapshotUID); err != nil {
		return fmt.Errorf("index PodGroupRestores by PodGroupSnapshot UID: %w", err)
	}
	return nil
}

// podGroupRestoreTargetPods is the podGroupRestoreTargetPodField index's
// values of a PodGroupRestore: the name of every target Pod.
func podGroupRestoreTargetPods(obj client.Object) []string {
	restore, ok := obj.(*snapshotv1alpha1.PodGroupRestore)
	if !ok {
		return nil
	}
	names := make([]string, 0, len(restore.Spec.Targets))
	for _, target := range restore.Spec.Targets {
		names = append(names, target.PodRef.Name)
	}
	return names
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
