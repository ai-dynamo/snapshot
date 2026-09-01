// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podcontract

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func restorePodFixture() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "restore-worker",
			Namespace:   "inference",
			Annotations: map[string]string{"example.com/team": "inference"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:    "main",
					Image:   "worker:latest",
					Command: []string{"python3"},
					Args:    []string{"serve.py"},
				},
				{Name: "sidecar", Image: "sidecar:latest"},
			},
		},
	}
}

func restoreRequest(mappings ...ContainerMapping) Request {
	return Request{
		SnapshotName:    "snapshot-a",
		SourceContainer: "main",
		Mappings:        mappings,
	}
}

func TestBuildShapesSingleDestination(t *testing.T) {
	original := restorePodFixture()
	before := original.DeepCopy()
	options := Options{
		SeccompProfile:    DefaultSeccompLocalhostProfile,
		EnableStartupGate: true,
	}

	shaped, err := Build(original, restoreRequest(), options)
	if err != nil {
		t.Fatalf("Build() failed: %v", err)
	}
	if !reflect.DeepEqual(original, before) {
		t.Fatal("Build() mutated its input")
	}
	if shaped.Annotations[RestoreFromAnnotation] != "snapshot-a" {
		t.Fatalf("%s = %q", RestoreFromAnnotation, shaped.Annotations[RestoreFromAnnotation])
	}
	if _, found := shaped.Annotations[RestoreContainerMapAnnotation]; found {
		t.Fatalf("same-name restore unexpectedly set %s", RestoreContainerMapAnnotation)
	}
	if shaped.Annotations["example.com/team"] != "inference" {
		t.Fatal("unrelated annotation was not preserved")
	}
	if len(shaped.Spec.Volumes) != 1 || shaped.Spec.Volumes[0].EmptyDir == nil {
		t.Fatalf("expected one snapshot control emptyDir, got %#v", shaped.Spec.Volumes)
	}

	main := &shaped.Spec.Containers[0]
	if !reflect.DeepEqual(main.Command, []string{"python3"}) || !reflect.DeepEqual(main.Args, []string{"serve.py"}) {
		t.Fatalf("workload command changed: command=%v args=%v", main.Command, main.Args)
	}
	if len(main.VolumeMounts) != 1 || main.VolumeMounts[0].SubPath != "main" {
		t.Fatalf("unexpected snapshot control mount: %#v", main.VolumeMounts)
	}
	for _, name := range []string{SnapshotControlDirEnv, LegacySnapshotControlDirEnv} {
		if got := restoreEnvValue(main.Env, name); got != SnapshotControlMountPath {
			t.Fatalf("%s = %q, want %q", name, got, SnapshotControlMountPath)
		}
	}
	for _, name := range []string{RestoreStandbyModeEnv, LegacyRestoreStandbyModeEnv} {
		if got := restoreEnvValue(main.Env, name); got != "" {
			t.Fatalf("generic builder injected workload-specific %s value %q", name, got)
		}
	}
	if main.StartupProbe == nil ||
		main.StartupProbe.Exec == nil ||
		!reflect.DeepEqual(
			main.StartupProbe.Exec.Command,
			[]string{"cat", SnapshotControlMountPath + "/" + RestoreCompleteFile},
		) {
		t.Fatalf("unexpected restore startup gate: %#v", main.StartupProbe)
	}
	if shaped.Spec.SecurityContext != nil {
		t.Fatalf("restore profile was applied at Pod scope: %#v", shaped.Spec.SecurityContext)
	}
	if main.SecurityContext == nil ||
		!matchesLocalhostSeccompProfile(main.SecurityContext.SeccompProfile, DefaultSeccompLocalhostProfile) {
		t.Fatalf("destination is missing expected seccomp profile: %#v", main.SecurityContext)
	}
	if !reflect.DeepEqual(shaped.Spec.Containers[1], before.Spec.Containers[1]) {
		t.Fatal("non-destination sidecar was modified")
	}
	if err := Validate(shaped, restoreRequest()); err != nil {
		t.Fatalf("Validate() rejected builder output: %v", err)
	}
}

