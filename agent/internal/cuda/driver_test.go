// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cuda

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/go-logr/logr"
	"golang.org/x/sys/unix"
)

func TestHelperDriverLibrary(t *testing.T) {
	for _, tc := range []struct {
		name      string
		host      string
		compat    string
		mapping   string
		wantError string
		unlink    bool
	}{
		{name: "host match", host: "driver-A"},
		{name: "compat match with different filename", host: "driver-B", compat: "driver-A"},
		{name: "unknown driver", host: "driver-B", wantError: "no agent-owned library matches"},
		{name: "same filename different content", host: "driver-B", compat: "driver-B", wantError: "no agent-owned library matches"},
		{name: "CPU process", mapping: "none"},
		{name: "repeated mapping of same inode", host: "driver-A", mapping: "repeated"},
		{name: "multiple mapped libraries", host: "driver-A", mapping: "multiple", wantError: "multiple CUDA libraries"},
		{name: "missing mapped file", host: "driver-A", mapping: "missing", wantError: "open mapped CUDA library"},
		{name: "mapped inode replaced", host: "driver-A", mapping: "replaced", wantError: "CUDA mapping changed"},
		{name: "trusted FD survives unlink", host: "driver-A", unlink: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			procDir := filepath.Join(dir, "proc", "123")
			compatDir := filepath.Join(dir, "cuda-compat")
			for _, path := range []string{filepath.Join(procDir, "map_files"), compatDir} {
				if err := os.MkdirAll(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			write := func(name, content string) string {
				t.Helper()
				path := filepath.Join(dir, name)
				if err := os.WriteFile(path, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
				return path
			}
			target := write("workload-driver", "driver-A")
			host := write("host-driver", tc.host)
			selected, companions := host, dir
			mappedName := "/workload/libcuda.so.615.0"
			if tc.compat != "" {
				companions = filepath.Join(compatDir, "libcuda.so.999.0")
				if err := os.Mkdir(companions, 0700); err != nil {
					t.Fatal(err)
				}
				selected = write("cuda-compat/libcuda.so.999.0/libcuda.so.1", tc.compat)
				if tc.name == "same filename different content" {
					mappedName = "/workload/" + filepath.Base(selected)
				}
			}
			var maps strings.Builder
			addMapping := func(address, backing, mappedPath string) {
				t.Helper()
				info, err := os.Stat(backing)
				if err != nil {
					t.Fatal(err)
				}
				stat := info.Sys().(*syscall.Stat_t)
				fmt.Fprintf(&maps, "%s r-xp 00000000 %02x:%02x %d %s\n", address,
					unix.Major(stat.Dev), unix.Minor(stat.Dev), stat.Ino, mappedPath)
				if tc.mapping == "missing" {
					return
				}
				if tc.mapping == "replaced" {
					backing = write("replacement", "driver-A")
				}
				if err := os.Link(backing, filepath.Join(procDir, "map_files", address)); err != nil {
					t.Fatal(err)
				}
			}
			if tc.mapping != "none" {
				addMapping("1000-2000", target, mappedName)
			}
			switch tc.mapping {
			case "repeated":
				addMapping("2000-3000", target, mappedName)
			case "multiple":
				addMapping("2000-3000", write("second-driver", "driver-A"), "/other/libcuda.so.595.0")
			}
			write("proc/123/maps", maps.String())

			driver, gotCompanions, err := helperDriverLibrary(procDir, host, compatDir)
			if driver != nil {
				defer driver.Close()
			}
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) || driver != nil {
					t.Fatalf("selection = %v, %v; want error containing %q and no driver", driver, err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.mapping == "none" {
				if driver != nil || gotCompanions != "" {
					t.Fatalf("CPU selection = %v, %q", driver, gotCompanions)
				}
				return
			}
			if driver == nil || driver.Name() != selected || gotCompanions != companions {
				t.Fatalf("selection = %v, %q; want %q, %q", driver, gotCompanions, selected, companions)
			}
			if tc.unlink {
				if err := os.Remove(selected); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := driver.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			content, err := io.ReadAll(driver)
			if err != nil || string(content) != "driver-A" {
				t.Fatalf("selected driver contents = %q, %v", content, err)
			}
		})
	}
}

func TestFilterProcessesCPUAndCancellation(t *testing.T) {
	if pids, err := FilterProcesses(context.Background(), []int{os.Getpid()}, logr.Discard()); err != nil || len(pids) != 0 {
		t.Fatalf("CPU process filter = %v, %v", pids, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := FilterProcesses(ctx, []int{os.Getpid()}, logr.Discard()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled filter error = %v", err)
	}
}
