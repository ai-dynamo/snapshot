// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/go-logr/logr/testr"
	"golang.org/x/sys/unix"

	"github.com/ai-dynamo/snapshot/agent/internal/nsmount"

	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

// testBundle lays out a bundle with the real directory structure, a shim in
// place of the ELF loader, and the host tar as the bundled tar. The shim drops
// the loader's own flags and execs the rest, so tests exercise the argv
// restoreTarCmd actually builds against a real extraction, without needing to
// ship a glibc.
func testBundle(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	libcDir := filepath.Join(dir, nsmount.BundleLibcDir)
	if err := os.MkdirAll(libcDir, 0o755); err != nil {
		t.Fatalf("create bundle libc dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, nsmount.BundleLibDir), 0o755); err != nil {
		t.Fatalf("create bundle lib dir: %v", err)
	}

	shim := "#!/bin/sh\n[ \"$1\" = \"--library-path\" ] && shift 2\nexec \"$@\"\n"
	if err := os.WriteFile(filepath.Join(libcDir, nsmount.BundleLoader), []byte(shim), 0o755); err != nil {
		t.Fatalf("write loader shim: %v", err)
	}

	hostTar, err := exec.LookPath("tar")
	if err != nil {
		t.Skipf("tar not available: %v", err)
	}
	if err := os.Symlink(hostTar, filepath.Join(dir, nsmount.BundleTar)); err != nil {
		t.Fatalf("link bundle tar: %v", err)
	}
	return dir
}

func TestRestoreTarCmd(t *testing.T) {
	t.Run("invokes the bundled tar through the bundled loader", func(t *testing.T) {
		cmd, err := restoreTarCmd("/tmp/snapshot-binaries", "-xf", "/tmp/diff.tar")
		if err != nil {
			t.Fatalf("restoreTarCmd: %v", err)
		}

		wantLoader := "/tmp/snapshot-binaries/libc/" + nsmount.BundleLoader
		if cmd.Path != wantLoader {
			t.Fatalf("exec path = %q, want the bundled loader %q", cmd.Path, wantLoader)
		}
		want := []string{
			wantLoader,
			"--library-path", "/tmp/snapshot-binaries/libc:/tmp/snapshot-binaries/lib",
			"/tmp/snapshot-binaries/tar",
			"-xf", "/tmp/diff.tar",
		}
		if !slices.Equal(cmd.Args, want) {
			t.Fatalf("argv = %q, want %q", cmd.Args, want)
		}
	})

	t.Run("bundle libc precedes bundle lib on the library path", func(t *testing.T) {
		// lib/ holds the agent's non-glibc closure; libc/ must win so the
		// loader and libc stay the matched pair glibc requires.
		cmd, err := restoreTarCmd("/bundle")
		if err != nil {
			t.Fatalf("restoreTarCmd: %v", err)
		}
		i := slices.Index(cmd.Args, "--library-path")
		if i < 0 || i+1 >= len(cmd.Args) {
			t.Fatalf("no --library-path in argv %q", cmd.Args)
		}
		if got, want := cmd.Args[i+1], "/bundle/libc:/bundle/lib"; got != want {
			t.Fatalf("library path = %q, want %q", got, want)
		}
	})

	t.Run("a non-absolute bundle dir is rejected", func(t *testing.T) {
		// Falling back to a PATH lookup here would find the placeholder's own
		// tar, which is the failure the bundled loader exists to prevent.
		for _, bundleDir := range []string{"", "snapshot-binaries", "./rel"} {
			if _, err := restoreTarCmd(bundleDir, "--version"); err == nil {
				t.Fatalf("restoreTarCmd(%q) should fail", bundleDir)
			}
		}
	})
}

func TestCaptureTarArgs(t *testing.T) {
	args := captureTarArgs("/upper", "/out.tar", types.OverlaySettings{Exclusions: []string{"/var/log"}}, []string{"/data"})

	// Load-bearing, not cosmetic — see captureTarArgs.
	if !slices.Contains(args, "--xattrs-exclude=trusted.*") {
		t.Fatalf("args %q must exclude the trusted.* xattr namespace", args)
	}
	if !slices.Contains(args, "--xattrs") {
		t.Fatalf("args %q must archive xattrs", args)
	}
	for _, want := range []string{"--exclude=./var/log", "--exclude=./data"} {
		if !slices.Contains(args, want) {
			t.Fatalf("args %q missing %q", args, want)
		}
	}
	if got, want := args[len(args)-5:], []string{"-C", "/upper", "-cf", "/out.tar", "."}; !slices.Equal(got, want) {
		t.Fatalf("trailing args = %q, want %q", got, want)
	}
}

func TestBuildExclusions(t *testing.T) {
	tests := []struct {
		name     string
		settings types.OverlaySettings
		want     map[string]bool // expected entries (true = must be present)
	}{
		{
			name: "normalizes rooted paths",
			settings: types.OverlaySettings{
				Exclusions: []string{"/proc", "/sys", "/root/.cache", "/tmp"},
			},
			want: map[string]bool{
				"./proc":        true,
				"./sys":         true,
				"./root/.cache": true,
				"./tmp":         true,
			},
		},
		{
			name: "strips leading dot and slash before prepending ./",
			settings: types.OverlaySettings{
				Exclusions: []string{"./proc", "/sys", "tmp"},
			},
			want: map[string]bool{
				"./proc": true,
				"./sys":  true,
				"./tmp":  true,
			},
		},
		{
			name: "glob patterns starting with * are untouched",
			settings: types.OverlaySettings{
				Exclusions: []string{"*/.cache/huggingface", "*.pyc", "*/__pycache__"},
			},
			want: map[string]bool{
				"*/.cache/huggingface": true,
				"*.pyc":                true,
				"*/__pycache__":        true,
			},
		},
		{
			name:     "empty settings produces empty slice",
			settings: types.OverlaySettings{},
			want:     map[string]bool{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := buildExclusions(tc.settings)
			gotSet := make(map[string]bool, len(got))
			for _, v := range got {
				gotSet[v] = true
			}
			for expected := range tc.want {
				if !gotSet[expected] {
					t.Errorf("expected %q in exclusions, got %v", expected, got)
				}
			}
			if len(got) != len(tc.want) {
				t.Errorf("len(exclusions) = %d, want %d; got %v", len(got), len(tc.want), got)
			}
		})
	}
}

func TestFindWhiteoutFiles(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, dir string) // create files in temp dir
		want  []string
	}{
		{
			name: "top-level whiteout",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, ".wh.somefile"), nil, 0644); err != nil {
					t.Fatalf("write whiteout: %v", err)
				}
			},
			want: []string{"somefile"},
		},
		{
			name: "nested whiteout returns relative path",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				sub := filepath.Join(dir, "subdir")
				if err := os.MkdirAll(sub, 0755); err != nil {
					t.Fatalf("mkdir subdir: %v", err)
				}
				if err := os.WriteFile(filepath.Join(sub, ".wh.nested"), nil, 0644); err != nil {
					t.Fatalf("write nested whiteout: %v", err)
				}
			},
			want: []string{"subdir/nested"},
		},
		{
			name: "no whiteouts returns empty",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, "regular"), nil, 0644); err != nil {
					t.Fatalf("write regular file: %v", err)
				}
			},
			want: nil,
		},
		{
			name:  "empty dir returns empty",
			setup: func(*testing.T, string) {},
			want:  nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.setup(t, dir)
			got, err := findWhiteoutFiles(dir)
			if err != nil {
				t.Fatalf("findWhiteoutFiles: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("got[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestCaptureRootfsDiff(t *testing.T) {
	t.Run("writes valid archive atomically", func(t *testing.T) {
		upperDir := t.TempDir()
		checkpointDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(upperDir, "generated.txt"), []byte("runtime data"), 0644); err != nil {
			t.Fatalf("write upperdir file: %v", err)
		}

		got, err := CaptureRootfsDiff(upperDir, checkpointDir, RootfsDiffOptions{})
		if err != nil {
			t.Fatalf("CaptureRootfsDiff: %v", err)
		}
		want := filepath.Join(checkpointDir, rootfsDiffFilename)
		if got.Path != want {
			t.Fatalf("got path %q, want %q", got.Path, want)
		}
		info, err := os.Stat(want)
		if err != nil {
			t.Fatalf("stat rootfs diff: %v", err)
		}
		if info.Size() == 0 {
			t.Fatal("rootfs diff should not be empty")
		}
		matches, err := filepath.Glob(filepath.Join(checkpointDir, rootfsDiffFilename+".*.tmp"))
		if err != nil {
			t.Fatalf("glob temp files: %v", err)
		}
		if len(matches) != 0 {
			t.Fatalf("temporary rootfs diff files were not cleaned up: %v", matches)
		}

		targetRoot := t.TempDir()
		if err := ApplyRootfsDiff(checkpointDir, targetRoot, testBundle(t), testr.New(t)); err != nil {
			t.Fatalf("ApplyRootfsDiff: %v", err)
		}
		data, err := os.ReadFile(filepath.Join(targetRoot, "generated.txt"))
		if err != nil {
			t.Fatalf("read extracted file: %v", err)
		}
		if string(data) != "runtime data" {
			t.Fatalf("extracted data = %q, want %q", string(data), "runtime data")
		}
	})

	t.Run("failed capture does not publish partial archive", func(t *testing.T) {
		checkpointDir := t.TempDir()
		missingUpperDir := filepath.Join(t.TempDir(), "missing")

		if _, err := CaptureRootfsDiff(missingUpperDir, checkpointDir, RootfsDiffOptions{}); err == nil {
			t.Fatal("CaptureRootfsDiff should fail for missing upperdir")
		}
		assertNoRootfsDiff(t, checkpointDir)
	})

	t.Run("exclusions keep listed files", func(t *testing.T) {
		upperDir := t.TempDir()
		checkpointDir := t.TempDir()
		writeUpperFile(t, upperDir, "root/.triton/cache/K/cuda_utils.so", "mapped launcher")
		writeUpperFile(t, upperDir, "root/.triton/cache/K/kernel.cubin", "cached kernel")
		writeUpperFile(t, upperDir, "root/.cache/vllm/torch_compile_cache/model", "compiled graph")
		writeUpperFile(t, upperDir, "root/.cache/flashinfer/jit.log", "open log")
		writeUpperFile(t, upperDir, "root/kept.txt", "not excluded")
		// Ancestor modes must survive even though the exclusion dropped the directories.
		if err := os.Chmod(filepath.Join(upperDir, "root/.triton/cache/K"), 0o700); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		// An upperdir symlink must never be followed to reach a kept name.
		outside := t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("host file"), 0o600); err != nil {
			t.Fatalf("write outside file: %v", err)
		}
		if err := os.Symlink(outside, filepath.Join(upperDir, "root/.cache/link")); err != nil {
			t.Fatalf("symlink: %v", err)
		}

		diff, err := CaptureRootfsDiff(upperDir, checkpointDir, RootfsDiffOptions{
			Exclusions: types.OverlaySettings{Exclusions: []string{"*/.triton", "*/.cache/vllm", "*/.cache/flashinfer", "*/.cache/link"}},
			Keep: []string{
				"/root/.triton/cache/K/cuda_utils.so",
				"/root/.cache/flashinfer/jit.log",
				"/root/kept.txt",
				"/root/.cache/link/secret",
				"/usr/lib/x86_64-linux-gnu/libc.so.6", // in the image, not the upperdir
				"/",
			},
		})
		if err != nil {
			t.Fatalf("CaptureRootfsDiff: %v", err)
		}
		wantKept := []string{"/root/.cache/flashinfer/jit.log", "/root/.triton/cache/K/cuda_utils.so"}
		if !slices.Equal(diff.Kept, wantKept) {
			t.Errorf("Kept = %q, want %q", diff.Kept, wantKept)
		}
		if want := int64(len("open log") + len("mapped launcher")); diff.KeptBytes != want {
			t.Errorf("KeptBytes = %d, want %d", diff.KeptBytes, want)
		}

		listing, err := exec.Command("tar", "-tf", diff.Path).Output()
		if err != nil {
			t.Fatalf("list rootfs diff: %v", err)
		}
		counts := make(map[string]int)
		for _, name := range strings.Split(strings.TrimSpace(string(listing)), "\n") {
			counts[name]++
		}
		for name, count := range counts {
			if count != 1 {
				t.Errorf("member %q archived %d times", name, count)
			}
		}

		targetRoot := t.TempDir()
		if err := ApplyRootfsDiff(checkpointDir, targetRoot, testBundle(t), testr.New(t)); err != nil {
			t.Fatalf("ApplyRootfsDiff: %v", err)
		}
		for rel, want := range map[string]string{
			"root/.triton/cache/K/cuda_utils.so": "mapped launcher",
			"root/.cache/flashinfer/jit.log":     "open log",
			"root/kept.txt":                      "not excluded",
		} {
			data, err := os.ReadFile(filepath.Join(targetRoot, rel))
			if err != nil || string(data) != want {
				t.Errorf("%s = %q, %v; want %q", rel, data, err, want)
			}
		}
		for _, rel := range []string{
			"root/.triton/cache/K/kernel.cubin",
			"root/.cache/vllm",
			"root/.cache/link",
		} {
			if _, err := os.Lstat(filepath.Join(targetRoot, rel)); !os.IsNotExist(err) {
				t.Errorf("%s should not be restored, stat error: %v", rel, err)
			}
		}
		info, err := os.Stat(filepath.Join(targetRoot, "root/.triton/cache/K"))
		if err != nil {
			t.Fatalf("stat restored ancestor: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Errorf("restored ancestor mode = %o, want 700", got)
		}
	})

	t.Run("a newline in another name does not hide a kept file", func(t *testing.T) {
		upperDir := t.TempDir()
		checkpointDir := t.TempDir()
		writeUpperFile(t, upperDir, "needed.txt", "kept")
		writeUpperFile(t, upperDir, "other\nneeded.txt", "archived")

		diff, err := CaptureRootfsDiff(upperDir, checkpointDir, RootfsDiffOptions{
			Exclusions: types.OverlaySettings{Exclusions: []string{"/needed.txt"}},
			Keep:       []string{"/needed.txt"},
		})
		if err != nil {
			t.Fatalf("CaptureRootfsDiff: %v", err)
		}
		if want := []string{"/needed.txt"}; !slices.Equal(diff.Kept, want) {
			t.Errorf("Kept = %q, want %q", diff.Kept, want)
		}
		targetRoot := t.TempDir()
		if err := ApplyRootfsDiff(checkpointDir, targetRoot, testBundle(t), testr.New(t)); err != nil {
			t.Fatalf("ApplyRootfsDiff: %v", err)
		}
		for rel, want := range map[string]string{"needed.txt": "kept", "other\nneeded.txt": "archived"} {
			data, err := os.ReadFile(filepath.Join(targetRoot, rel))
			if err != nil || string(data) != want {
				t.Errorf("%q = %q, %v; want %q", rel, data, err, want)
			}
		}
	})

	t.Run("an unexpected inspection error fails the capture", func(t *testing.T) {
		upperDir := t.TempDir()
		checkpointDir := t.TempDir()
		writeUpperFile(t, upperDir, "root/kept.txt", "kept")
		// A component longer than NAME_MAX fails Lstat with ENAMETOOLONG, which
		// is neither "missing" nor a reason to drop the path silently.
		tooLong := "/root/" + strings.Repeat("x", 300)

		_, err := CaptureRootfsDiff(upperDir, checkpointDir, RootfsDiffOptions{Keep: []string{"/root/kept.txt", tooLong}})
		if !errors.Is(err, unix.ENAMETOOLONG) || !errors.Is(err, ErrKeptFilesMissing) {
			t.Fatalf("CaptureRootfsDiff() error = %v, want ENAMETOOLONG and ErrKeptFilesMissing", err)
		}
		assertNoRootfsDiff(t, checkpointDir)
	})

	// A non-empty directory at the archive's final path fails the publish,
	// after the kept paths were inspected and the archive was written.
	blockPublish := func(t *testing.T, checkpointDir string) {
		t.Helper()
		writeUpperFile(t, checkpointDir, filepath.Join(rootfsDiffFilename, "occupied"), "")
	}

	t.Run("a later failure with a kept file in the upperdir is marked", func(t *testing.T) {
		upperDir := t.TempDir()
		checkpointDir := t.TempDir()
		writeUpperFile(t, upperDir, "root/kept.txt", "kept")
		blockPublish(t, checkpointDir)

		_, err := CaptureRootfsDiff(upperDir, checkpointDir, RootfsDiffOptions{Keep: []string{"/root/kept.txt"}})
		if !errors.Is(err, ErrKeptFilesMissing) || !strings.Contains(err.Error(), "failed to publish rootfs diff") {
			t.Fatalf("CaptureRootfsDiff() error = %v, want a publish failure marked ErrKeptFilesMissing", err)
		}
	})

	t.Run("a failure without kept files in the upperdir is not marked", func(t *testing.T) {
		upperDir := t.TempDir()
		checkpointDir := t.TempDir()
		writeUpperFile(t, upperDir, "root/other.txt", "not kept")
		blockPublish(t, checkpointDir)

		// The image provides the file; the upperdir has no copy to lose.
		_, err := CaptureRootfsDiff(upperDir, checkpointDir, RootfsDiffOptions{Keep: []string{"/usr/lib/x86_64-linux-gnu/libc.so.6"}})
		if err == nil || errors.Is(err, ErrKeptFilesMissing) {
			t.Fatalf("CaptureRootfsDiff() error = %v, want an unmarked publish failure", err)
		}
	})
}

func TestUnarchivedMembers(t *testing.T) {
	archived := map[string]struct{}{"": {}, "root": {}, "root/kept.txt": {}, "usr": {}}
	members, appended := unarchivedMembers([]string{"root/.triton/cache/K/a.so", "root/.triton/cache/K/b.so", "root/kept.txt"}, archived)

	wantMembers := []string{"./root/.triton", "./root/.triton/cache", "./root/.triton/cache/K", "./root/.triton/cache/K/a.so", "./root/.triton/cache/K/b.so"}
	if !slices.Equal(members, wantMembers) {
		t.Errorf("members = %q, want %q", members, wantMembers)
	}
	wantAppended := []string{"root/.triton/cache/K/a.so", "root/.triton/cache/K/b.so"}
	if !slices.Equal(appended, wantAppended) {
		t.Errorf("appended = %q, want %q", appended, wantAppended)
	}
	if _, ok := archived["root/.triton"]; ok {
		t.Error("unarchivedMembers modified the archived set")
	}
}

func writeUpperFile(t *testing.T, upperDir, rel, data string) {
	t.Helper()
	path := filepath.Join(upperDir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %q: %v", rel, err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("write %q: %v", rel, err)
	}
}

func assertNoRootfsDiff(t *testing.T, checkpointDir string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(checkpointDir, rootfsDiffFilename)); !os.IsNotExist(err) {
		t.Fatalf("rootfs diff should not be published after failure, stat error: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(checkpointDir, rootfsDiffFilename+".*.tmp"))
	if err != nil {
		t.Fatalf("glob temp files: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary rootfs diff files were not cleaned up: %v", matches)
	}
}

func TestApplyRootfsDiff(t *testing.T) {
	t.Run("missing archive is no-op", func(t *testing.T) {
		if err := ApplyRootfsDiff(t.TempDir(), t.TempDir(), testBundle(t), testr.New(t)); err != nil {
			t.Fatalf("ApplyRootfsDiff: %v", err)
		}
	})

	t.Run("empty archive is no-op", func(t *testing.T) {
		checkpointDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(checkpointDir, rootfsDiffFilename), nil, 0644); err != nil {
			t.Fatalf("write empty rootfs diff: %v", err)
		}
		if err := ApplyRootfsDiff(checkpointDir, t.TempDir(), testBundle(t), testr.New(t)); err != nil {
			t.Fatalf("ApplyRootfsDiff: %v", err)
		}
	})

	t.Run("non-empty invalid archive fails", func(t *testing.T) {
		checkpointDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(checkpointDir, rootfsDiffFilename), []byte("not a tar archive"), 0644); err != nil {
			t.Fatalf("write invalid rootfs diff: %v", err)
		}
		if err := ApplyRootfsDiff(checkpointDir, t.TempDir(), testBundle(t), testr.New(t)); err == nil {
			t.Fatal("ApplyRootfsDiff should fail for invalid non-empty archive")
		}
	})

	t.Run("non-ENOENT stat error is propagated", func(t *testing.T) {
		// Pass a regular file as checkpointPath so stat of
		// checkpointPath/rootfs-diff.tar returns ENOTDIR, not ENOENT.
		f, err := os.CreateTemp(t.TempDir(), "not-a-dir")
		if err != nil {
			t.Fatalf("create temp file: %v", err)
		}
		f.Close()
		if err := ApplyRootfsDiff(f.Name(), t.TempDir(), testBundle(t), testr.New(t)); err == nil {
			t.Fatal("ApplyRootfsDiff should propagate non-ENOENT stat error")
		}
	})

	t.Run("staged local copy is removed after extract", func(t *testing.T) {
		upperDir := t.TempDir()
		checkpointDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(upperDir, "generated.txt"), []byte("runtime data"), 0644); err != nil {
			t.Fatalf("write upperdir file: %v", err)
		}
		if _, err := CaptureRootfsDiff(upperDir, checkpointDir, RootfsDiffOptions{}); err != nil {
			t.Fatalf("CaptureRootfsDiff: %v", err)
		}

		before, err := filepath.Glob(filepath.Join(os.TempDir(), rootfsDiffFilename+".*.tmp"))
		if err != nil {
			t.Fatalf("glob staged copies: %v", err)
		}
		if err := ApplyRootfsDiff(checkpointDir, t.TempDir(), testBundle(t), testr.New(t)); err != nil {
			t.Fatalf("ApplyRootfsDiff: %v", err)
		}
		after, err := filepath.Glob(filepath.Join(os.TempDir(), rootfsDiffFilename+".*.tmp"))
		if err != nil {
			t.Fatalf("glob staged copies: %v", err)
		}
		if len(after) != len(before) {
			t.Fatalf("staged local copies leaked: before %v after %v", before, after)
		}
	})
}

func TestCaptureDeletedFiles(t *testing.T) {
	t.Run("dir with whiteouts writes JSON and returns true", func(t *testing.T) {
		upperDir := t.TempDir()
		checkpointDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(upperDir, ".wh.removed"), nil, 0644); err != nil {
			t.Fatalf("write whiteout: %v", err)
		}

		found, err := CaptureDeletedFiles(upperDir, checkpointDir)
		if err != nil {
			t.Fatalf("CaptureDeletedFiles: %v", err)
		}
		if !found {
			t.Fatal("expected found=true")
		}

		data, err := os.ReadFile(filepath.Join(checkpointDir, deletedFilesFilename))
		if err != nil {
			t.Fatalf("read deleted-files.json: %v", err)
		}
		var files []string
		if err := json.Unmarshal(data, &files); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(files) != 1 || files[0] != "removed" {
			t.Errorf("got %v, want [removed]", files)
		}
	})

	t.Run("dir with no whiteouts returns false and no file", func(t *testing.T) {
		upperDir := t.TempDir()
		checkpointDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(upperDir, "normalfile"), nil, 0644); err != nil {
			t.Fatalf("write regular file: %v", err)
		}

		found, err := CaptureDeletedFiles(upperDir, checkpointDir)
		if err != nil {
			t.Fatalf("CaptureDeletedFiles: %v", err)
		}
		if found {
			t.Fatal("expected found=false")
		}
		if _, err := os.Stat(filepath.Join(checkpointDir, deletedFilesFilename)); !os.IsNotExist(err) {
			t.Error("deleted-files.json should not exist")
		}
	})

	t.Run("empty upperDir returns false", func(t *testing.T) {
		found, err := CaptureDeletedFiles("", t.TempDir())
		if err != nil {
			t.Fatalf("CaptureDeletedFiles: %v", err)
		}
		if found {
			t.Fatal("expected found=false for empty upperDir")
		}
	})
}