func TestBuildShapesFanoutIdempotently(t *testing.T) {
	pod := restorePodFixture()
	pod.Spec.Containers = []corev1.Container{{Name: "engine-0"}, {Name: "engine-1"}}
	request := restoreRequest(
		ContainerMapping{Source: "main", Destination: "engine-1"},
		ContainerMapping{Source: "main", Destination: "engine-0"},
	)
	options := Options{SeccompProfile: DefaultSeccompLocalhostProfile}

	first, err := Build(pod, request, options)
	if err != nil {
		t.Fatalf("first Build() failed: %v", err)
	}
	second, err := Build(first, request, options)
	if err != nil {
		t.Fatalf("second Build() failed: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("Build() is not idempotent")
	}
	if got := first.Annotations[RestoreContainerMapAnnotation]; got != "main=engine-0,main=engine-1" {
		t.Fatalf("%s = %q", RestoreContainerMapAnnotation, got)
	}
	if len(first.Spec.Volumes) != 1 {
		t.Fatalf("expected one shared volume, got %d", len(first.Spec.Volumes))
	}
	for i, name := range []string{"engine-0", "engine-1"} {
		container := &first.Spec.Containers[i]
		if len(container.VolumeMounts) != 1 || container.VolumeMounts[0].SubPath != name {
			t.Fatalf("container %q mount = %#v", name, container.VolumeMounts)
		}
		if container.SecurityContext == nil ||
			!matchesLocalhostSeccompProfile(container.SecurityContext.SeccompProfile, DefaultSeccompLocalhostProfile) {
			t.Fatalf("container %q is missing expected seccomp profile: %#v", name, container.SecurityContext)
		}
	}
}

func TestBuildStartupGateIsOptIn(t *testing.T) {
	pod := restorePodFixture()
	pod.Spec.Containers[0].LivenessProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/live"}},
	}
	pod.Spec.Containers[0].ReadinessProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/ready"}},
	}
	pod.Spec.Containers[0].StartupProbe = &corev1.Probe{
		ProbeHandler:  corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/startup"}},
		PeriodSeconds: 3,
	}
	beforeLiveness := pod.Spec.Containers[0].LivenessProbe.DeepCopy()
	beforeReadiness := pod.Spec.Containers[0].ReadinessProbe.DeepCopy()
	beforeStartup := pod.Spec.Containers[0].StartupProbe.DeepCopy()

	ungated, err := Build(pod, restoreRequest(), Options{})
	if err != nil {
		t.Fatalf("ungated Build() failed: %v", err)
	}
	if !reflect.DeepEqual(ungated.Spec.Containers[0].StartupProbe, beforeStartup) {
		t.Fatal("zero-value options changed the workload startup probe")
	}

	gated, err := Build(pod, restoreRequest(), Options{EnableStartupGate: true})
	if err != nil {
		t.Fatalf("gated Build() failed: %v", err)
	}
	main := &gated.Spec.Containers[0]
	if !reflect.DeepEqual(main.LivenessProbe, beforeLiveness) || !reflect.DeepEqual(main.ReadinessProbe, beforeReadiness) {
		t.Fatal("workload liveness or readiness probe was modified")
	}
	if reflect.DeepEqual(main.StartupProbe, beforeStartup) {
		t.Fatal("workload startup probe was not replaced by the restore gate")
	}
	if !reflect.DeepEqual(pod.Spec.Containers[0].StartupProbe, beforeStartup) {
		t.Fatal("Build() mutated the input startup probe")
	}
	if !reflect.DeepEqual(main.StartupProbe, canonicalRestoreStartupProbe()) {
		t.Fatalf("unexpected restore startup gate: %#v", main.StartupProbe)
	}
}

