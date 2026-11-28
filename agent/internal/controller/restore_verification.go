// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ai-dynamo/snapshot/api/podcontract"
	v1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

type restoreVerificationError struct{ message string }

func (e *restoreVerificationError) Error() string { return e.message }

func verificationError(format string, args ...any) error {
	return &restoreVerificationError{message: fmt.Sprintf(format, args...)}
}

// validateRestoreVerification runs before artifact access, compatibility checks,
// or runtime work. The same gate applies to every consumer on every node.
func validateRestoreVerification(s *v1.PodSnapshot, c *v1.PodSnapshotContent, pod *corev1.Pod, now time.Time) error {
	p := s.Spec.RestoreVerification
	if p == nil {
		return nil
	}
	i := s.Status.RestoreVerifyInfo
	if s.Status.Phase == v1.PodSnapshotPhaseVerificationFailed || s.Status.Phase == v1.PodSnapshotPhaseFailed {
		message := "snapshot verification failed"
		if i != nil {
			message += ": " + i.Reason + ": " + i.Message
		}
		return verificationError("PodSnapshot %s/%s: %s", s.Namespace, s.Name, message)
	}
	if v1.IsRestoreVerified(s, c.UID) {
		return nil
	}
	if i == nil {
		return newRestorePendingError("VerificationPending", "Waiting for the Operator to record restore verification")
	}
	if c.UID == "" || i.ContentUID != c.UID || i.PolicyHash != v1.RestoreVerificationPolicyHash(p) || i.PodRef != p.PodRef {
		return verificationError("PodSnapshot %s/%s verification record does not match the artifact or policy", s.Namespace, s.Name)
	}
	if s.Status.Phase == v1.PodSnapshotPhaseCompleted || i.Outcome != v1.RestoreVerificationInProgress {
		return verificationError("PodSnapshot %s/%s has no matching successful verification", s.Namespace, s.Name)
	}
	// Once the Operator has started verification, a stalled controller cannot
	// strand proxies past that persisted deadline. Preflight failure handling
	// writes restore-failed and the Pod condition; it never cold-starts the Pod.
	if !now.Before(i.DeadlineAt.Time) {
		return verificationError("PodSnapshot %s/%s restore verification timed out", s.Namespace, s.Name)
	}
	if s.Status.Phase != v1.PodSnapshotPhaseRestoreVerifying ||
		pod.Namespace != s.Namespace || pod.Name != i.PodRef.Name || pod.UID != i.PodRef.UID || len(i.Containers) == 0 {
		return newRestorePendingError("VerificationPending", "Waiting for the snapshot canary to pass restore verification")
	}
	if len(s.Spec.Source.PodRef.Containers) != 1 {
		return verificationError("verification requires one checkpointed container")
	}
	mappings, err := podcontract.ContainerMappingsFromAnnotations(pod.Annotations, s.Spec.Source.PodRef.Containers[0])
	if err != nil || len(mappings) != len(i.Containers) {
		return verificationError("verification destination mapping changed")
	}
	for _, expected := range i.Containers {
		mapped := false
		for _, m := range mappings {
			if m.Destination == expected.Name {
				mapped = true
			}
		}
		if !mapped {
			return verificationError("verification destination mapping changed")
		}
		matched := false
		for _, actual := range pod.Status.ContainerStatuses {
			if actual.Name == expected.Name && actual.ContainerID == expected.ContainerID && actual.RestartCount == expected.RestartCount && actual.State.Running != nil {
				matched = true
				break
			}
		}
		if !matched {
			return verificationError("verification container %q was replaced or stopped", expected.Name)
		}
	}
	return nil
}

// Notify the proxy before making the Pod terminal. If kubelet has not mounted
// its control volume yet, retry notification rather than forgetting a waiting
// process. No runtime restore or cold-start action is performed here.
func (w *NodeController) failRestoreVerification(ctx context.Context, pod *corev1.Pod, cause error) bool {
	destinations, err := w.RestoreDestinations(ctx, pod)
	if err != nil {
		var pending *restorePendingError
		if errors.As(err, &pending) {
			return true
		}
		return w.failRestorePod(ctx, pod, fmt.Errorf("%w: invalid restore destinations: %v", cause, err))
	}
	for _, destination := range destinations {
		var markErr error
		id := containerIDForName(pod, destination)
		if id != "" {
			pid, _, resolveErr := w.runtime.ResolveContainer(ctx, id)
			if resolveErr == nil {
				markErr = w.writeControlSentinelFn(pid, podcontract.RestoreFailedFile, restoreFailedSentinelContents)
			} else {
				uid := string(pod.UID)
				if hostUID, uidErr := w.runtime.ResolveContainerPodUID(ctx, id); uidErr == nil {
					uid = hostUID
				}
				markErr = w.writePodControlSentinelFn(uid, destination, podcontract.RestoreFailedFile, restoreFailedSentinelContents)
			}
		} else {
			markErr = w.writePodControlSentinelFn(string(pod.UID), destination, podcontract.RestoreFailedFile, restoreFailedSentinelContents)
		}
		if markErr != nil {
			w.log.Error(markErr, "Failed to notify restore proxy of verification failure", "pod", pod.Name, "container", destination)
			return true
		}
	}
	return w.failRestorePod(ctx, pod, cause)
}
