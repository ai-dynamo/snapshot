// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nsmount

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/sys/unix"

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

const (
	binaryName        = "ns-bind-mount"
	defaultBinaryPath = "/usr/local/sbin/" + binaryName
	nsMntNsPathFmt    = "/proc/%d/ns/mnt"
	nsFdChildNum      = 3
	unmountTimeout    = 10 * time.Second
)

type mountRef interface {
	Unmount(ctx context.Context) error
	NsFd() *os.File
}

type mounter interface {
	MountBundle(ctx context.Context, pid int) (mountRef, error)
	MountCuInterpose(ctx context.Context, nsFd *os.File) (mountRef, error)
	MountCheckpoint(ctx context.Context, nsFd *os.File, checkpointPath string) (mountRef, error)
	MountPageBroker(ctx context.Context, nsFd *os.File, stagingPath string) (mountRef, error)
}

type execMounter struct {
	binaryPath string
	log        logr.Logger
}

func newExecMounter(path string, log logr.Logger) *execMounter {
	return &execMounter{binaryPath: path, log: log}
}

type execMountRef struct {
	binaryPath      string
	nsFd            *os.File
	unmountCmd      string
	destinationLeaf string
	createdDst      bool
	log             logr.Logger
	once            sync.Once
	unmountErr      error
}

func (h *execMountRef) NsFd() *os.File { return h.nsFd }

func (h *execMountRef) Unmount(ctx context.Context) error {
	h.once.Do(func() {
		defer h.nsFd.Close()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), unmountTimeout)
		defer cancel()
		args := []string{h.unmountCmd, strconv.Itoa(nsFdChildNum)}
		if h.destinationLeaf != "" {
			args = append(args, h.destinationLeaf)
		}
		if h.createdDst {
			args = append(args, "created")
		}
		cmd := exec.CommandContext(ctx, h.binaryPath, args...)
		cmd.ExtraFiles = []*os.File{h.nsFd}
		out, err := cmd.CombinedOutput()
		if err != nil {
			h.log.Error(err, "failed to unmount from namespace", "command", h.unmountCmd, "output", strings.TrimSpace(string(out)))
			h.unmountErr = fmt.Errorf("ns-bind-mount %s: %w\noutput: %s", h.unmountCmd, err, strings.TrimSpace(string(out)))
			return
		}
		h.log.Info("unmounted from namespace", "command", h.unmountCmd)
	})
	return h.unmountErr
}

func (m *execMounter) MountBundle(ctx context.Context, pid int) (mountRef, error) {
	nsFdPath := fmt.Sprintf(nsMntNsPathFmt, pid)
	nsFd, err := os.Open(nsFdPath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", nsFdPath, err)
	}
	ref, err := m.mount(ctx, nsFd, "mount-bundle-fd", "unmount-bundle-fd")
	if err != nil {
		return nil, err
	}
	return ref, nil
}

func (m *execMounter) MountCheckpoint(ctx context.Context, nsFd *os.File, checkpointPath string) (mountRef, error) {
	ref, err := m.mountInNamespace(ctx, nsFd, "mount-checkpoint-fd", "unmount-checkpoint-fd", checkpointPath)
	if err != nil {
		return nil, err
	}
	return ref, nil
}

func (m *execMounter) MountCuInterpose(ctx context.Context, nsFd *os.File) (mountRef, error) {
	leaf, err := cuInterposeDestinationLeaf(podcontract.CuInterposeMountPath)
	if err != nil {
		return nil, err
	}
	ref, err := m.mountInNamespace(ctx, nsFd, "mount-snapshot-cuda-fd", "unmount-snapshot-cuda-fd", leaf)
	if err != nil {
		return nil, err
	}
	// Retain the actual destination so cleanup cannot select a different mount.
	ref.destinationLeaf = leaf
	return ref, nil
}

// The pod contract owns the destination. The native helper permits only one
// directory under /tmp, retaining its fixed source and mount attributes.
func cuInterposeDestinationLeaf(destination string) (string, error) {
	leaf := path.Base(destination)
	if path.Dir(destination) != "/tmp" || destination != "/tmp/"+leaf || leaf == "." || leaf == ".." {
		return "", fmt.Errorf("cuinterpose destination must be a single directory under /tmp: %q", destination)
	}
	for _, char := range leaf {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '-' || char == '.') {
			return "", fmt.Errorf("cuinterpose destination has an unsupported directory name: %q", destination)
		}
	}
	return leaf, nil
}

func (m *execMounter) mountInNamespace(
	ctx context.Context,
	nsFd *os.File,
	mountCmd string,
	unmountCmd string,
	args ...string,
) (*execMountRef, error) {
	if nsFd == nil {
		return nil, fmt.Errorf("mount namespace fd is required")
	}
	dupFd, err := unix.Dup(int(nsFd.Fd()))
	if err != nil {
		return nil, fmt.Errorf("duplicate mount namespace fd: %w", err)
	}
	unix.CloseOnExec(dupFd)
	return m.mount(ctx, os.NewFile(uintptr(dupFd), nsFd.Name()), mountCmd, unmountCmd, args...)
}

func (m *execMounter) MountPageBroker(ctx context.Context, nsFd *os.File, stagingPath string) (mountRef, error) {
	if nsFd == nil {
		return nil, fmt.Errorf("mount namespace fd is required")
	}
	dupFd, err := unix.Dup(int(nsFd.Fd()))
	if err != nil {
		return nil, fmt.Errorf("duplicate mount namespace fd: %w", err)
	}
	unix.CloseOnExec(dupFd)
	ref, err := m.mount(ctx, os.NewFile(uintptr(dupFd), nsFd.Name()), "mount-pagebroker-fd", "unmount-pagebroker-fd", stagingPath)
	if err != nil {
		return nil, err
	}
	return ref, nil
}

func (m *execMounter) mount(ctx context.Context, nsFd *os.File, mountCmd, unmountCmd string, args ...string) (*execMountRef, error) {
	commandArgs := []string{mountCmd, strconv.Itoa(nsFdChildNum)}
	commandArgs = append(commandArgs, args...)
	cmd := exec.CommandContext(ctx, m.binaryPath, commandArgs...)
	cmd.ExtraFiles = []*os.File{nsFd}
	var stdout strings.Builder
	var stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		nsFd.Close()
		return nil, fmt.Errorf("ns-bind-mount %s: %w\noutput: %s", mountCmd, err, strings.TrimSpace(stderr.String()))
	}
	m.log.Info("mounted into namespace", "command", mountCmd)

	return &execMountRef{
		binaryPath: m.binaryPath,
		nsFd:       nsFd,
		unmountCmd: unmountCmd,
		// The role command emits created_dst=1 after it attaches the mount.
		// Preserve that contract so unmount removes only helper-created dirs.
		createdDst: strings.Contains(stdout.String(), "created_dst=1"),
		log:        m.log,
	}, nil
}
