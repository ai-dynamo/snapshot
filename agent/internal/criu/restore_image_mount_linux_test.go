// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package criu

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestMountFilesImageWithOpsMountsReplacementReadOnly(t *testing.T) {
	const mountFD = 42
	checkpointPath := "/tmp/checkpoint"
	replacementPath := "/tmp/rewrite/files.img"
	targetPath := filepath.Join(checkpointPath, filesImageFilename)

	var openTreeCalled, mountSetattrCalled, moveMountCalled, closeCalled, unmountCalled int
	ops := mountFilesImageOps{
		openTree: func(dirfd int, path string, flags uint) (int, error) {
			openTreeCalled++
			if dirfd != unix.AT_FDCWD || path != replacementPath {
				t.Fatalf("open_tree(%d, %q), want AT_FDCWD and %q", dirfd, path, replacementPath)
			}
			wantFlags := uint(unix.OPEN_TREE_CLONE | unix.OPEN_TREE_CLOEXEC)
			if flags != wantFlags {
				t.Fatalf("open_tree flags = %#x, want %#x", flags, wantFlags)
			}
			return mountFD, nil
		},
		mountSetattr: func(dirfd int, path string, flags uint, attr *unix.MountAttr) error {
			mountSetattrCalled++
			if dirfd != mountFD || path != "" || flags != unix.AT_EMPTY_PATH {
				t.Fatalf("mount_setattr(%d, %q, %#x), want mount fd, empty path, AT_EMPTY_PATH", dirfd, path, flags)
			}
			wantAttrs := uint64(unix.MOUNT_ATTR_RDONLY | unix.MOUNT_ATTR_NOSUID | unix.MOUNT_ATTR_NODEV)
			if attr.Attr_set != wantAttrs {
				t.Fatalf("mount attributes = %#x, want %#x", attr.Attr_set, wantAttrs)
			}
			return nil
		},
		moveMount: func(fromDirfd int, fromPath string, toDirfd int, toPath string, flags int) error {
			moveMountCalled++
			if fromDirfd != mountFD || fromPath != "" || toDirfd != unix.AT_FDCWD || toPath != targetPath {
				t.Fatalf("move_mount source/target = (%d, %q) -> (%d, %q)", fromDirfd, fromPath, toDirfd, toPath)
			}
			if flags != unix.MOVE_MOUNT_F_EMPTY_PATH {
				t.Fatalf("move_mount flags = %#x, want %#x", flags, unix.MOVE_MOUNT_F_EMPTY_PATH)
			}
			return nil
		},
		unmount: func(path string, flags int) error {
			unmountCalled++
			if path != targetPath || flags != unix.MNT_DETACH {
				t.Fatalf("unmount(%q, %#x), want target and MNT_DETACH", path, flags)
			}
			return nil
		},
		closeFD: func(fd int) error {
			closeCalled++
			if fd != mountFD {
				t.Fatalf("close fd = %d, want %d", fd, mountFD)
			}
			return nil
		},
	}

	cleanup, err := mountFilesImageWithOps(checkpointPath, replacementPath, ops)
	if err != nil {
		t.Fatalf("mount replacement files image: %v", err)
	}
	if openTreeCalled != 1 || mountSetattrCalled != 1 || moveMountCalled != 1 || closeCalled != 1 {
		t.Fatalf(
			"mount calls = open_tree:%d mount_setattr:%d move_mount:%d close:%d, want all 1",
			openTreeCalled,
			mountSetattrCalled,
			moveMountCalled,
			closeCalled,
		)
	}
	if err := cleanup(); err != nil {
		t.Fatalf("cleanup restore image view: %v", err)
	}
	if unmountCalled != 1 {
		t.Fatalf("unmount calls = %d, want 1", unmountCalled)
	}
}

