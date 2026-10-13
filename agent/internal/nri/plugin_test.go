// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nri

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/containerd/nri/pkg/api"
	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

type fakeResolver struct {
	destinations []string
	err          error
	calls        int
	pod          *corev1.Pod
}

func (r *fakeResolver) RestoreDestinations(_ context.Context, pod *corev1.Pod) ([]string, error) {
	r.calls++
	r.pod = pod
	return r.destinations, r.err
}

// testPlugin installs into a temporary directory instead of kubelet's.
func testPlugin(t *testing.T, resolver *fakeResolver) (*Plugin, string) {
	t.Helper()
	root := t.TempDir()
	proxy := filepath.Join(root, "proxy-src")
	require.NoError(t, os.WriteFile(proxy, []byte("proxy binary"), 0o755))
	p := NewPlugin(resolver, testr.New(t), "")
	p.proxyPath = proxy
	p.openControlDir = func(podUID, containerName string) (*os.File, error) {
		dir := filepath.Join(root, podUID, containerName)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		return os.Open(dir)
	}
	return p, root
}

func restoreSandbox(annotations map[string]string) *api.PodSandbox {
	return &api.PodSandbox{Name: "restore-worker", Namespace: "inference", Uid: "pod-uid", Annotations: annotations}
}

func TestCreateContainerScope(t *testing.T) {
	restoreFrom := map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"}
	for name, tc := range map[string]struct {
		annotations  map[string]string
		container    string
		destinations []string
		wantShaped   bool
		wantLookup   bool
	}{
		"not a restore Pod":       {annotations: map[string]string{"other": "x"}, container: "main", wantLookup: false},
		"destination container":   {annotations: restoreFrom, container: "main", destinations: []string{"main"}, wantShaped: true, wantLookup: true},
		"sidecar is left alone":   {annotations: restoreFrom, container: "sidecar", destinations: []string{"main"}, wantLookup: true},
		"one of two destinations": {annotations: restoreFrom, container: "engine-1", destinations: []string{"engine-0", "engine-1"}, wantShaped: true, wantLookup: true},
	} {
		t.Run(name, func(t *testing.T) {
			resolver := &fakeResolver{destinations: tc.destinations}
			p, root := testPlugin(t, resolver)

			adjust, updates, err := p.CreateContainer(context.Background(), restoreSandbox(tc.annotations), &api.Container{Name: tc.container})

			require.NoError(t, err)
			assert.Nil(t, updates)
			assert.Equal(t, tc.wantLookup, resolver.calls > 0)
			installed := filepath.Join(root, "pod-uid", tc.container, podcontract.RestoreProxyBinaryName)
			if !tc.wantShaped {
				assert.Nil(t, adjust)
				assert.NoFileExists(t, installed)
				return
			}
			require.NotNil(t, adjust)
			assert.Equal(t, []string{"/snapshot-control/snapshot-restore-proxy"}, adjust.GetArgs())
			data, err := os.ReadFile(installed)
			require.NoError(t, err)
			assert.Equal(t, "proxy binary", string(data))
			info, err := os.Stat(installed)
			require.NoError(t, err)
			assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())
		})
	}
}

func TestCreateContainerPassesPodIdentityToResolver(t *testing.T) {
	t.Run("plain cluster", func(t *testing.T) {
		annotations := map[string]string{
			podcontract.RestoreFromAnnotation:         "snapshot-a",
			podcontract.RestoreContainerMapAnnotation: "main=engine-0",
		}
		resolver := &fakeResolver{destinations: []string{"engine-0"}}
		p, _ := testPlugin(t, resolver)

		_, _, err := p.CreateContainer(context.Background(), restoreSandbox(annotations), &api.Container{Name: "engine-0"})

		require.NoError(t, err)
		assert.Equal(t, "restore-worker", resolver.pod.Name)
		assert.Equal(t, "inference", resolver.pod.Namespace)
		assert.Equal(t, "pod-uid", string(resolver.pod.UID))
		assert.Equal(t, annotations, resolver.pod.Annotations)
	})

	t.Run("vCluster", func(t *testing.T) {
		resolver := &fakeResolver{destinations: []string{"main"}}
		p, root := testPlugin(t, resolver)
		sandbox := &api.PodSandbox{
			Name: "restore-worker-x-inference-x-tenant", Namespace: "tenant", Uid: "host-uid",
			Annotations: map[string]string{
				podcontract.RestoreFromAnnotation: "snapshot-a",
				vclusterNamespaceAnnotation:       "inference",
				vclusterNameAnnotation:            "restore-worker",
				vclusterUIDAnnotation:             "pod-uid",
			},
		}

		adjust, _, err := p.CreateContainer(context.Background(), sandbox, &api.Container{Name: "main"})

		require.NoError(t, err)
		require.NotNil(t, adjust)
		assert.Equal(t, "inference", resolver.pod.Namespace)
		assert.Equal(t, "restore-worker", resolver.pod.Name)
		assert.Equal(t, "pod-uid", string(resolver.pod.UID))
		assert.FileExists(t, filepath.Join(root, "host-uid", "main", podcontract.RestoreProxyBinaryName),
			"kubelet's directories use the host Pod's UID")
	})
}

