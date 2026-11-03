// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"slices"

	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metav1validation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// Helpers shared by the controllers that build a PodSnapshot from a template:
// SnapshotJob, and PodGroupSnapshot for each of its members.

// newPodSnapshot builds a PodSnapshot that checkpoints source with the given
// settings. The caller owns the metadata: name, namespace, labels, annotations
// and any owner reference. The source's containers and the container settings
// are copied, so the result never shares them with the request it came from.
func newPodSnapshot(
	objectMeta metav1.ObjectMeta,
	source snapshotv1alpha1.PodReference,
	containers []snapshotv1alpha1.ContainerSnapshotConfig,
) *snapshotv1alpha1.PodSnapshot {
	return &snapshotv1alpha1.PodSnapshot{
		TypeMeta: metav1.TypeMeta{
			APIVersion: snapshotv1alpha1.GroupVersion.String(),
			Kind:       snapshotv1alpha1.KindPodSnapshot,
		},
		ObjectMeta: objectMeta,
		Spec: snapshotv1alpha1.PodSnapshotSpec{
			Source: snapshotv1alpha1.PodSnapshotSource{
				PodRef: snapshotv1alpha1.PodReference{
					Name:       source.Name,
					UID:        source.UID,
					Containers: slices.Clone(source.Containers),
				},
			},
			Containers: copyContainerSettings(containers),
		},
	}
}

// validateTemplateMetadata rejects template labels and annotations that the
// API server would refuse on the PodSnapshot built from them, and labels
// reserved for the controller that builds it. The CRDs check label keys and
// value lengths, but not label value syntax or the total annotation size, so a
// template that passes admission can still produce a PodSnapshot that cannot
// be created. path names the template's metadata, so errors point at the field
// the user wrote.
func validateTemplateMetadata(metadata *snapshotv1alpha1.PodSnapshotTemplateMetadata, path *field.Path, reservedLabels ...string) error {
	if metadata == nil {
		return nil
	}
	labelsPath := path.Child("labels")
	errs := metav1validation.ValidateLabels(metadata.Labels, labelsPath)
	errs = append(errs, apivalidation.ValidateAnnotations(metadata.Annotations, path.Child("annotations"))...)
	for _, reserved := range reservedLabels {
		if _, found := metadata.Labels[reserved]; found {
			errs = append(errs, field.Forbidden(labelsPath.Key(reserved), "label is controller-owned"))
		}
	}
	return errs.ToAggregate()
}
