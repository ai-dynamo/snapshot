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

// NSMounter mounts a caller-selected source directory at one of the helper's
// fixed destinations in a placeholder container's mount namespace.
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
	return nsm.mount(ctx, pid, SnapshotBinSrc, SnapshotBinDst, MountOptions{ReadOnly: true})
}

// MountArtifact exposes one validated checkpoint artifact read-only and
// non-executable.
func (nsm *NSMounter) MountArtifact(ctx context.Context, pid int, src string) (MountPoint, error) {
	return nsm.mount(ctx, pid, src, CheckpointDst, MountOptions{ReadOnly: true, NoExec: true})
}

func (nsm *NSMounter) mount(ctx context.Context, pid int, src, dst string, options MountOptions) (MountPoint, error) {
	if err := validateMountSource(src, dst); err != nil {
		return nil, err
	}
	nsm.log.Info("mounting into placeholder namespace", "pid", pid, "src", src, "dst", dst)

	ref, err := nsm.mounter.Mount(ctx, pid, src, dst, options)
	if err != nil {
		return nil, err
	}

	nsm.log.Info("mounted into placeholder namespace", "pid", pid, "dst", dst)
	return &mountPoint{mount: ref}, nil
}

func validateMountSource(src, dst string) error {
	var root string
	switch dst {
	case SnapshotBinDst:
		root = SnapshotBinSrc
	case CheckpointDst:
		root = CheckpointSrc
	default:
		return fmt.Errorf("unsupported mount destination %q", dst)
	}
	return validateWithin(root, src)
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
