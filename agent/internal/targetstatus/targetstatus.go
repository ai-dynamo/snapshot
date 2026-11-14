// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package targetstatus writes the node agent's conditions on a group restore
// target, the restore Pod of one member of a PodGroupRestore.
//
// The Pod is single-use: its binding names the only attempt that may report on
// it, and the operator reads the reports from the Pod it pinned, by UID. So
// every write is fenced. It lands only on the Pod with the member's pinned
// UID, and only if the Pod's resourceVersion is still the one the agent read.
// After a conflict, Apply reads the Pod again and checks it again before it
// retries, so a Pod replaced under the same name invalidates every write still
// outstanding for the old one.
//
// A write is a server-side apply of the Pod's status subresource that sets
// metadata.uid and metadata.resourceVersion, which the API server checks. It
// needs no permission beyond patch on pods/status, and it changes only the
// conditions its field manager owns, so the kubelet's conditions are kept.
package targetstatus

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	corev1apply "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

// The field managers of the agent's conditions on a restore Pod.
const (
	// RestoreFieldManager owns nvidia.com/Restored, and on a group target
	// nvidia.com/GroupRestoreFailed: every restore condition the agent
	// writes but one.
	RestoreFieldManager = "snapshot-agent-restore"

	// StartedFieldManager owns nvidia.com/RestoreStarted alone. A field
	// manager drops what it owned and leaves out of its next apply, so no
	// later write of the agent's other conditions can remove the record.
	StartedFieldManager = "snapshot-agent-restore-started"
)

// conflictLimit bounds the reads and writes of one Apply. The kubelet writes a
// starting Pod's status often, so a write can lose a few races in a row.
const conflictLimit = 8

// ErrReplaced reports that the Pod is not the one the member is pinned to: it
// was deleted and created again under the same name.
var ErrReplaced = errors.New("the restore Pod was replaced")

// Apply writes conditions on the status of pod, as manager, fenced on uid: the
// Pod's pinned UID, or, before the operator pins the member, the UID of the Pod
// the agent read. pod is the agent's last read of the Pod. It returns the Pod
// as written.
//
// A condition keeps its lastTransitionTime while its status does not change.
// Apply refuses to write nvidia.com/RestoreStarted with any status but True,
// or under any field manager but StartedFieldManager, and leaves an existing
// RestoreStarted as it is. Under RestoreFieldManager, it keeps an existing
// nvidia.com/GroupRestoreFailed that conditions leave out, so a later report
// cannot drop the group's reason.
//
// It reports ErrReplaced when the Pod's UID is not uid, and the API server's
// NotFound once the Pod is gone.
func Apply(ctx context.Context, client kubernetes.Interface, pod *corev1.Pod, uid types.UID, manager string, conditions ...corev1.PodCondition) (*corev1.Pod, error) {
	if err := validate(manager, conditions); err != nil {
		return nil, err
	}
	current := pod
	for range conflictLimit {
		if current.UID != uid {
			return nil, fmt.Errorf("pod %s/%s has UID %s, not %s: %w", current.Namespace, current.Name, current.UID, uid, ErrReplaced)
		}
		written, err := client.CoreV1().Pods(current.Namespace).ApplyStatus(ctx, configuration(current, manager, conditions),
			metav1.ApplyOptions{FieldManager: manager, Force: true})
		if err == nil {
			return written, nil
		}
		if !apierrors.IsConflict(err) {
			return nil, err
		}
		if current, err = client.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{}); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("pod %s/%s: the status write conflicted %d times", pod.Namespace, pod.Name, conflictLimit)
}

// validate refuses a write that could withdraw the start record.
func validate(manager string, conditions []corev1.PodCondition) error {
	for _, c := range conditions {
		if c.Type != podcontract.RestoreStartedCondition {
			continue
		}
		if manager != StartedFieldManager {
			return fmt.Errorf("%s is written only by %s", podcontract.RestoreStartedCondition, StartedFieldManager)
		}
		if c.Status != corev1.ConditionTrue {
			return fmt.Errorf("%s is never set to %s", podcontract.RestoreStartedCondition, c.Status)
		}
	}
	return nil
}

// configuration is the apply of conditions on current, fenced on its UID and
// resourceVersion.
func configuration(current *corev1.Pod, manager string, conditions []corev1.PodCondition) *corev1apply.PodApplyConfiguration {
	status := corev1apply.PodStatus()
	written := make(map[corev1.PodConditionType]bool, len(conditions))
	for _, c := range conditions {
		written[c.Type] = true
		status.WithConditions(conditionApply(current, c))
	}
	if manager == RestoreFieldManager && !written[podcontract.GroupRestoreFailedCondition] {
		if kept := find(current, podcontract.GroupRestoreFailedCondition); kept != nil {
			status.WithConditions(conditionApply(current, *kept))
		}
	}
	return corev1apply.Pod(current.Name, current.Namespace).
		WithUID(current.UID).
		WithResourceVersion(current.ResourceVersion).
		WithStatus(status)
}

// conditionApply returns c as an apply, with the lastTransitionTime the Pod's
// condition of its type has when the status does not change. A RestoreStarted
// already on the Pod is written back unchanged.
func conditionApply(current *corev1.Pod, c corev1.PodCondition) *corev1apply.PodConditionApplyConfiguration {
	existing := find(current, c.Type)
	switch {
	case existing != nil && c.Type == podcontract.RestoreStartedCondition:
		c = *existing
	case existing != nil && existing.Status == c.Status && !existing.LastTransitionTime.IsZero():
		c.LastTransitionTime = existing.LastTransitionTime
	case c.LastTransitionTime.IsZero():
		c.LastTransitionTime = metav1.Now()
	}
	apply := corev1apply.PodCondition().
		WithType(c.Type).
		WithStatus(c.Status).
		WithLastTransitionTime(c.LastTransitionTime)
	if c.Reason != "" {
		apply.WithReason(c.Reason)
	}
	if c.Message != "" {
		apply.WithMessage(c.Message)
	}
	return apply
}

func find(pod *corev1.Pod, conditionType corev1.PodConditionType) *corev1.PodCondition {
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type == conditionType {
			return &pod.Status.Conditions[i]
		}
	}
	return nil
}
