// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package criu

import (
	"encoding/binary"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	criustats "github.com/checkpoint-restore/go-criu/v8/stats"
)

// DumpStats is the dump half of CRIU's stats-dump image.
//
// Durations are CRIU's own timers, in the same units CRIU prints ("N us").
// They are not a partition of the agent's criu_dump wall time. freezing is
// outside the frozen window. memwrite runs inside memdump: the page dump
// starts TIME_MEMDUMP, xfer_pages() is the TIME_MEMWRITE window, then
// TIME_MEMDUMP stops. So memdump is at least memwrite, and the read time is
// memdump minus memwrite. The double-counted last write interval is only in
// the pre-dump path, which this agent does not use. Do not add the timers
// together or to criu_dump.
type DumpStats struct {
	Freezing           time.Duration
	Frozen             time.Duration
	MemDump            time.Duration
	MemWrite           time.Duration
	PagesScanned       uint64
	PagesSkippedParent uint64
	PagesWritten       uint64
	PagesLazy          uint64
}

// PagesWrittenBytes is PagesWritten converted at the host page size.
func (s DumpStats) PagesWrittenBytes() uint64 {
	return s.PagesWritten * uint64(os.Getpagesize())
}

// ReadDumpStats reads stats-dump from a completed CRIU image directory.
func ReadDumpStats(checkpointDir string) (DumpStats, error) {
	data, err := os.ReadFile(filepath.Join(checkpointDir, criustats.StatsDump))
	if err != nil {
		return DumpStats{}, fmt.Errorf("read %s: %w", criustats.StatsDump, err)
	}
	payload, err := statsPayload(data)
	if err != nil {
		return DumpStats{}, err
	}
	return parseStatsEntry(payload)
}

func statsPayload(data []byte) ([]byte, error) {
	// CRIU always writes the service magic, then the stats magic, then the
	// payload length. Checking the length here avoids the panic in go-criu's
	// readStatisticsFile on a truncated file.
	const header = criustats.PayloadOffset
	if len(data) < header {
		return nil, fmt.Errorf("%s is too short (%d bytes)", criustats.StatsDump, len(data))
	}
	service := binary.LittleEndian.Uint32(data[criustats.PrimaryMagicOffset:criustats.SecondaryMagicOffset])
	if service != criustats.ImgServiceMagic {
		return nil, fmt.Errorf("%s service magic 0x%08x, want 0x%08x", criustats.StatsDump, service, criustats.ImgServiceMagic)
	}
	magic := binary.LittleEndian.Uint32(data[criustats.SecondaryMagicOffset:criustats.SizeOffset])
	if magic != criustats.StatsMagic {
		return nil, fmt.Errorf("%s magic 0x%08x, want 0x%08x", criustats.StatsDump, magic, criustats.StatsMagic)
	}
	size := binary.LittleEndian.Uint32(data[criustats.SizeOffset:criustats.PayloadOffset])
	if uint64(size) > uint64(len(data)-header) {
		return nil, fmt.Errorf("%s payload length %d exceeds remaining %d", criustats.StatsDump, size, len(data)-header)
	}
	return data[header : header+int(size)], nil
}

func parseStatsEntry(payload []byte) (DumpStats, error) {
	entry := &criustats.StatsEntry{}
	if err := entry.UnmarshalVT(payload); err != nil {
		return DumpStats{}, fmt.Errorf("unmarshal %s: %w", criustats.StatsDump, err)
	}
	dump := entry.GetDump()
	if dump == nil {
		return DumpStats{}, fmt.Errorf("%s has no dump entry", criustats.StatsDump)
	}
	return DumpStats{
		Freezing:           usecDuration(uint64(dump.GetFreezingTime())),
		Frozen:             usecDuration(uint64(dump.GetFrozenTime())),
		MemDump:            usecDuration(uint64(dump.GetMemdumpTime())),
		MemWrite:           usecDuration(uint64(dump.GetMemwriteTime())),
		PagesScanned:       dump.GetPagesScanned(),
		PagesSkippedParent: dump.GetPagesSkippedParent(),
		PagesWritten:       dump.GetPagesWritten(),
		PagesLazy:          dump.GetPagesLazy(),
	}, nil
}

func usecDuration(usec uint64) time.Duration {
	return time.Duration(usec) * time.Microsecond
}

// DirectoryBytes sums the apparent size (st_size) of regular files under root.
// Symlinks are not followed, so a CRIU parent link is not counted twice.
// Allocated size (st_blocks) is recorded separately by the benchmark inventory.
func DirectoryBytes(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("measure %s: %w", root, err)
	}
	return total, nil
}
