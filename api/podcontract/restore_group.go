// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podcontract

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// GroupTargetRequest names the container of a group restore target that
// receives the member's processes.
type GroupTargetRequest struct {
	DestinationContainer string
}

// BuildGroupTarget returns a deep copy of pod shaped as a group restore target,
// for an owner to create: a Pod template, or a Pod-creation webhook. It adds
// RestoreGroupSchedulingGate once, so a Pod that already carries it keeps one,
// and shapes the destination container as Build does for a standalone
// restore: the control volume, mount and environment, and the seccomp profile
// when options.SeccompProfile is set. It always adds the restore startup
// probe, which a group target needs whatever options.EnableStartupGate says.
//
// It writes no annotation: the operator writes the claim. It therefore
// refuses a Pod that carries RestoreFromAnnotation, RestoreGroupUIDAnnotation
// or RestoreContainerMapAnnotation, and a destination that is not a valid
// container name or not in the Pod. The caller's Pod is never mutated,
// including when it refuses one.
func BuildGroupTarget(pod *corev1.Pod, request GroupTargetRequest, options Options) (*corev1.Pod, error) {
	if pod == nil {
		return nil, fmt.Errorf("restore pod is nil")
	}
	destination, err := groupTargetDestination(request)
	if err != nil {
		return nil, err
	}
	for _, key := range []string{RestoreFromAnnotation, RestoreGroupUIDAnnotation, RestoreContainerMapAnnotation} {
		if _, found := pod.Annotations[key]; found {
			return nil, fmt.Errorf("a group target must not carry %s when it is created", key)
		}
	}
	options.EnableStartupGate = true
	result := pod.DeepCopy()
	if !HasRestoreGroupGate(result) {
		result.Spec.SchedulingGates = append(result.Spec.SchedulingGates,
			corev1.PodSchedulingGate{Name: RestoreGroupSchedulingGate})
	}
	destinations := []string{destination}
	if err := ensureRestorePodSpec(&result.Spec, destinations, options); err != nil {
		return nil, err
	}
	if err := validateGroupTarget(&result.Spec, destinations, options); err != nil {
		return nil, fmt.Errorf("validate shaped group target: %w", err)
	}
	return result, nil
}

// ValidateGroupTarget verifies that pod has the shape of a group restore
// target for request: the control volume, and on the destination container
// the control mount and environment and the canonical restore startup probe.
// Like Validate, it leaves the seccomp profile to the producer. It checks
// neither RestoreGroupSchedulingGate nor the claim, which change during the
// Pod's life. The operator calls it on a target before it claims the Pod. It
// performs no API read and never mutates the Pod.
func ValidateGroupTarget(pod *corev1.Pod, request GroupTargetRequest) error {
	if pod == nil {
		return fmt.Errorf("restore pod is nil")
	}
	destination, err := groupTargetDestination(request)
	if err != nil {
		return err
	}
	return validateGroupTarget(&pod.Spec, []string{destination}, Options{EnableStartupGate: true})
}

func groupTargetDestination(request GroupTargetRequest) (string, error) {
	destination := strings.TrimSpace(request.DestinationContainer)
	if err := validateRestoreContainerName("destination", destination); err != nil {
		return "", err
	}
	return destination, nil
}

func validateGroupTarget(spec *corev1.PodSpec, destinations []string, options Options) error {
	if err := validateRestoreDestinations(spec, destinations); err != nil {
		return err
	}
	return validateCanonicalDestinations(spec, destinations, options)
}

// HasRestoreGroupGate reports whether pod still carries
// RestoreGroupSchedulingGate: it is a group target that no PodGroupRestore has
// claimed yet.
func HasRestoreGroupGate(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	for _, gate := range pod.Spec.SchedulingGates {
		if gate.Name == RestoreGroupSchedulingGate {
			return true
		}
	}
	return false
}

// RestoreGroupClaim is what the operator's claim writes on a group target.
type RestoreGroupClaim struct {
	// UID is the UID of the PodGroupRestore that claims the Pod, written to
	// RestoreGroupUIDAnnotation.
	UID types.UID

	// Mapping is the target's mapping from the PodGroupRestore, written to
	// RestoreContainerMapAnnotation as exactly one pair.
	Mapping ContainerMapping
}

