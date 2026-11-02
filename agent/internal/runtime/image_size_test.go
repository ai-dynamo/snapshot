// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func writeProcStatusWithMemory(t *testing.T, procRoot string, pid int, body string) {
	t.Helper()
	dir := filepath.Join(procRoot, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "status"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The floor must stay a floor. Summing the tree would count pages shared
// between processes once each, and counting file-backed pages would add bytes a
// dump records as mappings rather than writing — either overstates, and an
// overstatement refuses a capture that would have fit.
func TestMinimumImageSizeTakesTheLargestProcessAnonymousMemory(t *testing.T) {
	procRoot := t.TempDir()
	writeProcStatusWithMemory(t, procRoot, 42,
		"Name:\tpython\nVmHWM:\t999999 kB\nVmRSS:\t8192 kB\nRssAnon:\t2048 kB\nRssFile:\t6144 kB\n")
	writeProcStatusWithMemory(t, procRoot, 43,
		"Name:\tpython\nVmRSS:\t5120 kB\nRssAnon:\t1024 kB\nRssFile:\t4096 kB\n")

	got, err := MinimumImageSize(procRoot, []int{42, 43})
	if err != nil {
		t.Fatalf("MinimumImageSize() = %v", err)
	}
	if want := int64(2048 * 1024); got != want {
		t.Errorf("MinimumImageSize() = %d, want %d (the largest RssAnon, not a sum)", got, want)
	}
}

// Kernel threads have no RssAnon and contribute nothing, so their absence is
// not an error — treating it as one would disable the guard on any real tree.
func TestMinimumImageSizeToleratesAProcessWithoutAnonymousMemory(t *testing.T) {
	procRoot := t.TempDir()
	writeProcStatusWithMemory(t, procRoot, 42, "Name:\tkworker\nState:\tS (sleeping)\n")

	got, err := MinimumImageSize(procRoot, []int{42})
	if err != nil {
		t.Fatalf("MinimumImageSize() = %v", err)
	}
	if got != 0 {
		t.Errorf("MinimumImageSize() = %d, want 0", got)
	}
}

func TestMinimumImageSizeReportsAnUnreadableProcess(t *testing.T) {
	if _, err := MinimumImageSize(t.TempDir(), []int{42}); err == nil {
		t.Error("a process that cannot be read should not size silently")
	}
}

// A guard that quietly sizes an image at zero is a guard that never fires.
func TestMinimumImageSizeRejectsAMalformedValue(t *testing.T) {
	procRoot := t.TempDir()
	writeProcStatusWithMemory(t, procRoot, 42, "RssAnon:\tnot-a-number kB\n")

	if _, err := MinimumImageSize(procRoot, []int{42}); err == nil {
		t.Error("a malformed RssAnon should not be read as zero")
	}
}

func TestStagingShortfall(t *testing.T) {
	for _, tc := range []struct{ minimum, total, want int64 }{
		{minimum: 100, total: 40, want: 60},
		{minimum: 100, total: 100, want: 0},
		{minimum: 40, total: 100, want: 0},
	} {
		t.Run(fmt.Sprintf("%d/%d", tc.minimum, tc.total), func(t *testing.T) {
			if got := StagingShortfall(tc.minimum, tc.total); got != tc.want {
				t.Errorf("StagingShortfall() = %d, want %d", got, tc.want)
			}
		})
	}
}

// Total, not free: free space moves as other captures take and release it.
func TestStagingCapacityReportsTheWholeVolume(t *testing.T) {
	total, err := StagingCapacity(t.TempDir())
	if err != nil {
		t.Fatalf("StagingCapacity() = %v", err)
	}
	if total <= 0 {
		t.Errorf("StagingCapacity() = %d, want a positive size", total)
	}
	if _, err := StagingCapacity("/nonexistent/staging"); err == nil {
		t.Error("an absent path should not report capacity")
	}
}
