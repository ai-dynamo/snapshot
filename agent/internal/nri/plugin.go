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
	"slices"
	"sync"
	"time"

	"github.com/containerd/nri/pkg/api"
	"github.com/containerd/nri/pkg/stub"
	"github.com/go-logr/logr"
	"golang.org/x/mod/semver"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ktypes "k8s.io/apimachinery/pkg/types"

	"github.com/ai-dynamo/snapshot/agent/internal/nsmount"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/api/podcontract"
)

const (
	// PluginName and PluginIndex register the plugin with the runtime. The
	// index orders it among other NRI plugins on the node.
	PluginName  = "snapshot"
	PluginIndex = "50"

	// HostNamespaceEnv holds the agent's host namespace for NewPlugin.
	HostNamespaceEnv = "SNAPSHOT_NRI_HOST_NAMESPACE"

	// ProxyBinaryPath is where the agent image ships the proxy.
	ProxyBinaryPath = nsmount.SnapshotBinSrc + "/" + podcontract.RestoreProxyBinaryName

	// argsNRIVersion is the first NRI version whose runtimes apply a
	// container's args adjustment, containerd 2.2 and later. Older runtimes
	// accept it and drop it silently.
	argsNRIVersion = "v0.10.0"

	// registerTimeout bounds Start. After registering, the stub waits for
	// the runtime's Configure call without a deadline, and a runtime that
	// drops the connection in between would hold the agent there forever.
	registerTimeout = 30 * time.Second

	// lookupTimeout bounds the API read for Pods without a container map. It
	// runs inside the runtime's CreateContainer call. NRI gives that whole call
	// 2 s by default, and drops a plugin that takes longer, so the read gets
	// 1 s and copying the proxy the rest.
	lookupTimeout = time.Second
)

// vCluster syncs a Pod to the host under another name, namespace and UID, and
// NRI reports the host Pod. vCluster records the Pod's own identity in these
// annotations on the host Pod.
const (
	vclusterNamespaceAnnotation = "vcluster.loft.sh/object-namespace"
	vclusterNameAnnotation      = "vcluster.loft.sh/object-name"
	vclusterUIDAnnotation       = "vcluster.loft.sh/object-uid"
)

// DestinationResolver returns the containers a restore Pod restores into.
type DestinationResolver interface {
	RestoreDestinations(ctx context.Context, pod *corev1.Pod) ([]string, error)
}

// Plugin is the NRI plugin.
type Plugin struct {
	resolver      DestinationResolver
	log           logr.Logger
	hostNamespace string
	// openControlDir, proxyPath and socketPath are replaced in tests. An
	// empty socketPath is NRI's default.
	openControlDir func(podUID, containerName string) (*os.File, error)
	proxyPath      string
	socketPath     string
	// runtimeNRIVersion is the stub's, set in Start.
	runtimeNRIVersion func() string
	// stub and closed are set when Start registers.
	stub   stub.Stub
	closed chan struct{}
}

// NewPlugin returns a plugin that resolves destinations through resolver. If
// hostNamespace is set, the plugin acts only on Pods in that namespace on the
// node. Set it when other clusters share the nodes, such as vCluster tenants:
// the runtime calls every plugin for every Pod on the node, and restore Pods of
// another cluster would fail here. One vCluster's Pods all run in one host
// namespace.
func NewPlugin(resolver DestinationResolver, log logr.Logger, hostNamespace string) *Plugin {
	return &Plugin{
		resolver:       resolver,
		log:            log,
		hostNamespace:  hostNamespace,
		openControlDir: snapshotruntime.OpenPodControlDir,
		proxyPath:      ProxyBinaryPath,
	}
}

// Start registers with the runtime. It fails when the runtime has no NRI, or
// one too old for the restore proxy, and restores on the node then run without
// the proxy.
func (p *Plugin) Start(ctx context.Context) error {
	closed := make(chan struct{})
	var once sync.Once
	opts := []stub.Option{
		stub.WithPluginName(PluginName),
		stub.WithPluginIdx(PluginIndex),
		stub.WithOnClose(func() { once.Do(func() { close(closed) }) }),
	}
	if p.socketPath != "" {
		opts = append(opts, stub.WithSocketPath(p.socketPath))
	}
	s, err := stub.New(p, opts...)
	if err != nil {
		return fmt.Errorf("create NRI plugin: %w", err)
	}
	// Configure, called during Start, reads the runtime's NRI version.
	p.runtimeNRIVersion = s.RuntimeNRIVersion
	if err := within(registerTimeout, func() error { return s.Start(ctx) }); err != nil {
		return fmt.Errorf("NRI plugin could not register with the container runtime: %w", err)
	}
	p.stub, p.closed = s, closed
	return nil
}

