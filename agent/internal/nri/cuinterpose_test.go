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

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

var (
	controlMount  = &api.Mount{Destination: podcontract.SnapshotControlMountPath, Type: "bind", Source: "/var/lib/kubelet/pods/pod-uid/volume-subpaths/snapshot-control/main/0"}
	deliveryMount = &api.Mount{Destination: podcontract.CuInterposeMountPath, Type: "bind", Source: "/var/lib/kubelet/pods/pod-uid/volumes/kubernetes.io~empty-dir/snapshot-cuda"}
)

func captureSandbox(annotations map[string]string) *api.PodSandbox {
	return &api.PodSandbox{Name: "worker", Namespace: "inference", Uid: "pod-uid", Annotations: annotations}
}

func optedIn() map[string]string {
	return map[string]string{podcontract.CuInterposeAnnotation: "enabled"}
}

func TestCreateContainerDeliversCuInterpose(t *testing.T) {
	restorer := &fakeRestorer{}
	p, root := testPlugin(t, restorer)
	// The runtime resolves the image's entrypoint into args, so the plugin
	// wraps whatever process the container would have started.
	ctr := &api.Container{
		Name:   "main",
		Args:   []string{"/entrypoint.sh", "serve", "--port", "8000"},
		Env:    []string{"MODEL=llama"},
		Mounts: []*api.Mount{controlMount},
	}

	adjust, updates, err := p.CreateContainer(context.Background(), captureSandbox(optedIn()), ctr)

	require.NoError(t, err)
	assert.Nil(t, updates)
	assert.Zero(t, restorer.calls, "a capture Pod is not a restore Pod")
	require.NotNil(t, adjust)
	assert.Equal(t, append(podcontract.CuInterposeLauncherPrefix(), "/entrypoint.sh", "serve", "--port", "8000"), adjust.GetArgs())
	assert.Empty(t, adjust.GetEnv(), "the environment reaches the launcher unchanged")

	require.Len(t, adjust.GetMounts(), 1)
	mount := adjust.GetMounts()[0]
	assert.Equal(t, podcontract.CuInterposeMountPath, mount.GetDestination())
	assert.Equal(t, "bind", mount.GetType())
	assert.Equal(t, "/pod-uid/main/snapshot-cuda", mount.GetSource())
	assert.Contains(t, mount.GetOptions(), "ro")
	assert.NotContains(t, mount.GetOptions(), "noexec", "the launcher runs from the mount")

	delivered := filepath.Join(root, "pod-uid", "main", "snapshot-cuda")
	for name, want := range map[string]struct {
		data string
		perm os.FileMode
	}{
		"libcuinterpose.so":      {"frontend", 0o644},
		"libcuinterpose_core.so": {"core", 0o644},
		"cuinterpose-launch":     {"launcher", 0o755},
	} {
		data, err := os.ReadFile(filepath.Join(delivered, name))
		require.NoError(t, err, name)
		assert.Equal(t, want.data, string(data), name)
		info, err := os.Stat(filepath.Join(delivered, name))
		require.NoError(t, err, name)
		assert.Equal(t, want.perm, info.Mode().Perm(), name)
	}
}

func TestCreateContainerCuInterposeScope(t *testing.T) {
	launched := append(podcontract.CuInterposeLauncherPrefix(), "worker")
	for name, tc := range map[string]struct {
		annotations map[string]string
		ctr         *api.Container
	}{
		"not opted in": {
			annotations: map[string]string{"other": "x"},
			ctr:         &api.Container{Name: "main", Args: []string{"worker"}, Mounts: []*api.Mount{controlMount}},
		},
		"disabled": {
			annotations: map[string]string{podcontract.CuInterposeAnnotation: "disabled"},
			ctr:         &api.Container{Name: "main", Args: []string{"worker"}, Mounts: []*api.Mount{controlMount}},
		},
		"invalid annotation leaves the Pod to start": {
			annotations: map[string]string{podcontract.CuInterposeAnnotation: "yes"},
			ctr:         &api.Container{Name: "main", Args: []string{"worker"}, Mounts: []*api.Mount{controlMount}},
		},
		"helper without the control mount": {
			annotations: optedIn(),
			ctr:         &api.Container{Name: "saver", Args: []string{"saver"}},
		},
		"already launched": {
			annotations: optedIn(),
			ctr:         &api.Container{Name: "main", Args: launched, Mounts: []*api.Mount{controlMount, deliveryMount}},
		},
		"delivers its own libraries": {
			annotations: optedIn(),
			ctr:         &api.Container{Name: "main", Args: []string{"worker"}, Mounts: []*api.Mount{controlMount, deliveryMount}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			p, root := testPlugin(t, &fakeRestorer{})

			adjust, _, err := p.CreateContainer(context.Background(), captureSandbox(tc.annotations), tc.ctr)

			require.NoError(t, err)
			assert.Nil(t, adjust)
			assert.NoDirExists(t, filepath.Join(root, "pod-uid", tc.ctr.GetName(), "snapshot-cuda"))
		})
	}
}

