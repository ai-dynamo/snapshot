// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nri

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/containerd/nri/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ktypes "k8s.io/apimachinery/pkg/types"

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

// binding is what the operator's claim writes on a group target.
func binding(mapping string) map[string]string {
	return map[string]string{
		podcontract.RestoreGroupUIDAnnotation:     "restore-uid",
		podcontract.RestoreContainerMapAnnotation: mapping,
	}
}

func TestCreateContainerMakesTheProxyPID1OfABoundTargetsDestination(t *testing.T) {
	restorer := &fakeRestorer{}
	p, root := testPlugin(t, restorer)
	sandbox := restoreSandbox(binding("main=engine"))

	adjust, updates, err := p.CreateContainer(context.Background(), sandbox, &api.Container{Id: "engine-id", Name: "engine"})
	require.NoError(t, err)
	assert.Nil(t, updates)
	require.NotNil(t, adjust)
	assert.Equal(t, []string{podcontract.RestoreProxyPath}, adjust.GetArgs())
	assert.FileExists(t, filepath.Join(root, "pod-uid", "engine", podcontract.RestoreProxyBinaryName))
	assert.Zero(t, restorer.calls, "a bound target's destination needs no API read")

	adjust, _, err = p.CreateContainer(context.Background(), sandbox, &api.Container{Id: "sidecar-id", Name: "sidecar"})
	require.NoError(t, err)
	assert.Nil(t, adjust, "a bound target's other containers keep their command")
	assert.NoFileExists(t, filepath.Join(root, "pod-uid", "sidecar", podcontract.RestoreProxyBinaryName))

	require.NoError(t, p.PostStartContainer(context.Background(), sandbox, &api.Container{Id: "engine-id", Name: "engine"}))
	assert.Equal(t, []started{{
		pod: ktypes.NamespacedName{Namespace: "inference", Name: "restore-worker"}, uid: "pod-uid",
		container: "engine", containerID: "engine-id",
	}}, restorer.started, "the restore starts once the prepared container runs")
}

func TestCreateContainerRefusesAnInvalidBinding(t *testing.T) {
	for name, annotations := range map[string]map[string]string{
		"no container map": {podcontract.RestoreGroupUIDAnnotation: "restore-uid"},
		"two pairs":        binding("main=engine-0,main=engine-1"),
		"an empty binding": {podcontract.RestoreGroupUIDAnnotation: "", podcontract.RestoreContainerMapAnnotation: "main=engine"},
		"also a standalone Pod": func() map[string]string {
			a := binding("main=engine")
			a[podcontract.RestoreFromAnnotation] = "snap"
			return a
		}(),
		"a malformed map": binding("engine"),
	} {
		t.Run(name, func(t *testing.T) {
			restorer := &fakeRestorer{destinations: []string{"engine"}}
			p, root := testPlugin(t, restorer)

			adjust, _, err := p.CreateContainer(context.Background(), restoreSandbox(annotations), &api.Container{Name: "engine"})
			require.ErrorContains(t, err, "invalid pod group binding", "the Pod's own command must never run")
			assert.Nil(t, adjust)
			assert.Zero(t, restorer.calls)
			_, statErr := os.Stat(filepath.Join(root, "pod-uid"))
			assert.ErrorIs(t, statErr, os.ErrNotExist)
		})
	}
}

func TestCreateContainerKeepsTheStandalonePathWithoutABinding(t *testing.T) {
	restorer := &fakeRestorer{destinations: []string{"main"}}
	p, _ := testPlugin(t, restorer)
	annotations := map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"}

	adjust, _, err := p.CreateContainer(context.Background(), restoreSandbox(annotations), &api.Container{Name: "main"})
	require.NoError(t, err)
	require.NotNil(t, adjust)
	assert.Equal(t, 1, restorer.calls, "a Pod without a binding resolves its destinations as before")
}

func TestCreateContainerKeepsBoundTargetsOfAnotherHostNamespace(t *testing.T) {
	restorer := &fakeRestorer{}
	p, _ := testPlugin(t, restorer)
	p.hostNamespace = "tenant"

	adjust, _, err := p.CreateContainer(context.Background(), restoreSandbox(binding("main")), &api.Container{Name: "engine"})
	require.NoError(t, err, "a Pod of another install is not this plugin's to refuse")
	assert.Nil(t, adjust)
}
