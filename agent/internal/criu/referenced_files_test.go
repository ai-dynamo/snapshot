// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package criu

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/checkpoint-restore/go-criu/v8/crit"
	criu_core "github.com/checkpoint-restore/go-criu/v8/crit/images/criu-core"
	"github.com/checkpoint-restore/go-criu/v8/crit/images/fdinfo"
	"github.com/checkpoint-restore/go-criu/v8/crit/images/fown"
	"github.com/checkpoint-restore/go-criu/v8/crit/images/mnt"
	"github.com/checkpoint-restore/go-criu/v8/crit/images/pstree"
	"github.com/checkpoint-restore/go-criu/v8/crit/images/regfile"
	remap_file_path "github.com/checkpoint-restore/go-criu/v8/crit/images/remap-file-path"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
)

const (
	testRootMount  = 100
	testCacheMount = 200
	testShmMount   = 300
)

func TestReadReferencedPaths(t *testing.T) {
	dir := t.TempDir()
	writeTestDump(t, dir,
		[]*mnt.MntEntry{
			newMountEntry(testRootMount, 50, "/"),
			newMountEntry(testCacheMount, testRootMount, "/cache"),
			newMountEntry(testShmMount, testRootMount, "/dev/shm"),
		},
		newRegFileEntry(1, "/root/.triton/cache/K/cuda_utils.so", testRootMount),
		newRegFileEntry(2, "/usr/lib/x86_64-linux-gnu/libc.so.6", testRootMount),
		// Different file entries can reference the same path.
		newRegFileEntry(3, "/root/.triton/cache/K/cuda_utils.so", testRootMount),
		newRegFileEntry(4, "/vllm-workspace/", testRootMount),
		newRegFileEntry(5, "relative/name", testRootMount),
		// The tmpfs at /cache hides whatever the upperdir holds at this path.
		newRegFileEntry(6, "/cache/model.bin", testCacheMount),
		// Unlinked files on the root mount and on /dev/shm, and the links CRIU made to them.
		newRegFileEntry(7, "/root/.cache/vllm/scratch", testRootMount),
		newRegFileEntry(8, "/root/.cache/vllm/link_remap.8", -1),
		newRegFileEntry(9, "/dev/shm/segment", testShmMount),
		newRegFileEntry(10, "/dev/shm/link_remap.10", -1),
		// Neither a mount nor a link remap tells where this one lives.
		newRegFileEntry(11, "/unknown", -1),
		newUnixSocketEntry(12, []byte("/tmp/sock\x00"), 112, unix.SOCK_STREAM, linuxUnixSocketStateListen),
	)
	writeTestImage(t, dir, remapFilePathImageFilename, "REMAP_FPATH", &remap_file_path.RemapFilePathEntry{},
		newRemapEntry(7, 8, remap_file_path.RemapType_LINKED),
		newRemapEntry(9, 10, remap_file_path.RemapType_LINKED),
		// A ghost remap points at a ghost image, not a file entry.
		newRemapEntry(2, 11, remap_file_path.RemapType_GHOST),
	)

	got, err := ReadReferencedPaths(dir)
	if err != nil {
		t.Fatalf("ReadReferencedPaths: %v", err)
	}
	want := []string{
		"/root/.cache/vllm/link_remap.8",
		"/root/.cache/vllm/scratch",
		"/root/.triton/cache/K/cuda_utils.so",
		"/usr/lib/x86_64-linux-gnu/libc.so.6",
		"/vllm-workspace",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("ReadReferencedPaths() = %q, want %q", got, want)
	}
}

func TestReadReferencedPathsWithoutRemapImage(t *testing.T) {
	dir := t.TempDir()
	writeTestDump(t, dir, []*mnt.MntEntry{newMountEntry(testRootMount, 50, "/")},
		newRegFileEntry(1, "/root/kept", testRootMount),
		newRegFileEntry(2, "/root/link_remap.2", -1),
	)

	got, err := ReadReferencedPaths(dir)
	if err != nil {
		t.Fatalf("ReadReferencedPaths: %v", err)
	}
	if want := []string{"/root/kept"}; !slices.Equal(got, want) {
		t.Fatalf("ReadReferencedPaths() = %q, want %q", got, want)
	}
}

func TestReadReferencedPathsUsesTheVisibleRootMount(t *testing.T) {
	dir := t.TempDir()
	// A second mount at / covers the first one.
	writeTestDump(t, dir, []*mnt.MntEntry{newMountEntry(testRootMount, 50, "/"), newMountEntry(150, testRootMount, "/")},
		newRegFileEntry(1, "/covered", testRootMount),
		newRegFileEntry(2, "/visible", 150),
	)

	got, err := ReadReferencedPaths(dir)
	if err != nil {
		t.Fatalf("ReadReferencedPaths: %v", err)
	}
	if want := []string{"/visible"}; !slices.Equal(got, want) {
		t.Fatalf("ReadReferencedPaths() = %q, want %q", got, want)
	}
}

