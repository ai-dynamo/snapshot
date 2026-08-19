// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package criu

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestMountFilesImageWithOps(t *testing.T) {
	const mountFD = 42
	target := filepath.Join("/checkpoint", filesImageFilename)
	var calls []string
	ops := mountFilesImageOps{
		openTree: func(dirfd int, path string, flags uint) (int, error) {
			if dirfd != unix.AT_FDCWD || path != "/rewrite/files.img" || flags != unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC {
				t.Fatalf("unexpected open_tree arguments: %d %q %#x", dirfd, path, flags)
			}
			calls = append(calls, "open")
			return mountFD, nil
		},
		mountSetattr: func(fd int, path string, flags uint, attr *unix.MountAttr) error {
			want := uint64(unix.MOUNT_ATTR_RDONLY | unix.MOUNT_ATTR_NOSUID | unix.MOUNT_ATTR_NODEV | unix.MOUNT_ATTR_NOEXEC)
			if fd != mountFD || path != "" || flags != unix.AT_EMPTY_PATH || attr.Attr_set != want {
				t.Fatalf("unexpected mount_setattr arguments: %d %q %#x %#x", fd, path, flags, attr.Attr_set)
			}
			calls = append(calls, "attrs")
			return nil
		},
		moveMount: func(fromFD int, fromPath string, toFD int, toPath string, flags int) error {
			if fromFD != mountFD || fromPath != "" || toFD != unix.AT_FDCWD || toPath != target || flags != unix.MOVE_MOUNT_F_EMPTY_PATH {
				t.Fatalf("unexpected move_mount arguments: %d %q %d %q %#x", fromFD, fromPath, toFD, toPath, flags)
			}
			calls = append(calls, "move")
			return nil
		},
		unmount: func(path string, flags int) error {
			if path != target || flags != unix.MNT_DETACH {
				t.Fatalf("unexpected unmount arguments: %q %#x", path, flags)
			}
			calls = append(calls, "unmount")
			return nil
		},
		closeFD: func(fd int) error {
			if fd != mountFD {
				t.Fatalf("close fd = %d, want %d", fd, mountFD)
			}
			calls = append(calls, "close")
			return nil
		},
	}

	cleanup, err := mountFilesImageWithOps("/checkpoint", "/rewrite/files.img", ops)
	if err != nil {
		t.Fatal(err)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	want := []string{"open", "attrs", "move", "close", "unmount"}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("calls = %v, want %v", calls, want)
		}
	}
}

func TestMountFilesImageClosesDetachedMountOnFailure(t *testing.T) {
	wantErr := errors.New("mount failed")
	for _, failMove := range []bool{false, true} {
		t.Run(map[bool]string{false: "attributes", true: "move"}[failMove], func(t *testing.T) {
			closed := 0
			ops := mountFilesImageOps{
				openTree: func(int, string, uint) (int, error) { return 42, nil },
				mountSetattr: func(int, string, uint, *unix.MountAttr) error {
					if !failMove {
						return wantErr
					}
					return nil
				},
				moveMount: func(int, string, int, string, int) error { return wantErr },
				closeFD:   func(int) error { closed++; return nil },
			}
			cleanup, err := mountFilesImageWithOps("/checkpoint", "/rewrite/files.img", ops)
			if !errors.Is(err, wantErr) || cleanup != nil || closed != 1 {
				t.Fatalf("has cleanup=%t error=%v closed=%d", cleanup != nil, err, closed)
			}
		})
	}
}

func TestMountFilesImageCleanupErrors(t *testing.T) {
	for _, tc := range []struct {
		err     error
		wantErr bool
	}{{unix.ENOENT, false}, {unix.EINVAL, false}, {unix.EPERM, true}} {
		ops := mountFilesImageOps{
			openTree:     func(int, string, uint) (int, error) { return 42, nil },
			mountSetattr: func(int, string, uint, *unix.MountAttr) error { return nil },
			moveMount:    func(int, string, int, string, int) error { return nil },
			unmount:      func(string, int) error { return tc.err },
			closeFD:      func(int) error { return nil },
		}
		cleanup, err := mountFilesImageWithOps("/checkpoint", "/rewrite/files.img", ops)
		if err != nil {
			t.Fatal(err)
		}
		if err := cleanup(); (err != nil) != tc.wantErr {
			t.Fatalf("cleanup error = %v, wantErr %t", err, tc.wantErr)
		}
	}
}

func TestMountFilesImageAcrossDevices(t *testing.T) {
	if os.Getenv("SNAPSHOT_PRIVILEGED_TESTS") != "1" {
		t.Skip("set SNAPSHOT_PRIVILEGED_TESTS=1 in a privileged Linux environment")
	}

	root := t.TempDir()
	checkpoint := filepath.Join(root, "checkpoint")
	if err := os.Mkdir(checkpoint, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mount("tmpfs", checkpoint, "tmpfs", unix.MS_NODEV|unix.MS_NOSUID, "size=1m"); err != nil {
		t.Fatal(err)
	}
	defer unix.Unmount(checkpoint, unix.MNT_DETACH) //nolint:errcheck

	original := filepath.Join(checkpoint, filesImageFilename)
	pages := filepath.Join(checkpoint, "pages-1.img")
	replacement := filepath.Join(root, "replacement.img")
	for path, data := range map[string]string{original: "original", pages: "pages", replacement: "rewritten"} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pageStat, replacementStat := statFile(t, pages), statFile(t, replacement)
	if pageStat.Dev == replacementStat.Dev {
		t.Fatalf("test requires different devices, both are %d", pageStat.Dev)
	}
	if err := unix.MountSetattr(unix.AT_FDCWD, checkpoint, 0, &unix.MountAttr{Attr_set: unix.MOUNT_ATTR_RDONLY}); err != nil {
		t.Fatal(err)
	}

	cleanup, err := mountFilesImage(checkpoint, replacement)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup() //nolint:errcheck
	if err := os.Remove(replacement); err != nil {
		t.Fatal(err)
	}
	viewedFiles, viewedPages := statFile(t, original), statFile(t, pages)
	if viewedFiles.Dev != replacementStat.Dev || viewedFiles.Ino != replacementStat.Ino {
		t.Fatalf("files.img inode = %d:%d, want %d:%d", viewedFiles.Dev, viewedFiles.Ino, replacementStat.Dev, replacementStat.Ino)
	}
	if viewedPages.Dev != pageStat.Dev || viewedPages.Ino != pageStat.Ino {
		t.Fatalf("pages inode changed from %d:%d to %d:%d", pageStat.Dev, pageStat.Ino, viewedPages.Dev, viewedPages.Ino)
	}
	if _, err := os.OpenFile(original, os.O_WRONLY, 0); !errors.Is(err, unix.EROFS) {
		t.Fatalf("write error = %v, want EROFS", err)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(original)
	if err != nil || string(data) != "original" {
		t.Fatalf("original files.img after cleanup = %q, %v", data, err)
	}
}

func statFile(t *testing.T, path string) unix.Stat_t {
	t.Helper()
	var stat unix.Stat_t
	if err := unix.Stat(path, &stat); err != nil {
		t.Fatal(err)
	}
	return stat
}
