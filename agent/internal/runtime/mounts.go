// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/moby/sys/mountinfo"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

// ReadMountInfo reads and parses mountinfo for a container process via /host/proc.
func ReadMountInfo(pid int) ([]types.MountInfo, error) {
	mountinfoPath := fmt.Sprintf("%s/%d/mountinfo", HostProcPath, pid)
	f, err := os.Open(mountinfoPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open mountinfo: %w", err)
	}
	defer f.Close()
	return parseMountInfo(f)
}

func parseMountInfo(r io.Reader) ([]types.MountInfo, error) {
	infos, err := mountinfo.GetMountsFromReader(r, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to parse mountinfo: %w", err)
	}

	mounts := make([]types.MountInfo, 0, len(infos))
	for _, info := range infos {
		mounts = append(mounts, types.MountInfo{
			MountPoint: info.Mountpoint,
			FSType:     info.FSType,
			VFSOptions: info.VFSOptions,
			ReadOnly:   slices.Contains(strings.Split(info.Options, ","), "ro"),
			Device:     fmt.Sprintf("%d:%d", info.Major, info.Minor),
		})
	}
	return mounts, nil
}

// ClassifyMounts sets IsOCIManaged on each mount by matching against the
// container's OCI spec (mounts, masked paths, readonly paths).
// Handles /run/ ↔ /var/run/ aliasing since some images symlink one to the other.
func ClassifyMounts(mounts []types.MountInfo, ociSpec *specs.Spec, rootFS string) []types.MountInfo {
	ociSet := collectOCIManagedPaths(ociSpec, rootFS)

	for i := range mounts {
		mp := mounts[i].MountPoint
		if _, ok := ociSet[mp]; ok {
			mounts[i].IsOCIManaged = true
			continue
		}
		// /run/ ↔ /var/run/ aliasing
		if strings.HasPrefix(mp, "/run/") {
			if _, ok := ociSet["/var"+mp]; ok {
				mounts[i].IsOCIManaged = true
				continue
			}
		}
		if strings.HasPrefix(mp, "/var/run/") {
			if _, ok := ociSet[strings.TrimPrefix(mp, "/var")]; ok {
				mounts[i].IsOCIManaged = true
			}
		}
	}

	return mounts
}

// BuildMountPolicy classifies mounts and masked paths for CRIU dump.
// Mounts must already have IsOCIManaged set by ClassifyMounts.
//
// Policy (evaluated top to bottom):
//  1. Skip: non-OCI /proc/*, /sys/*, /run/* submounts (virtual/runtime, not in placeholder)
//     and NVIDIA device plugin allocation markers (named after the source's GPUs)
//  2. Native: the container's procfs (CRIU mounts it for the restored PID namespace)
//  3. Native: /dev/shm tmpfs (CRIU saves and restores content)
//  4. Masked: OCI masked non-directory paths that exist under rootFS → /dev/null
//  5. Externalize: everything else (OCI mounts the runtime recreates in placeholder)
func BuildMountPolicy(mounts []types.MountInfo, rootFS string, maskedPaths []string) (map[string]string, []string) {
	extMap := make(map[string]string, len(mounts))
	var skipped []string
	procDevice := containerProcDevice(mounts)

	for _, m := range mounts {
		if m.MountPoint == "" {
			continue
		}

		// Skip non-OCI virtual/runtime mounts — these won't exist in the placeholder
		if !m.IsOCIManaged && (strings.HasPrefix(m.MountPoint, "/proc/") || strings.HasPrefix(m.MountPoint, "/sys/") || strings.HasPrefix(m.MountPoint, "/run/")) {
			skipped = append(skipped, m.MountPoint)
			continue
		}

		// Skip device plugin allocation markers — the placeholder has its own
		if isDeviceListMarker(m.MountPoint) {
			skipped = append(skipped, m.MountPoint)
			continue
		}

		// Leave the container's procfs, /proc and its read-only binds such as
		// /proc/sys, to CRIU, which mounts it for the restored PID namespace.
		// The placeholder's procfs numbers processes in the outer namespace,
		// where /proc/<getpid()> is missing or another process. Any other
		// procfs, such as the host's from a hostPath volume, stays external:
		// CRIU can only rebuild binds of a filesystem it mounts itself.
		if m.FSType == "proc" && m.Device == procDevice {
			continue
		}

		// Let CRIU handle /dev/shm content natively — don't externalize it.
		if m.MountPoint == "/dev/shm" && m.FSType == "tmpfs" {
			continue
		}

		extMap[m.MountPoint] = m.MountPoint
	}

	// Masked paths map to /dev/null. Only non-directory paths that exist under rootFS.
	for _, p := range maskedPaths {
		hostPath := filepath.Join(rootFS, p)
		info, err := os.Lstat(hostPath)
		if err != nil || info.IsDir() {
			continue
		}
		extMap[p] = "/dev/null"
	}

	return extMap, skipped
}

// containerProcDevice returns the device of the procfs mounted at /proc, or ""
// when there is none.
func containerProcDevice(mounts []types.MountInfo) string {
	for _, m := range mounts {
		if m.MountPoint == "/proc" && m.FSType == "proc" {
			return m.Device
		}
	}
	return ""
}

// deviceListMarkerDirs hold the allocation markers of the NVIDIA device plugin's
// volume-mounts strategy: /dev/null bound at <dir>/<GPU UUID>. Kubelet adds them
// to the OCI spec, but only the container runtime reads them, to create the
// container. Externalizing one would demand the source GPU's UUID in the restore
// pod, which a restore onto any other GPU cannot have.
var deviceListMarkerDirs = []string{"/run/nvidia-container-devices/", "/var/run/nvidia-container-devices/"}

func isDeviceListMarker(mountPoint string) bool {
	return slices.ContainsFunc(deviceListMarkerDirs, func(dir string) bool {
		return strings.HasPrefix(mountPoint, dir)
	})
}

// RemountProcSys remounts /proc/sys read-write or read-only.
func RemountProcSys(rw bool) error {
	flags := uintptr(syscall.MS_BIND | syscall.MS_REMOUNT)
	if !rw {
		flags |= syscall.MS_RDONLY
	}
	if err := syscall.Mount("proc", "/proc/sys", "", flags, ""); err != nil {
		mode := "rw"
		if !rw {
			mode = "ro"
		}
		return fmt.Errorf("failed to remount /proc/sys %s: %w", mode, err)
	}
	return nil
}