// Tests share nodes with other clusters, and set a host namespace so the
// plugin leaves their restore Pods alone.
func TestCreateContainerHostNamespace(t *testing.T) {
	restoreFrom := map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"}
	for name, tc := range map[string]struct {
		hostNamespace string
		wantShaped    bool
	}{
		"not set":           {hostNamespace: "", wantShaped: true},
		"Pod's namespace":   {hostNamespace: "inference", wantShaped: true},
		"another namespace": {hostNamespace: "tenant", wantShaped: false},
	} {
		t.Run(name, func(t *testing.T) {
			resolver := &fakeResolver{destinations: []string{"main"}}
			p, root := testPlugin(t, resolver)
			p.hostNamespace = tc.hostNamespace

			adjust, _, err := p.CreateContainer(context.Background(), restoreSandbox(restoreFrom), &api.Container{Name: "main"})

			require.NoError(t, err)
			installed := filepath.Join(root, "pod-uid", "main", podcontract.RestoreProxyBinaryName)
			if !tc.wantShaped {
				assert.Nil(t, adjust)
				assert.Zero(t, resolver.calls, "no API reads for another cluster's Pod")
				assert.NoFileExists(t, installed)
				return
			}
			require.NotNil(t, adjust)
			assert.FileExists(t, installed)
		})
	}
}

// Failing creation makes kubelet retry. Letting the container start would run
// the image's own command in a container the agent is about to restore into.
func TestCreateContainerFailsClosed(t *testing.T) {
	restoreFrom := map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"}

	t.Run("destination lookup fails", func(t *testing.T) {
		p, _ := testPlugin(t, &fakeResolver{err: errors.New("PodSnapshot not found")})

		adjust, _, err := p.CreateContainer(context.Background(), restoreSandbox(restoreFrom), &api.Container{Name: "main"})

		require.Error(t, err)
		assert.Nil(t, adjust)
	})

	t.Run("proxy binary missing", func(t *testing.T) {
		p, _ := testPlugin(t, &fakeResolver{destinations: []string{"main"}})
		p.proxyPath = filepath.Join(t.TempDir(), "missing")

		adjust, _, err := p.CreateContainer(context.Background(), restoreSandbox(restoreFrom), &api.Container{Name: "main"})

		require.Error(t, err)
		assert.Nil(t, adjust)
	})
}

func TestInstallProxyReplacesAnEarlierCopy(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	dir := filepath.Join(root, "control", "main")
	require.NoError(t, os.WriteFile(src, []byte("new"), 0o755))
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, podcontract.RestoreProxyBinaryName), []byte("old"), 0o644))

	d, err := os.Open(dir)
	require.NoError(t, err)
	defer d.Close()

	require.NoError(t, installProxy(src, d))

	data, err := os.ReadFile(filepath.Join(dir, podcontract.RestoreProxyBinaryName))
	require.NoError(t, err)
	assert.Equal(t, "new", string(data))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "no temporary file may be left behind")
}

// Restores need NRI, so the agent stops when the runtime has none.
func TestRunFailsWithoutNRI(t *testing.T) {
	p, _ := testPlugin(t, &fakeResolver{})
	p.socketPath = filepath.Join(t.TempDir(), "nri.sock") // nothing listens here
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := p.Run(ctx)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "could not register with the container runtime")
	assert.NoError(t, ctx.Err(), "fails at once, without waiting")
}

func TestDropsArgs(t *testing.T) {
	for version, want := range map[string]bool{
		"v0.8.0":  true, // containerd 2.0 and 2.1
		"v0.9.1":  true,
		"v0.10.0": false, // containerd 2.2
		"v0.11.0": false,
		"":        false, // unknown: not reported
		"unknown": false,
	} {
		assert.Equal(t, want, dropsArgs(version), version)
	}
}

func TestConfigure(t *testing.T) {
	t.Run("a runtime that applies args keeps the default events", func(t *testing.T) {
		p, _ := testPlugin(t, &fakeResolver{})
		p.runtimeNRIVersion = func() string { return "v0.11.0" }

		events, err := p.Configure(context.Background(), "", "containerd", "v2.2.1")

		require.NoError(t, err)
		assert.Zero(t, events, "zero keeps the events the plugin implements")
	})

	// Registration fails, and the agent stops, as without NRI.
	t.Run("a runtime that drops args is refused", func(t *testing.T) {
		p, _ := testPlugin(t, &fakeResolver{})
		p.runtimeNRIVersion = func() string { return "v0.8.0" }

		_, err := p.Configure(context.Background(), "", "containerd", "v2.1.5")

		require.Error(t, err)
		assert.Contains(t, err.Error(), "containerd 2.2 or newer")
	})
}
