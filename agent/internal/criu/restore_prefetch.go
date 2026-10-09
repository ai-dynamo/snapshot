// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package criu

import (
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sys/unix"
)

// prefetchRestoreImages overlaps NFS reads that CRIU otherwise issues serially,
// notably while reconstructing deleted files from ghost images. Ordinary reads
// populate reclaimable file cache without changing shared filesystem settings.
// CPU page payloads are streamed separately and must not enter this cache.
func prefetchRestoreImages(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	var fs unix.Statfs_t
	if err := unix.Fstatfs(int(dir.Fd()), &fs); err != nil {
		return err
	}
	if fs.Type != unix.NFS_SUPER_MAGIC {
		return nil
	}
	return prefetchImageFiles(dir)
}

func prefetchImageFiles(dir *os.File) error {
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return err
	}
	names := make(chan string, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.Type().IsRegular() && strings.HasSuffix(name, ".img") && !strings.HasPrefix(name, "pages-") {
			names <- name
		}
	}
	close(names)

	var workers errgroup.Group
	for range min(32, len(names)) {
		workers.Go(func() error {
			buf := make([]byte, 1<<20)
			for name := range names {
				if err := prefetchImageFile(dir, name, buf); err != nil {
					return fmt.Errorf("prefetch %s: %w", name, err)
				}
			}
			return nil
		})
	}
	return workers.Wait()
}

func prefetchImageFile(dir *os.File, name string, buf []byte) error {
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular image file")
	}
	for {
		_, err := f.Read(buf)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
