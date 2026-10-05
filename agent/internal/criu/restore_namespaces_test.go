// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package criu

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/checkpoint-restore/go-criu/v8/crit"
	"github.com/checkpoint-restore/go-criu/v8/crit/images/pstree"
	criurpc "github.com/checkpoint-restore/go-criu/v8/rpc"
	"github.com/go-logr/logr/testr"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"

	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

type namespaceFDRecorder struct {
	key  string
	file *os.File
}

func (r *namespaceFDRecorder) AddInheritFd(key string, file *os.File) { r.key, r.file = key, file }

func openNamespaceForTest(t *testing.T, name string) *os.File {
	t.Helper()
	file, err := os.Open("/proc/self/ns/" + name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

func externalPIDManifest() *types.CheckpointManifest {
	return &types.CheckpointManifest{CRIUDump: types.CRIUDumpManifest{
		External: []string{"net[123]:extNetNs", "pid[456]:extPodPidNs"},
		ExtMnt:   map[string]string{"/": "/"},
	}}
}

func TestRegisterExternalPIDNamespace(t *testing.T) {
	destination := openNamespaceForTest(t, "pid")
	r := &namespaceFDRecorder{}
	owned, err := registerExternalPIDNamespace(r, externalPIDManifest(), destination)
	if err != nil {
		t.Fatal(err)
	}
	defer owned.Close()
	if r.key != externalPIDNamespaceKey || r.file != owned || owned.Fd() == destination.Fd() {
		t.Fatalf("registration = %q %v; want owned duplicate", r.key, r.file)
	}
	originalInfo, err := destination.Stat()
	if err != nil {
		t.Fatal(err)
	}
	duplicateInfo, err := owned.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(originalInfo, duplicateInfo) {
		t.Fatal("registered a different namespace")
	}
	// CRIU cleanup must not close the helper's descriptor before CUDA unlock.
	closeFiles([]*os.File{owned})
	if _, err := owned.Stat(); err == nil {
		t.Fatal("CRIU descriptor remains open")
	}
	if _, err := destination.Stat(); err != nil {
		t.Fatalf("borrowed descriptor closed: %v", err)
	}
}

func TestRegisterExternalPIDNamespaceCompatibilityAndErrors(t *testing.T) {
	closed := openNamespaceForTest(t, "pid")
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		external []string
		file     *os.File
		wantErr  bool
	}{
		{"legacy checkpoint", []string{"net[123]:extNetNs"}, nil, false},
		{"no metadata", nil, nil, false},
		{"missing destination", []string{"pid[456]:extPodPidNs"}, nil, true},
		{"closed destination", []string{"pid[456]:extPodPidNs"}, closed, true},
		{"unsupported key", []string{"pid[456]:other"}, nil, true},
		{"invalid inode", []string{"pid[0]:extPodPidNs"}, nil, true},
		{"duplicate namespace", []string{"pid[456]:extPodPidNs", "pid[789]:extPodPidNs"}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &types.CheckpointManifest{CRIUDump: types.CRIUDumpManifest{External: tc.external}}
			r := &namespaceFDRecorder{}
			file, err := registerExternalPIDNamespace(r, m, tc.file)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v", err)
			}
			if file != nil || r.file != nil {
				t.Fatal("unexpected descriptor registration")
			}
		})
	}
}

