// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package criu

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"time"

	criustats "github.com/checkpoint-restore/go-criu/v8/stats"
)

const (
	// CRIU writes service images (stats, irmap cache) with this header magic
	// before the per-image magic. See IMG_SERVICE_MAGIC in criu's magic.h.
	imgServiceMagic = 0x55105940
	// STATS_MAGIC, the per-image magic for stats-dump / stats-restore.
	statsMagic = 0x57093306

	statsDumpFilename = "stats-dump"
)

// DumpStats is the dump half of CRIU's stats-dump image.
//
// Durations are CRIU's own timers, in the same units CRIU prints ("N us").
// They are not a partition of the agent's criu_dump wall time: freezing is
// outside the frozen window, memdump and memwrite sit inside it, and CRIU
// starts a memwrite timer both around the whole page transfer and around
// each write, so the last write interval can be counted twice. Use them to
// see whether a dump is dominated by reading the frozen process (memdump)
// or by writing pages (memwrite). Do not add them together or to criu_dump.
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
	data, err := os.ReadFile(filepath.Join(checkpointDir, statsDumpFilename))
	if err != nil {
		return DumpStats{}, fmt.Errorf("read %s: %w", statsDumpFilename, err)
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
	const header = 12
	if len(data) < header {
		return nil, fmt.Errorf("%s is too short (%d bytes)", statsDumpFilename, len(data))
	}
	service := binary.LittleEndian.Uint32(data[0:4])
	if service != imgServiceMagic {
		return nil, fmt.Errorf("%s service magic 0x%08x, want 0x%08x", statsDumpFilename, service, imgServiceMagic)
	}
	magic := binary.LittleEndian.Uint32(data[4:8])
	if magic != statsMagic {
		return nil, fmt.Errorf("%s magic 0x%08x, want 0x%08x", statsDumpFilename, magic, statsMagic)
	}
	size := binary.LittleEndian.Uint32(data[8:12])
	if uint64(size) > uint64(len(data)-header) {
		return nil, fmt.Errorf("%s payload length %d exceeds remaining %d", statsDumpFilename, size, len(data)-header)
	}
	return data[header : header+int(size)], nil
}

func parseStatsEntry(payload []byte) (DumpStats, error) {
	entry := &criustats.StatsEntry{}
	if err := entry.UnmarshalVT(payload); err != nil {
		return DumpStats{}, fmt.Errorf("unmarshal %s: %w", statsDumpFilename, err)
	}
	dump := entry.GetDump()
	if dump == nil {
		return DumpStats{}, fmt.Errorf("%s has no dump entry", statsDumpFilename)
	}
	if dump.FreezingTime == nil && dump.FrozenTime == nil && dump.MemdumpTime == nil && dump.MemwriteTime == nil {
		return DumpStats{}, fmt.Errorf("%s dump entry has no timing fields", statsDumpFilename)
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
