// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// MinimumImageSize reports a floor on the bytes a dump of this tree must write.
//
// It is the largest single process's resident anonymous memory. Those pages are
// private and dirty, so they cannot be reconstructed from a file and must
// appear in the image; sharing them with another process does not make the
// image smaller. Summing the tree instead would count shared pages once per
// process and overstate, and resident memory as a whole counts file-backed
// pages that a dump records as mappings rather than writing out — either would
// make this an over-estimate, and an over-estimate refuses captures that fit.
func MinimumImageSize(procRoot string, pids []int) (int64, error) {
	var largest int64
	for _, pid := range pids {
		anon, err := readRssAnon(procRoot, pid)
		if err != nil {
			return 0, err
		}
		if anon > largest {
			largest = anon
		}
	}
	return largest, nil
}

// readRssAnon returns one process's resident anonymous memory in bytes.
func readRssAnon(procRoot string, pid int) (int64, error) {
	statusPath := filepath.Join(procRoot, strconv.Itoa(pid), "status")
	statusBytes, err := os.ReadFile(statusPath)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", statusPath, err)
	}
	for _, line := range strings.Split(string(statusBytes), "\n") {
		field, value, found := strings.Cut(line, ":")
		if !found || field != "RssAnon" {
			continue
		}
		// RssAnon is reported as "<n> kB"; the kernel always uses kB here.
		fields := strings.Fields(value)
		if len(fields) == 0 {
			return 0, fmt.Errorf("empty RssAnon for PID %d", pid)
		}
		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse RssAnon %q for PID %d: %w", fields[0], pid, err)
		}
		return kb * 1024, nil
	}
	// Kernel threads have no RssAnon and contribute nothing to the image.
	return 0, nil
}