func writeProcessTreeImage(t *testing.T, dir string, pid uint32) {
	t.Helper()
	file, err := os.Create(filepath.Join(dir, "pstree.img"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	entry := &pstree.PstreeEntry{Pid: proto.Uint32(pid), Ppid: proto.Uint32(0), Pgid: proto.Uint32(pid), Sid: proto.Uint32(pid), Threads: []uint32{pid}}
	image := &crit.CriuImage{Magic: "PSTREE", EntryType: entry, Entries: []*crit.CriuEntry{{Message: entry}}}
	if err := crit.New(nil, file, "", false, false).Encode(image); err != nil {
		t.Fatal(err)
	}
}

func TestBuildRestoreOptsExternalPIDOneIsUnsupported(t *testing.T) {
	for _, pid := range []uint32{1, 42} {
		dir := t.TempDir()
		writeProcessTreeImage(t, dir, pid)
		_, err := BuildRestoreOpts(externalPIDManifest(), dir, "", testr.New(t))
		if pid == 1 {
			if err == nil || !strings.Contains(err.Error(), "placeholder occupies PID 1") {
				t.Fatalf("error = %v", err)
			}
		} else if err != nil {
			t.Fatal(err)
		}
	}
}

func TestBuildRestoreOptsLegacyDoesNotReadProcessTree(t *testing.T) {
	m := externalPIDManifest()
	m.CRIUDump.External = []string{"net[123]:extNetNs"}
	if _, err := BuildRestoreOpts(m, t.TempDir(), "", testr.New(t)); err != nil {
		t.Fatal(err)
	}
}

func TestBuildRestoreOptsExternalRejectsMissingProcessTree(t *testing.T) {
	if _, err := BuildRestoreOpts(externalPIDManifest(), t.TempDir(), "", testr.New(t)); err == nil {
		t.Fatal("accepted missing process tree")
	}
}

func TestPrepareRestoreImageDirUsesPinnedMountNamespace(t *testing.T) {
	mount := openNamespaceForTest(t, "mnt")
	dir := t.TempDir()
	writeFilesImage(t, dir, nil)
	got, cleanup, err := prepareRestoreImageDirInNamespace(dir, t.TempDir(), mount)
	if err != nil {
		t.Fatal(err)
	}
	if got != dir {
		t.Fatalf("image path = %q", got)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := mount.Stat(); err != nil {
		t.Fatal(err)
	}
	if err := mount.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := prepareRestoreImageDirInNamespace(dir, t.TempDir(), mount); err == nil {
		t.Fatal("accepted closed mount descriptor")
	}
}

func TestExecuteRestoreFailureCleansResources(t *testing.T) {
	for _, external := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "external"}[external], func(t *testing.T) {
			scratch := t.TempDir()
			t.Setenv("TMPDIR", scratch)
			dir, bundle := t.TempDir(), t.TempDir()
			writeFilesImage(t, dir, nil)
			// A deliberately failed service worker exercises the CRIU error cleanup.
			if err := os.WriteFile(filepath.Join(bundle, "criu"), []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
				t.Fatal(err)
			}
			m := externalPIDManifest()
			if !external {
				m.CRIUDump.External = nil
			}
			namespaces := RestoreNamespaceFiles{PID: openNamespaceForTest(t, "pid"), Mount: openNamespaceForTest(t, "mnt")}
			before := namespaceDescriptorCount(t, "pid")
			_, cleanup, _, _, err := executeRestore(&criurpc.CriuOpts{}, m, dir, bundle, namespaces, "/proc/self/ns/net", testr.New(t))
			if err == nil || !strings.Contains(err.Error(), "CRIU restore failed") || cleanup != nil {
				t.Fatalf("restore error = %v, cleanup present = %t", err, cleanup != nil)
			}
			entries, err := os.ReadDir(scratch)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("scratch resources leaked: %v", entries)
			}
			if after := namespaceDescriptorCount(t, "pid"); after != before {
				t.Fatalf("PID descriptors before=%d after=%d", before, after)
			}
			if _, err := namespaces.PID.Stat(); err != nil {
				t.Fatal(err)
			}
			if _, err := namespaces.Mount.Stat(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func namespaceDescriptorCount(t *testing.T, namespace string) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
		if err == nil && strings.HasPrefix(target, namespace+":[") {
			count++
		}
	}
	return count
}

func TestOrdinaryCaptureKeepsNamespaceAndLifecycleOptions(t *testing.T) {
	state := &types.CheckpointContainerSnapshot{PID: 1, RootFS: t.TempDir(), NetNSInode: 123}
	opts, err := BuildDumpOptions(state, nil, t.TempDir(), testr.New(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(opts.External) != 1 || opts.External[0] != "net[123]:extNetNs" {
		t.Fatalf("external namespaces = %v", opts.External)
	}
	if opts.GetPid() != 1 || opts.GetLeaveRunning() {
		t.Fatal("ordinary capture lifecycle changed")
	}
}

// This subprocess implements only the CRIU RPC success handshake. It proves
// descriptor registration and cleanup without claiming a real workload restore.
func TestNamespaceRestoreServiceWorker(t *testing.T) {
	if os.Getenv("SNAPSHOT_TEST_CRIU_WORKER") != "1" {
		return
	}
	fd, err := strconv.Atoi(os.Args[len(os.Args)-1])
	if err != nil {
		os.Exit(2)
	}
	buffer := make([]byte, 65536)
	n, _, err := unix.Recvfrom(fd, buffer, 0)
	if err != nil {
		os.Exit(3)
	}
	req := &criurpc.CriuReq{}
	if err := req.UnmarshalVT(buffer[:n]); err != nil {
		os.Exit(4)
	}
	found := false
	for _, inherited := range req.GetOpts().GetInheritFd() {
		if inherited.GetKey() != externalPIDNamespaceKey {
			continue
		}
		file := os.NewFile(uintptr(inherited.GetFd()), "pid-namespace")
		original, err := os.Stat("/proc/self/ns/pid")
		if err != nil {
			os.Exit(5)
		}
		actual, err := file.Stat()
		if err != nil || !os.SameFile(original, actual) {
			os.Exit(6)
		}
		found = true
	}
	if !found {
		os.Exit(7)
	}
	notify := &criurpc.CriuResp{Type: criurpc.CriuReqType_NOTIFY.Enum(), Success: proto.Bool(true), Notify: &criurpc.CriuNotify{Script: proto.String("post-restore"), Pid: proto.Int32(42)}}
	data, err := notify.MarshalVT()
	if err != nil || unix.Sendto(fd, data, 0, nil) != nil {
		os.Exit(8)
	}
	if _, _, err := unix.Recvfrom(fd, buffer, 0); err != nil {
		os.Exit(9)
	}
	response := &criurpc.CriuResp{Type: criurpc.CriuReqType_RESTORE.Enum(), Success: proto.Bool(true)}
	data, err = response.MarshalVT()
	if err != nil || unix.Sendto(fd, data, 0, nil) != nil {
		os.Exit(10)
	}
	os.Exit(0)
}

func TestExecuteRestoreSuccessKeepsNamespacesUntilCleanup(t *testing.T) {
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	t.Setenv("SNAPSHOT_TEST_CRIU_WORKER", "1")
	dir, bundle := t.TempDir(), t.TempDir()
	writeFilesImage(t, dir, nil)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// os.Executable is shell-quoted for paths containing spaces or apostrophes.
	script := "#!/bin/sh\nexec '" + strings.ReplaceAll(executable, "'", "'\\''") + "' -test.run=^TestNamespaceRestoreServiceWorker$ -- \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bundle, "criu"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	namespaces := RestoreNamespaceFiles{PID: openNamespaceForTest(t, "pid"), Mount: openNamespaceForTest(t, "mnt")}
	before := namespaceDescriptorCount(t, "pid")
	pid, cleanup, _, _, err := executeRestore(&criurpc.CriuOpts{}, externalPIDManifest(), dir, bundle, namespaces, "/proc/self/ns/net", testr.New(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanup() }()
	if pid != 42 {
		t.Fatalf("restored pid = %d", pid)
	}
	if during := namespaceDescriptorCount(t, "pid"); during != before+1 {
		t.Fatalf("PID namespace released before CUDA phase: before=%d during=%d", before, during)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if after := namespaceDescriptorCount(t, "pid"); after != before {
		t.Fatalf("PID descriptor leaked: before=%d after=%d", before, after)
	}
	entries, err := os.ReadDir(scratch)
	if err != nil || len(entries) != 0 {
		t.Fatalf("scratch cleanup: %v, %v", entries, err)
	}
	if _, err := namespaces.PID.Stat(); err != nil {
		t.Fatal(err)
	}
	if _, err := namespaces.Mount.Stat(); err != nil {
		t.Fatal(err)
	}
}
