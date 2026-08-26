// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"archive/tar"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	"golang.org/x/sys/unix"
)

var testModTime = time.Unix(1700000000, 0)

type tarEntry struct {
	hdr  tar.Header
	data string
}

func writeTestArchive(t *testing.T, entries []tarEntry) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.tar")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create archive: %v", err)
	}
	tw := tar.NewWriter(f)
	for i := range entries {
		hdr := entries[i].hdr
		if hdr.Mode == 0 {
			hdr.Mode = 0o644
		}
		if hdr.ModTime.IsZero() {
			hdr.ModTime = testModTime
		}
		if hdr.Format == tar.FormatUnknown {
			hdr.Format = tar.FormatPAX
		}
		hdr.Size = int64(len(entries[i].data))
		if err := tw.WriteHeader(&hdr); err != nil {
			t.Fatalf("write header %q: %v", hdr.Name, err)
		}
		if _, err := io.WriteString(tw, entries[i].data); err != nil {
			t.Fatalf("write data %q: %v", hdr.Name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close archive: %v", err)
	}
	return path
}

func TestExtractRootfsDiff(t *testing.T) {
	t.Run("extracts files and directories with metadata", func(t *testing.T) {
		src := writeTestArchive(t, []tarEntry{
			{hdr: tar.Header{Name: "./dir/", Typeflag: tar.TypeDir, Mode: 0o750}},
			{hdr: tar.Header{Name: "./dir/file.txt", Typeflag: tar.TypeReg, Mode: 0o640}, data: "content"},
		})
		root := t.TempDir()
		if err := ExtractRootfsDiff(src, root, testr.New(t)); err != nil {
			t.Fatalf("ExtractRootfsDiff: %v", err)
		}

		data, err := os.ReadFile(filepath.Join(root, "dir", "file.txt"))
		if err != nil {
			t.Fatalf("read extracted file: %v", err)
		}
		if string(data) != "content" {
			t.Errorf("content = %q, want %q", data, "content")
		}
		fi, err := os.Stat(filepath.Join(root, "dir", "file.txt"))
		if err != nil {
			t.Fatalf("stat extracted file: %v", err)
		}
		if fi.Mode().Perm() != 0o640 {
			t.Errorf("file mode = %o, want 640", fi.Mode().Perm())
		}
		if !fi.ModTime().Equal(testModTime) {
			t.Errorf("file mtime = %v, want %v", fi.ModTime(), testModTime)
		}
		di, err := os.Stat(filepath.Join(root, "dir"))
		if err != nil {
			t.Fatalf("stat extracted dir: %v", err)
		}
		if di.Mode().Perm() != 0o750 {
			t.Errorf("dir mode = %o, want 750", di.Mode().Perm())
		}
	})

	t.Run("skips entries whose target already exists", func(t *testing.T) {
		src := writeTestArchive(t, []tarEntry{
			{hdr: tar.Header{Name: "./existing", Typeflag: tar.TypeReg}, data: "replaced"},
			{hdr: tar.Header{Name: "./created", Typeflag: tar.TypeReg}, data: "created"},
		})
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "existing"), []byte("preserved"), 0o644); err != nil {
			t.Fatalf("seed existing file: %v", err)
		}
		if err := ExtractRootfsDiff(src, root, testr.New(t)); err != nil {
			t.Fatalf("ExtractRootfsDiff: %v", err)
		}

		data, err := os.ReadFile(filepath.Join(root, "existing"))
		if err != nil {
			t.Fatalf("read existing file: %v", err)
		}
		if string(data) != "preserved" {
			t.Errorf("existing file = %q, want untouched %q", data, "preserved")
		}
		data, err = os.ReadFile(filepath.Join(root, "created"))
		if err != nil {
			t.Fatalf("read created file: %v", err)
		}
		if string(data) != "created" {
			t.Errorf("created file = %q, want %q", data, "created")
		}
	})

	t.Run("restores symlinks including dangling ones", func(t *testing.T) {
		src := writeTestArchive(t, []tarEntry{
			{hdr: tar.Header{Name: "./target", Typeflag: tar.TypeReg}, data: "x"},
			{hdr: tar.Header{Name: "./link", Typeflag: tar.TypeSymlink, Linkname: "target"}},
			{hdr: tar.Header{Name: "./dangling", Typeflag: tar.TypeSymlink, Linkname: "no-such-file"}},
		})
		root := t.TempDir()
		if err := ExtractRootfsDiff(src, root, testr.New(t)); err != nil {
			t.Fatalf("ExtractRootfsDiff: %v", err)
		}

		dest, err := os.Readlink(filepath.Join(root, "link"))
		if err != nil {
			t.Fatalf("readlink: %v", err)
		}
		if dest != "target" {
			t.Errorf("link dest = %q, want %q", dest, "target")
		}
		dest, err = os.Readlink(filepath.Join(root, "dangling"))
		if err != nil {
			t.Fatalf("readlink dangling: %v", err)
		}
		if dest != "no-such-file" {
			t.Errorf("dangling dest = %q, want %q", dest, "no-such-file")
		}
	})

	t.Run("restores hardlinks", func(t *testing.T) {
		src := writeTestArchive(t, []tarEntry{
			{hdr: tar.Header{Name: "./original", Typeflag: tar.TypeReg}, data: "shared"},
			{hdr: tar.Header{Name: "./alias", Typeflag: tar.TypeLink, Linkname: "./original"}},
		})
		root := t.TempDir()
		if err := ExtractRootfsDiff(src, root, testr.New(t)); err != nil {
			t.Fatalf("ExtractRootfsDiff: %v", err)
		}

		a, err := os.Stat(filepath.Join(root, "original"))
		if err != nil {
			t.Fatalf("stat original: %v", err)
		}
		b, err := os.Stat(filepath.Join(root, "alias"))
		if err != nil {
			t.Fatalf("stat alias: %v", err)
		}
		if !os.SameFile(a, b) {
			t.Error("alias is not a hardlink to original")
		}
	})

	t.Run("hardlink to a skipped entry links to the existing file", func(t *testing.T) {
		src := writeTestArchive(t, []tarEntry{
			{hdr: tar.Header{Name: "./original", Typeflag: tar.TypeReg}, data: "replaced"},
			{hdr: tar.Header{Name: "./alias", Typeflag: tar.TypeLink, Linkname: "./original"}},
		})
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "original"), []byte("preserved"), 0o644); err != nil {
			t.Fatalf("seed existing file: %v", err)
		}
		if err := ExtractRootfsDiff(src, root, testr.New(t)); err != nil {
			t.Fatalf("ExtractRootfsDiff: %v", err)
		}

		data, err := os.ReadFile(filepath.Join(root, "alias"))
		if err != nil {
			t.Fatalf("read alias: %v", err)
		}
		if string(data) != "preserved" {
			t.Errorf("alias content = %q, want the pre-existing %q", data, "preserved")
		}
	})

	t.Run("restores FIFOs", func(t *testing.T) {
		src := writeTestArchive(t, []tarEntry{
			{hdr: tar.Header{Name: "./pipe", Typeflag: tar.TypeFifo, Mode: 0o600}},
		})
		root := t.TempDir()
		if err := ExtractRootfsDiff(src, root, testr.New(t)); err != nil {
			t.Fatalf("ExtractRootfsDiff: %v", err)
		}

		fi, err := os.Stat(filepath.Join(root, "pipe"))
		if err != nil {
			t.Fatalf("stat fifo: %v", err)
		}
		if fi.Mode()&os.ModeNamedPipe == 0 {
			t.Errorf("mode = %v, want a named pipe", fi.Mode())
		}
	})

	t.Run("preserves setuid bit", func(t *testing.T) {
		src := writeTestArchive(t, []tarEntry{
			{hdr: tar.Header{Name: "./suid", Typeflag: tar.TypeReg, Mode: 0o4755}, data: "x"},
		})
		root := t.TempDir()
		if err := ExtractRootfsDiff(src, root, testr.New(t)); err != nil {
			t.Fatalf("ExtractRootfsDiff: %v", err)
		}

		fi, err := os.Stat(filepath.Join(root, "suid"))
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if fi.Mode()&os.ModeSetuid == 0 {
			t.Errorf("mode = %v, want setuid set", fi.Mode())
		}
	})

	t.Run("restores xattrs from PAX records", func(t *testing.T) {
		probe := filepath.Join(t.TempDir(), "probe")
		if err := os.WriteFile(probe, nil, 0o644); err != nil {
			t.Fatalf("create probe file: %v", err)
		}
		if err := unix.Setxattr(probe, "user.probe", []byte("v"), 0); err != nil {
			t.Skipf("test filesystem does not support user xattrs: %v", err)
		}

		src := writeTestArchive(t, []tarEntry{
			{hdr: tar.Header{
				Name:       "./attrfile",
				Typeflag:   tar.TypeReg,
				PAXRecords: map[string]string{"SCHILY.xattr.user.snapshot": "roundtrip"},
			}, data: "x"},
		})
		root := t.TempDir()
		if err := ExtractRootfsDiff(src, root, testr.New(t)); err != nil {
			t.Fatalf("ExtractRootfsDiff: %v", err)
		}

		buf := make([]byte, 64)
		n, err := unix.Getxattr(filepath.Join(root, "attrfile"), "user.snapshot", buf)
		if err != nil {
			t.Fatalf("getxattr: %v", err)
		}
		if string(buf[:n]) != "roundtrip" {
			t.Errorf("xattr = %q, want %q", buf[:n], "roundtrip")
		}
	})

	t.Run("path traversal entries cannot escape the target root", func(t *testing.T) {
		src := writeTestArchive(t, []tarEntry{
			{hdr: tar.Header{Name: "../escape", Typeflag: tar.TypeReg}, data: "x"},
		})
		outer := t.TempDir()
		root := filepath.Join(outer, "root")
		if err := os.Mkdir(root, 0o755); err != nil {
			t.Fatalf("mkdir root: %v", err)
		}
		if err := ExtractRootfsDiff(src, root, testr.New(t)); err != nil {
			t.Fatalf("ExtractRootfsDiff: %v", err)
		}

		if _, err := os.Lstat(filepath.Join(outer, "escape")); !os.IsNotExist(err) {
			t.Fatal("traversal entry escaped the target root")
		}
		// SecureJoin clamps the entry inside the root instead.
		if _, err := os.Lstat(filepath.Join(root, "escape")); err != nil {
			t.Errorf("clamped entry missing inside root: %v", err)
		}
	})

	t.Run("writes through malicious symlinks stay inside the target root", func(t *testing.T) {
		outer := t.TempDir()
		root := filepath.Join(outer, "root")
		if err := os.Mkdir(root, 0o755); err != nil {
			t.Fatalf("mkdir root: %v", err)
		}
		src := writeTestArchive(t, []tarEntry{
			{hdr: tar.Header{Name: "./evil", Typeflag: tar.TypeSymlink, Linkname: outer}},
			{hdr: tar.Header{Name: "./evil/pwned", Typeflag: tar.TypeReg}, data: "x"},
		})
		if err := ExtractRootfsDiff(src, root, testr.New(t)); err != nil {
			t.Fatalf("ExtractRootfsDiff: %v", err)
		}

		if _, err := os.Lstat(filepath.Join(outer, "pwned")); !os.IsNotExist(err) {
			t.Fatal("write through symlink escaped the target root")
		}
	})

	t.Run("truncated archive is an error", func(t *testing.T) {
		full := writeTestArchive(t, []tarEntry{
			{hdr: tar.Header{Name: "./file", Typeflag: tar.TypeReg}, data: "content"},
		})
		data, err := os.ReadFile(full)
		if err != nil {
			t.Fatalf("read archive: %v", err)
		}
		truncated := filepath.Join(t.TempDir(), "truncated.tar")
		if err := os.WriteFile(truncated, data[:len(data)/2], 0o644); err != nil {
			t.Fatalf("write truncated archive: %v", err)
		}
		if err := ExtractRootfsDiff(truncated, t.TempDir(), testr.New(t)); err == nil {
			t.Fatal("ExtractRootfsDiff should fail on a truncated archive")
		}
	})
}

