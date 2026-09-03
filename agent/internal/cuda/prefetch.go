// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cuda

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"golang.org/x/sys/unix"
)

const customStoragePrefetchBufferBytes = 8 * 1024 * 1024

var customStorageExtentFilePattern = regexp.MustCompile(`^device-[0-9]{4}\.bin(?:\.part-[0-9]{4})?$`)

// CustomStoragePrefetchResult describes a completed best-effort page-cache
// preload. Duration is service time and may overlap CRIU restore.
type CustomStoragePrefetchResult struct {
	Files    int
	Bytes    int64
	Duration time.Duration
}

// PrefetchCustomStorageArtifacts validates and reads every CUDA CustomStorage
// extent into the node page cache. Snapshot starts this before CRIU restore so
// durable storage I/O overlaps process restore; the CUDA helper still performs
// the authoritative read into registered host buffers after target PIDs exist.
func PrefetchCustomStorageArtifacts(ctx context.Context, checkpointDir string) (CustomStoragePrefetchResult, error) {
	start := time.Now()
	root := filepath.Join(checkpointDir, "cuda-custom-storage")
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return CustomStoragePrefetchResult{}, fmt.Errorf("inspect CUDA CustomStorage artifact directory: %w", err)
	}
	if !rootInfo.IsDir() {
		return CustomStoragePrefetchResult{}, fmt.Errorf("CUDA CustomStorage artifact path is not a directory")
	}

	var paths []string
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if customStorageExtentFilePattern.MatchString(entry.Name()) {
				return fmt.Errorf("CUDA CustomStorage extent %s is a symlink", path)
			}
			return nil
		}
		if entry.IsDir() || !customStorageExtentFilePattern.MatchString(entry.Name()) {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("CUDA CustomStorage extent %s is not a regular file", path)
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		return CustomStoragePrefetchResult{}, fmt.Errorf("discover CUDA CustomStorage extents: %w", err)
	}
	if len(paths) == 0 {
		return CustomStoragePrefetchResult{}, fmt.Errorf("CUDA CustomStorage artifact contains no extent files")
	}
	sort.Strings(paths)

	buffer := make([]byte, customStoragePrefetchBufferBytes)
	result := CustomStoragePrefetchResult{Files: len(paths)}
	for _, path := range paths {
		bytesRead, err := prefetchCustomStorageFile(ctx, path, buffer)
		if err != nil {
			return CustomStoragePrefetchResult{}, err
		}
		result.Bytes += bytesRead
	}
	result.Duration = time.Since(start)
	return result, nil
}

func prefetchCustomStorageFile(ctx context.Context, path string, buffer []byte) (int64, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return 0, fmt.Errorf("open CUDA CustomStorage extent %s: %w", path, err)
	}
	defer unix.Close(fd)

	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return 0, fmt.Errorf("stat CUDA CustomStorage extent %s: %w", path, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Size <= 0 {
		return 0, fmt.Errorf("CUDA CustomStorage extent %s is not a nonempty regular file", path)
	}
	_ = unix.Fadvise(fd, 0, stat.Size, unix.FADV_WILLNEED)

	// FADV_WILLNEED is only an asynchronous hint and may return before the
	// extent reaches the page cache. Reading the complete file makes this
	// best-effort prefetch observable and ensures the later CUDA restore can
	// consume cached pages when the filesystem honors normal buffered I/O.
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return 0, fmt.Errorf("prefetch CUDA CustomStorage extent %s: %w", path, err)
		}
		read, err := unix.Read(fd, buffer)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("read CUDA CustomStorage extent %s: %w", path, err)
		}
		total += int64(read)
		if read == 0 {
			break
		}
	}
	if total != stat.Size {
		return 0, fmt.Errorf("CUDA CustomStorage extent %s changed size while prefetching: read %d, expected %d", path, total, stat.Size)
	}
	return total, nil
}
