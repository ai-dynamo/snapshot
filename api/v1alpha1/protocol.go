// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// RestoreContainerMapping maps the one captured source container to a restore
// destination in the target Pod.
// +kubebuilder:object:generate=false
type RestoreContainerMapping struct {
	Source      string
	Destination string
}

// GetRestoreFromSnapshotName returns the same-namespace PodSnapshot named by
// the restore-from annotation.
func GetRestoreFromSnapshotName(annotations map[string]string) (string, error) {
	snapshotName := strings.TrimSpace(annotations[RestoreFromAnnotation])
	if snapshotName == "" {
		return "", fmt.Errorf("%s must name a PodSnapshot", RestoreFromAnnotation)
	}
	if errs := validation.IsDNS1123Subdomain(snapshotName); len(errs) != 0 {
		return "", fmt.Errorf("%s value %q is not a valid PodSnapshot name: %s", RestoreFromAnnotation, snapshotName, strings.Join(errs, "; "))
	}
	return snapshotName, nil
}

// RestoreContainerMappingsFromAnnotations parses the optional flat restore
// mapping. Absence keeps the existing same-name restore behavior.
func RestoreContainerMappingsFromAnnotations(annotations map[string]string, capturedSource string) ([]RestoreContainerMapping, error) {
	raw, ok := annotations[RestoreContainerMapAnnotation]
	if !ok {
		capturedSource = strings.TrimSpace(capturedSource)
		return []RestoreContainerMapping{{Source: capturedSource, Destination: capturedSource}}, nil
	}
	parts := strings.Split(strings.TrimSpace(raw), ",")
	mappings := make([]RestoreContainerMapping, 0, len(parts))
	for _, part := range parts {
		pair := strings.Split(part, "=")
		if len(pair) != 2 {
			return nil, fmt.Errorf("invalid %s entry %q: expected source=destination", RestoreContainerMapAnnotation, strings.TrimSpace(part))
		}
		mappings = append(mappings, RestoreContainerMapping{
			Source:      strings.TrimSpace(pair[0]),
			Destination: strings.TrimSpace(pair[1]),
		})
	}
	return mappings, nil
}

// ValidateRestoreContainerMappings enforces the one-source-to-many-destinations
// contract after parsing.
func ValidateRestoreContainerMappings(mappings []RestoreContainerMapping, capturedSource string) error {
	capturedSource = strings.TrimSpace(capturedSource)
	if errs := validation.IsDNS1123Label(capturedSource); len(errs) != 0 {
		return fmt.Errorf("captured source container %q is invalid: %s", capturedSource, strings.Join(errs, "; "))
	}
	if len(mappings) == 0 {
		return fmt.Errorf("restore container mapping must contain at least one destination")
	}
	destinations := make(map[string]struct{}, len(mappings))
	for _, mapping := range mappings {
		source := mapping.Source
		destination := mapping.Destination
		if errs := validation.IsDNS1123Label(source); len(errs) != 0 {
			return fmt.Errorf("invalid restore source container %q: %s", source, strings.Join(errs, "; "))
		}
		if errs := validation.IsDNS1123Label(destination); len(errs) != 0 {
			return fmt.Errorf("invalid restore destination container %q: %s", destination, strings.Join(errs, "; "))
		}
		if source != capturedSource {
			return fmt.Errorf("restore source container %q does not match captured container %q", source, capturedSource)
		}
		if _, duplicate := destinations[destination]; duplicate {
			return fmt.Errorf("duplicate restore destination container %q", destination)
		}
		destinations[destination] = struct{}{}
	}
	return nil
}