// requireGNUTar skips the test unless the system tar is GNU tar — the tool
// CaptureRootfsDiff actually runs. Non-GNU hosts (macOS bsdtar) skip.
func requireGNUTar(t *testing.T) {
	t.Helper()
	out, err := exec.Command("tar", "--version").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "GNU tar") {
		t.Skipf("GNU tar not available (output: %q, err: %v)", out, err)
	}
}

func runCommand(t *testing.T, name string, args ...string) {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v (output: %s)", name, args, err, out)
	}
}

// TestExtractRootfsDiffFromGNUTar verifies the extractor against an archive
// produced by the same tool and flags CaptureRootfsDiff uses, when a GNU tar
// is available on the test host (always true in the CI tester image).
func TestExtractRootfsDiffFromGNUTar(t *testing.T) {
	requireGNUTar(t)

	srcDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(srcDir, "nested"), 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "nested", "generated.txt"), []byte("payload"), 0o640); err != nil {
		t.Fatalf("write source file: %v", err)
	}
	if err := os.Symlink("generated.txt", filepath.Join(srcDir, "nested", "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	archive := filepath.Join(t.TempDir(), "diff.tar")
	runCommand(t, "tar", "--xattrs", "-C", srcDir, "-cf", archive, ".")

	root := t.TempDir()
	if err := ExtractRootfsDiff(archive, root, testr.New(t)); err != nil {
		t.Fatalf("ExtractRootfsDiff: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(root, "nested", "generated.txt"))
	if err != nil {
		t.Fatalf("read extracted file: %v", err)
	}
	if string(data) != "payload" {
		t.Errorf("content = %q, want %q", data, "payload")
	}
	if dest, err := os.Readlink(filepath.Join(root, "nested", "link")); err != nil || dest != "generated.txt" {
		t.Errorf("link dest = %q, err = %v, want %q", dest, err, "generated.txt")
	}
}
