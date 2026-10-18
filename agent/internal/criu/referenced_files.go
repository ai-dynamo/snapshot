// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package criu

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"

	criu_core "github.com/checkpoint-restore/go-criu/v8/crit/images/criu-core"
	"github.com/checkpoint-restore/go-criu/v8/crit/images/fdinfo"
	"github.com/checkpoint-restore/go-criu/v8/crit/images/mnt"
	"github.com/checkpoint-restore/go-criu/v8/crit/images/pstree"
	"github.com/checkpoint-restore/go-criu/v8/crit/images/regfile"
	remap_file_path "github.com/checkpoint-restore/go-criu/v8/crit/images/remap-file-path"
)

const (
	pstreeImageFilename        = "pstree.img"
	remapFilePathImageFilename = "remap-fpath.img"
)

// ReadReferencedPaths returns, sorted and deduplicated, the absolute paths of
// the regular files and directories on the container's root mount that a dump
// references: open descriptors, file-backed mappings, executables, working and
// root directories, and the link-remap names CRIU created for unlinked files.
// Restore re-opens each of them by path.
//
// Entries on any other mount are left out. Their paths resolve through that
// mount, so the rootfs upperdir can only hold a different file at the same path.
func ReadReferencedPaths(checkpointDir string) ([]string, error) {
	rootMountID, err := readRootMountID(checkpointDir)
	if err != nil {
		return nil, err
	}
	files, err := decodeFilesImage(checkpointDir)
	if err != nil {
		return nil, err
	}
	regularFiles := make(map[uint32]*regfile.RegFileEntry)
	for i, entry := range files.Entries {
		fileEntry, ok := entry.Message.(*fdinfo.FileEntry)
		if !ok {
			return nil, fmt.Errorf("unexpected %s entry %d type %T", filesImageFilename, i, entry.Message)
		}
		if fileEntry.GetType() == fdinfo.FdTypes_REG && fileEntry.Reg != nil {
			regularFiles[fileEntry.Reg.GetId()] = fileEntry.Reg
		}
	}
	linkRemaps, err := readLinkRemaps(checkpointDir)
	if err != nil {
		return nil, err
	}

	var paths []string
	for id, file := range regularFiles {
		mountID := int64(file.GetMntId())
		// CRIU records no mount for the hard link it makes to an unlinked file.
		// The link is made next to the file, so it is on the file's mount.
		if originalID, ok := linkRemaps[id]; ok && mountID < 0 {
			if original := regularFiles[originalID]; original != nil {
				mountID = int64(original.GetMntId())
			}
		}
		if mountID != int64(rootMountID) {
			continue
		}
		if name := file.GetName(); path.IsAbs(name) {
			paths = append(paths, path.Clean(name))
		}
	}
	slices.Sort(paths)
	return slices.Compact(paths), nil
}

// readRootMountID returns the ID of the mount visible at "/" in the root
// task's mount namespace, which is the container's root filesystem.
func readRootMountID(checkpointDir string) (uint32, error) {
	tree, err := decodeImage(checkpointDir, pstreeImageFilename, &pstree.PstreeEntry{})
	if err != nil {
		return 0, err
	}
	if len(tree.Entries) == 0 {
		return 0, fmt.Errorf("%s has no tasks", pstreeImageFilename)
	}
	root, ok := tree.Entries[0].Message.(*pstree.PstreeEntry)
	if !ok {
		return 0, fmt.Errorf("unexpected %s entry 0 type %T", pstreeImageFilename, tree.Entries[0].Message)
	}
	idsFilename := fmt.Sprintf("ids-%d.img", root.GetPid())
	ids, err := decodeImage(checkpointDir, idsFilename, &criu_core.TaskKobjIdsEntry{})
	if err != nil {
		return 0, err
	}
	if len(ids.Entries) != 1 {
		return 0, fmt.Errorf("%s has %d entries, want 1", idsFilename, len(ids.Entries))
	}
	rootIDs, ok := ids.Entries[0].Message.(*criu_core.TaskKobjIdsEntry)
	if !ok || rootIDs.MntNsId == nil {
		return 0, fmt.Errorf("%s records no mount namespace", idsFilename)
	}
	mountsFilename := fmt.Sprintf("mountpoints-%d.img", rootIDs.GetMntNsId())
	mounts, err := decodeImage(checkpointDir, mountsFilename, &mnt.MntEntry{})
	if err != nil {
		return 0, err
	}

	// A mount stacked on "/" covers the one it is mounted on.
	var roots []*mnt.MntEntry
	covered := make(map[uint32]bool)
	for i, entry := range mounts.Entries {
		mount, ok := entry.Message.(*mnt.MntEntry)
		if !ok {
			return 0, fmt.Errorf("unexpected %s entry %d type %T", mountsFilename, i, entry.Message)
		}
		if mount.GetMountpoint() == "/" {
			roots = append(roots, mount)
			covered[mount.GetParentMntId()] = true
		}
	}
	roots = slices.DeleteFunc(roots, func(mount *mnt.MntEntry) bool { return covered[mount.GetMntId()] })
	if len(roots) != 1 {
		return 0, fmt.Errorf("%s has %d visible mounts at /, want 1", mountsFilename, len(roots))
	}
	return roots[0].GetMntId(), nil
}

// readLinkRemaps maps the file ID of each hard link CRIU made to an unlinked
// file to the ID of that file. A dump without remapped files has no remap image.
func readLinkRemaps(checkpointDir string) (map[uint32]uint32, error) {
	image, err := decodeImage(checkpointDir, remapFilePathImageFilename, &remap_file_path.RemapFilePathEntry{})
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	remaps := make(map[uint32]uint32)
	for i, entry := range image.Entries {
		remap, ok := entry.Message.(*remap_file_path.RemapFilePathEntry)
		if !ok {
			return nil, fmt.Errorf("unexpected %s entry %d type %T", remapFilePathImageFilename, i, entry.Message)
		}
		if remap.GetRemapType() == remap_file_path.RemapType_LINKED {
			remaps[remap.GetRemapId()] = remap.GetOrigId()
		}
	}
	return remaps, nil
}
