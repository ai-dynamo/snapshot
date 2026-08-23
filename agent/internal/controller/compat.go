// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"runtime"

	corev1 "k8s.io/api/core/v1"

	"github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/ai-dynamo/snapshot/api/compat"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// refuseRestore records one restore this node will not attempt. Both gates
// report through here, so a refusal reads the same whichever one turned it down,
// and nothing is requeued: retrying on this node cannot change the answer.
func (w *NodeController) refuseRestore(pod *corev1.Pod, incompatible *compat.IncompatibleError) bool {
	w.log.Info("Refusing restore; this node cannot run the checkpoint",
		"pod", fmt.Sprintf("%s/%s", pod.Namespace, pod.Name),
		"gate", string(incompatible.Gate),
		"reason", compat.Reasons(incompatible.Mismatches),
	)
	return false
}

// podFacts reads what one container of a pod runs as and is allowed. It serves
// both sides of a comparison: what a capture records about the source pod, and
// what a restore target offers.
//
// A container that is not in the pod, or a status the kubelet has not published
// yet, leaves the facts it would have supplied unknown.
func podFacts(pod *corev1.Pod, containerName string) compat.Facts {
	if pod == nil {
		return compat.Facts{}
	}

	facts := compat.Facts{}
	for _, container := range pod.Spec.Containers {
		if container.Name != containerName {
			continue
		}
		facts.Image = container.Image
		facts.CPULimit = limitString(container.Resources.Limits, corev1.ResourceCPU)
		facts.MemoryLimit = limitString(container.Resources.Limits, corev1.ResourceMemory)
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == containerName {
			facts.ImageID = status.ImageID
		}
	}
	return facts
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
		snapshotv1alpha1.SkipCompatCheckFromAnnotations(pod.Annotations)
}

// preflightCompatibility runs the pre-flight compatibility gate for one restore.
// A nil error means the restore may be attempted.
func (w *NodeController) preflightCompatibility(pod *corev1.Pod, artifact *restoreArtifact) error {
	log := w.log.WithValues("pod", fmt.Sprintf("%s/%s", pod.Namespace, pod.Name), "container", artifact.ContainerName)
	if artifact.SkipCompatCheck {
		log.Info("Restore compatibility check skipped by request", "gate", string(compat.GatePreflight))
		return nil
	}

	manifest, err := types.ReadManifest(artifact.Path)
	if err != nil {
		// An unreadable manifest is not an incompatibility. The restore path
		// reads it again and reports the real error from there, so refusing here
		// would relabel a broken artifact as an incompatible one.
		log.V(1).Info("Skipping restore compatibility gate; checkpoint manifest is unreadable",
			"artifact_path", artifact.Path,
			"error", err.Error(),
		)
		return nil
	}

	mismatches := w.compareFn(
		compat.GatePreflight,
		manifest.CompatFacts(),
		w.preflightTargetFacts(pod, artifact.ContainerName),
	)
	if len(mismatches) == 0 {
		return nil
	}
	return compat.NewIncompatibleError(compat.GatePreflight, mismatches)
}

// preflightTargetFacts describes what this node and this pod offer a restore, as
// far as it is knowable before the placeholder container exists. It is assembled
// per restore from facts the agent already holds, so the gate costs no syscalls
// and no API reads.
func (w *NodeController) preflightTargetFacts(pod *corev1.Pod, containerName string) compat.Facts {
	facts := podFacts(pod, containerName)
	// The agent's own architecture, which is the node's: this binary could not
	// be running here otherwise.
	facts.CPUArch = runtime.GOARCH
	facts.KernelVersion = w.config.HostKernelVersion
	return facts
}
