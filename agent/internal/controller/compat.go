// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"runtime"

	corev1 "k8s.io/api/core/v1"

	"github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/ai-dynamo/snapshot/api/compat"
	"github.com/ai-dynamo/snapshot/api/podcontract"
)

// refuseRestore records a restore this node will not attempt. It is terminal
// like any other restore failure and reports through the same condition, with
// its own reason so an operator can tell a checkpoint that cannot run here from
// one that tried and broke.
func (w *NodeController) refuseRestore(ctx context.Context, pod *corev1.Pod, incompatible *compat.IncompatibleError) bool {
	reason := compat.Reasons(incompatible.Mismatches)
	w.logRestoreRefusal(pod, incompatible, reason)
	return w.finishRestore(
		ctx,
		pod,
		corev1.ConditionFalse,
		podcontract.RestoreReasonIncompatible,
		refusalMessage(reason),
	) != nil
}

// refusalMessage puts a sentence around the mismatches, which on their own read
// as a bare "<check>: source <x>, target <y>". Both gates report through it so
// the condition and the event say the same thing wherever the refusal came
// from. The log keeps the mismatches unwrapped, since that is the field an
// operator greps.
func refusalMessage(reasons string) string {
	return "Refused restore; this node cannot run the checkpoint: " + reasons
}

func (w *NodeController) logRestoreRefusal(pod *corev1.Pod, incompatible *compat.IncompatibleError, reason string) {
	w.log.Info("Refusing restore; this node cannot run the checkpoint",
		"pod", fmt.Sprintf("%s/%s", pod.Namespace, pod.Name),
		"gate", string(incompatible.Gate),
		"reason", reason,
	)
}

// restoreRefused reports a pod whose restore the gates turned down, as opposed
// to one that was attempted and failed.
func restoreRefused(pod *corev1.Pod) bool {
	condition := findRestoredCondition(pod)
	return condition != nil &&
		condition.Status == corev1.ConditionFalse &&
		condition.Reason == podcontract.RestoreReasonIncompatible
}

// skipRequestedAfterRefusal reports a pod that the gates turned down and that
// has since asked for them to be skipped. Nothing else reopens a terminal
// restore, which is what makes the skip request an escape hatch and not a retry.
func (w *NodeController) skipRequestedAfterRefusal(pod *corev1.Pod) bool {
	return restoreRefused(pod) && w.skipCompatCheckRequested(pod)
}

// podEnvironment reads what one container of a pod runs as and is allowed. It serves
// both sides of a comparison: what a capture records about the source pod, and
// what a restore target offers.
//
// A container that is not in the pod leaves its env unknown.
func podEnvironment(pod *corev1.Pod, containerName string) compat.Environment {
	env := compat.Environment{}
	for _, container := range pod.Spec.Containers {
		if container.Name != containerName {
			continue
		}
		env.Image = container.Image
		env.CPULimit = limitString(container.Resources.Limits, corev1.ResourceCPU)
		env.MemoryLimit = limitString(container.Resources.Limits, corev1.ResourceMemory)
	}
	return env
}

// limitString keeps an unset limit unset. A missing quantity formats as "0",
// which would otherwise read as a container limited to nothing.
func limitString(limits corev1.ResourceList, name corev1.ResourceName) string {
	quantity, ok := limits[name]
	if !ok {
		return ""
	}
	return quantity.String()
}

// skipCompatCheckRequested reports whether this restore was asked to skip
// the compatibility gates, by the pod that is being restored or by the node
// it landed on.
func (w *NodeController) skipCompatCheckRequested(pod *corev1.Pod) bool {
	return w.skipCompatCheckFn() ||
		podcontract.SkipCompatCheckFromAnnotations(pod.Annotations)
}

// preflightCompatibility runs the pre-flight compatibility gate for one restore.
// A nil error means the restore may be attempted.
func (w *NodeController) preflightCompatibility(
	ctx context.Context,
	pod *corev1.Pod,
	artifact *restoreArtifact,
	mappings []podcontract.ContainerMapping,
	skipCompatCheck bool,
) error {
	log := w.log.WithValues("pod", fmt.Sprintf("%s/%s", pod.Namespace, pod.Name), "container", artifact.SourceContainerName)
	if skipCompatCheck {
		log.Info("Restore compatibility check skipped by request", "gate", string(compat.GatePreflight))
		return nil
	}

	manifest, err := types.ReadManifest(artifact.Path)
	if err != nil {
		// An unreadable manifest is not an incompatibility. The restore path
		// reads it again and reports the real error from there, so refusing here
		// would relabel a broken artifact as an incompatible one.
		log.Info("Skipping restore compatibility gate; checkpoint manifest is unreadable",
			"artifact_path", artifact.Path,
			"error", err.Error(),
		)
		emitPodEvent(ctx, w.clientset, log, pod, snapshotEventComponent, corev1.EventTypeWarning,
			restoreCompatUncheckedReason,
			fmt.Sprintf("Restore compatibility not checked for container %s: checkpoint manifest at %s is unreadable: %v",
				artifact.SourceContainerName, artifact.Path, err),
		)
		return nil
	}

	sourceEnv := manifest.CompatEnvironment()
	for _, mapping := range mappings {
		mismatches := w.compareFn(
			compat.GatePreflight,
			sourceEnv,
			w.preflightTargetEnvironment(pod, mapping.Destination),
		)
		if len(mismatches) != 0 {
			return compat.NewIncompatibleError(compat.GatePreflight, mismatches)
		}
	}
	return nil
}

// preflightTargetEnvironment describes what this node and this pod offer a restore, as
// far as it is knowable before the placeholder container exists. It is assembled
// per restore from env the agent already holds, so the gate costs no syscalls
// and no API reads.
func (w *NodeController) preflightTargetEnvironment(pod *corev1.Pod, containerName string) compat.Environment {
	env := podEnvironment(pod, containerName)
	// The agent's own architecture, which is the node's: this binary could not
	// be running here otherwise.
	env.CPUArch = runtime.GOARCH
	env.KernelVersion = w.config.HostKernelVersion
	return env
}
