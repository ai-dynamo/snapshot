// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package criu

import (
	"os"
	"reflect"
	"strings"
	"testing"

	criurpc "github.com/checkpoint-restore/go-criu/v8/rpc"

	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

func TestGPUMountAliases(t *testing.T) {
	for _, tc := range []struct {
		name      string
		deviceMap string
		targets   map[string]string
		want      map[string]string
		wantErr   bool
	}{
		{"overlapping paths", "A=C,B=D", map[string]string{"C": "/dev/nvidia1", "D": "/dev/nvidia2"}, map[string]string{"/dev/nvidia0": "/dev/nvidia1", "/dev/nvidia1": "/dev/nvidia2"}, false},
		{"swap", "A=C,B=D", map[string]string{"C": "/dev/nvidia1", "D": "/dev/nvidia0"}, map[string]string{"/dev/nvidia0": "/dev/nvidia1", "/dev/nvidia1": "/dev/nvidia0"}, false},
		{"identity UUID with changed path", "", map[string]string{"A": "/dev/nvidia2", "B": "/dev/nvidia1"}, map[string]string{"/dev/nvidia0": "/dev/nvidia2"}, false},
		{"missing target", "A=C,B=D", map[string]string{"C": "/dev/nvidia2"}, nil, true},
		{"unknown source", "X=C", map[string]string{"C": "/dev/nvidia2"}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &types.CheckpointManifest{
				CUDA:     types.CUDAManifest{SourceGPUUUIDs: []string{"A", "B"}, DevicePaths: map[string]string{"A": "/dev/nvidia0", "B": "/dev/nvidia1"}},
				CRIUDump: types.CRIUDumpManifest{ExtMnt: map[string]string{"/dev/nvidia0": "/dev/nvidia0", "/dev/nvidia1": "/dev/nvidia1"}},
			}
			got, err := GPUMountAliases(m, tc.deviceMap, tc.targets)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v", err)
			}
			if !tc.wantErr && !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("aliases = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestGPUMountAliasesLeavesOldCheckpointsUnchanged(t *testing.T) {
	m := &types.CheckpointManifest{
		CRIUDump: types.CRIUDumpManifest{ExtMnt: map[string]string{"/dev/nvidia7": "/dev/nvidia7"}},
	}
	got, err := GPUMountAliases(m, "", nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("old checkpoint aliases = %v, %v", got, err)
	}
}

func TestParseManageCgroupsMode(t *testing.T) {
	tests := []struct {
		raw      string
		wantMode criurpc.CriuCgMode
		wantErr  bool
	}{
		{raw: "ignore", wantMode: criurpc.CriuCgMode_IGNORE},
		{raw: "soft", wantMode: criurpc.CriuCgMode_SOFT},
		{raw: "full", wantMode: criurpc.CriuCgMode_FULL},
		{raw: "strict", wantMode: criurpc.CriuCgMode_STRICT},
		// Case insensitive + whitespace trimming
		{raw: "IGNORE", wantMode: criurpc.CriuCgMode_IGNORE},
		{raw: " Soft ", wantMode: criurpc.CriuCgMode_SOFT},
		{raw: "  FULL  ", wantMode: criurpc.CriuCgMode_FULL},
		// Empty string defaults to SOFT (matches Helm default)
		{raw: "", wantMode: criurpc.CriuCgMode_SOFT},
		// Invalid
		{raw: "bogus", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			mode, _, err := parseManageCgroupsMode(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error for %q, got mode=%v", tc.raw, mode)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.raw, err)
			}
			if mode != tc.wantMode {
				t.Errorf("mode = %v, want %v", mode, tc.wantMode)
			}
		})
	}
}
func TestReadLogTail(t *testing.T) {
	t.Run("returns whole small log", func(t *testing.T) {
		path := t.TempDir() + "/dump.log"
		if err := os.WriteFile(path, []byte("short log"), 0644); err != nil {
			t.Fatalf("write log: %v", err)
		}

		if got := readLogTail(path); got != "short log" {
			t.Fatalf("readLogTail() = %q, want %q", got, "short log")
		}
	})

	t.Run("truncates large log", func(t *testing.T) {
		path := t.TempDir() + "/dump.log"
		content := "prefix-" + strings.Repeat("x", dumpLogTailMaxSize+1)
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatalf("write log: %v", err)
		}

		got := readLogTail(path)
		if !strings.HasPrefix(got, "...<truncated>...\n") {
			t.Fatalf("readLogTail() missing truncation marker: %q", got[:min(len(got), 32)])
		}
		if !strings.HasSuffix(got, strings.Repeat("x", dumpLogTailMaxSize)) {
			t.Fatal("readLogTail() did not keep the log tail")
		}
	})
}

func TestApplyCommonSettings(t *testing.T) {
	t.Run("valid mode sets all fields", func(t *testing.T) {
		opts := &criurpc.CriuOpts{}
		settings := &types.CRIUSettings{
			LogLevel:          4,
			ShellJob:          true,
			TcpEstablished:    true,
			FileLocks:         true,
			ExtUnixSk:         true,
			LinkRemap:         true,
			ManageCgroupsMode: "soft",
		}

		if err := applyCommonSettings(opts, settings); err != nil {
			t.Fatalf("applyCommonSettings: %v", err)
		}

		if opts.GetLogLevel() != 4 {
			t.Errorf("LogLevel = %d", opts.GetLogLevel())
		}
		if !opts.GetShellJob() {
			t.Error("ShellJob should be true")
		}
		if !opts.GetTcpEstablished() {
			t.Error("TcpEstablished should be true")
		}
		if opts.GetTcpClose() {
			t.Error("TcpClose should be false")
		}
		if !opts.GetFileLocks() {
			t.Error("FileLocks should be true")
		}
		if !opts.GetExtUnixSk() {
			t.Error("ExtUnixSk should be true")
		}
		if !opts.GetLinkRemap() {
			t.Error("LinkRemap should be true")
		}
		if !opts.GetManageCgroups() {
			t.Error("ManageCgroups should be true")
		}
		if opts.GetManageCgroupsMode() != criurpc.CriuCgMode_SOFT {
			t.Errorf("ManageCgroupsMode = %v, want SOFT", opts.GetManageCgroupsMode())
		}
	})

	t.Run("imageIoMode direct sets IMAGE_IO_DIRECT", func(t *testing.T) {
		opts := &criurpc.CriuOpts{}
		settings := &types.CRIUSettings{ImageIoMode: "direct"}
		if err := applyCommonSettings(opts, settings); err != nil {
			t.Fatalf("applyCommonSettings: %v", err)
		}
		if opts.GetImageIoMode() != criurpc.CriuImageIoMode_IMAGE_IO_DIRECT {
			t.Errorf("ImageIoMode = %v, want IMAGE_IO_DIRECT", opts.GetImageIoMode())
		}
	})

	t.Run("imageIoMode empty defaults to IMAGE_IO_DIRECT", func(t *testing.T) {
		opts := &criurpc.CriuOpts{}
		settings := &types.CRIUSettings{}
		if err := applyCommonSettings(opts, settings); err != nil {
			t.Fatalf("applyCommonSettings: %v", err)
		}
		if opts.GetImageIoMode() != criurpc.CriuImageIoMode_IMAGE_IO_DIRECT {
			t.Errorf("ImageIoMode = %v, want IMAGE_IO_DIRECT", opts.GetImageIoMode())
		}
	})

	t.Run("imageIoMode writeback sets IMAGE_IO_WRITEBACK", func(t *testing.T) {
		opts := &criurpc.CriuOpts{}
		settings := &types.CRIUSettings{ImageIoMode: "writeback"}
		if err := applyCommonSettings(opts, settings); err != nil {
			t.Fatalf("applyCommonSettings: %v", err)
		}
		if opts.GetImageIoMode() != criurpc.CriuImageIoMode_IMAGE_IO_WRITEBACK {
			t.Errorf("ImageIoMode = %v, want IMAGE_IO_WRITEBACK", opts.GetImageIoMode())
		}
	})

	t.Run("invalid imageIoMode returns error", func(t *testing.T) {
		opts := &criurpc.CriuOpts{}
		settings := &types.CRIUSettings{ImageIoMode: "bogus"}
		if err := applyCommonSettings(opts, settings); err == nil {
			t.Error("expected error for invalid ImageIoMode")
		}
	})

	t.Run("invalid mode returns error", func(t *testing.T) {
		opts := &criurpc.CriuOpts{}
		settings := &types.CRIUSettings{ManageCgroupsMode: "invalid"}
		if err := applyCommonSettings(opts, settings); err == nil {
			t.Error("expected error for invalid ManageCgroupsMode")
		}
	})

	t.Run("conflicting tcp settings return error", func(t *testing.T) {
		opts := &criurpc.CriuOpts{}
		settings := &types.CRIUSettings{
			TcpClose:       true,
			TcpEstablished: true,
		}
		if err := applyCommonSettings(opts, settings); err == nil {
			t.Error("expected error for conflicting tcp settings")
		}
	})
}

func TestOverrideLibDir(t *testing.T) {
	t.Run("replaces existing libdir line", func(t *testing.T) {
		conf := "log-level 4\nlibdir /usr/local/lib/snapshot/criu-plugins\nshell-job\n"
		got := overrideLibDir(conf, "/tmp/snapshot-binaries/criu-plugins")
		if strings.Contains(got, "/usr/local/lib/snapshot/criu-plugins") {
			t.Error("old libdir should have been replaced")
		}
		if !strings.Contains(got, "libdir /tmp/snapshot-binaries/criu-plugins") {
			t.Errorf("new libdir not found in output: %q", got)
		}
	})

	t.Run("appends libdir when absent", func(t *testing.T) {
		conf := "log-level 4\nshell-job\n"
		got := overrideLibDir(conf, "/tmp/snapshot-binaries/criu-plugins")
		if !strings.Contains(got, "libdir /tmp/snapshot-binaries/criu-plugins") {
			t.Errorf("libdir not appended: %q", got)
		}
	})

	t.Run("handles empty config", func(t *testing.T) {
		got := overrideLibDir("", "/tmp/snapshot-binaries/criu-plugins")
		if !strings.Contains(got, "libdir /tmp/snapshot-binaries/criu-plugins") {
			t.Errorf("libdir not appended to empty config: %q", got)
		}
	})
}

func TestBuildRestoreExtMounts(t *testing.T) {
	t.Run("normal manifest with ExtMnt", func(t *testing.T) {
		m := &types.CheckpointManifest{
			CRIUDump: types.CRIUDumpManifest{
				ExtMnt: map[string]string{
					"/etc/hostname": "/etc/hostname",
					"/proc/acpi":    "/dev/null",
				},
			},
		}
		mounts, err := buildRestoreExtMounts(m)
		if err != nil {
			t.Fatalf("buildRestoreExtMounts: %v", err)
		}

		// Should contain value→value self-mappings plus "/" → "."
		mountMap := make(map[string]string, len(mounts))
		for _, em := range mounts {
			mountMap[em.GetKey()] = em.GetVal()
		}

		if mountMap["/"] != "." {
			t.Errorf("root mapping: got %q, want %q", mountMap["/"], ".")
		}
		if mountMap["/etc/hostname"] != "/etc/hostname" {
			t.Errorf("/etc/hostname mapping: got %q", mountMap["/etc/hostname"])
		}
		if mountMap["/dev/null"] != "/dev/null" {
			t.Errorf("/dev/null mapping: got %q", mountMap["/dev/null"])
		}
	})

	t.Run("values of / or empty are skipped", func(t *testing.T) {
		m := &types.CheckpointManifest{
			CRIUDump: types.CRIUDumpManifest{
				ExtMnt: map[string]string{
					"/root_mount": "/",
					"/empty_val":  "",
					"/good":       "/good",
				},
			},
		}
		mounts, err := buildRestoreExtMounts(m)
		if err != nil {
			t.Fatalf("buildRestoreExtMounts: %v", err)
		}

		mountMap := make(map[string]string, len(mounts))
		for _, em := range mounts {
			mountMap[em.GetKey()] = em.GetVal()
		}

		// "/" and "" values should be skipped from the value→value mapping
		// but "/" → "." root mapping always exists
		if mountMap["/"] != "." {
			t.Errorf("root mapping missing")
		}
		if _, ok := mountMap[""]; ok {
			t.Error("empty string should not be a key in restore map")
		}
		if mountMap["/good"] != "/good" {
			t.Errorf("/good mapping missing")
		}
	})

	t.Run("empty ExtMnt returns error", func(t *testing.T) {
		m := &types.CheckpointManifest{
			CRIUDump: types.CRIUDumpManifest{},
		}
		_, err := buildRestoreExtMounts(m)
		if err == nil {
			t.Error("expected error for empty ExtMnt")
		}
	})
}
