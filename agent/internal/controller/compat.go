// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"

	"github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/ai-dynamo/snapshot/api/compat"
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

// preflightCompatibility runs the pre-flight compatibility gate for one restore.
// A nil error means the restore may be attempted.
func (w *NodeController) preflightCompatibility(pod *corev1.Pod, artifact *restoreArtifact) error {
	log := w.log.WithValues("pod", fmt.Sprintf("%s/%s", pod.Namespace, pod.Name), "container", artifact.ContainerName)

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

	// Target facts are read by the rules that need them, since each one costs a
	// syscall or an API read on a path that runs before every restore.
	mismatches := w.compareFn(compat.GatePreflight, manifest.CompatFacts(), compat.Facts{})
	if len(mismatches) == 0 {
		return nil
	}
	return compat.NewIncompatibleError(compat.GatePreflight, mismatches)
}
