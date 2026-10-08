// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package criu

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/checkpoint-restore/go-criu/v8/crit"
	"github.com/checkpoint-restore/go-criu/v8/crit/images/mnt"
	criurpc "github.com/checkpoint-restore/go-criu/v8/rpc"
	"github.com/go-logr/logr"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
)

func TestAutomaticMountEngine(t *testing.T) {
	if os.Getenv("SNAPSHOT_MOUNT_ENGINE_TEST") == "" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestAutomaticMountEngine$", "-test.v")
		cmd.Env = append(os.Environ(), "SNAPSHOT_MOUNT_ENGINE_TEST=1")
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Cloneflags:  unix.CLONE_NEWUSER | unix.CLONE_NEWNS,
			UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
			GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		}
		output, err := cmd.CombinedOutput()
		if err != nil {
			if errors.Is(err, syscall.EPERM) {
				t.Skip("isolated mount tests require user namespaces")
			}
			t.Fatalf("isolated mount test: %v\n%s", err, output)
		}
		if bytes.Contains(output, []byte("--- SKIP: TestAutomaticMountEngine (")) {
			t.Skipf("isolated mount test skipped:\n%s", output)
		}
		t.Logf("isolated mount test:\n%s", output)
		return
	}
	must := func(t *testing.T, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
		t.Skipf("isolated mount tests require mount permission: %v", err)
	} else {
		must(t, err)
	}
	source := t.TempDir()
	must(t, unix.Mount("tmpfs", source, "tmpfs", 0, "size=1m"))
	defer unix.Unmount(source, unix.MNT_DETACH) //nolint:errcheck
	must(t, unix.Mount("", source, "", unix.MS_SHARED, ""))
	if err := testCopySharing(t, source); errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) {
		t.Skip("kernel does not support MOVE_MOUNT_SET_GROUP")
	} else {
		must(t, err)
	}
	must(t, unix.Mount("", source, "", unix.MS_PRIVATE, ""))
	checkpoint := t.TempDir()
	image := filepath.Join(checkpoint, "mountpoints-1.img")
	mount := testExternalSlaveMount(source)
	writeMountImage(t, image, mount)
	original, err := os.ReadFile(image)
	must(t, err)
	newOpts := func() *criurpc.CriuOpts {
		return &criurpc.CriuOpts{
			MntnsCompatMode: proto.Bool(false),
			ExtMnt:          []*criurpc.ExtMountMap{{Key: proto.String("device"), Val: proto.String(source)}},
		}
	}
	t.Run("private external source selects compatibility without recapture", func(t *testing.T) {
		if err := testCopySharing(t, source); !errors.Is(err, unix.EINVAL) {
			t.Fatalf("kernel copy from private mount = %v, want EINVAL", err)
		}
		opts := newOpts()
		must(t, selectRestoreMountEngine(opts, checkpoint, logr.Discard()))
		if !opts.GetMntnsCompatMode() {
			t.Fatal("private source cannot supply checkpoint's external master; compatibility mode was not selected")
		}
		after, err := os.ReadFile(image)
		must(t, err)
		if !bytes.Equal(original, after) {
			t.Fatal("mount engine selection modified the checkpoint")
		}
	})
	t.Run("shared source keeps mount-v2", func(t *testing.T) {
		must(t, unix.Mount("", source, "", unix.MS_SHARED, ""))
		must(t, testCopySharing(t, source))
		opts := newOpts()
		must(t, selectRestoreMountEngine(opts, checkpoint, logr.Discard()))
		if opts.GetMntnsCompatMode() {
			t.Fatal("compatible source unnecessarily selected the older engine")
		}
	})
	t.Run("private overmount hides shared mount", func(t *testing.T) {
		must(t, unix.Mount("tmpfs", source, "tmpfs", 0, "size=1m"))
		defer unix.Unmount(source, unix.MNT_DETACH) //nolint:errcheck
		must(t, unix.Mount("", source, "", unix.MS_PRIVATE, ""))
		if err := testCopySharing(t, source); !errors.Is(err, unix.EINVAL) {
			t.Fatalf("kernel copy from overmount = %v, want EINVAL", err)
		}
		opts := newOpts()
		must(t, selectRestoreMountEngine(opts, checkpoint, logr.Discard()))
		if !opts.GetMntnsCompatMode() {
			t.Fatal("looked at hidden shared mount instead of visible private overmount")
		}
	})
	t.Run("slave source keeps mount-v2", func(t *testing.T) {
		slave := t.TempDir()
		must(t, unix.Mount(source, slave, "", unix.MS_BIND, ""))
		defer unix.Unmount(slave, unix.MNT_DETACH) //nolint:errcheck
		must(t, unix.Mount("", slave, "", unix.MS_SLAVE, ""))
		must(t, testCopySharing(t, slave))
		opts := newOpts()
		opts.ExtMnt[0].Val = proto.String(slave)
		must(t, selectRestoreMountEngine(opts, checkpoint, logr.Discard()))
		if opts.GetMntnsCompatMode() {
			t.Fatal("slave source unnecessarily selected the older engine")
		}
	})
	t.Run("old external mount encoding selects compatibility", func(t *testing.T) {
		must(t, unix.Mount("", source, "", unix.MS_PRIVATE, ""))
		old := testExternalSlaveMount(source)
		old.ExtKey, old.ExtMount, old.Root = nil, proto.Bool(true), proto.String("device")
		writeMountImage(t, image, old)
		opts := newOpts()
		must(t, selectRestoreMountEngine(opts, checkpoint, logr.Discard()))
		if !opts.GetMntnsCompatMode() {
			t.Fatal("older checkpoint did not get automatic selection")
		}
	})
	t.Run("private candidate in a mixed sharing group selects compatibility", func(t *testing.T) {
		must(t, unix.Mount("", source, "", unix.MS_SHARED, ""))
		peer := t.TempDir()
		must(t, unix.Mount(source, peer, "", unix.MS_BIND, ""))
		defer unix.Unmount(peer, unix.MNT_DETACH) //nolint:errcheck
		must(t, unix.Mount("", peer, "", unix.MS_PRIVATE, ""))
		private := testExternalSlaveMount(peer)
		private.MntId, private.ExtKey = proto.Uint32(11), proto.String("peer")
		for _, entries := range [][]*mnt.MntEntry{{mount, private}, {private, mount}} {
			writeMountImage(t, image, entries...)
			opts := newOpts()
			opts.ExtMnt = append(opts.ExtMnt, &criurpc.ExtMountMap{Key: proto.String("peer"), Val: proto.String(peer)})
			must(t, selectRestoreMountEngine(opts, checkpoint, logr.Discard()))
			if !opts.GetMntnsCompatMode() {
				t.Fatal("shared peer masked a private candidate for the external master")
			}
		}
	})
	t.Run("internal master does not require external sharing", func(t *testing.T) {
		must(t, unix.Mount("", source, "", unix.MS_PRIVATE, ""))
		parent := testExternalSlaveMount(source)
		parent.MasterId, parent.SharedId = proto.Uint32(0), proto.Uint32(42)
		writeMountImage(t, image, mount, parent)
		opts := newOpts()
		must(t, selectRestoreMountEngine(opts, checkpoint, logr.Discard()))
		if opts.GetMntnsCompatMode() {
			t.Fatal("internally restored master unnecessarily selected the older engine")
		}
	})
}

