// SPDX-FileCopyrightText: Copyright (c) 2025-2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package protocol

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

func TestEnsureControlVolume(t *testing.T) {
	t.Run("adds volume mount and env from empty", func(t *testing.T) {
		ps := &corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}}
		EnsureControlVolume(ps, &ps.Containers[0])

		if len(ps.Volumes) != 1 || ps.Volumes[0].Name != snapshotv1alpha1.SnapshotControlVolumeName || ps.Volumes[0].EmptyDir == nil {
			t.Fatalf("expected one %s emptyDir volume, got %#v", snapshotv1alpha1.SnapshotControlVolumeName, ps.Volumes)
		}
		c := ps.Containers[0]
		if len(c.VolumeMounts) != 1 || c.VolumeMounts[0].Name != snapshotv1alpha1.SnapshotControlVolumeName || c.VolumeMounts[0].MountPath != snapshotv1alpha1.SnapshotControlMountPath {
			t.Fatalf("expected one %s mount at %s, got %#v", snapshotv1alpha1.SnapshotControlVolumeName, snapshotv1alpha1.SnapshotControlMountPath, c.VolumeMounts)
		}
		if c.VolumeMounts[0].SubPath != "main" {
			t.Fatalf("expected subPath=%q, got %q", "main", c.VolumeMounts[0].SubPath)
		}
		if len(c.Env) != 2 {
			t.Fatalf("expected two control-dir env vars, got %#v", c.Env)
		}
		for _, name := range []string{snapshotv1alpha1.SnapshotControlDirEnv, snapshotv1alpha1.LegacySnapshotControlDirEnv} {
			found := false
			for _, e := range c.Env {
				if e.Name == name {
					found = true
					if e.Value != snapshotv1alpha1.SnapshotControlMountPath {
						t.Fatalf("expected env %s=%s, got %#v", name, snapshotv1alpha1.SnapshotControlMountPath, e)
					}
				}
			}
			if !found {
				t.Fatalf("expected env %s, got %#v", name, c.Env)
			}
		}
	})

	t.Run("per-container subPath isolates multi-container pods", func(t *testing.T) {
		ps := &corev1.PodSpec{Containers: []corev1.Container{
			{Name: "engine-0"},
			{Name: "engine-1"},
		}}
		EnsureControlVolume(ps, &ps.Containers[0])
		EnsureControlVolume(ps, &ps.Containers[1])

		if len(ps.Volumes) != 1 {
			t.Fatalf("expected single shared emptyDir, got %#v", ps.Volumes)
		}
		if got := ps.Containers[0].VolumeMounts[0].SubPath; got != "engine-0" {
			t.Fatalf("engine-0 subPath=%q", got)
		}
		if got := ps.Containers[1].VolumeMounts[0].SubPath; got != "engine-1" {
			t.Fatalf("engine-1 subPath=%q", got)
		}
	})

	t.Run("idempotent", func(t *testing.T) {
		ps := &corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}}
		EnsureControlVolume(ps, &ps.Containers[0])
		EnsureControlVolume(ps, &ps.Containers[0])
		c := ps.Containers[0]
		if len(ps.Volumes) != 1 || len(c.VolumeMounts) != 1 || len(c.Env) != 2 {
			t.Fatalf("expected single volume/mount and two envs after two calls, got volumes=%d mounts=%d env=%d", len(ps.Volumes), len(c.VolumeMounts), len(c.Env))
		}
	})

	t.Run("legacy env pre-set backfills canonical", func(t *testing.T) {
		ps := &corev1.PodSpec{Containers: []corev1.Container{{
			Name: "main",
			Env:  []corev1.EnvVar{{Name: snapshotv1alpha1.LegacySnapshotControlDirEnv, Value: snapshotv1alpha1.SnapshotControlMountPath}},
		}}}
		EnsureControlVolume(ps, &ps.Containers[0])
		c := ps.Containers[0]
		if len(c.Env) != 2 {
			t.Fatalf("expected legacy env preserved and canonical env added, got %#v", c.Env)
		}
		legacyCount := 0
		for _, e := range c.Env {
			if e.Name == snapshotv1alpha1.LegacySnapshotControlDirEnv {
				legacyCount++
			}
		}
		if legacyCount != 1 {
			t.Fatalf("expected legacy env not duplicated, got %#v", c.Env)
		}
	})

	t.Run("canonical env pre-set backfills legacy", func(t *testing.T) {
		ps := &corev1.PodSpec{Containers: []corev1.Container{{
			Name: "main",
			Env:  []corev1.EnvVar{{Name: snapshotv1alpha1.SnapshotControlDirEnv, Value: snapshotv1alpha1.SnapshotControlMountPath}},
		}}}
		EnsureControlVolume(ps, &ps.Containers[0])
		c := ps.Containers[0]
		if len(c.Env) != 2 {
			t.Fatalf("expected canonical env preserved and legacy env added, got %#v", c.Env)
		}
		canonicalCount := 0
		for _, e := range c.Env {
			if e.Name == snapshotv1alpha1.SnapshotControlDirEnv {
				canonicalCount++
			}
		}
		if canonicalCount != 1 {
			t.Fatalf("expected canonical env not duplicated, got %#v", c.Env)
		}
	})

	t.Run("nil pod spec no-op", func(t *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("expected no panic, got %v", r)
			}
		}()
		EnsureControlVolume(nil, &corev1.Container{})
	})

	t.Run("nil container no-op", func(t *testing.T) {
		ps := &corev1.PodSpec{}
		EnsureControlVolume(ps, nil)
		if len(ps.Volumes) != 0 {
			t.Fatalf("expected no volumes when container is nil, got %#v", ps.Volumes)
		}
	})

	t.Run("preserves existing entries", func(t *testing.T) {
		ps := &corev1.PodSpec{
			Volumes: []corev1.Volume{{
				Name:         "other",
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			}},
			Containers: []corev1.Container{{
				Name:         "main",
				VolumeMounts: []corev1.VolumeMount{{Name: "other", MountPath: "/other"}},
				Env:          []corev1.EnvVar{{Name: "OTHER", Value: "x"}},
			}},
		}
		EnsureControlVolume(ps, &ps.Containers[0])
		c := ps.Containers[0]
		if len(ps.Volumes) != 2 || len(c.VolumeMounts) != 2 || len(c.Env) != 3 {
			t.Fatalf("expected existing + control entries, got volumes=%#v mounts=%#v env=%#v", ps.Volumes, c.VolumeMounts, c.Env)
		}
	})
}
