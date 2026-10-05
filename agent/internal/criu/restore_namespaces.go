// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package criu

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/checkpoint-restore/go-criu/v8/crit"
	"github.com/checkpoint-restore/go-criu/v8/crit/images/pstree"
	"golang.org/x/sys/unix"

	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

// RestoreNamespaceFiles pins destination namespaces. ExecuteRestore borrows
// these files; the helper owns them through CRIU restore and CUDA unlock.
// Nil files preserve restoration of existing non-external-PID checkpoints.
type RestoreNamespaceFiles struct {
	PID   *os.File
	Mount *os.File
}

const externalPIDNamespaceKey = "extPodPidNs"

var externalPIDNamespacePattern = regexp.MustCompile(`^pid\[[1-9][0-9]*\]:extPodPidNs$`)

func hasExternalPIDNamespace(m *types.CheckpointManifest) (bool, error) {
	found := false
	for _, external := range m.CRIUDump.External {
		if !strings.HasPrefix(external, "pid[") {
			continue
		}
		if found || !externalPIDNamespacePattern.MatchString(external) {
			return false, fmt.Errorf("unsupported external PID namespace declaration %q", external)
		}
		found = true
	}
	return found, nil
}

// Do not allow an external PID-1 image to reach the destructive restore phases.
// The destination placeholder already owns PID 1; replacing it is unfinished.
func validateExternalPIDNamespace(m *types.CheckpointManifest, checkpointPath string) error {
	external, err := hasExternalPIDNamespace(m)
	if err != nil || !external {
		return err
	}
	file, err := os.Open(filepath.Join(checkpointPath, "pstree.img"))
	if err != nil {
		return fmt.Errorf("open external PID namespace process tree: %w", err)
	}
	defer file.Close()
	image, err := crit.New(file, nil, "", false, false).Decode(&pstree.PstreeEntry{})
	if err != nil {
		return fmt.Errorf("decode external PID namespace process tree: %w", err)
	}
	if len(image.Entries) == 0 {
		return fmt.Errorf("external PID namespace process tree is empty")
	}
	for _, entry := range image.Entries {
		if entry.Message.(*pstree.PstreeEntry).GetPid() == 1 {
			return fmt.Errorf("direct restore of captured PID 1 is unsupported: destination placeholder occupies PID 1")
		}
	}
	return nil
}

type inheritFDRegistrar interface{ AddInheritFd(string, *os.File) }

// Return an owned duplicate for ExecuteRestore's existing cleanup paths. The
// borrowed helper descriptor remains valid for dependent CUDA operations.
func registerExternalPIDNamespace(c inheritFDRegistrar, m *types.CheckpointManifest, destination *os.File) (*os.File, error) {
	external, err := hasExternalPIDNamespace(m)
	if err != nil || !external {
		return nil, err
	}
	if destination == nil {
		return nil, fmt.Errorf("external PID namespace requires a destination descriptor")
	}
	fd, err := unix.FcntlInt(destination.Fd(), unix.F_DUPFD_CLOEXEC, 3)
	if err != nil {
		return nil, fmt.Errorf("duplicate destination PID namespace: %w", err)
	}
	file := os.NewFile(uintptr(fd), "destination-pid-namespace")
	c.AddInheritFd(externalPIDNamespaceKey, file)
	return file, nil
}

func prepareRestoreImageDirInNamespace(checkpointPath, scratchDir string, mount *os.File) (string, func() error, error) {
	if mount == nil {
		return prepareRestoreImageDir(checkpointPath, scratchDir)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(mount.Fd()), &stat); err != nil {
		return "", nil, fmt.Errorf("stat destination mount namespace: %w", err)
	}
	return prepareRestoreImageDirForRestoreID(checkpointPath, stat.Ino, scratchDir)
}
