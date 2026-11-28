// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ai-dynamo/snapshot/api/podcontract"
	v1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

const verificationPollInterval = 2 * time.Second

func (sr *PodSnapshotReconciler) verificationNow() time.Time {
	if sr.now != nil {
		return sr.now()
	}
	return time.Now()
}

// advanceSnapshotLifecycle runs after capture conditions have been mirrored.
// Status is persisted by the caller, including the canary identity before the
// Agent can admit it. No Pod is created by this controller.
func (sr *PodSnapshotReconciler) advanceSnapshotLifecycle(ctx context.Context, s *v1.PodSnapshot, c *v1.PodSnapshotContent) (ctrl.Result, error) {
	// Auxiliary failure is terminal without rewriting the capture conditions.
	if s.Status.Phase == v1.PodSnapshotPhaseFailed {
		return ctrl.Result{}, nil
	}
	if v1.IsPodSnapshotFailed(s) {
		s.Status.Phase = v1.PodSnapshotPhaseFailed
		return ctrl.Result{}, nil
	}
	if !v1.IsPodSnapshotContentSucceeded(c) {
		s.Status.Phase = v1.PodSnapshotPhasePending
		if c.Status.CheckpointStartedAt != nil {
			s.Status.Phase = v1.PodSnapshotPhaseCheckpointing
		}
		return ctrl.Result{}, nil
	}
	// Once auxiliary completion was observed, verification/completion no longer
	// depends on retaining the SnapshotJob or its source Job.
	switch s.Status.Phase {
	case v1.PodSnapshotPhaseRestoreVerifying, v1.PodSnapshotPhaseVerificationFailed, v1.PodSnapshotPhaseCompleted:
	case v1.PodSnapshotPhaseCheckpointSucceeded:
		complete, err := sr.snapshotAuxiliaryWorkComplete(ctx, s)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !complete {
			if s.Status.Phase == v1.PodSnapshotPhaseFailed {
				return ctrl.Result{}, nil
			}
			return ctrl.Result{RequeueAfter: verificationPollInterval}, nil
		}
	default:
		s.Status.Phase = v1.PodSnapshotPhaseCheckpointSucceeded
		return ctrl.Result{RequeueAfter: time.Millisecond}, nil
	}
	if s.Spec.RestoreVerification == nil {
		s.Status.Phase = v1.PodSnapshotPhaseCompleted
		return ctrl.Result{}, nil
	}
	if s.Status.Phase == v1.PodSnapshotPhaseVerificationFailed {
		return ctrl.Result{}, nil
	}
	return sr.verifyRestore(ctx, s, c)
}

func (sr *PodSnapshotReconciler) snapshotAuxiliaryWorkComplete(ctx context.Context, s *v1.PodSnapshot) (bool, error) {
	name := s.Labels[v1.SnapshotJobOwnerLabel]
	if name == "" {
		return true, nil
	}
	j := &v1.SnapshotJob{}
	err := sr.NonCacheReadClient.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: name}, j)
	if err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	if apierrors.IsNotFound(err) || string(j.UID) != s.Labels[v1.SnapshotJobOwnerUIDLabel] ||
		j.Status.PodSnapshotName != s.Name || j.Status.PodSnapshotUID != s.UID {
		s.Status.Phase = v1.PodSnapshotPhaseFailed
		sr.Recorder.Event(s, corev1.EventTypeWarning, "SnapshotJobChanged", "Cannot verify the producing SnapshotJob identity")
		return false, nil
	}
	if v1.IsSnapshotJobFailed(j) {
		s.Status.Phase = v1.PodSnapshotPhaseFailed
		sr.Recorder.Event(s, corev1.EventTypeWarning, "SnapshotJobFailed", "Required SnapshotJob work failed")
		return false, nil
	}
	return v1.IsSnapshotJobCompleted(j), nil
}

