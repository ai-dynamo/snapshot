// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cuda

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/ai-dynamo/snapshot/api/podcontract"
)

func writeMappedLibraries(t *testing.T, procRoot string, pid int, frontend, core string) string {
	t.Helper()
	process := filepath.Join(procRoot, strconv.Itoa(pid))
	directory := filepath.Join(process, "root", podcontract.CuInterposeMountPath)
	require.NoError(t, os.MkdirAll(directory, 0700))
	var maps string
	for _, library := range []struct{ name, contents string }{
		{"libcuinterpose.so", frontend}, {"libcuinterpose_core.so", core},
	} {
		if library.contents == "" {
			continue
		}
		path := filepath.Join(directory, library.name)
		require.NoError(t, os.WriteFile(path, []byte(library.contents), 0600))
		info, err := os.Stat(path)
		require.NoError(t, err)
		stat := info.Sys().(*syscall.Stat_t)
		maps += fmt.Sprintf("1000-2000 r-xp 00000000 %02x:%02x %d %s\n",
			unix.Major(stat.Dev), unix.Minor(stat.Dev), stat.Ino, filepath.Join(podcontract.CuInterposeMountPath, library.name))
	}
	require.NoError(t, os.WriteFile(filepath.Join(process, "maps"), []byte(maps), 0600))
	return directory
}

func TestInspectCuInterposeLibraries(t *testing.T) {
	for _, tc := range []struct {
		name          string
		first, second [2]string
		requested     bool
		wantLoaded    bool
		wantError     string
	}{
		{name: "native"},
		{name: "requested but absent", requested: true, wantError: "not active"},
		{name: "loaded but not requested", first: [2]string{"front", "core"}, second: [2]string{"front", "core"}, wantLoaded: true},
		{name: "delivered", requested: true, first: [2]string{"front", "core"}, second: [2]string{"front", "core"}, wantLoaded: true},
		{name: "partial coverage", first: [2]string{"front", "core"}, wantError: "participants [2]"},
		{name: "missing core", first: [2]string{"front", ""}, wantError: "libcuinterpose_core.so"},
		{name: "core without frontend", first: [2]string{"", "core"}, wantError: "frontend must be mapped"},
		{name: "different frontend", first: [2]string{"front", "core"}, second: [2]string{"other", "core"}, wantError: "hashes differ"},
		{name: "different core", first: [2]string{"front", "core"}, second: [2]string{"front", "diff"}, wantError: "hashes differ"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			procRoot := t.TempDir()
			writeMappedLibraries(t, procRoot, 1, tc.first[0], tc.first[1])
			writeMappedLibraries(t, procRoot, 2, tc.second[0], tc.second[1])
			inspection, err := InspectCuInterposeLibraries(procRoot, []int{1, 2}, tc.requested)
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.wantLoaded, inspection != nil)
		})
	}
	_, err := InspectCuInterposeLibraries(t.TempDir(), nil, true)
	require.ErrorContains(t, err, "not active")
}

func TestInspectCuInterposeRejectsReplacedOrDeletedMappings(t *testing.T) {
	for _, tc := range []struct {
		name      string
		deleted   bool
		wantError string
	}{
		{name: "replaced", wantError: "was replaced since it was loaded"},
		{name: "deleted", deleted: true, wantError: "is deleted or has an unsupported path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			procRoot := t.TempDir()
			dir := writeMappedLibraries(t, procRoot, 1, "front", "core")
			if tc.deleted {
				maps := filepath.Join(procRoot, "1/maps")
				contents, err := os.ReadFile(maps)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(maps, append(contents[:len(contents)-1], []byte(" (deleted)\n")...), 0600))
			} else {
				path := filepath.Join(dir, "libcuinterpose.so")
				require.NoError(t, os.Rename(path, path+".old"))
				require.NoError(t, os.WriteFile(path, []byte("front"), 0600))
			}
			_, err := InspectCuInterposeLibraries(procRoot, []int{1}, false)
			require.ErrorContains(t, err, tc.wantError)
		})
	}
}

func TestInspectCuInterposeRepeatedMappingRegions(t *testing.T) {
	for _, tc := range []struct {
		name         string
		changedField int
		value        string
	}{
		{name: "consistent"},
		{name: "conflicting device", changedField: 3, value: "ff:ff"},
		{name: "conflicting inode", changedField: 4, value: "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			procRoot := t.TempDir()
			writeMappedLibraries(t, procRoot, 1, "front", "core")
			mapsPath := filepath.Join(procRoot, "1/maps")
			contents, err := os.ReadFile(mapsPath)
			require.NoError(t, err)
			var regions []string
			for i, line := range strings.Split(strings.TrimSpace(string(contents)), "\n") {
				fields := strings.Fields(line)
				fields[0] = fmt.Sprintf("%x-%x", 0x1000+i*0x3000, 0x2000+i*0x3000)
				regions = append(regions, strings.Join(fields, " "))
				fields[0] = fmt.Sprintf("%x-%x", 0x2000+i*0x3000, 0x3000+i*0x3000)
				fields[1], fields[2] = "rw-p", "00001000"
				if i == 0 && tc.changedField != 0 {
					fields[tc.changedField] = tc.value
				}
				regions = append(regions, strings.Join(fields, " "))
			}
			require.NoError(t, os.WriteFile(mapsPath, []byte(strings.Join(regions, "\n")+"\n"), 0600))
			inspection, err := InspectCuInterposeLibraries(procRoot, []int{1}, true)
			if tc.changedField != 0 {
				require.ErrorContains(t, err, "conflicting device/inode identities")
				return
			}
			require.NoError(t, err)
			require.Equal(t, fmt.Sprintf("%x", sha256.Sum256([]byte("front"))), inspection.libraries[types.CuInterposeFrontend].SHA256)
			require.Equal(t, fmt.Sprintf("%x", sha256.Sum256([]byte("core"))), inspection.libraries[types.CuInterposeCore].SHA256)
		})
	}
}