func TestBuildCanonicalizesEquivalentMappings(t *testing.T) {
	t.Run("fanout", func(t *testing.T) {
		pod := restorePodFixture()
		pod.Spec.Containers = []corev1.Container{{Name: "engine-0"}, {Name: "engine-1"}}
		pod.Annotations[RestoreContainerMapAnnotation] = "main=engine-1, main=engine-0"
		request := restoreRequest(
			ContainerMapping{Source: "main", Destination: "engine-0"},
			ContainerMapping{Source: "main", Destination: "engine-1"},
		)

		shaped, err := Build(pod, request, Options{})
		if err != nil {
			t.Fatalf("Build() failed: %v", err)
		}
		if got := shaped.Annotations[RestoreContainerMapAnnotation]; got != "main=engine-0,main=engine-1" {
			t.Fatalf("canonical mapping = %q", got)
		}
	})

	t.Run("same name", func(t *testing.T) {
		pod := restorePodFixture()
		pod.Annotations[RestoreContainerMapAnnotation] = "main=main"
		shaped, err := Build(pod, restoreRequest(), Options{})
		if err != nil {
			t.Fatalf("Build() failed: %v", err)
		}
		if _, found := shaped.Annotations[RestoreContainerMapAnnotation]; found {
			t.Fatalf("canonical same-name restore retained %s", RestoreContainerMapAnnotation)
		}
	})
}

func TestBuildRejectsConflictsAtomically(t *testing.T) {
	runtimeDefault := corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
	tests := []struct {
		name     string
		mutate   func(*corev1.Pod)
		mappings []ContainerMapping
	}{
		{
			name: "restore annotation",
			mutate: func(pod *corev1.Pod) {
				pod.Annotations[RestoreFromAnnotation] = "snapshot-b"
			},
		},
		{
			name: "mapping annotation",
			mutate: func(pod *corev1.Pod) {
				pod.Annotations[RestoreContainerMapAnnotation] = "main=sidecar"
			},
		},
		{
			name: "control volume",
			mutate: func(pod *corev1.Pod) {
				pod.Spec.Volumes = []corev1.Volume{{
					Name: SnapshotControlVolumeName,
					VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: "wrong",
					}},
				}}
			},
		},
		{
			name: "control mount name",
			mutate: func(pod *corev1.Pod) {
				pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{
					Name: SnapshotControlVolumeName, MountPath: "/wrong", SubPath: "main",
				}}
			},
		},
		{
			name: "control mount path",
			mutate: func(pod *corev1.Pod) {
				pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{
					Name: "other", MountPath: SnapshotControlMountPath,
				}}
			},
		},
		{
			name: "control environment",
			mutate: func(pod *corev1.Pod) {
				pod.Spec.Containers[0].Env = []corev1.EnvVar{{Name: SnapshotControlDirEnv, Value: "/wrong"}}
			},
		},
		{
			name: "container seccomp",
			mutate: func(pod *corev1.Pod) {
				pod.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{SeccompProfile: runtimeDefault.DeepCopy()}
			},
		},
		{
			name:     "missing destination",
			mappings: []ContainerMapping{{Source: "main", Destination: "missing"}},
		},
		{
			name: "duplicate destination",
			mappings: []ContainerMapping{
				{Source: "main", Destination: "main"},
				{Source: "main", Destination: "main"},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pod := restorePodFixture()
			if test.mutate != nil {
				test.mutate(pod)
			}
			before := pod.DeepCopy()

			if _, err := Build(
				pod,
				restoreRequest(test.mappings...),
				Options{SeccompProfile: DefaultSeccompLocalhostProfile},
			); err == nil {
				t.Fatal("Build() unexpectedly succeeded")
			}
			if !reflect.DeepEqual(pod, before) {
				t.Fatal("Build() mutated input after failure")
			}
		})
	}
}