// Exercise the same kernel operation that CRIU uses, without restoring a
// process or touching the parent namespace. Both mounts have the same root
// and filesystem, isolating the propagation mismatch from other EINVAL causes.
func testCopySharing(t *testing.T, source string) error {
	t.Helper()
	target := t.TempDir()
	if err := unix.Mount(source, target, "", unix.MS_BIND, ""); err != nil {
		t.Fatal(err)
	}
	defer unix.Unmount(target, unix.MNT_DETACH) //nolint:errcheck
	if err := unix.Mount("", target, "", unix.MS_PRIVATE, ""); err != nil {
		t.Fatal(err)
	}
	return unix.MoveMount(unix.AT_FDCWD, source, unix.AT_FDCWD, target, unix.MOVE_MOUNT_SET_GROUP)
}

func TestExplicitMountCompatibilityPreserved(t *testing.T) {
	opts := &criurpc.CriuOpts{MntnsCompatMode: proto.Bool(true)}
	if err := selectRestoreMountEngine(opts, "/no/checkpoint/needed", logr.Discard()); err != nil {
		t.Fatal(err)
	}
	if !opts.GetMntnsCompatMode() {
		t.Fatal("explicit compatibility mode was overridden")
	}
}

func testExternalSlaveMount(source string) *mnt.MntEntry {
	return &mnt.MntEntry{
		Fstype: proto.Uint32(uint32(mnt.Fstype_TMPFS)), MntId: proto.Uint32(10),
		RootDev: proto.Uint32(0), ParentMntId: proto.Uint32(1), Flags: proto.Uint32(0),
		Root: proto.String("/"), Mountpoint: proto.String(source), Source: proto.String("tmpfs"),
		Options: proto.String(""), MasterId: proto.Uint32(42), ExtKey: proto.String("device"),
	}
}

func writeMountImage(t *testing.T, path string, mounts ...*mnt.MntEntry) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck
	image := &crit.CriuImage{Magic: "MNTS", EntryType: &mnt.MntEntry{}}
	for _, mount := range mounts {
		image.Entries = append(image.Entries, &crit.CriuEntry{Message: mount})
	}
	if err := crit.New(nil, f, "", false, false).Encode(image); err != nil {
		t.Fatal(err)
	}
}
