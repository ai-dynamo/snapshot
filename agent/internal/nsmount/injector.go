// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package nsmount bind-mounts directories into a foreign process's mount
// namespace via the ns-bind-mount C helper (cmd/ns-bind-mount).
package nsmount

import (
	"context"
	"fmt"
	"os"

	"github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/go-logr/logr"
)

const (
	// SnapshotBinSrc is the agent-side directory containing the binary bundle.
	SnapshotBinSrc = "/snapshot-binaries"
	// SnapshotBinDst is the mount destination inside the placeholder namespace.
	SnapshotBinDst = "/tmp/snapshot-binaries"
	// CheckpointSrc is the fixed agent-side checkpoint mount.
	CheckpointSrc = types.CheckpointBasePath
	// CheckpointDst is the mount destination for checkpoint data inside the
	// placeholder namespace.
	CheckpointDst = "/tmp/checkpoint"
	// PageBrokerRestoreSrc is the agent-side PageBroker restore staging root.
	PageBrokerRestoreSrc = "/pagebroker/staging/restore"
	// PageBrokerDst is the mount destination for PageBroker staging inside the placeholder namespace.
	PageBrokerDst = "/tmp/pagebroker"
)

// MountPoint represents an active bind-mount of a directory inside a foreign
// namespace. The caller must call Unmount when done.
type MountPoint interface {
	// Unmount removes the bind-mount from the target namespace.
	// It is idempotent and bounds the supplied context with an internal timeout.
	Unmount(ctx context.Context) error

	// NsFd returns the pinned mount-namespace fd opened at Mount time.
	// Valid until Unmount is called. Test mocks may return nil.
	NsFd() *os.File
}

// NSMounter installs the binary bundle and a selected checkpoint artifact at
// their fixed destinations in a placeholder container's mount namespace.
type NSMounter struct {
	mounter mounter
	log     logr.Logger
}

// New returns an NSMounter backed by the ns-bind-mount binary at its default
// location.
func New(log logr.Logger) *NSMounter {
	return newWithMounter(newExecMounter(defaultBinaryPath, log), log)
}

func newWithMounter(m mounter, log logr.Logger) *NSMounter {
	return &NSMounter{
		mounter: m,
		log:     log,
	}
}

// MountBundle exposes the agent binary bundle read-only and executable.
func (nsm *NSMounter) MountBundle(ctx context.Context, pid int) (MountPoint, error) {
	nsm.log.Info("mounting bundle into placeholder namespace", "pid", pid)
	ref, err := nsm.mounter.MountBundle(ctx, pid)
	if err != nil {
		return nil, err
	}
	return &mountPoint{mount: ref}, nil
}

// MountArtifact exposes one validated checkpoint artifact read-only and
// non-executable in the namespace pinned by namespaceMount.
func (nsm *NSMounter) MountArtifact(ctx context.Context, namespaceMount MountPoint, src string) (MountPoint, error) {
	if err := validateWithin(CheckpointSrc, src); err != nil {
		return nil, err
	}
	if namespaceMount == nil || namespaceMount.NsFd() == nil {
		return nil, fmt.Errorf("mount artifact: pinned mount namespace is required")
	}
	nsm.log.Info("mounting checkpoint into placeholder namespace", "src", src)
	ref, err := nsm.mounter.MountCheckpoint(ctx, namespaceMount.NsFd(), src)
	if err != nil {
		return nil, err
	}
	return &mountPoint{mount: ref}, nil
}

// MountPageBroker exposes a staged PageBroker restore using the namespace
// already pinned for the agent bundle.
func (nsm *NSMounter) MountPageBroker(ctx context.Context, namespaceMount MountPoint, src string) (MountPoint, error) {
	if err := validateWithin(PageBrokerRestoreSrc, src); err != nil {
		return nil, err
	}
	if namespaceMount == nil || namespaceMount.NsFd() == nil {
		return nil, fmt.Errorf("mount PageBroker: pinned mount namespace is required")
	}
	ref, err := nsm.mounter.MountPageBroker(ctx, namespaceMount.NsFd(), src)
	if err != nil {
		return nil, err
	}
	return &mountPoint{mount: ref}, nil
}

type mountPoint struct {
	mount mountRef
}

func (h *mountPoint) Unmount(ctx context.Context) error {
	return h.mount.Unmount(ctx)
}

func (h *mountPoint) NsFd() *os.File {
	return h.mount.NsFd()
}
