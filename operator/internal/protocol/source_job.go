// SPDX-FileCopyrightText: Copyright (c) 2025-2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package protocol

import (
	"fmt"
	"path/filepath"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

type SourceJobOptions struct {
	Namespace             string
	TargetContainer       string
	SeccompProfile        string
	Name                  string
	ActiveDeadlineSeconds *int64
	TTLSecondsAfterFinish *int32
	WrapLaunchJob         bool
}

func NewSourceJob(podTemplate *corev1.PodTemplateSpec, opts SourceJobOptions) (*batchv1.Job, error) {
	podTemplate = podTemplate.DeepCopy()
	for _, annotation := range []string{
		podcontract.RestoreFromAnnotation,
		podcontract.RestoreContainerMapAnnotation,
	} {
		if _, restoreRequested := podTemplate.Annotations[annotation]; restoreRequested {
			return nil, fmt.Errorf("source job pod template must not set %s", annotation)
		}
	}
	if podTemplate.Labels == nil {
		podTemplate.Labels = map[string]string{}
	}
	if podTemplate.Annotations == nil {
		podTemplate.Annotations = map[string]string{}
	}
	podTemplate.Annotations = DisableSidecarInjection(podTemplate.Annotations)
	podTemplate.Spec.RestartPolicy = corev1.RestartPolicyNever
	if opts.SeccompProfile != "" {
		EnsureLocalhostSeccompProfile(&podTemplate.Spec, opts.SeccompProfile)
	}
	if len(podTemplate.Spec.Containers) == 0 {
		return nil, fmt.Errorf("source job requires at least one container")
	}

	// Snapshot contract: exactly one target container per Job. The caller (the operator,
	// snapshotctl) resolves the single target and passes it in opts so there is no
	// Containers[0]-vs-"main" ambiguity.
	targetName := opts.TargetContainer
	if targetName == "" {
		return nil, fmt.Errorf("source job pod template: opts.TargetContainer is required")
	}
	var targetContainer *corev1.Container
	for i := range podTemplate.Spec.Containers {
		if podTemplate.Spec.Containers[i].Name == targetName {
			targetContainer = &podTemplate.Spec.Containers[i]
			break
		}
	}
	if targetContainer == nil {
		return nil, fmt.Errorf("source job pod template has no container named %q (from opts.TargetContainer)", targetName)
	}

	// Snapshot contract: control volume + ready-file readiness probe. The
	// agent reads the pod's Ready condition before starting CRIU dump, so
	// the workload signals "model loaded, safe to checkpoint" by writing
	// $SNAPSHOT_CONTROL_DIR/ready-for-snapshot. Any per-container
	// liveness/startup probes are cleared — a source job runs to a
	// quiesce-and-sit state, not a long-lived serving state.
	EnsureControlVolume(&podTemplate.Spec, targetContainer)
	targetContainer.ReadinessProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{
				Command: []string{"cat", filepath.Join(podcontract.SnapshotControlMountPath, podcontract.ReadyForSnapshotFile)},
			},
		},
		PeriodSeconds: 1,
	}
	targetContainer.LivenessProbe = nil
	targetContainer.StartupProbe = nil

	if opts.WrapLaunchJob {
		if err := podcontract.EnsureCUDACheckpointLaunchJob(targetContainer, "cuda-checkpoint"); err != nil {
			return nil, fmt.Errorf("source job: %w", err)
		}
	}

	return &batchv1.Job{
		TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      opts.Name,
			Namespace: opts.Namespace,
		},
		Spec: batchv1.JobSpec{
			ActiveDeadlineSeconds:   opts.ActiveDeadlineSeconds,
			BackoffLimit:            ptr.To[int32](0),
			TTLSecondsAfterFinished: opts.TTLSecondsAfterFinish,
			Template:                *podTemplate,
		},
	}, nil
}

// EnsureLocalhostSeccompProfile sets the pod-level localhost seccomp profile
// to the given path, allocating PodSecurityContext if needed. An empty profile
// is a no-op so callers can disable injection entirely without conditional
// branching at the call site (e.g. on OpenShift, where custom localhost
// profiles require privileged SCC, or with a CRIU build that allows io_uring).
func EnsureLocalhostSeccompProfile(podSpec *corev1.PodSpec, profile string) {
	if profile == "" {
		return // no seccomp restriction requested (e.g. OCP or io_uring-capable CRIU)
	}
	if podSpec.SecurityContext == nil {
		podSpec.SecurityContext = &corev1.PodSecurityContext{}
	}
	podSpec.SecurityContext.SeccompProfile = &corev1.SeccompProfile{
		Type:             corev1.SeccompProfileTypeLocalhost,
		LocalhostProfile: &profile,
	}
}

// DisableSidecarInjection stamps sidecar opt-out annotations on a
// pod annotation map. Source Jobs must complete when the target container
// exits; an injected sidecar that outlives the checkpoint keeps the pod alive,
// preventing Kubernetes from marking the Job complete.
//
// Mutates and returns the passed-in map. Allocates a new map when annotations
// is nil; callers must use the returned value.
func DisableSidecarInjection(annotations map[string]string) map[string]string {
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[linkerdInjectAnnotation] = linkerdInjectDisabled
	annotations[istioSidecarInjectAnnotation] = istioSidecarInjectDisabled
	return annotations
}