func TestCreateContainerRestorePodIsNotShapedForCapture(t *testing.T) {
	annotations := optedIn()
	annotations[podcontract.RestoreFromAnnotation] = "snapshot-a"
	restorer := &fakeRestorer{destinations: []string{"main"}}
	p, root := testPlugin(t, restorer)

	adjust, _, err := p.CreateContainer(context.Background(), captureSandbox(annotations),
		&api.Container{Name: "main", Args: []string{"worker"}, Mounts: []*api.Mount{controlMount}})

	require.NoError(t, err)
	require.NotNil(t, adjust)
	assert.Equal(t, []string{podcontract.RestoreProxyPath}, adjust.GetArgs())
	assert.Empty(t, adjust.GetMounts())
	assert.NoDirExists(t, filepath.Join(root, "pod-uid", "main", "snapshot-cuda"))
}

func TestCreateContainerCuInterposeOutsideHostNamespace(t *testing.T) {
	p, _ := testPlugin(t, &fakeRestorer{})
	p.hostNamespace = "tenant"

	adjust, _, err := p.CreateContainer(context.Background(), captureSandbox(optedIn()),
		&api.Container{Name: "main", Args: []string{"worker"}, Mounts: []*api.Mount{controlMount}})

	require.NoError(t, err)
	assert.Nil(t, adjust)
}

func TestCreateContainerCuInterposeFailures(t *testing.T) {
	t.Run("missing payload fails creation", func(t *testing.T) {
		p, _ := testPlugin(t, &fakeRestorer{})
		p.cuInterposeLauncher = filepath.Join(t.TempDir(), "missing")

		_, _, err := p.CreateContainer(context.Background(), captureSandbox(optedIn()),
			&api.Container{Name: "main", Args: []string{"worker"}, Mounts: []*api.Mount{controlMount}})

		require.ErrorContains(t, err, "install multi-GPU support")
	})

	t.Run("no process", func(t *testing.T) {
		p, _ := testPlugin(t, &fakeRestorer{})

		_, _, err := p.CreateContainer(context.Background(), captureSandbox(optedIn()),
			&api.Container{Name: "main", Mounts: []*api.Mount{controlMount}})

		require.ErrorContains(t, err, "no process")
	})

	t.Run("control directory outside the node root", func(t *testing.T) {
		p, _ := testPlugin(t, &fakeRestorer{})
		p.nodeRoot = "/elsewhere"

		_, _, err := p.CreateContainer(context.Background(), captureSandbox(optedIn()),
			&api.Container{Name: "main", Args: []string{"worker"}, Mounts: []*api.Mount{controlMount}})

		require.ErrorContains(t, err, "outside the node root")
	})

	t.Run("symlinked delivery directory is refused", func(t *testing.T) {
		p, root := testPlugin(t, &fakeRestorer{})
		control := filepath.Join(root, "pod-uid", "main")
		require.NoError(t, os.MkdirAll(control, 0o755))
		require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(control, "snapshot-cuda")))

		_, _, err := p.CreateContainer(context.Background(), captureSandbox(optedIn()),
			&api.Container{Name: "main", Args: []string{"worker"}, Mounts: []*api.Mount{controlMount}})

		require.ErrorContains(t, err, "install multi-GPU support")
	})
}
