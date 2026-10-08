// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package nri makes snapshot-restore-proxy PID 1 in restore destination
// containers. It is an NRI plugin: on container creation it copies the proxy
// into the container's control directory and replaces the container's command
// with it, before the container starts. The Pod object is not changed.
package nri

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/containerd/nri/pkg/api"
	"github.com/containerd/nri/pkg/stub"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ktypes "k8s.io/apimachinery/pkg/types"

	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/api/podcontract"
)

const (
	// PluginName and PluginIndex register the plugin with the runtime. The
	// index orders it among other NRI plugins on the node.
	PluginName  = "snapshot"
	PluginIndex = "50"

	// ProxyBinaryPath is where the agent image ships the proxy.
	ProxyBinaryPath = "/snapshot-binaries/" + podcontract.RestoreProxyBinaryName

	// lookupTimeout bounds the API read for Pods without a container map. It
	// runs inside the runtime's CreateContainer call, which NRI also bounds.
	lookupTimeout = time.Second
)

// proxyCommand is the proxy as the container sees it: the control directory
// is mounted at SnapshotControlMountPath.
var proxyCommand = []string{path.Join(podcontract.SnapshotControlMountPath, podcontract.RestoreProxyBinaryName)}

// DestinationResolver returns the containers a restore Pod restores into.
type DestinationResolver interface {
	RestoreDestinations(ctx context.Context, pod *corev1.Pod) ([]string, error)
}

// Plugin is the NRI plugin.
type Plugin struct {
	resolver DestinationResolver
	log      logr.Logger
	// controlDir and proxyPath are replaced in tests.
	controlDir func(podUID, containerName string) (string, error)
	proxyPath  string
}

// NewPlugin returns a plugin that resolves destinations through resolver.
func NewPlugin(resolver DestinationResolver, log logr.Logger) *Plugin {
	return &Plugin{
		resolver:   resolver,
		log:        log,
		controlDir: snapshotruntime.PodControlDir,
		proxyPath:  ProxyBinaryPath,
	}
}

// Run registers with the runtime and serves until ctx ends. It returns an
// error if registration fails or the connection to the runtime drops: a node
// that cannot shape restore containers must not look healthy.
func (p *Plugin) Run(ctx context.Context) error {
	closed := make(chan struct{})
	var once sync.Once
	s, err := stub.New(p,
		stub.WithPluginName(PluginName),
		stub.WithPluginIdx(PluginIndex),
		stub.WithOnClose(func() { once.Do(func() { close(closed) }) }),
	)
	if err != nil {
		return fmt.Errorf("create NRI plugin: %w", err)
	}
	ran := make(chan error, 1)
	go func() { ran <- s.Run(ctx) }()
	select {
	case <-closed:
		s.Stop()
		return errors.New("NRI connection to the container runtime closed")
	case err := <-ran:
		if ctx.Err() != nil {
			return nil
		}
		if err == nil {
			err = errors.New("stopped")
		}
		return fmt.Errorf("NRI plugin: %w", err)
	}
}

// CreateContainer shapes restore destination containers and leaves every
// other container alone. An error fails the container's creation, and kubelet
// retries it: running the image's own command instead would start the
// workload from scratch in a container the agent is about to restore into.
func (p *Plugin) CreateContainer(ctx context.Context, pod *api.PodSandbox, ctr *api.Container) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	if _, ok := pod.GetAnnotations()[podcontract.RestoreFromAnnotation]; !ok {
		return nil, nil, nil
	}
	log := p.log.WithValues("pod", pod.GetNamespace()+"/"+pod.GetName(), "container", ctr.GetName())

	lookupCtx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	destinations, err := p.resolver.RestoreDestinations(lookupCtx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:        pod.GetName(),
		Namespace:   pod.GetNamespace(),
		UID:         ktypes.UID(pod.GetUid()),
		Annotations: pod.GetAnnotations(),
	}})
	if err != nil {
		log.Error(err, "Cannot resolve restore destinations")
		return nil, nil, fmt.Errorf("snapshot: resolve restore destinations: %w", err)
	}
	if !slices.Contains(destinations, ctr.GetName()) {
		return nil, nil, nil
	}

	dir, err := p.controlDir(pod.GetUid(), ctr.GetName())
	if err == nil {
		err = installProxy(p.proxyPath, dir)
	}
	if err != nil {
		log.Error(err, "Cannot install restore proxy")
		return nil, nil, fmt.Errorf("snapshot: install restore proxy: %w", err)
	}

	adjust := &api.ContainerAdjustment{}
	adjust.SetArgs(proxyCommand)
	log.Info("Restore proxy is PID 1", "control_dir", dir)
	return adjust, nil, nil
}

// installProxy copies the proxy binary into dir. It writes a temporary file
// and renames it, so the container never sees a partial binary. It copies on
// every creation: the emptyDir is per Pod, and the binary is a few MB.
func installProxy(src, dir string) (retErr error) {
	// kubelet normally creates the subPath directory before CreateContainer.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp, err := os.CreateTemp(dir, "."+podcontract.RestoreProxyBinaryName+".*")
	if err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, podcontract.RestoreProxyBinaryName))
}