// ClaimGroupTarget returns a deep copy of pod that differs from it only by the
// claim: RestoreGroupUIDAnnotation and a single-pair
// RestoreContainerMapAnnotation are written, and RestoreGroupSchedulingGate is
// removed. Other scheduling gates, such as a gang scheduler's, stay. It refuses
// a Pod without the gate, a Pod that already carries a binding, a map or
// RestoreFromAnnotation, an empty UID, and a mapping whose names are not valid
// container names.
//
// It performs no API call. The operator sends the result as an update
// conditional on the Pod's resourceVersion, so the binding, the map and the
// gate's removal land together, before the Pod can be scheduled.
func ClaimGroupTarget(pod *corev1.Pod, claim RestoreGroupClaim) (*corev1.Pod, error) {
	if pod == nil {
		return nil, fmt.Errorf("restore pod is nil")
	}
	if !HasRestoreGroupGate(pod) {
		return nil, fmt.Errorf("the Pod is not an unclaimed group target: it has no %s scheduling gate",
			RestoreGroupSchedulingGate)
	}
	for _, key := range []string{RestoreGroupUIDAnnotation, RestoreContainerMapAnnotation, RestoreFromAnnotation} {
		if _, found := pod.Annotations[key]; found {
			return nil, fmt.Errorf("a group target to claim must not carry %s", key)
		}
	}
	if strings.TrimSpace(string(claim.UID)) == "" {
		return nil, fmt.Errorf("a claim needs the UID of its PodGroupRestore")
	}
	if err := validateContainerMappingNames(claim.Mapping); err != nil {
		return nil, err
	}

	result := pod.DeepCopy()
	if result.Annotations == nil {
		result.Annotations = make(map[string]string, 2)
	}
	result.Annotations[RestoreGroupUIDAnnotation] = string(claim.UID)
	result.Annotations[RestoreContainerMapAnnotation] = formatContainerMapping(claim.Mapping)
	gates := make([]corev1.PodSchedulingGate, 0, len(result.Spec.SchedulingGates))
	for _, gate := range result.Spec.SchedulingGates {
		if gate.Name != RestoreGroupSchedulingGate {
			gates = append(gates, gate)
		}
	}
	if len(gates) == 0 {
		gates = nil
	}
	result.Spec.SchedulingGates = gates
	return result, nil
}

// RestoreGroupClaimOf reads the claim of a group target from its annotations.
// It takes annotations rather than a Pod because the NRI plugin has only the
// sandbox's, which the claim reaches before the Pod is scheduled. It returns
// found == false, and no error, when the Pod carries no
// RestoreGroupUIDAnnotation: no PodGroupRestore has claimed it. A claimed Pod
// must carry a RestoreContainerMapAnnotation of exactly one pair, must not
// carry RestoreFromAnnotation, and must use valid names: an error means that
// the Pod is bound but its claim is invalid.
func RestoreGroupClaimOf(annotations map[string]string) (claim RestoreGroupClaim, found bool, err error) {
	uid, bound := annotations[RestoreGroupUIDAnnotation]
	if !bound {
		return RestoreGroupClaim{}, false, nil
	}
	if _, found := annotations[RestoreFromAnnotation]; found {
		return RestoreGroupClaim{}, true, fmt.Errorf("a claimed group target must not carry %s", RestoreFromAnnotation)
	}
	if strings.TrimSpace(uid) == "" {
		return RestoreGroupClaim{}, true, fmt.Errorf("%s is empty", RestoreGroupUIDAnnotation)
	}
	if _, found := annotations[RestoreContainerMapAnnotation]; !found {
		return RestoreGroupClaim{}, true, fmt.Errorf("a claimed group target must carry %s", RestoreContainerMapAnnotation)
	}
	mappings, err := ContainerMappingsFromAnnotations(annotations, "")
	if err != nil {
		return RestoreGroupClaim{}, true, err
	}
	if len(mappings) != 1 {
		return RestoreGroupClaim{}, true, fmt.Errorf("%s of a claimed group target must hold exactly one pair, not %d",
			RestoreContainerMapAnnotation, len(mappings))
	}
	if err := validateContainerMappingNames(mappings[0]); err != nil {
		return RestoreGroupClaim{}, true, err
	}
	return RestoreGroupClaim{UID: types.UID(uid), Mapping: mappings[0]}, true, nil
}
