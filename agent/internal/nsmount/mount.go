// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nsmount

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/sys/unix"
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
	binaryPath string
	nsFd       *os.File
	unmountCmd string
	createdDst bool
	log        logr.Logger
	once       sync.Once
	unmountErr error
}

func (h *execMountRef) NsFd() *os.File { return h.nsFd }

func (h *execMountRef) Unmount(ctx context.Context) error {
	h.once.Do(func() {
		defer h.nsFd.Close()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), unmountTimeout)
		defer cancel()
		args := []string{h.unmountCmd, strconv.Itoa(nsFdChildNum)}
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
	return m.mount(ctx, nsFd, "mount-bundle-fd", "unmount-bundle-fd")
}

func (m *execMounter) MountCheckpoint(ctx context.Context, nsFd *os.File, checkpointPath string) (mountRef, error) {
	if nsFd == nil {
		return nil, fmt.Errorf("mount namespace fd is required")
	}
	dupFd, err := unix.Dup(int(nsFd.Fd()))
	if err != nil {
		return nil, fmt.Errorf("duplicate mount namespace fd: %w", err)
	}
	unix.CloseOnExec(dupFd)
	return m.mount(ctx, os.NewFile(uintptr(dupFd), nsFd.Name()), "mount-checkpoint-fd", "unmount-checkpoint-fd", checkpointPath)
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
	return m.mount(ctx, os.NewFile(uintptr(dupFd), nsFd.Name()), "mount-pagebroker-fd", "unmount-pagebroker-fd", stagingPath)
}

func (m *execMounter) mount(ctx context.Context, nsFd *os.File, mountCmd, unmountCmd string, args ...string) (mountRef, error) {
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
