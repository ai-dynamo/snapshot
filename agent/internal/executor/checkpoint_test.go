// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"testing"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequiredDeclaredVolumes(t *testing.T) {
	candidates := []snapshotv1alpha1.CheckpointSourceDeclaredVolume{
		{Path: "/model-cache", Volume: "model-cache", VolumeSource: "PersistentVolumeClaim/model-cache"},
		{Path: "/scratch", Volume: "scratch", VolumeSource: "EmptyDir"},
		{Path: "/not-externalized", Volume: "ignored", VolumeSource: "ConfigMap/ignored"},
	}

	volumes := requiredDeclaredVolumes(candidates, map[string]string{
		"/model-cache":         "/model-cache",
		"/scratch":             "/scratch",
		"/etc/hosts":           "/etc/hosts",
		"/var/run/secrets/k8s": "/var/run/secrets/k8s",
	})

	assert.Equal(t, candidates[:2], volumes)
}

func TestRequiredDeclaredVolumesRecordsKnownZero(t *testing.T) {
	volumes := requiredDeclaredVolumes(nil, nil)

	require.NotNil(t, volumes)
	assert.Empty(t, volumes)
}

func TestRequiredDeclaredVolumesMatchesRunAliases(t *testing.T) {
	candidates := []snapshotv1alpha1.CheckpointSourceDeclaredVolume{
		{Path: "/var/run/model-cache", Volume: "model-cache", VolumeSource: "PersistentVolumeClaim/model-cache"},
		{Path: "/run/credentials", Volume: "credentials", VolumeSource: "Secret/credentials"},
	}

	volumes := requiredDeclaredVolumes(candidates, map[string]string{
		"/run/model-cache":     "/run/model-cache",
		"/var/run/credentials": "/var/run/credentials",
		"/etc/hosts":           "/etc/hosts",
	})

	assert.Equal(t, candidates, volumes)
}

func TestRequiredDeclaredVolumesMatchesAliasedCandidates(t *testing.T) {
	candidates := []snapshotv1alpha1.CheckpointSourceDeclaredVolume{
		{Path: "/run/data", Volume: "first", VolumeSource: "EmptyDir"},
		{Path: "/var/run/data", Volume: "second", VolumeSource: "EmptyDir"},
	}

	volumes := requiredDeclaredVolumes(candidates, map[string]string{"/run/data": "/run/data"})

	assert.Equal(t, candidates, volumes)
}
