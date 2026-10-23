// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package criu

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

func TestReadDumpStatsReadsServiceHeader(t *testing.T) {
	dir := t.TempDir()
	payload := statsEntry(dumpStatsMessage(dumpStatsFixture{
		freezingUsec: 12_000,
		frozenUsec:   2_000_000,
		memdumpUsec:  400_000,
		memwriteUsec: 4_500_000,
		pagesWritten: 100,
		pagesScanned: 110,
		pagesLazy:    3,
	}))
	if err := os.WriteFile(filepath.Join(dir, statsDumpFilename), frameStats(true, payload), 0o644); err != nil {
		t.Fatal(err)
	}

	stats, err := ReadDumpStats(dir)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Freezing != 12*time.Millisecond || stats.Frozen != 2*time.Second {
		t.Fatalf("freeze timings = %s / %s", stats.Freezing, stats.Frozen)
	}
	if stats.MemDump != 400*time.Millisecond || stats.MemWrite != 4500*time.Millisecond {
		t.Fatalf("memory timings = %s / %s", stats.MemDump, stats.MemWrite)
	}
	if stats.PagesWritten != 100 || stats.PagesWrittenBytes() != 100*uint64(os.Getpagesize()) {
		t.Fatalf("pages = %d (%d bytes)", stats.PagesWritten, stats.PagesWrittenBytes())
	}
	if stats.PagesScanned != 110 || stats.PagesLazy != 3 {
		t.Fatalf("scan/lazy = %d/%d", stats.PagesScanned, stats.PagesLazy)
	}
}

func TestReadDumpStatsRejectsBareStatsMagic(t *testing.T) {
	dir := t.TempDir()
	payload := statsEntry(dumpStatsMessage(dumpStatsFixture{freezingUsec: 1, memwriteUsec: 2}))
	if err := os.WriteFile(filepath.Join(dir, statsDumpFilename), frameStats(false, payload), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDumpStats(dir); err == nil {
		t.Fatal("expected bare stats magic to be rejected")
	}
}

func TestReadDumpStatsRejectsWrongMagic(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, statsDumpFilename), []byte("nope-nope-nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDumpStats(dir); err == nil {
		t.Fatal("expected magic mismatch")
	}
}

func TestDirectoryBytesSkipsSymlinks(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pages-1.img"), []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "pages-1.img"), filepath.Join(dir, "parent")); err != nil {
		t.Fatal(err)
	}
	got, err := DirectoryBytes(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != 10 {
		t.Fatalf("bytes = %d, want 10", got)
	}
}

type dumpStatsFixture struct {
	freezingUsec       uint64
	frozenUsec         uint64
	memdumpUsec        uint64
	memwriteUsec       uint64
	pagesScanned       uint64
	pagesSkippedParent uint64
	pagesWritten       uint64
	pagesLazy          uint64
}

func dumpStatsMessage(fix dumpStatsFixture) []byte {
	var buf []byte
	buf = appendVarintField(buf, 1, fix.freezingUsec)
	buf = appendVarintField(buf, 2, fix.frozenUsec)
	buf = appendVarintField(buf, 3, fix.memdumpUsec)
	buf = appendVarintField(buf, 4, fix.memwriteUsec)
	buf = appendVarintField(buf, 5, fix.pagesScanned)
	buf = appendVarintField(buf, 6, fix.pagesSkippedParent)
	buf = appendVarintField(buf, 7, fix.pagesWritten)
	buf = appendVarintField(buf, 9, fix.pagesLazy)
	return buf
}

func statsEntry(dump []byte) []byte {
	var buf []byte
	buf = protowire.AppendTag(buf, 1, protowire.BytesType)
	return protowire.AppendBytes(buf, dump)
}

func appendVarintField(buf []byte, num protowire.Number, value uint64) []byte {
	buf = protowire.AppendTag(buf, num, protowire.VarintType)
	return protowire.AppendVarint(buf, value)
}

func frameStats(serviceHeader bool, payload []byte) []byte {
	buf := make([]byte, 0, 12+len(payload))
	var magic [4]byte
	if serviceHeader {
		binary.LittleEndian.PutUint32(magic[:], imgServiceMagic)
		buf = append(buf, magic[:]...)
	}
	binary.LittleEndian.PutUint32(magic[:], statsMagic)
	buf = append(buf, magic[:]...)
	var size [4]byte
	binary.LittleEndian.PutUint32(size[:], uint32(len(payload)))
	buf = append(buf, size[:]...)
	return append(buf, payload...)
}
