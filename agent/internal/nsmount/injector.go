// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package nsmount bind-mounts directories into a foreign process's mount
// namespace via the ns-bind-mount C helper (cmd/ns-bind-mount).
package nsmount

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/go-logr/logr"
	"golang.org/x/sys/unix"
)

const (
	// SnapshotBinSrc is the agent-side directory containing the binary bundle.
	SnapshotBinSrc = "/snapshot-binaries"
	// SnapshotBinDst is the mount destination inside the placeholder namespace.
	SnapshotBinDst = "/tmp/snapshot-binaries"
	// CheckpointDst is the mount destination for checkpoint data inside the
	// placeholder namespace.
	CheckpointDst = "/tmp/checkpoint"
)

// MountPoint represents an active bind-mount of a directory inside a foreign
// namespace. The caller must call Unmount when done.
type MountPoint interface {
	// Path returns an in-namespace path below the mount point. name must be a
	// single path element with no separators or dot-dot components.
	Path(name string) (string, error)

	// Unmount removes the bind-mount from the target namespace.
	// Idempotent — safe to call multiple times.
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
	options MountOptions
}

// probeKernelMountAPI verifies that mount_setattr (Linux 5.12) is available.
func probeKernelMountAPI() error {
	err := unix.MountSetattr(-1, "", 0, nil)
	if errors.Is(err, syscall.ENOSYS) {
		return fmt.Errorf("ns-bind-mount requires Linux 5.12+ (mount_setattr): kernel does not support it")
	}
	return nil
}

// New returns an NSMounter backed by the ns-bind-mount binary at its default
// location.
func New(log logr.Logger) (*NSMounter, error) {
	if err := probeKernelMountAPI(); err != nil {
		return nil, err
	}
	m, err := newExecMounter(defaultBinaryPath, log)
	if err != nil {
		return nil, err
	}
	return newWithMounter(m, log), nil
}

func newWithMounter(m mounter, log logr.Logger) *NSMounter {
	return &NSMounter{
		mounter: m,
		log:     log,
		options: MountOptions{ReadOnly: true},
	}
}

// WithNoExec returns a mounter that shares the same helper but prevents
// execution from its mounted tree. The receiver remains executable.
func (nsm *NSMounter) WithNoExec() *NSMounter {
	clone := *nsm
	clone.options.NoExec = true
	return &clone
}

// Mount exposes src read-only at dst inside pid's mount namespace.
func (nsm *NSMounter) Mount(ctx context.Context, pid int, src, dst string) (MountPoint, error) {
	nsm.log.Info("mounting into placeholder namespace", "pid", pid, "src", src, "dst", dst)

	ref, err := nsm.mounter.Mount(ctx, pid, src, dst, nsm.options)
	if err != nil {
		return nil, err
	}

	nsm.log.Info("mounted into placeholder namespace", "pid", pid, "dst", ref.TargetPath())
	return &mountPoint{mount: ref}, nil
}

type mountPoint struct {
	mount mountRef
}

func (h *mountPoint) Path(name string) (string, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, os.PathSeparator) {
		return "", fmt.Errorf("nsmount: invalid path element %q", name)
	}
	return filepath.Join(h.mount.TargetPath(), name), nil
}

func (h *mountPoint) Unmount(ctx context.Context) error {
	return h.mount.Unmount(ctx)
}

func (h *mountPoint) NsFd() *os.File {
	return h.mount.NsFd()
}