func TestApplyDeletedFiles(t *testing.T) {
	log := testr.New(t)

	t.Run("deletes listed files from target", func(t *testing.T) {
		checkpointDir := t.TempDir()
		targetRoot := t.TempDir()

		// Create target file that should be deleted
		if err := os.WriteFile(filepath.Join(targetRoot, "old-cache"), []byte("data"), 0644); err != nil {
			t.Fatalf("write target file: %v", err)
		}

		// Write deleted-files.json
		data, err := json.Marshal([]string{"old-cache"})
		if err != nil {
			t.Fatalf("marshal deleted files: %v", err)
		}
		if err := os.WriteFile(filepath.Join(checkpointDir, deletedFilesFilename), data, 0644); err != nil {
			t.Fatalf("write deleted-files.json: %v", err)
		}

		if err := ApplyDeletedFiles(checkpointDir, targetRoot, log); err != nil {
			t.Fatalf("ApplyDeletedFiles: %v", err)
		}

		if _, err := os.Stat(filepath.Join(targetRoot, "old-cache")); !os.IsNotExist(err) {
			t.Error("old-cache should have been deleted")
		}
	})

	t.Run("missing deleted-files.json is a no-op", func(t *testing.T) {
		if err := ApplyDeletedFiles(t.TempDir(), t.TempDir(), log); err != nil {
			t.Fatalf("ApplyDeletedFiles: %v", err)
		}
	})

	t.Run("path traversal entry is skipped", func(t *testing.T) {
		checkpointDir := t.TempDir()
		targetRoot := t.TempDir()

		// Create a file outside targetRoot that the traversal would try to delete
		outsideDir := t.TempDir()
		secretFile := filepath.Join(outsideDir, "passwd")
		if err := os.WriteFile(secretFile, []byte("secret"), 0644); err != nil {
			t.Fatalf("write secret file: %v", err)
		}

		// Construct a relative path that escapes targetRoot
		rel, err := filepath.Rel(targetRoot, secretFile)
		if err != nil {
			t.Fatalf("build relative path: %v", err)
		}
		data, err := json.Marshal([]string{rel})
		if err != nil {
			t.Fatalf("marshal deleted files: %v", err)
		}
		if err := os.WriteFile(filepath.Join(checkpointDir, deletedFilesFilename), data, 0644); err != nil {
			t.Fatalf("write deleted-files.json: %v", err)
		}

		if err := ApplyDeletedFiles(checkpointDir, targetRoot, log); err != nil {
			t.Fatalf("ApplyDeletedFiles: %v", err)
		}

		// The file outside targetRoot must still exist
		if _, err := os.Stat(secretFile); err != nil {
			t.Error("path traversal should have been blocked, but file was deleted")
		}
	})

	t.Run("already-missing file causes no error", func(t *testing.T) {
		checkpointDir := t.TempDir()
		targetRoot := t.TempDir()

		data, err := json.Marshal([]string{"nonexistent"})
		if err != nil {
			t.Fatalf("marshal deleted files: %v", err)
		}
		if err := os.WriteFile(filepath.Join(checkpointDir, deletedFilesFilename), data, 0644); err != nil {
			t.Fatalf("write deleted-files.json: %v", err)
		}

		if err := ApplyDeletedFiles(checkpointDir, targetRoot, log); err != nil {
			t.Fatalf("ApplyDeletedFiles: %v", err)
		}
	})

	t.Run("empty entry is skipped", func(t *testing.T) {
		checkpointDir := t.TempDir()
		targetRoot := t.TempDir()

		data, err := json.Marshal([]string{""})
		if err != nil {
			t.Fatalf("marshal deleted files: %v", err)
		}
		if err := os.WriteFile(filepath.Join(checkpointDir, deletedFilesFilename), data, 0644); err != nil {
			t.Fatalf("write deleted-files.json: %v", err)
		}

		if err := ApplyDeletedFiles(checkpointDir, targetRoot, log); err != nil {
			t.Fatalf("ApplyDeletedFiles: %v", err)
		}
	})
}