func TestReadReferencedPathsFailsOnUnreadableDump(t *testing.T) {
	writeDump := func(t *testing.T, dir string) {
		t.Helper()
		writeTestDump(t, dir, []*mnt.MntEntry{newMountEntry(testRootMount, 50, "/")}, newRegFileEntry(1, "/root/kept", testRootMount))
	}
	corrupt := func(name string) func(*testing.T, string) {
		return func(t *testing.T, dir string) {
			t.Helper()
			writeDump(t, dir)
			if err := os.WriteFile(filepath.Join(dir, name), []byte("not a CRIU image"), 0o600); err != nil {
				t.Fatalf("corrupt %s: %v", name, err)
			}
		}
	}
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, string)
		want  string
	}{
		{name: "no images", setup: func(*testing.T, string) {}, want: "pstree.img"},
		{
			name: "missing ids image",
			setup: func(t *testing.T, dir string) {
				writeDump(t, dir)
				if err := os.Remove(filepath.Join(dir, "ids-1.img")); err != nil {
					t.Fatalf("remove ids image: %v", err)
				}
			},
			want: "ids-1.img",
		},
		{name: "corrupt mount image", setup: corrupt("mountpoints-13.img"), want: "mountpoints-13.img"},
		{name: "corrupt files image", setup: corrupt(filesImageFilename), want: filesImageFilename},
		{name: "corrupt remap image", setup: corrupt(remapFilePathImageFilename), want: remapFilePathImageFilename},
		{
			name: "no root mount",
			setup: func(t *testing.T, dir string) {
				writeTestDump(t, dir, []*mnt.MntEntry{newMountEntry(testCacheMount, testRootMount, "/cache")})
			},
			want: "0 visible mounts at /",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.setup(t, dir)
			if _, err := ReadReferencedPaths(dir); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ReadReferencedPaths() error = %v, want one naming %q", err, tc.want)
			}
		})
	}
}

// writeTestDump writes the images ReadReferencedPaths reads, for a dump whose
// root task, PID 1, is in mount namespace 13.
func writeTestDump(t *testing.T, dir string, mounts []*mnt.MntEntry, files ...*fdinfo.FileEntry) {
	t.Helper()
	zero := proto.Uint32(0)
	writeTestImage(t, dir, pstreeImageFilename, "PSTREE", &pstree.PstreeEntry{},
		&pstree.PstreeEntry{Pid: proto.Uint32(1), Ppid: zero, Pgid: proto.Uint32(1), Sid: proto.Uint32(1)})
	writeTestImage(t, dir, "ids-1.img", "IDS", &criu_core.TaskKobjIdsEntry{},
		&criu_core.TaskKobjIdsEntry{VmId: zero, FilesId: zero, FsId: zero, SighandId: zero, MntNsId: proto.Uint32(13)})
	entries := make([]proto.Message, len(mounts))
	for i, mount := range mounts {
		entries[i] = mount
	}
	writeTestImage(t, dir, "mountpoints-13.img", "MNTS", &mnt.MntEntry{}, entries...)
	writeFilesImage(t, dir, files)
}

func writeTestImage(t *testing.T, dir, name, magic string, entryType proto.Message, entries ...proto.Message) {
	t.Helper()
	file, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	image := &crit.CriuImage{Magic: magic, EntryType: entryType}
	for _, entry := range entries {
		image.Entries = append(image.Entries, &crit.CriuEntry{Message: entry})
	}
	if err := crit.New(nil, file, "", false, false).Encode(image); err != nil {
		_ = file.Close()
		t.Fatalf("encode %s: %v", name, err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close %s: %v", name, err)
	}
}

func newMountEntry(id, parent uint32, mountpoint string) *mnt.MntEntry {
	zero := proto.Uint32(0)
	return &mnt.MntEntry{
		Fstype:      zero,
		MntId:       proto.Uint32(id),
		RootDev:     zero,
		ParentMntId: proto.Uint32(parent),
		Flags:       zero,
		Root:        proto.String("/"),
		Mountpoint:  proto.String(mountpoint),
		Source:      proto.String("none"),
		Options:     proto.String(""),
	}
}

func newRemapEntry(originalID, remapID uint32, remapType remap_file_path.RemapType) *remap_file_path.RemapFilePathEntry {
	return &remap_file_path.RemapFilePathEntry{
		OrigId:    proto.Uint32(originalID),
		RemapId:   proto.Uint32(remapID),
		RemapType: remapType.Enum(),
	}
}

func newRegFileEntry(id uint32, name string, mountID int32) *fdinfo.FileEntry {
	zero32 := uint32(0)
	return &fdinfo.FileEntry{
		Type: fdinfo.FdTypes_REG.Enum(),
		Id:   proto.Uint32(id),
		Reg: &regfile.RegFileEntry{
			Id:    proto.Uint32(id),
			Flags: &zero32,
			Pos:   proto.Uint64(0),
			Fown: &fown.FownEntry{
				Uid:     &zero32,
				Euid:    &zero32,
				Signum:  &zero32,
				PidType: &zero32,
				Pid:     &zero32,
			},
			Name:  proto.String(name),
			MntId: proto.Int32(mountID),
		},
	}
}
