//go:build integration

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package crdinstaller_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	snapshotv1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/ai-dynamo/snapshot/api/v1alpha1/crds"
)

func TestContainerConfigAdmission(t *testing.T) {
	var defs []*apiextensionsv1.CustomResourceDefinition
	for _, def := range []string{crds.PodSnapshotCRD(), crds.PodSnapshotContentCRD(), crds.SnapshotJobCRD()} {
		var crd apiextensionsv1.CustomResourceDefinition
		require.NoError(t, yaml.Unmarshal([]byte(def), &crd))
		defs = append(defs, &crd)
	}
	env := &envtest.Environment{CRDs: defs}
	t.Cleanup(func() { assert.NoError(t, env.Stop()) })
	cfg, err := env.Start()
	require.NoError(t, err)
	scheme := runtime.NewScheme()
	require.NoError(t, snapshotv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	cl, err := client.New(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)
	ctx := t.Context()
	require.NoError(t, cl.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "workload"}}))

	// The settings types are shared, so their rules are covered once, on
	// PodSnapshot. The other kinds check their own list rules below.
	for i, tc := range []struct {
		name   string
		config snapshotv1.ContainerSnapshotConfig
		valid  bool
	}{
		{"name-only", snapshotv1.ContainerSnapshotConfig{Name: "main"}, true},
		{"file-probe", config(fileProbe("/tmp/ready"), nil), true},
		{"http-probe", config(httpProbe("/checkpoint/status", 8000), nil), true},
		{"file-post-restore", config(nil, filePostRestore("/tmp/released")), true},
		{"http-post-restore", config(nil, httpPostRestore("/checkpoint/resume", 8000)), true},
		{"both", config(fileProbe("/tmp/ready"), filePostRestore("/tmp/released")), true},
		{"empty-lifecycle", snapshotv1.ContainerSnapshotConfig{Name: "main", Lifecycle: &snapshotv1.SnapshotLifecycle{}}, true},
		{"root-dir-file", config(fileProbe("/x"), nil), true},
		{"other-container", snapshotv1.ContainerSnapshotConfig{Name: "sidecar"}, false},
		{"probe-without-handler", config(&snapshotv1.CheckpointReadinessProbe{}, nil), false},
		{"probe-with-both-handlers", config(func() *snapshotv1.CheckpointReadinessProbe {
			p := fileProbe("/tmp/ready")
			p.HTTPGet = httpProbe("/ok", 80).HTTPGet
			return p
		}(), nil), false},
		{"post-restore-without-handler", config(nil, &snapshotv1.PostRestoreAction{}), false},
		{"post-restore-with-both-handlers", config(nil, func() *snapshotv1.PostRestoreAction {
			a := filePostRestore("/tmp/released")
			a.HTTPPost = httpPostRestore("/ok", 80).HTTPPost
			return a
		}()), false},
		{"same-file-path", config(fileProbe("/tmp/marker"), filePostRestore("/tmp/marker")), false},
		{"relative-file-path", config(fileProbe("tmp/ready"), nil), false},
		{"dot-file-path", config(fileProbe("/tmp/./ready"), nil), false},
		{"dot-dot-file-path", config(nil, filePostRestore("/tmp/../etc/released")), false},
		{"root-file-path", config(fileProbe("/"), nil), false},
		{"double-slash-file-path", config(fileProbe("/tmp//ready"), nil), false},
		{"trailing-slash-file-path", config(nil, filePostRestore("/tmp/released/")), false},
		{"same-file-spelled-differently", config(fileProbe("/tmp/marker"), filePostRestore("/tmp//marker")), false},
		{"long-file-path", config(fileProbe("/"+strings.Repeat("a", 4096)), nil), false},
		{"http-path-without-slash", config(httpProbe("status", 8000), nil), false},
		{"http-path-with-host", config(httpProbe("//host/status", 8000), nil), false},
		{"http-path-with-query", config(nil, httpPostRestore("/resume?now=1", 8000)), false},
		{"http-path-with-fragment", config(httpProbe("/status#x", 8000), nil), false},
		{"port-zero", config(httpProbe("/status", 0), nil), false},
		{"port-too-high", config(nil, httpPostRestore("/resume", 65536)), false},
		{"negative-period", config(func() *snapshotv1.CheckpointReadinessProbe {
			p := fileProbe("/tmp/ready")
			p.PeriodSeconds = -1
			return p
		}(), nil), false},
		{"negative-initial-delay", config(func() *snapshotv1.CheckpointReadinessProbe {
			p := fileProbe("/tmp/ready")
			p.InitialDelaySeconds = -1
			return p
		}(), nil), false},
	} {
		t.Run("podsnapshot-"+tc.name, func(t *testing.T) {
			snap := newSettingsSnapshot(fmt.Sprintf("settings-%d", i), tc.config)
			err := cl.Create(ctx, snap)
			if !tc.valid {
				require.True(t, apierrors.IsInvalid(err), "invalid settings accepted: %v", err)
				return
			}
			require.NoError(t, err)
		})
	}

	t.Run("podsnapshot-defaults", func(t *testing.T) {
		snap := newSettingsSnapshot("defaults", config(fileProbe("/tmp/ready"), httpPostRestore("/resume", 8000)))
		require.NoError(t, cl.Create(ctx, snap))
		stored := &snapshotv1.PodSnapshot{}
		require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(snap), stored))
		probe := stored.Spec.Containers[0].CheckpointReadinessProbe
		assert.Equal(t, int32(0), probe.InitialDelaySeconds)
		assert.Equal(t, int32(1), probe.PeriodSeconds)
		assert.Equal(t, int32(1), probe.TimeoutSeconds)
		assert.Equal(t, int32(1), probe.SuccessThreshold)
		postRestore := stored.Spec.Containers[0].Lifecycle.PostRestore
		assert.Equal(t, int32(1), postRestore.PeriodSeconds)
		assert.Equal(t, int32(1), postRestore.TimeoutSeconds)
	})

	t.Run("podsnapshot-partial-defaults", func(t *testing.T) {
		probe := httpProbe("/status", 8000)
		probe.PeriodSeconds = 5
		probe.SuccessThreshold = 2
		postRestore := filePostRestore("/tmp/released")
		postRestore.TimeoutSeconds = 3
		snap := newSettingsSnapshot("partial-defaults", config(probe, postRestore))
		require.NoError(t, cl.Create(ctx, snap))
		stored := &snapshotv1.PodSnapshot{}
		require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(snap), stored))
		gotProbe := stored.Spec.Containers[0].CheckpointReadinessProbe
		assert.Equal(t, int32(5), gotProbe.PeriodSeconds, "a value the user set is kept")
		assert.Equal(t, int32(2), gotProbe.SuccessThreshold, "a value the user set is kept")
		assert.Equal(t, int32(0), gotProbe.InitialDelaySeconds)
		assert.Equal(t, int32(1), gotProbe.TimeoutSeconds)
		gotPostRestore := stored.Spec.Containers[0].Lifecycle.PostRestore
		assert.Equal(t, int32(3), gotPostRestore.TimeoutSeconds, "a value the user set is kept")
		assert.Equal(t, int32(1), gotPostRestore.PeriodSeconds)
	})

	t.Run("podsnapshot-two-entries", func(t *testing.T) {
		snap := newSettingsSnapshot("two-entries", snapshotv1.ContainerSnapshotConfig{Name: "main"})
		snap.Spec.Containers = append(snap.Spec.Containers, snapshotv1.ContainerSnapshotConfig{Name: "main"})
		err := cl.Create(ctx, snap)
		require.True(t, apierrors.IsInvalid(err), "two entries accepted: %v", err)
	})

	t.Run("podsnapshot-immutable", func(t *testing.T) {
		snap := newSettingsSnapshot("immutable", config(fileProbe("/tmp/ready"), nil))
		require.NoError(t, cl.Create(ctx, snap))
		snap.Spec.Containers[0].CheckpointReadinessProbe.File.Path = "/tmp/other"
		err := cl.Update(ctx, snap)
		require.True(t, apierrors.IsInvalid(err), "settings change accepted: %v", err)
	})

	for _, tc := range []struct {
		name   string
		config snapshotv1.ContainerSnapshotConfig
		valid  bool
	}{
		{"target-container", config(fileProbe("/tmp/ready"), filePostRestore("/tmp/released")), true},
		{"other-container", snapshotv1.ContainerSnapshotConfig{Name: "sidecar"}, false},
	} {
		t.Run("snapshotjob-"+tc.name, func(t *testing.T) {
			job := newSettingsJob("job-"+tc.name, tc.config)
			err := cl.Create(ctx, job)
			if !tc.valid {
				require.True(t, apierrors.IsInvalid(err), "invalid settings accepted: %v", err)
				return
			}
			require.NoError(t, err)
			stored := &snapshotv1.SnapshotJob{}
			require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(job), stored))
			assert.Equal(t, int32(1), stored.Spec.PodSnapshotTemplate.Containers[0].CheckpointReadinessProbe.PeriodSeconds)
		})
	}

	for _, tc := range []struct {
		name   string
		config snapshotv1.ContainerSnapshotConfig
		valid  bool
	}{
		{"source-container", config(fileProbe("/tmp/ready"), filePostRestore("/tmp/released")), true},
		{"other-container", snapshotv1.ContainerSnapshotConfig{Name: "sidecar"}, false},
	} {
		t.Run("content-"+tc.name, func(t *testing.T) {
			content := newStorageContent("content-"+tc.name, false, "")
			content.Spec.Containers = []snapshotv1.ContainerSnapshotConfig{tc.config}
			err := cl.Create(ctx, content)
			if !tc.valid {
				require.True(t, apierrors.IsInvalid(err), "invalid settings accepted: %v", err)
				return
			}
			require.NoError(t, err)
			changed := content.DeepCopy()
			changed.Spec.Containers = nil
			err = cl.Update(ctx, changed)
			require.True(t, apierrors.IsInvalid(err), "removing saved settings accepted: %v", err)
		})
	}
}