// capNetRawXattr is a VFS_CAP_REVISION_2 vfs_cap_data blob granting
// CAP_NET_RAW as permitted+effective: magic_etc, then two {permitted,
// inheritable} u32 pairs, little-endian. The kernel validates this on write,
// so it cannot be an arbitrary value.
var capNetRawXattr = []byte{
	0x01, 0x00, 0x00, 0x02, // VFS_CAP_REVISION_2 | VFS_CAP_FLAGS_EFFECTIVE
	0x00, 0x20, 0x00, 0x00, // data[0].permitted   = 1 << CAP_NET_RAW(13)
	0x00, 0x00, 0x00, 0x00, // data[0].inheritable
	0x00, 0x00, 0x00, 0x00, // data[1].permitted
	0x00, 0x00, 0x00, 0x00, // data[1].inheritable
}

// TestRootfsDiffPreservesCapabilities covers the extract-side xattr mask:
// tar archives security.capability but restores only user.* unless the mask is
// widened, so a workload binary would come back stripped of its capabilities.
func TestRootfsDiffPreservesCapabilities(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("setting security.capability requires root")
	}

	upperDir := t.TempDir()
	checkpointDir := t.TempDir()
	targetRoot := t.TempDir()

	binary := filepath.Join(upperDir, "prog")
	if err := os.WriteFile(binary, []byte("payload"), 0o755); err != nil {
		t.Fatalf("write upperdir file: %v", err)
	}
	if err := unix.Lsetxattr(binary, "security.capability", capNetRawXattr, 0); err != nil {
		t.Skipf("filesystem refused security.capability: %v", err)
	}
	if err := unix.Lsetxattr(binary, "user.snapshot.cached", []byte("yes"), 0); err != nil {
		t.Fatalf("set user xattr: %v", err)
	}

	if _, err := CaptureRootfsDiff(upperDir, checkpointDir, RootfsDiffOptions{}); err != nil {
		t.Fatalf("CaptureRootfsDiff: %v", err)
	}
	if err := ApplyRootfsDiff(checkpointDir, targetRoot, testBundle(t), testr.New(t)); err != nil {
		t.Fatalf("ApplyRootfsDiff: %v", err)
	}

	restored := filepath.Join(targetRoot, "prog")
	for _, tc := range []struct {
		name string
		want []byte
	}{
		{"security.capability", capNetRawXattr},
		{"user.snapshot.cached", []byte("yes")},
	} {
		got := make([]byte, 64)
		n, err := unix.Lgetxattr(restored, tc.name, got)
		if err != nil {
			t.Fatalf("restored file lost %s: %v", tc.name, err)
		}
		if !slices.Equal(got[:n], tc.want) {
			t.Fatalf("%s = %v, want %v", tc.name, got[:n], tc.want)
		}
	}
}
