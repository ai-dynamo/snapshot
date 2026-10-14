// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package criu

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

const (
	// CRIU writes service images (stats, irmap cache) with this header magic
	// before the per-image magic. See IMG_SERVICE_MAGIC in criu's magic.h.
	imgServiceMagic = 0x55105940
	// STATS_MAGIC, the per-image magic for stats-dump / stats-restore.
	statsMagic = 0x57093306

	statsDumpFilename = "stats-dump"

	// CRIU counts pages in 4KiB units on the architectures Snapshot supports.
	pageBytes = 4096
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

// PagesWrittenBytes is PagesWritten converted at CRIU's 4KiB page size.
func (s DumpStats) PagesWrittenBytes() uint64 {
	return s.PagesWritten * pageBytes
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
	if len(data) < 8 {
		return nil, fmt.Errorf("%s is too short (%d bytes)", statsDumpFilename, len(data))
	}
	off := 0
	magic := binary.LittleEndian.Uint32(data[off : off+4])
	off += 4
	if magic == imgServiceMagic {
		if len(data) < off+8 {
			return nil, fmt.Errorf("%s truncated after service magic", statsDumpFilename)
		}
		magic = binary.LittleEndian.Uint32(data[off : off+4])
		off += 4
	}
	if magic != statsMagic {
		return nil, fmt.Errorf("%s magic 0x%08x, want 0x%08x", statsDumpFilename, magic, statsMagic)
	}
	if len(data) < off+4 {
		return nil, fmt.Errorf("%s truncated before payload length", statsDumpFilename)
	}
	size := binary.LittleEndian.Uint32(data[off : off+4])
	off += 4
	if uint64(size) > uint64(len(data)-off) {
		return nil, fmt.Errorf("%s payload length %d exceeds remaining %d", statsDumpFilename, size, len(data)-off)
	}
	return data[off : off+int(size)], nil
}

func parseStatsEntry(buf []byte) (DumpStats, error) {
	var dump []byte
	for len(buf) > 0 {
		num, typ, n := protowire.ConsumeTag(buf)
		if n < 0 {
			return DumpStats{}, fmt.Errorf("%s has an invalid stats entry tag", statsDumpFilename)
		}
		buf = buf[n:]
		if num == 1 && typ == protowire.BytesType {
			value, m := protowire.ConsumeBytes(buf)
			if m < 0 {
				return DumpStats{}, fmt.Errorf("%s has an invalid dump entry", statsDumpFilename)
			}
			buf = buf[m:]
			dump = append([]byte(nil), value...)
			continue
		}
		m := protowire.ConsumeFieldValue(num, typ, buf)
		if m < 0 {
			return DumpStats{}, fmt.Errorf("%s has an invalid stats entry field %d", statsDumpFilename, num)
		}
		buf = buf[m:]
	}
	if dump == nil {
		return DumpStats{}, fmt.Errorf("%s has no dump entry", statsDumpFilename)
	}
	return parseDumpStats(dump)
}

func parseDumpStats(buf []byte) (DumpStats, error) {
	var stats DumpStats
	var sawTiming bool
	for len(buf) > 0 {
		num, typ, n := protowire.ConsumeTag(buf)
		if n < 0 {
			return DumpStats{}, fmt.Errorf("%s has an invalid dump stats tag", statsDumpFilename)
		}
		buf = buf[n:]
		if typ != protowire.VarintType {
			m := protowire.ConsumeFieldValue(num, typ, buf)
			if m < 0 {
				return DumpStats{}, fmt.Errorf("%s has an invalid dump stats field %d", statsDumpFilename, num)
			}
			buf = buf[m:]
			continue
		}
		value, m := protowire.ConsumeVarint(buf)
		if m < 0 {
			return DumpStats{}, fmt.Errorf("%s has an invalid dump stats varint %d", statsDumpFilename, num)
		}
		buf = buf[m:]
		switch num {
		case 1:
			stats.Freezing = usecDuration(value)
			sawTiming = true
		case 2:
			stats.Frozen = usecDuration(value)
			sawTiming = true
		case 3:
			stats.MemDump = usecDuration(value)
			sawTiming = true
		case 4:
			stats.MemWrite = usecDuration(value)
			sawTiming = true
		case 5:
			stats.PagesScanned = value
		case 6:
			stats.PagesSkippedParent = value
		case 7:
			stats.PagesWritten = value
		case 9:
			stats.PagesLazy = value
		}
	}
	if !sawTiming {
		return DumpStats{}, fmt.Errorf("%s dump entry has no timing fields", statsDumpFilename)
	}
	return stats, nil
}

func usecDuration(usec uint64) time.Duration {
	return time.Duration(usec) * time.Microsecond
}
