// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package criu

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/checkpoint-restore/go-criu/v8/crit"
	"github.com/checkpoint-restore/go-criu/v8/crit/images/mnt"
	criurpc "github.com/checkpoint-restore/go-criu/v8/rpc"
	"github.com/go-logr/logr"
	"github.com/moby/sys/mountinfo"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
)

// Select against the actual restore namespace, after GPU aliases are prepared.
// Mount-v2 cannot copy an external master from a private mount. Kernel feature
// probes do not detect this mismatch, and CRIU does not fall back after that
// operation fails. Override only this restore's options, never the artifact.
func selectRestoreMountEngine(opts *criurpc.CriuOpts, checkpointPath string, log logr.Logger) error {
	if opts.GetMntnsCompatMode() {
		return nil
	}
	mounts, err := readCheckpointMounts(checkpointPath)
	if err != nil {
		return err
	}
	shared := make(map[uint32]bool)
	for _, mount := range mounts {
		if id := mount.GetSharedId(); id != 0 {
			shared[id] = true
		}
	}
	external := make(map[string]string)
	for _, mount := range opts.ExtMnt {
		external[mount.GetKey()] = mount.GetVal()
	}
	var liveMounts []*mountinfo.Info
	for _, mount := range mounts {
		if master := mount.GetMasterId(); master == 0 || shared[master] {
			continue
		}
		key := mount.GetExtKey()
		if mount.GetExtMount() { // Older CRIU images stored the external key in root.
			key = mount.GetRoot()
		}
		source := external[key]
		if source == "" {
			// CRIU can also derive an external master from the container root.
			if mount.GetMountpoint() != "/" || key != "" {
				continue
			}
			source = opts.GetRoot()
		}
		if source == "" {
			continue
		}
		var st unix.Statx_t
		if err := unix.Statx(unix.AT_FDCWD, source, 0, unix.STATX_MNT_ID, &st); err != nil {
			if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) {
				return nil // CRIU handles feature fallback on older kernels.
			}
			return fmt.Errorf("inspect external mount %s: %w", source, err)
		}
		if st.Mask&unix.STATX_MNT_ID == 0 {
			// Old kernels without mount IDs also lack mount-v2's newer APIs;
			// leave kernel feature fallback to CRIU.
			continue
		}
		if liveMounts == nil {
			f, err := os.Open("/proc/thread-self/mountinfo")
			if err != nil {
				return err
			}
			liveMounts, err = mountinfo.GetMountsFromReader(f, nil)
			closeErr := f.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
		}
		found := false
		for _, live := range liveMounts {
			if uint64(live.ID) != st.Mnt_id {
				continue
			}
			found = true
			canSupplyMaster := false
			for _, field := range strings.Fields(live.Optional) {
				canSupplyMaster = canSupplyMaster || strings.HasPrefix(field, "shared:") || strings.HasPrefix(field, "master:")
			}
			if !canSupplyMaster {
				// CRIU chooses one source for an external master. Another
				// shared member does not prevent it from choosing this private
				// source. Conservatively switch if any candidate is private.
				opts.MntnsCompatMode = proto.Bool(true)
				log.Info("Selected compatible CRIU mount engine for private external mount",
					"source", source, "checkpoint_master", mount.GetMasterId())
				return nil
			}
			break
		}
		if !found {
			return fmt.Errorf("external mount %s changed during mount engine selection", source)
		}
	}
	return nil
}

func readCheckpointMounts(checkpointPath string) ([]*mnt.MntEntry, error) {
	paths, err := filepath.Glob(filepath.Join(checkpointPath, "mountpoints-*.img"))
	if err != nil {
		return nil, err
	}
	var mounts []*mnt.MntEntry
	for _, path := range paths {
		fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			return nil, fmt.Errorf("open mount image %s: %w", path, err)
		}
		f := os.NewFile(uintptr(fd), path)
		info, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("stat mount image %s: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			_ = f.Close()
			return nil, fmt.Errorf("mount image %s is not a regular file", path)
		}
		image, decodeErr := crit.New(f, nil, "", false, false).Decode(&mnt.MntEntry{})
		closeErr := f.Close()
		if decodeErr != nil {
			return nil, fmt.Errorf("decode mount image %s: %w", path, decodeErr)
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if image.Magic != "MNTS" {
			return nil, fmt.Errorf("unexpected mount image type %s in %s", image.Magic, path)
		}
		for _, entry := range image.Entries {
			mounts = append(mounts, entry.Message.(*mnt.MntEntry))
		}
	}
	return mounts, nil
}
