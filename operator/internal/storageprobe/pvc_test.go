// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storageprobe

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPVCCheckLeavesNoObjectBehind(t *testing.T) {
	mount := t.TempDir()
	err := Default{}.Check(t.Context(), Target{ClusterID: "cluster-a", PVC: &PVC{MountPath: mount, BasePath: "/team-a"}})
	require.NoError(t, err)

	entries, err := os.ReadDir(filepath.Join(mount, "team-a", "artifacts"))
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestPVCCheckUsesTheClusterIDInTheObjectName(t *testing.T) {
	mount := t.TempDir()
	artifacts := filepath.Join(mount, "artifacts")
	require.NoError(t, os.MkdirAll(artifacts, 0o755))
	other := filepath.Join(artifacts, accessCheckName("cluster-b"))
	require.NoError(t, os.WriteFile(other, []byte("another cluster"), 0o644))

	require.NoError(t, Default{}.Check(t.Context(), Target{ClusterID: "cluster-a", PVC: &PVC{MountPath: mount, BasePath: "/"}}))

	got, err := os.ReadFile(other)
	require.NoError(t, err)
	assert.Equal(t, "another cluster", string(got))
}

func TestPVCCheckNamesTheStepThatFailed(t *testing.T) {
	mount := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(mount, "artifacts"), nil, 0o644))

	err := Default{}.Check(t.Context(), Target{ClusterID: "c", PVC: &PVC{MountPath: mount, BasePath: "/"}})

	var failure *Failure
	require.ErrorAs(t, err, &failure)
	assert.Equal(t, StepWrite, failure.Step)
	assert.False(t, failure.Credentials)
}

func TestPVCCheckFailsOnAMountThatIsNotThere(t *testing.T) {
	err := Default{}.Check(t.Context(), Target{
		ClusterID: "c", PVC: &PVC{MountPath: filepath.Join(t.TempDir(), "missing", "deeper", "still"), BasePath: "/"},
	})
	require.NoError(t, err, "MkdirAll creates the directories under a writable parent")

	readOnly := t.TempDir()
	require.NoError(t, os.Chmod(readOnly, 0o500))
	t.Cleanup(func() { _ = os.Chmod(readOnly, 0o700) })
	err = Default{}.Check(t.Context(), Target{ClusterID: "c", PVC: &PVC{MountPath: readOnly, BasePath: "/"}})
	if os.Geteuid() != 0 {
		var failure *Failure
		require.ErrorAs(t, err, &failure)
		assert.Equal(t, StepWrite, failure.Step)
	}
}

func TestPVCCheckRefusesABasePathThatLeavesTheClaim(t *testing.T) {
	err := Default{}.Check(t.Context(), Target{ClusterID: "c", PVC: &PVC{MountPath: t.TempDir(), BasePath: "/a/../../b"}})
	assert.NoError(t, err, "path.Clean keeps a rooted path inside the claim")
}

func TestPVCCheckStopsOnACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := Default{}.Check(ctx, Target{ClusterID: "c", PVC: &PVC{MountPath: t.TempDir(), BasePath: "/"}})
	require.Error(t, err)
}

func TestCheckRequiresExactlyOneStore(t *testing.T) {
	assert.Error(t, Default{}.Check(t.Context(), Target{ClusterID: "c"}))
	assert.Error(t, Default{}.Check(t.Context(), Target{ClusterID: "c", PVC: &PVC{}, S3: &S3{}}))
}