func config(probe *snapshotv1.CheckpointReadinessProbe, postRestore *snapshotv1.PostRestoreAction) snapshotv1.ContainerSnapshotConfig {
	c := snapshotv1.ContainerSnapshotConfig{Name: "main", CheckpointReadinessProbe: probe}
	if postRestore != nil {
		c.Lifecycle = &snapshotv1.SnapshotLifecycle{PostRestore: postRestore}
	}
	return c
}

func fileProbe(path string) *snapshotv1.CheckpointReadinessProbe {
	return &snapshotv1.CheckpointReadinessProbe{CheckpointReadinessProbeHandler: snapshotv1.CheckpointReadinessProbeHandler{
		File: &snapshotv1.ControlFileAction{Path: path},
	}}
}

func httpProbe(path string, port int32) *snapshotv1.CheckpointReadinessProbe {
	return &snapshotv1.CheckpointReadinessProbe{CheckpointReadinessProbeHandler: snapshotv1.CheckpointReadinessProbeHandler{
		HTTPGet: &snapshotv1.HTTPGetAction{Path: path, Port: port},
	}}
}

func filePostRestore(path string) *snapshotv1.PostRestoreAction {
	return &snapshotv1.PostRestoreAction{PostRestoreHandler: snapshotv1.PostRestoreHandler{
		File: &snapshotv1.ControlFileAction{Path: path},
	}}
}