func TestInspectCuInterposeRejectsUnsupportedPathWhenNotRequested(t *testing.T) {
	procRoot := t.TempDir()
	writeMappedLibraries(t, procRoot, 1, "front", "core")
	maps := filepath.Join(procRoot, "1/maps")
	contents, err := os.ReadFile(maps)
	require.NoError(t, err)
	contents = []byte(strings.ReplaceAll(string(contents), podcontract.CuInterposeMountPath, "/some directory"))
	require.NoError(t, os.WriteFile(maps, contents, 0600))
	_, err = InspectCuInterposeLibraries(procRoot, []int{1}, false)
	require.ErrorContains(t, err, "must be delivered")
}

func TestVerifyCuInterposeLibraryIdentity(t *testing.T) {
	procRoot := t.TempDir()
	directory := writeMappedLibraries(t, procRoot, 1, "front", "core")
	inspection, err := InspectCuInterposeLibraries(procRoot, []int{1}, true)
	require.NoError(t, err)
	identity := inspection.Manifest([]int{1}, []int{1})
	require.NoError(t, VerifyCuInterposeLibraryIdentity(directory, identity))
	for _, name := range []string{"libcuinterpose.so", "libcuinterpose_core.so"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(directory, name)
			original, err := os.ReadFile(path)
			require.NoError(t, err)
			changed := append([]byte(nil), original...)
			changed[0] ^= 1 // Same file size must not imply compatibility.
			require.NoError(t, os.WriteFile(path, changed, 0600))
			err = VerifyCuInterposeLibraryIdentity(directory, identity)
			require.ErrorContains(t, err, name+" SHA-256 mismatch")
			require.ErrorContains(t, err, fmt.Sprintf("expected %x", sha256.Sum256(original)))
			require.ErrorContains(t, err, fmt.Sprintf("actual %x", sha256.Sum256(changed)))
			require.NoError(t, os.Remove(path))
			err = VerifyCuInterposeLibraryIdentity(directory, identity)
			require.ErrorIs(t, err, os.ErrNotExist)
			require.ErrorContains(t, err, "open restore library "+name)
			require.NoError(t, os.WriteFile(path, original, 0600))
		})
	}
}

func TestInspectFrontendOnlyCUDAProcesses(t *testing.T) {
	for _, tc := range []struct {
		name         string
		workerActive bool
		parentCore   string
		wantError    string
	}{
		{name: "mixed", workerActive: true, parentCore: "core"},
		{name: "all frontend only", parentCore: "core"},
		{name: "missing core", workerActive: true, wantError: "libcuinterpose_core.so"},
		{name: "different inactive core", workerActive: true, parentCore: "diff", wantError: "hashes differ"},
		{name: "nonregular inactive core", workerActive: true, parentCore: "fifo", wantError: "regular file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			procRoot := t.TempDir()
			parentDir := writeMappedLibraries(t, procRoot, 1001, "front", "")
			corePath := filepath.Join(parentDir, "libcuinterpose_core.so")
			if tc.parentCore == "fifo" {
				require.NoError(t, unix.Mkfifo(corePath, 0600))
			} else if tc.parentCore != "" {
				require.NoError(t, os.WriteFile(corePath, []byte(tc.parentCore), 0600))
			}
			if tc.workerActive {
				writeMappedLibraries(t, procRoot, 1623, "front", "core")
			} else {
				workerDir := writeMappedLibraries(t, procRoot, 1623, "front", "")
				require.NoError(t, os.WriteFile(filepath.Join(workerDir, "libcuinterpose_core.so"), []byte("core"), 0600))
			}
			// No coordinator sockets exist. A mapped core must still be selected so a
			// missing endpoint fails coordinator inspection, not coverage.
			hostPIDs := []int{1001, 1623}
			inspection, err := InspectCuInterposeLibraries(procRoot, hostPIDs, true)
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
				return
			}
			require.NoError(t, err)
			require.Equal(t, fmt.Sprintf("%x", sha256.Sum256([]byte("front"))), inspection.libraries[types.CuInterposeFrontend].SHA256)
			require.Equal(t, fmt.Sprintf("%x", sha256.Sum256([]byte("core"))), inspection.libraries[types.CuInterposeCore].SHA256)
			identity := inspection.Manifest(hostPIDs, []int{1, 623})
			if tc.workerActive {
				require.Equal(t, []int{1623}, inspection.coordinatorHostPIDs)
				require.Equal(t, []int{623}, identity.PIDs)
			} else {
				require.Empty(t, inspection.coordinatorHostPIDs)
				require.Equal(t, []int{}, identity.PIDs)
			}
			require.NoError(t, VerifyCuInterposeLibraryIdentity(parentDir, identity))
			require.NoError(t, os.WriteFile(corePath, []byte("diff"), 0600))
			require.ErrorContains(t, VerifyCuInterposeLibraryIdentity(parentDir, identity), "SHA-256 mismatch")
		})
	}
}
