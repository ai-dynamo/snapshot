// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package criu

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/checkpoint-restore/go-criu/v8/crit"
	criu_core "github.com/checkpoint-restore/go-criu/v8/crit/images/criu-core"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sys/unix"
)

// LSMProfileNone is CRIU's lsm_profile for a restore target without a
// security label: CRIU then applies no label at all. Without it, CRIU would
// apply the labels it recorded at dump time.
const LSMProfileNone = "none:"

const (
	coreImagePattern  = "core-*.img"
	coreImageMagic    = "CORE"
	kdatCacheDir      = "run"
	kdatCacheFilename = "criu.kdat"

	coreImageReaders = 32
)

// RecordedLSMLabels reports whether a dump recorded a security label for every
// process with credentials. CRIU applies a restore target's label only to
// processes it recorded a label for, and records none when it could not see
// the node's LSM at dump time. Capture records the answer in the manifest, so
// a restore needs no image reads to refuse an unlabeled checkpoint.
func RecordedLSMLabels(checkpointDir string) (bool, error) {
	paths, err := filepath.Glob(filepath.Join(checkpointDir, coreImagePattern))
	if err != nil {
		return false, fmt.Errorf("list CRIU core images: %w", err)
	}
	// A checkpoint has one core image per thread, often thousands. They are
	// small, so reading them one at a time from network storage is bound by
	// round trips: 1,799 images took 1.7 s serially and 0.1 s 32 at a time.
	var withLabel, withoutLabel atomic.Int64
	var g errgroup.Group
	g.SetLimit(coreImageReaders)
	for _, path := range paths {
		g.Go(func() error {
			labeled, unlabeled, err := countRecordedLSMLabels(path)
			withLabel.Add(int64(labeled))
			withoutLabel.Add(int64(unlabeled))
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return false, err
	}
	return withLabel.Load() > 0 && withoutLabel.Load() == 0, nil
}

// countRecordedLSMLabels counts the credentials in one core image with and
// without a recorded security label. Entries without credentials, as for a
// zombie, count as neither: CRIU restores no label for them.
func countRecordedLSMLabels(path string) (withLabel, withoutLabel int, err error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return 0, 0, fmt.Errorf("open %s: %w", filepath.Base(path), err)
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return 0, 0, fmt.Errorf("stat %s: %w", filepath.Base(path), err)
	}
	if !info.Mode().IsRegular() {
		return 0, 0, fmt.Errorf("%s is not a regular file", filepath.Base(path))
	}
	image, err := crit.New(f, nil, "", false, false).Decode(&criu_core.CoreEntry{})
	if err != nil {
		return 0, 0, fmt.Errorf("decode %s: %w", filepath.Base(path), err)
	}
	if image.Magic != coreImageMagic {
		return 0, 0, fmt.Errorf("%s is a %s image, not a core image", filepath.Base(path), image.Magic)
	}
	for _, entry := range image.Entries {
		core, ok := entry.Message.(*criu_core.CoreEntry)
		if !ok {
			return 0, 0, fmt.Errorf("unexpected %s entry type %T", filepath.Base(path), entry.Message)
		}
		creds := core.GetThreadCore().GetCreds()
		if creds == nil {
			continue
		}
		if creds.GetLsmProfile() == "" {
			withoutLabel++
		} else {
			withLabel++
		}
	}
	return withLabel, withoutLabel, nil
}

// RemoveKdatCache deletes CRIU's cache of kernel facts from the restore
// target, so CRIU detects the kernel's features itself. Call it after the
// rootfs diff is applied. /run resolves inside root, and the file is unlinked
// without following a symlink.
func RemoveKdatCache(root string) error {
	rootFD, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", root, err)
	}
	defer unix.Close(rootFD)
	runFD, err := unix.Openat2(rootFD, kdatCacheDir, &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_MAGICLINKS,
	})
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open /%s: %w", kdatCacheDir, err)
	}
	defer unix.Close(runFD)
	if err := unix.Unlinkat(runFD, kdatCacheFilename, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("remove /%s/%s: %w", kdatCacheDir, kdatCacheFilename, err)
	}
	return nil
}