// within runs fn, and returns an error if it takes longer than timeout. fn
// keeps running after that, and its result is dropped.
func within(timeout time.Duration, fn func() error) error {
	errC := make(chan error, 1)
	go func() { errC <- fn() }()
	select {
	case err := <-errC:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("no answer in %s", timeout)
	}
}

// Wait serves a started plugin until ctx ends. It returns an error if the
// connection to the runtime drops, and the agent then exits: it registers
// again when it restarts, or restores without the proxy if it cannot.
func (p *Plugin) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		p.stub.Stop()
		return nil
	case <-p.closed:
		// Stopping the stub also closes the connection, so a shutdown can
		// land here too.
		p.stub.Stop()
		if ctx.Err() != nil {
			return nil
		}
		return errors.New("NRI connection to the container runtime closed")
	}
}

// Configure is called when the plugin registers. It refuses a runtime too old
// to apply the restore proxy command, which fails the registration, as a
// runtime without NRI does.
func (p *Plugin) Configure(_ context.Context, _, runtime, version string) (api.EventMask, error) {
	nriVersion := ""
	if p.runtimeNRIVersion != nil {
		nriVersion = p.runtimeNRIVersion()
	}
	if dropsArgs(nriVersion) {
		return 0, fmt.Errorf("container runtime %s/%s has NRI %s, which ignores the restore proxy command; "+
			"the restore proxy needs NRI %s or newer (containerd 2.2 or newer)", runtime, version, nriVersion, argsNRIVersion)
	}
	p.log.Info("Registered with the container runtime", "runtime", runtime+"/"+version, "nri_version", nriVersion)
	return 0, nil
}

// dropsArgs reports whether a runtime with this NRI version drops a
// container's args adjustment. An unknown version is not reported.
func dropsArgs(nriVersion string) bool {
	return semver.IsValid(nriVersion) && semver.Compare(nriVersion, argsNRIVersion) < 0
}

// CreateContainer shapes restore destination containers and leaves every
// other container alone. An error fails the container's creation, and kubelet
// retries it: running the image's own command instead would start the
// workload from scratch in a container the agent is about to restore into.
func (p *Plugin) CreateContainer(ctx context.Context, pod *api.PodSandbox, ctr *api.Container) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	if _, ok := pod.GetAnnotations()[podcontract.RestoreFromAnnotation]; !ok {
		return nil, nil, nil
	}
	if p.hostNamespace != "" && pod.GetNamespace() != p.hostNamespace {
		return nil, nil, nil
	}
	namespace, name, uid := podIdentity(pod)
	log := p.log.WithValues("pod", namespace+"/"+name, "container", ctr.GetName())

	lookupCtx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	destinations, err := p.resolver.RestoreDestinations(lookupCtx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:        name,
		Namespace:   namespace,
		UID:         ktypes.UID(uid),
		Annotations: pod.GetAnnotations(),
	}})
	if err != nil {
		log.Error(err, "Cannot resolve restore destinations")
		return nil, nil, fmt.Errorf("snapshot: resolve restore destinations: %w", err)
	}
	if !slices.Contains(destinations, ctr.GetName()) {
		return nil, nil, nil
	}

	// kubelet's directories use the UID of the Pod on the node.
	dir, err := p.openControlDir(pod.GetUid(), ctr.GetName())
	if err == nil {
		err = installProxy(p.proxyPath, dir)
		_ = dir.Close()
	}
	if err != nil {
		log.Error(err, "Cannot install restore proxy")
		return nil, nil, fmt.Errorf("snapshot: install restore proxy: %w", err)
	}

	adjust := &api.ContainerAdjustment{}
	adjust.SetArgs([]string{podcontract.RestoreProxyPath})
	log.Info("Restore proxy is PID 1", "control_dir", dir.Name())
	return adjust, nil, nil
}

// podIdentity returns the Pod's namespace, name and UID in its own cluster.
func podIdentity(pod *api.PodSandbox) (namespace, name, uid string) {
	a := pod.GetAnnotations()
	namespace, name, uid = a[vclusterNamespaceAnnotation], a[vclusterNameAnnotation], a[vclusterUIDAnnotation]
	if namespace == "" || name == "" || uid == "" {
		return pod.GetNamespace(), pod.GetName(), pod.GetUid()
	}
	return namespace, name, uid
}

// installProxy copies the proxy binary into dir. It copies on every creation:
// the emptyDir is per Pod, and the binary is a few MB.
func installProxy(src string, dir *os.File) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	return snapshotruntime.WriteFileInDir(dir, podcontract.RestoreProxyBinaryName, 0o755, func(w io.Writer) error {
		_, err := io.Copy(w, in)
		return err
	})
}
