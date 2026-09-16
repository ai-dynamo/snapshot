// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package runtime provides low-level host and container-runtime primitives for snapshot execution.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	securejoin "github.com/cyphar/filepath-securejoin"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	internalapi "k8s.io/cri-api/pkg/apis"
	remote "k8s.io/cri-client/pkg"
)

// Default socket paths and runtime-type identifiers.
const (
	ContainerdSocket = "/run/containerd/containerd.sock"
	CRIOSocket       = "/var/run/crio/crio.sock"

	RuntimeContainerd = "containerd"
	RuntimeCRIO       = "crio"

	criConnectTimeout = 2 * time.Second
	criCallTimeout    = 10 * time.Second
)

// Runtime abstracts the container-identity APIs behind a two-backend switch.
// Resolve methods return non-nil *specs.Spec with PID > 0 on success, or an error.
type Runtime interface {
	ResolveContainer(ctx context.Context, id string) (int, *specs.Spec, error)
	ResolveContainerIDByPod(ctx context.Context, pod, ns, ctr string) (string, error)
	ResolveContainerByPod(ctx context.Context, pod, ns, ctr string) (int, *specs.Spec, error)
	ResolveContainerImageID(ctx context.Context, id string) (string, error)
	// TerminateContainer requests zero-grace termination through CRI using the
	// runtime container ID. Implementations validate any supplied runtime scheme
	// before dispatch.
	TerminateContainer(ctx context.Context, id string) error
	Close() error
}

// criSchemes are the ContainerStatus.ContainerID prefixes kubelet emits across
// runtimes. Prefixes don't overlap, so stripping is order-independent.
var criSchemes = []string{"containerd://", "cri-o://", "crio://"}

// StripCRIScheme trims the kubelet-format scheme prefix from a
// ContainerStatus.ContainerID. Returns id unchanged if no known scheme matches.
func StripCRIScheme(id string) string {
	for _, scheme := range criSchemes {
		if s, ok := strings.CutPrefix(id, scheme); ok {
			return s
		}
	}
	return id
}

// containerIDForRuntime validates the kubelet-format runtime scheme before a
// destructive operation. Bare CRI IDs remain valid for internal callers, but a
// recognized scheme for another backend and unknown scheme-like prefixes fail
// closed instead of being retargeted to the active runtime.
func containerIDForRuntime(id string, allowedSchemes ...string) (string, error) {
	if id == "" {
		return "", errors.New("container ID is empty")
	}
	for _, scheme := range allowedSchemes {
		if stripped, ok := strings.CutPrefix(id, scheme); ok {
			if stripped == "" {
				return "", fmt.Errorf("container ID after %q is empty", scheme)
			}
			if strings.Contains(stripped, "://") {
				return "", errors.New("container ID contains a nested runtime scheme")
			}
			return stripped, nil
		}
	}
	for _, scheme := range criSchemes {
		if strings.HasPrefix(id, scheme) {
			return "", fmt.Errorf("container ID uses mismatched runtime scheme %q", scheme)
		}
	}
	if strings.Contains(id, "://") {
		return "", errors.New("container ID uses an unknown runtime scheme")
	}
	return id, nil
}

// newRemoteRuntimeService bounds connection establishment separately from the
// timeout that cri-client stores and reapplies to runtime RPCs.
func newRemoteRuntimeService(socket string) (internalapi.RuntimeService, error) {
	ctx, cancel := context.WithTimeout(context.Background(), criConnectTimeout)
	defer cancel()
	// The context and useStreaming arguments were added in cri-client v0.36.2
	// (CVE pin); re-check this call when Dynamo updates that dependency.
	return remote.NewRemoteRuntimeService(ctx, socket, criCallTimeout, nil, false)
}

// stopContainerIfPresent is the shared desired-state operation for retrying
// recovery and finalizer callers. A missing container already satisfies the
// postcondition; every other CRI error remains actionable.
func stopContainerIfPresent(ctx context.Context, service internalapi.RuntimeService, id string) error {
	ctx, cancel := context.WithTimeout(ctx, criCallTimeout)
	defer cancel()

	err := service.StopContainer(ctx, id, 0)
	if status.Code(err) == codes.NotFound {
		return nil
	}
	return err
}

// defaultSocketFor returns the conventional socket path for a runtime type.
// Returns empty for unknown types; New validates before calling.
func defaultSocketFor(runtimeType string) string {
	switch runtimeType {
	case RuntimeCRIO:
		return CRIOSocket
	case RuntimeContainerd:
		return ContainerdSocket
	default:
		return ""
	}
}

// New constructs a Runtime backend for the given type and socket. Pass an
// empty socket to use the per-type default.
func New(runtimeType, socket string) (Runtime, error) {
	if socket == "" {
		socket = defaultSocketFor(runtimeType)
	}
	switch runtimeType {
	case RuntimeContainerd:
		return NewContainerdRuntime(socket)
	case RuntimeCRIO:
		return NewCRIORuntime(socket)
	default:
		return nil, fmt.Errorf("unsupported runtime %q (expected %q or %q)", runtimeType, RuntimeContainerd, RuntimeCRIO)
	}
}

// collectOCIManagedPaths returns the set of paths the OCI runtime considers
// "managed": mount destinations, masked paths, and readonly paths, normalized
// relative to the container rootfs.
func collectOCIManagedPaths(ociSpec *specs.Spec, rootFS string) map[string]struct{} {
	set := map[string]struct{}{}
	if ociSpec == nil {
		return set
	}

	paths := make([]string, 0, len(ociSpec.Mounts))
	for _, mount := range ociSpec.Mounts {
		paths = append(paths, mount.Destination)
	}
	if ociSpec.Linux != nil {
		paths = append(paths, ociSpec.Linux.MaskedPaths...)
		paths = append(paths, ociSpec.Linux.ReadonlyPaths...)
	}
	for _, raw := range paths {
		if p := normalizeOCIPath(raw, rootFS); p != "" {
			set[p] = struct{}{}
		}
	}
	return set
}

// normalizeOCIPath resolves an OCI spec path relative to rootFS, following
// symlinks within the rootfs boundary (matching runc's addCriuDumpMount pattern).
// Example: "/run/secrets" → "/var/run/secrets" when the container's /run symlinks to /var/run.
func normalizeOCIPath(raw, rootFS string) string {
	p := filepath.Clean(strings.TrimSpace(raw))
	if p == "" || p == "." {
		return ""
	}
	if rootFS == "" {
		return p
	}
	// On SecureJoin error (e.g. /proc/<pid> races away), fall back to the
	// cleaned logical path — matching naively is safer than dropping the
	// entry, since under-classification can make CRIU skip a real mount.
	if resolved, err := securejoin.SecureJoin(rootFS, p); err == nil {
		p = strings.TrimPrefix(resolved, filepath.Clean(rootFS))
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}