func httpPostRestore(path string, port int32) *snapshotv1.PostRestoreAction {
	return &snapshotv1.PostRestoreAction{PostRestoreHandler: snapshotv1.PostRestoreHandler{
		HTTPPost: &snapshotv1.HTTPPostAction{Path: path, Port: port},
	}}
}

func newSettingsSnapshot(name string, settings snapshotv1.ContainerSnapshotConfig) *snapshotv1.PodSnapshot {
	return &snapshotv1.PodSnapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "workload", Name: name},
		Spec: snapshotv1.PodSnapshotSpec{
			Source:     snapshotv1.PodSnapshotSource{PodRef: snapshotv1.PodReference{Name: "source", Containers: []string{"main"}}},
			Containers: []snapshotv1.ContainerSnapshotConfig{settings},
		},
	}
}

func newSettingsJob(name string, settings snapshotv1.ContainerSnapshotConfig) *snapshotv1.SnapshotJob {
	return &snapshotv1.SnapshotJob{
		ObjectMeta: metav1.ObjectMeta{Namespace: "workload", Name: name},
		Spec: snapshotv1.SnapshotJobSpec{
			PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "main", Image: "workload:latest"}},
			}},
			PodSnapshotTemplate: snapshotv1.PodSnapshotTemplate{
				TargetContainers: []string{"main"},
				Containers:       []snapshotv1.ContainerSnapshotConfig{settings},
			},
		},
	}
}
