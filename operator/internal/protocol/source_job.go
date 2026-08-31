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

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
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
		snapshotv1alpha1.RestoreFromAnnotation,
		snapshotv1alpha1.RestoreContainerMapAnnotation,
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
				Command: []string{"cat", filepath.Join(snapshotv1alpha1.SnapshotControlMountPath, snapshotv1alpha1.ReadyForSnapshotFile)},
			},
		},
		PeriodSeconds: 1,
	}
	targetContainer.LivenessProbe = nil
	targetContainer.StartupProbe = nil

	if opts.WrapLaunchJob {
		if len(targetContainer.Command) == 0 {
			return nil, fmt.Errorf("source job requires container.command when cuda-checkpoint launch-job wrapping is enabled")
		}
		targetContainer.Command, targetContainer.Args = wrapWithCudaCheckpointLaunchJob(
			targetContainer.Command,
			targetContainer.Args,
		)
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

// wrapWithCudaCheckpointLaunchJob rewrites the container's entrypoint so the
// workload is launched under `cuda-checkpoint --launch-job`, required for
// multi-GPU checkpoints. The launch-job file is copied from its transient
// procfs FD into the per-pod snapshot control volume before the original
// command starts. The workload inherits that stable path, while the snapshot
// agent stages the capture-time contents into the content-owned artifact.
func wrapWithCudaCheckpointLaunchJob(command []string, args []string) ([]string, []string) {
	const persistJobFileScript = `set -eu
job_file="$1"
shift
if [ -z "${CUDA_CHECKPOINT_JOB_FILE:-}" ]; then
    echo "CUDA_CHECKPOINT_JOB_FILE is missing; cuda-checkpoint --launch-job requires NVIDIA driver 610 or newer" >&2
    exit 1
fi
umask 077
cat "$CUDA_CHECKPOINT_JOB_FILE" > "$job_file"
export CUDA_CHECKPOINT_JOB_FILE="$job_file"
exec "$@"`

	wrappedArgs := make([]string, 0, len(command)+len(args)+7)
	wrappedArgs = append(wrappedArgs, "--launch-job", "/bin/sh", "-c", persistJobFileScript, "dynamo-cuda-checkpoint", snapshotv1alpha1.CUDAJobFilePath)
	wrappedArgs = append(wrappedArgs, command...)
	wrappedArgs = append(wrappedArgs, args...)
	return []string{"cuda-checkpoint"}, wrappedArgs
}
