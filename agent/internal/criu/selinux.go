// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package criu

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/go-logr/logr"
	"golang.org/x/sys/unix"
)

const (
	// SELinuxFSPath is where libselinux looks for selinuxfs.
	SELinuxFSPath = "/sys/fs/selinux"
	// SELinuxConfigDir holds the config file libselinux requires before it
	// reports SELinux as enabled.
	SELinuxConfigDir = "/etc/selinux"

	selinuxConfigFilename = "config"
	selfLabelPath         = "/proc/self/attr/current"
)

// RunsUnderSELinux reports whether this process has an SELinux label, which
// tells a container that its node runs SELinux even when selinuxfs is hidden.
func RunsUnderSELinux() bool {
	label, err := os.ReadFile(selfLabelPath)
	if err != nil {
		return false
	}
	return IsSELinuxLabel(strings.TrimRight(string(label), "\x00\n"))
}

// IsSELinuxLabel applies CRIU's own test: an SELinux label has at least three
// colons, as in system_u:system_r:container_t:s0:c1,c2. An AppArmor label such
// as "unconfined" or "cri-containerd.apparmor.d (enforce)" has none.
func IsSELinuxLabel(label string) bool {
	return strings.Count(label, ":") >= 3
}

// ExposeSELinux lets CRIU see SELinux from inside a container. CRIU detects it
// through libselinux, which needs selinuxfs mounted and a config file in
// configDir, and a container usually has neither: CRI-O covers
// /sys/fs/selinux with an empty tmpfs, and few images carry the config.
// Without them CRIU records no labels at dump and refuses an SELinux
// lsm_profile at restore. The returned func removes what ExposeSELinux
// mounted or created, and is safe to call more than once.
func ExposeSELinux(fsPath, configDir string, log logr.Logger) (func() error, error) {
	unmountFS, err := mountSELinuxFS(fsPath, log)
	if err != nil {
		return nil, err
	}
	hideConfig, err := provideSELinuxConfig(configDir, log)
	if err != nil {
		return nil, errors.Join(err, unmountFS())
	}
	var once sync.Once
	var undoErr error
	return func() error {
		once.Do(func() { undoErr = errors.Join(hideConfig(), unmountFS()) })
		return undoErr
	}, nil
}

// mountSELinuxFS mounts selinuxfs read-only at path unless it is already there.
func mountSELinuxFS(path string, log logr.Logger) (func() error, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if st.Type == unix.SELINUX_MAGIC {
		return func() error { return nil }, nil
	}
	if err := unix.Mount("selinuxfs", path, "selinuxfs", unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NOEXEC, ""); err != nil {
		return nil, fmt.Errorf("mount selinuxfs at %s: %w", path, err)
	}
	log.V(1).Info("Mounted selinuxfs for CRIU", "path", path)
	return func() error { return detach(path) }, nil
}

// provideSELinuxConfig mounts a tmpfs holding an empty config over dir when dir
// has no config, creating dir first if it is missing. libselinux only checks
// that the file exists.
func provideSELinuxConfig(dir string, log logr.Logger) (func() error, error) {
	config := filepath.Join(dir, selinuxConfigFilename)
	if _, err := os.Stat(config); err == nil {
		return func() error { return nil }, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("stat %s: %w", config, err)
	}
	created, err := ensureDir(dir)
	if err != nil {
		return nil, err
	}
	removeDir := func() error { return nil }
	if created {
		removeDir = func() error { return os.Remove(dir) }
	}
	if err := unix.Mount("tmpfs", dir, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "mode=0755,size=4k"); err != nil {
		return nil, errors.Join(fmt.Errorf("mount tmpfs at %s: %w", dir, err), removeDir())
	}
	undo := func() error {
		if err := detach(dir); err != nil {
			return err
		}
		return removeDir()
	}
	if err := os.WriteFile(config, nil, 0o644); err != nil {
		return nil, errors.Join(fmt.Errorf("write %s: %w", config, err), undo())
	}
	log.V(1).Info("Mounted an SELinux config for CRIU", "path", config)
	return undo, nil
}

// ensureDir creates dir and reports whether it did. An existing dir must be a
// real directory: the restore target's image could make it a symlink.
func ensureDir(dir string) (bool, error) {
	if err := os.Mkdir(dir, 0o755); err == nil {
		return true, nil
	} else if !errors.Is(err, fs.ErrExist) {
		return false, fmt.Errorf("create %s: %w", dir, err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return false, fmt.Errorf("stat %s: %w", dir, err)
	}
	if !info.IsDir() {
		return false, fmt.Errorf("%s is not a directory", dir)
	}
	return false, nil
}

func detach(path string) error {
	if err := unix.Unmount(path, unix.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount %s: %w", path, err)
	}
	return nil
}