func TestMountFilesImageWithOpsClosesDetachedMountOnFailure(t *testing.T) {
	wantErr := errors.New("mount_setattr failed")
	var closeCalled, moveMountCalled int
	ops := mountFilesImageOps{
		openTree: func(int, string, uint) (int, error) { return 42, nil },
		mountSetattr: func(int, string, uint, *unix.MountAttr) error {
			return wantErr
		},
		moveMount: func(int, string, int, string, int) error {
			moveMountCalled++
			return nil
		},
		closeFD: func(int) error {
			closeCalled++
			return nil
		},
	}

	cleanup, err := mountFilesImageWithOps("/tmp/checkpoint", "/tmp/rewrite/files.img", ops)
	if !errors.Is(err, wantErr) {
		t.Fatalf("create error = %v, want %v", err, wantErr)
	}
	if cleanup != nil {
		t.Fatal("cleanup returned for unattached mount")
	}
	if closeCalled != 1 {
		t.Fatalf("close calls = %d, want 1", closeCalled)
	}
	if moveMountCalled != 0 {
		t.Fatalf("move_mount calls = %d, want 0", moveMountCalled)
	}
}

func TestMountFilesImageCleanupToleratesMountAlreadyGone(t *testing.T) {
	for _, unmountErr := range []error{unix.ENOENT, unix.EINVAL} {
		t.Run(unmountErr.Error(), func(t *testing.T) {
			ops := mountFilesImageOps{
				openTree:     func(int, string, uint) (int, error) { return 42, nil },
				mountSetattr: func(int, string, uint, *unix.MountAttr) error { return nil },
				moveMount:    func(int, string, int, string, int) error { return nil },
				unmount:      func(string, int) error { return unmountErr },
				closeFD:      func(int) error { return nil },
			}

			cleanup, err := mountFilesImageWithOps("/tmp/checkpoint", "/tmp/rewrite/files.img", ops)
			if err != nil {
				t.Fatalf("create restore image view: %v", err)
			}
			if err := cleanup(); err != nil {
				t.Fatalf("cleanup restore image view: %v", err)
			}
		})
	}
}