func (sr *PodSnapshotReconciler) verifyRestore(ctx context.Context, s *v1.PodSnapshot, c *v1.PodSnapshotContent) (ctrl.Result, error) {
	p := s.Spec.RestoreVerification
	now := sr.verificationNow()
	i := s.Status.RestoreVerifyInfo
	if i == nil {
		i = &v1.RestoreVerificationInfo{ContentUID: c.UID, PolicyHash: v1.RestoreVerificationPolicyHash(p), PodRef: p.PodRef,
			StartedAt: metav1.NewTime(now), DeadlineAt: metav1.NewTime(now.Add(time.Duration(p.TimeoutSeconds) * time.Second)),
			Outcome: v1.RestoreVerificationInProgress}
		s.Status.RestoreVerifyInfo = i
		s.Status.Phase = v1.PodSnapshotPhaseRestoreVerifying
		if p.PodRef.Name == "" || p.PodRef.UID == "" || p.TimeoutSeconds <= p.ObservationWindowSeconds || p.ObservationWindowSeconds < 0 {
			return sr.failVerification(s, "VerificationPolicyInvalid", "Verification requires a pinned Pod UID and a timeout longer than the observation window"), nil
		}
		// Persist the deadline before any API read or restore admission.
		return ctrl.Result{RequeueAfter: verificationPollInterval}, nil
	}
	if c.UID == "" || i.ContentUID != c.UID || i.PolicyHash != v1.RestoreVerificationPolicyHash(p) || i.PodRef != p.PodRef {
		return sr.failVerification(s, "VerificationIdentityChanged", "The content or verification policy no longer matches the recorded attempt"), nil
	}
	if s.Status.Phase == v1.PodSnapshotPhaseCompleted {
		if !v1.IsRestoreVerified(s, c.UID) {
			return sr.failVerification(s, "VerificationRecordInvalid", "Completed has no matching successful verification record"), nil
		}
		return ctrl.Result{}, nil
	}
	if !now.Before(i.DeadlineAt.Time) {
		return sr.failVerification(s, "VerificationTimeout", "Restore verification exceeded its total timeout"), nil
	}
	pod := &corev1.Pod{}
	if err := sr.NonCacheReadClient.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: p.PodRef.Name}, pod); err != nil {
		if apierrors.IsNotFound(err) {
			return sr.failVerification(s, "VerificationPodDeleted", "The verification Pod was deleted"), nil
		}
		return ctrl.Result{}, err
	}
	if pod.UID != p.PodRef.UID {
		return sr.failVerification(s, "VerificationPodChanged", "The verification Pod was recreated"), nil
	}
	if pod.DeletionTimestamp != nil || pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
		return sr.failVerification(s, "VerificationPodExited", "The verification Pod exited or is deleting"), nil
	}
	if pod.Annotations[podcontract.RestoreFromAnnotation] != s.Name {
		return sr.failVerification(s, "VerificationSnapshotChanged", "The verification Pod no longer references this snapshot"), nil
	}
	containers, ready, reason, message := verificationContainers(s, pod)
	if reason != "" {
		return sr.failVerification(s, reason, message), nil
	}
	outcome := podcontract.ClassifyRestoreOutcome(pod.Status.Conditions)
	if outcome == podcontract.RestoreOutcomeFailed || outcome == podcontract.RestoreOutcomePartiallySucceeded {
		return sr.failVerification(s, "VerificationRestoreFailed", "The verification Pod reported an unsuccessful restore"), nil
	}
	if len(i.Containers) == 0 {
		if outcome == podcontract.RestoreOutcomeSucceeded {
			return sr.failVerification(s, "VerificationAlreadyRestored", "The Pod restored before its verification execution was recorded"), nil
		}
		if containers != nil {
			i.Containers = containers
		}
		return verificationWait(i, now), nil
	}
	if containers == nil || !reflect.DeepEqual(i.Containers, containers) {
		return sr.failVerification(s, "VerificationContainerChanged", "A verification container restarted or was replaced"), nil
	}
	if outcome != podcontract.RestoreOutcomeSucceeded || !ready {
		i.HealthySince = nil
		return verificationWait(i, now), nil
	}
	// A Ready transition after HealthySince also resets the window when the
	// controller was unavailable during a readiness loss and recovery.
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && i.HealthySince != nil && condition.LastTransitionTime.After(i.HealthySince.Time) {
			i.HealthySince = nil
		}
	}
	if i.HealthySince == nil {
		t := metav1.NewTime(now)
		i.HealthySince = &t
	}
	if now.Sub(i.HealthySince.Time) < time.Duration(p.ObservationWindowSeconds)*time.Second {
		return verificationWait(i, now), nil
	}
	t := metav1.NewTime(now)
	i.CompletedAt, i.Outcome, i.Reason = &t, v1.RestoreVerificationSucceeded, "VerificationSucceeded"
	i.Message = "Restore and application readiness passed the observation window"
	s.Status.Phase = v1.PodSnapshotPhaseCompleted
	return ctrl.Result{}, nil
}

func verificationWait(i *v1.RestoreVerificationInfo, now time.Time) ctrl.Result {
	return ctrl.Result{RequeueAfter: min(verificationPollInterval, i.DeadlineAt.Sub(now))}
}

func (sr *PodSnapshotReconciler) failVerification(s *v1.PodSnapshot, reason, message string) ctrl.Result {
	i := s.Status.RestoreVerifyInfo
	t := metav1.NewTime(sr.verificationNow())
	i.CompletedAt, i.Outcome, i.Reason, i.Message = &t, v1.RestoreVerificationFailed, reason, message
	s.Status.Phase = v1.PodSnapshotPhaseVerificationFailed
	sr.Recorder.Event(s, corev1.EventTypeWarning, reason, message)
	return ctrl.Result{}
}

func verificationContainers(s *v1.PodSnapshot, p *corev1.Pod) ([]v1.RestoreVerificationContainer, bool, string, string) {
	if len(s.Spec.Source.PodRef.Containers) != 1 {
		return nil, false, "VerificationTargetInvalid", "Expected one checkpointed container"
	}
	mappings, err := podcontract.ContainerMappingsFromAnnotations(p.Annotations, s.Spec.Source.PodRef.Containers[0])
	if err != nil {
		return nil, false, "VerificationTargetInvalid", err.Error()
	}
	var executions []v1.RestoreVerificationContainer
	ready := false
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			ready = true
		}
	}
	for _, m := range mappings {
		probe := false
		for _, c := range p.Spec.Containers {
			if c.Name == m.Destination && c.ReadinessProbe != nil {
				h := c.ReadinessProbe.ProbeHandler
				probe = h.HTTPGet != nil || h.Exec != nil || h.TCPSocket != nil || h.GRPC != nil
			}
		}
		if !probe {
			return nil, false, "VerificationProbeMissing", fmt.Sprintf("Destination %q requires an application readiness probe", m.Destination)
		}
		var status *corev1.ContainerStatus
		for idx := range p.Status.ContainerStatuses {
			if p.Status.ContainerStatuses[idx].Name == m.Destination {
				status = &p.Status.ContainerStatuses[idx]
				break
			}
		}
		if status != nil && status.State.Terminated != nil {
			return nil, false, "VerificationContainerExited", "A verification container exited"
		}
		if status == nil || status.ContainerID == "" || status.State.Running == nil {
			return nil, false, "", ""
		}
		executions = append(executions, v1.RestoreVerificationContainer{Name: m.Destination, ContainerID: status.ContainerID, RestartCount: status.RestartCount})
		ready = ready && status.Ready
	}
	return executions, ready, "", ""
}
