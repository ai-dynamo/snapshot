// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"archive/tar"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/checkpoint-restore/go-criu/v8/crit"
	criu_core "github.com/checkpoint-restore/go-criu/v8/crit/images/criu-core"
	"github.com/checkpoint-restore/go-criu/v8/crit/images/fdinfo"
	"github.com/checkpoint-restore/go-criu/v8/crit/images/fown"
	"github.com/checkpoint-restore/go-criu/v8/crit/images/mnt"
	"github.com/checkpoint-restore/go-criu/v8/crit/images/pstree"
	"github.com/checkpoint-restore/go-criu/v8/crit/images/regfile"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

const (
	overlayTestRootMount  = 100
	overlayTestCacheMount = 200
)

// A dump that restore cannot decode fails the capture, so captureCheckpoint
// returns before the PageBroker commit and checkpoint aborts the transaction.
func TestCaptureOverlayFailsWhenTheDumpCannotBeRead(t *testing.T) {
	upperDir := t.TempDir()
	checkpointDir := t.TempDir()
	writeOverlayTestFile(t, upperDir, "root/kept.txt", "kept")
	writeOverlayTestDump(t, checkpointDir)
	require.NoError(t, os.WriteFile(filepath.Join(checkpointDir, "files.img"), []byte("not a CRIU image"), 0o600))

	err := captureOverlay(upperDir, types.OverlayManifest{}, checkpointDir, logr.Discard())
	require.ErrorContains(t, err, "read the files the dump references")
	require.ErrorContains(t, err, "files.img")
	assert.NoFileExists(t, filepath.Join(checkpointDir, "rootfs-diff.tar"))
}

// A file open on a tmpfs at /cache keeps a different upperdir file at the same
// path out of the archive: restore reopens it through the tmpfs, not the rootfs.
func TestCaptureOverlayKeepsOnlyRootMountFiles(t *testing.T) {
	upperDir := t.TempDir()
	checkpointDir := t.TempDir()
	writeOverlayTestFile(t, upperDir, "cache/model.bin", "hidden by the tmpfs")
	writeOverlayTestFile(t, upperDir, "root/.triton/cache/K/cuda_utils.so", "mapped")
	writeOverlayTestDump(t, checkpointDir,
		newOverlayTestRegFile(1, "/cache/model.bin", overlayTestCacheMount),
		newOverlayTestRegFile(2, "/root/.triton/cache/K/cuda_utils.so", overlayTestRootMount),
	)

	err := captureOverlay(upperDir, types.OverlayManifest{
		Exclusions: types.OverlaySettings{Exclusions: []string{"/cache", "*/.triton/cache"}},
	}, checkpointDir, logr.Discard())
	require.NoError(t, err)

	members := overlayTestArchiveMembers(t, filepath.Join(checkpointDir, "rootfs-diff.tar"))
	assert.Contains(t, members, "./root/.triton/cache/K/cuda_utils.so")
	assert.NotContains(t, members, "./cache/model.bin")
}

// writeOverlayTestDump writes the images criu.ReadReferencedPaths reads: the
// root task, PID 1, is in mount namespace 13, whose root mount has a tmpfs at
// /cache.
func writeOverlayTestDump(t *testing.T, dir string, files ...*fdinfo.FileEntry) {
	t.Helper()
	zero := proto.Uint32(0)
	writeOverlayTestImage(t, dir, "pstree.img", "PSTREE",
		&pstree.PstreeEntry{Pid: proto.Uint32(1), Ppid: zero, Pgid: proto.Uint32(1), Sid: proto.Uint32(1)})
	writeOverlayTestImage(t, dir, "ids-1.img", "IDS",
		&criu_core.TaskKobjIdsEntry{VmId: zero, FilesId: zero, FsId: zero, SighandId: zero, MntNsId: proto.Uint32(13)})
	mount := func(id, parent uint32, mountpoint string) proto.Message {
		return &mnt.MntEntry{
			Fstype: zero, MntId: proto.Uint32(id), RootDev: zero, ParentMntId: proto.Uint32(parent), Flags: zero,
			Root: proto.String("/"), Mountpoint: proto.String(mountpoint), Source: proto.String("none"), Options: proto.String(""),
		}
	}
	writeOverlayTestImage(t, dir, "mountpoints-13.img", "MNTS",
		mount(overlayTestRootMount, 50, "/"), mount(overlayTestCacheMount, overlayTestRootMount, "/cache"))
	entries := make([]proto.Message, len(files))
	for i, file := range files {
		entries[i] = file
	}
	writeOverlayTestImage(t, dir, "files.img", "FILES", entries...)
}

func writeOverlayTestImage(t *testing.T, dir, name, magic string, entries ...proto.Message) {
	t.Helper()
	file, err := os.Create(filepath.Join(dir, name))
	require.NoError(t, err)
	image := &crit.CriuImage{Magic: magic}
	for _, entry := range entries {
		image.Entries = append(image.Entries, &crit.CriuEntry{Message: entry})
	}
	require.NoError(t, crit.New(nil, file, "", false, false).Encode(image))
	require.NoError(t, file.Close())
}

func newOverlayTestRegFile(id uint32, name string, mountID int32) *fdinfo.FileEntry {
	zero := proto.Uint32(0)
	return &fdinfo.FileEntry{
		Type: fdinfo.FdTypes_REG.Enum(),
		Id:   proto.Uint32(id),
		Reg: &regfile.RegFileEntry{
			Id:    proto.Uint32(id),
			Flags: zero,
			Pos:   proto.Uint64(0),
			Fown:  &fown.FownEntry{Uid: zero, Euid: zero, Signum: zero, PidType: zero, Pid: zero},
			Name:  proto.String(name),
			MntId: proto.Int32(mountID),
		},
	}
}

func writeOverlayTestFile(t *testing.T, dir, rel, data string) {
	t.Helper()
	path := filepath.Join(dir, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(data), 0o644))
}

func overlayTestArchiveMembers(t *testing.T, path string) []string {
	t.Helper()
	archive, err := os.Open(path)
	require.NoError(t, err)
	defer archive.Close()
	reader := tar.NewReader(archive)
	var members []string
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return members
		}
		require.NoError(t, err)
		members = append(members, header.Name)
	}
}