func TestMountFilesImageAcrossDevices(t *testing.T) {
	if os.Getenv("SNAPSHOT_PRIVILEGED_TESTS") != "1" {
		t.Skip("set SNAPSHOT_PRIVILEGED_TESTS=1 in a privileged Linux environment")
	}

	baseDir := t.TempDir()
	checkpointPath := filepath.Join(baseDir, "checkpoint")
	if err := os.Mkdir(checkpointPath, 0700); err != nil {
		t.Fatalf("create checkpoint mountpoint: %v", err)
	}
	if err := unix.Mount("tmpfs", checkpointPath, "tmpfs", unix.MS_NODEV|unix.MS_NOSUID, "size=1m"); err != nil {
		t.Fatalf("mount checkpoint tmpfs: %v", err)
	}
	defer func() {
		if err := unix.Unmount(checkpointPath, unix.MNT_DETACH); err != nil {
			t.Errorf("unmount checkpoint tmpfs: %v", err)
		}
	}()

	originalFilesImage := []byte("original files image")
	pageImage := []byte("large image remains on checkpoint storage")
	if err := os.WriteFile(filepath.Join(checkpointPath, filesImageFilename), originalFilesImage, 0600); err != nil {
		t.Fatalf("write original files image: %v", err)
	}
	pageImagePath := filepath.Join(checkpointPath, "pages-1.img")
	if err := os.WriteFile(pageImagePath, pageImage, 0600); err != nil {
		t.Fatalf("write page image: %v", err)
	}
	if err := unix.MountSetattr(
		unix.AT_FDCWD,
		checkpointPath,
		0,
		&unix.MountAttr{Attr_set: unix.MOUNT_ATTR_RDONLY},
	); err != nil {
		t.Fatalf("make checkpoint mount read-only: %v", err)
	}

	replacementPath := filepath.Join(baseDir, "replacement-files.img")
	replacementFilesImage := []byte("rewritten files image")
	if err := os.WriteFile(replacementPath, replacementFilesImage, 0600); err != nil {
		t.Fatalf("write replacement files image: %v", err)
	}

	var originalPageStat, replacementStat unix.Stat_t
	if err := unix.Stat(pageImagePath, &originalPageStat); err != nil {
		t.Fatalf("stat original page image: %v", err)
	}
	if err := unix.Stat(replacementPath, &replacementStat); err != nil {
		t.Fatalf("stat replacement files image: %v", err)
	}
	if originalPageStat.Dev == replacementStat.Dev {
		t.Fatalf("checkpoint and replacement devices are both %d, want different devices", originalPageStat.Dev)
	}

	cleanup, err := mountFilesImage(checkpointPath, replacementPath)
	if err != nil {
		t.Fatalf("mount replacement files image: %v", err)
	}
	cleaned := false
	defer func() {
		if !cleaned {
			if err := cleanup(); err != nil {
				t.Errorf("cleanup restore image view: %v", err)
			}
		}
	}()

	if err := os.Remove(replacementPath); err != nil {
		t.Fatalf("unlink replacement files image: %v", err)
	}

	gotFilesImage, err := os.ReadFile(filepath.Join(checkpointPath, filesImageFilename))
	if err != nil {
		t.Fatalf("read mounted files image: %v", err)
	}
	if !bytes.Equal(gotFilesImage, replacementFilesImage) {
		t.Fatalf("mounted files image = %q, want %q", gotFilesImage, replacementFilesImage)
	}
	gotPageImage, err := os.ReadFile(filepath.Join(checkpointPath, "pages-1.img"))
	if err != nil {
		t.Fatalf("read page image: %v", err)
	}
	if !bytes.Equal(gotPageImage, pageImage) {
		t.Fatalf("page image = %q, want %q", gotPageImage, pageImage)
	}

	var viewedFilesStat, viewedPageStat unix.Stat_t
	if err := unix.Stat(filepath.Join(checkpointPath, filesImageFilename), &viewedFilesStat); err != nil {
		t.Fatalf("stat mounted files image: %v", err)
	}
	if err := unix.Stat(filepath.Join(checkpointPath, "pages-1.img"), &viewedPageStat); err != nil {
		t.Fatalf("stat viewed page image: %v", err)
	}
	if viewedFilesStat.Dev != replacementStat.Dev || viewedFilesStat.Ino != replacementStat.Ino {
		t.Fatalf(
			"mounted files image inode = %d:%d, want replacement %d:%d",
			viewedFilesStat.Dev,
			viewedFilesStat.Ino,
			replacementStat.Dev,
			replacementStat.Ino,
		)
	}
	if viewedPageStat.Dev != originalPageStat.Dev || viewedPageStat.Ino != originalPageStat.Ino {
		t.Fatalf(
			"viewed page image inode = %d:%d, want original %d:%d",
			viewedPageStat.Dev,
			viewedPageStat.Ino,
			originalPageStat.Dev,
			originalPageStat.Ino,
		)
	}
	writableFilesImage, err := os.OpenFile(filepath.Join(checkpointPath, filesImageFilename), os.O_WRONLY, 0)
	if writableFilesImage != nil {
		_ = writableFilesImage.Close()
	}
	if !errors.Is(err, unix.EROFS) {
		t.Fatalf("open mounted files image for writing error = %v, want EROFS", err)
	}

	if err := cleanup(); err != nil {
		t.Fatalf("cleanup restore image view: %v", err)
	}
	cleaned = true
	gotOriginalFilesImage, err := os.ReadFile(filepath.Join(checkpointPath, filesImageFilename))
	if err != nil {
		t.Fatalf("read original files image after cleanup: %v", err)
	}
	if !bytes.Equal(gotOriginalFilesImage, originalFilesImage) {
		t.Fatalf("original files image after cleanup = %q, want %q", gotOriginalFilesImage, originalFilesImage)
	}
}
