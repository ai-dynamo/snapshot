// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"archive/tar"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr"
)

// Benchmarks comparing the in-process extractor against the exec'd GNU tar it
// replaced, on the two regimes that bound rootfs-diff extraction:
//
//   - largefile:  one big archive member — write-bandwidth bound (the typical
//     rootfs diff: Triton/JIT caches and other bulky runtime artifacts).
//   - manyfiles:  tens of thousands of small members — per-entry syscall bound
//     (the extractor's worst case: SecureJoin adds per-component lookups).
//
// Both extractors consume the identical archive. GNU tar runs with the exact
// flags the pre-Go implementation used. Run with:
//
//	go test -bench BenchmarkExtractRootfsDiff -run '^$' ./internal/runtime/
const (
	benchLargeFileSize   = 256 << 20 // one 256 MiB member
	benchManyFilesCount  = 20000
	benchManyFilesSize   = 4 << 10 // 4 KiB per member
	benchManyFilesPerDir = 100
)

func BenchmarkExtractRootfsDiff(b *testing.B) {
	fixtures := []struct {
		name    string
		archive string
	}{
		{"largefile", benchArchiveLargeFile(b)},
		{"manyfiles", benchArchiveManyFiles(b)},
	}
	for _, fx := range fixtures {
		b.Run(fx.name+"/go", func(b *testing.B) {
			benchExtract(b, fx.archive, func(target string) error {
				return ExtractRootfsDiff(fx.archive, target, logr.Discard())
			})
		})
		b.Run(fx.name+"/gnutar", func(b *testing.B) {
			requireGNUTar(b)
			benchExtract(b, fx.archive, func(target string) error {
				return exec.Command("tar", "--skip-old-files", "--blocking-factor=2048",
					"-C", target, "-xf", fx.archive).Run()
			})
		})
	}
}

func benchExtract(b *testing.B, archive string, extract func(target string) error) {
	b.Helper()
	fi, err := os.Stat(archive)
	if err != nil {
		b.Fatalf("stat archive: %v", err)
	}
	b.SetBytes(fi.Size())
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		target, err := os.MkdirTemp("", "untar-bench-*")
		if err != nil {
			b.Fatalf("create target dir: %v", err)
		}
		b.StartTimer()
		if err := extract(target); err != nil {
			b.Fatalf("extract: %v", err)
		}
		b.StopTimer()
		if err := os.RemoveAll(target); err != nil {
			b.Fatalf("clean target dir: %v", err)
		}
		b.StartTimer()
	}
}

func benchArchiveLargeFile(b *testing.B) string {
	b.Helper()
	return benchWriteArchive(b, "largefile.tar", func(add func(name string, size int)) {
		add("./cache.bin", benchLargeFileSize)
	})
}

func benchArchiveManyFiles(b *testing.B) string {
	b.Helper()
	return benchWriteArchive(b, "manyfiles.tar", func(add func(name string, size int)) {
		for i := 0; i < benchManyFilesCount; i++ {
			add(fmt.Sprintf("./d%04d/f%05d", i/benchManyFilesPerDir, i), benchManyFilesSize)
		}
	})
}

// benchWriteArchive builds a tar fixture whose members are filled with a
// repeating byte pattern, emitting directory entries parent-first the way
// CaptureRootfsDiff's tar walk does.
func benchWriteArchive(b *testing.B, name string, addMembers func(add func(name string, size int))) string {
	b.Helper()
	path := filepath.Join(b.TempDir(), name)
	f, err := os.Create(path)
	if err != nil {
		b.Fatalf("create archive: %v", err)
	}
	tw := tar.NewWriter(f)

	block := make([]byte, 1<<20)
	for i := range block {
		block[i] = byte(i)
	}
	seenDirs := map[string]bool{}
	add := func(name string, size int) {
		if dir := filepath.Dir(name); dir != "." && !seenDirs[dir] {
			seenDirs[dir] = true
			hdr := tar.Header{Name: dir + "/", Typeflag: tar.TypeDir, Mode: 0o755,
				ModTime: testModTime, Format: tar.FormatPAX}
			if err := tw.WriteHeader(&hdr); err != nil {
				b.Fatalf("write dir header %q: %v", dir, err)
			}
		}
		hdr := tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644,
			Size: int64(size), ModTime: testModTime, Format: tar.FormatPAX}
		if err := tw.WriteHeader(&hdr); err != nil {
			b.Fatalf("write header %q: %v", name, err)
		}
		for size > 0 {
			n := min(size, len(block))
			if _, err := tw.Write(block[:n]); err != nil {
				b.Fatalf("write data %q: %v", name, err)
			}
			size -= n
		}
	}
	addMembers(add)

	if err := tw.Close(); err != nil {
		b.Fatalf("close tar writer: %v", err)
	}
	if err := f.Close(); err != nil {
		b.Fatalf("close archive: %v", err)
	}
	return path
}
