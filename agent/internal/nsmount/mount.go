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
)

const (
	binaryName        = "ns-bind-mount"
	defaultBinaryPath = "/usr/local/sbin/" + binaryName
	nsMntNsPathFmt    = "/proc/%d/ns/mnt"
	nsFdChildNum      = 3
	unmountTimeout    = 10 * time.Second
)

type MountOptions struct {
	ReadOnly bool
	NoExec   bool
}

type mountRef interface {
	Unmount(ctx context.Context) error
	NsFd() *os.File
}

type mounter interface {
	Mount(ctx context.Context, pid int, src, dst string, opts MountOptions) (mountRef, error)
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
	dst        string
	createdDst bool
	log        logr.Logger
	once       sync.Once
	unmountErr error
}

func (h *execMountRef) NsFd() *os.File { return h.nsFd }

func (h *execMountRef) Unmount(ctx context.Context) error {
	h.once.Do(func() {
		defer h.nsFd.Close()
		ctx, cancel := context.WithTimeout(ctx, unmountTimeout)
		defer cancel()
		args := []string{"umount-fd", strconv.Itoa(nsFdChildNum), h.dst}
		if h.createdDst {
			args = append(args, "created")
		}
		cmd := exec.CommandContext(ctx, h.binaryPath, args...)
		cmd.ExtraFiles = []*os.File{h.nsFd}
		out, err := cmd.CombinedOutput()
		if err != nil {
			h.log.Error(err, "failed to unmount from namespace", "dst", h.dst, "output", strings.TrimSpace(string(out)))
			h.unmountErr = fmt.Errorf("ns-bind-mount umount-fd %s: %w\noutput: %s", h.dst, err, strings.TrimSpace(string(out)))
			return
		}
		h.log.Info("unmounted from namespace", "dst", h.dst)
	})
	return h.unmountErr
}

func (m *execMounter) Mount(ctx context.Context, pid int, src, dst string, opts MountOptions) (mountRef, error) {
	nsFdPath := fmt.Sprintf(nsMntNsPathFmt, pid)
	nsFd, err := os.Open(nsFdPath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", nsFdPath, err)
	}

	args := []string{"mount-fd", strconv.Itoa(nsFdChildNum), src, dst}
	if opts.ReadOnly {
		args = append(args, "ro")
	}
	if opts.NoExec {
		args = append(args, "noexec")
	}
	cmd := exec.CommandContext(ctx, m.binaryPath, args...)
	cmd.ExtraFiles = []*os.File{nsFd}
	var stdout strings.Builder
	var stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		nsFd.Close()
		return nil, fmt.Errorf("ns-bind-mount mount-fd %s -> %s: %w\noutput: %s", src, dst, err, strings.TrimSpace(stderr.String()))
	}
	m.log.Info("mounted into namespace", "src", src, "dst", dst, "readonly", opts.ReadOnly, "noexec", opts.NoExec, "pid", pid)

	return &execMountRef{
		binaryPath: m.binaryPath,
		nsFd:       nsFd,
		dst:        dst,
		// mount-fd emits created_dst=1 after it attaches the mount. Preserve
		// that contract so umount-fd removes only directories the helper made.
		createdDst: strings.Contains(stdout.String(), "created_dst=1"),
		log:        m.log,
	}, nil
}