func TestBuildScopesSeccompToRestoreDestinations(t *testing.T) {
	runtimeDefault := &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
	pod := restorePodFixture()
	pod.Spec.SecurityContext = &corev1.PodSecurityContext{SeccompProfile: runtimeDefault.DeepCopy()}
	beforeSidecar := pod.Spec.Containers[1].DeepCopy()

	shaped, err := Build(
		pod,
		restoreRequest(),
		Options{SeccompProfile: DefaultSeccompLocalhostProfile},
	)
	if err != nil {
		t.Fatalf("Build() failed: %v", err)
	}
	if !reflect.DeepEqual(shaped.Spec.SecurityContext.SeccompProfile, runtimeDefault) {
		t.Fatalf("Pod seccomp profile changed: %#v", shaped.Spec.SecurityContext.SeccompProfile)
	}
	main := &shaped.Spec.Containers[0]
	if main.SecurityContext == nil ||
		!matchesLocalhostSeccompProfile(main.SecurityContext.SeccompProfile, DefaultSeccompLocalhostProfile) {
		t.Fatalf("destination is missing expected seccomp profile: %#v", main.SecurityContext)
	}
	if !reflect.DeepEqual(&shaped.Spec.Containers[1], beforeSidecar) {
		t.Fatalf("non-destination sidecar changed: %#v", shaped.Spec.Containers[1])
	}
}

func TestValidateMinimumContract(t *testing.T) {
	pod, err := Build(restorePodFixture(), restoreRequest(), Options{
		SeccompProfile:    DefaultSeccompLocalhostProfile,
		EnableStartupGate: true,
	})
	if err != nil {
		t.Fatalf("Build() failed: %v", err)
	}

	// Legacy environment compatibility, startup gating, and seccomp selection
	// are producer policy rather than agent-facing minimum protocol.
	pod.Spec.Containers[0].Env = removeRestoreEnv(pod.Spec.Containers[0].Env, LegacySnapshotControlDirEnv)
	pod.Spec.Containers[0].StartupProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"true"}}},
	}
	pod.Spec.Containers[0].SecurityContext.SeccompProfile = nil
	if err := Validate(pod, restoreRequest()); err != nil {
		t.Fatalf("Validate() rejected minimum restore protocol: %v", err)
	}

	pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env, corev1.EnvVar{
		Name:  LegacySnapshotControlDirEnv,
		Value: "/wrong",
	})
	if err := Validate(pod, restoreRequest()); err == nil {
		t.Fatal("Validate() accepted conflicting deprecated environment alias")
	}
}

func TestValidateRejectsMinimumContractDrift(t *testing.T) {
	valid, err := Build(restorePodFixture(), restoreRequest(), Options{})
	if err != nil {
		t.Fatalf("Build() failed: %v", err)
	}
	tests := map[string]func(*corev1.Pod){
		"annotation": func(pod *corev1.Pod) {
			delete(pod.Annotations, RestoreFromAnnotation)
		},
		"volume": func(pod *corev1.Pod) {
			pod.Spec.Volumes = nil
		},
		"mount": func(pod *corev1.Pod) {
			pod.Spec.Containers[0].VolumeMounts[0].SubPath = "other"
		},
		"environment": func(pod *corev1.Pod) {
			pod.Spec.Containers[0].Env = removeRestoreEnv(pod.Spec.Containers[0].Env, SnapshotControlDirEnv)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			pod := valid.DeepCopy()
			mutate(pod)
			if err := Validate(pod, restoreRequest()); err == nil {
				t.Fatal("Validate() unexpectedly succeeded")
			}
		})
	}
}

func TestBuildAllowsUnmanagedSeccomp(t *testing.T) {
	pod := restorePodFixture()
	shaped, err := Build(pod, restoreRequest(), Options{})
	if err != nil {
		t.Fatalf("Build() failed: %v", err)
	}
	if shaped.Spec.SecurityContext != nil {
		t.Fatalf("empty option unexpectedly changed security context: %#v", shaped.Spec.SecurityContext)
	}
	if shaped.Spec.Containers[0].SecurityContext != nil {
		t.Fatalf(
			"empty option unexpectedly changed destination security context: %#v",
			shaped.Spec.Containers[0].SecurityContext,
		)
	}
}

func restoreEnvValue(env []corev1.EnvVar, name string) string {
	for _, item := range env {
		if item.Name == name {
			return item.Value
		}
	}
	return ""
}

func removeRestoreEnv(env []corev1.EnvVar, name string) []corev1.EnvVar {
	result := make([]corev1.EnvVar, 0, len(env))
	for _, item := range env {
		if item.Name != name {
			result = append(result, item)
		}
	}
	return result
}
