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

func writeProcStatusWithRSS(t *testing.T, procRoot string, pid int, body string) {
	t.Helper()
	dir := filepath.Join(procRoot, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "status"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestEstimateImageSizeSumsResidentMemory(t *testing.T) {
	procRoot := t.TempDir()
	// VmRSS is kB, and VmRSS must not be confused with its neighbours.
	writeProcStatusWithRSS(t, procRoot, 42, "Name:\tpython\nVmHWM:\t999999 kB\nVmRSS:\t2048 kB\nVmData:\t4096 kB\n")
	writeProcStatusWithRSS(t, procRoot, 43, "Name:\tpython\nVmRSS:\t1024 kB\n")

	got, err := EstimateImageSize(procRoot, []int{42, 43})
	if err != nil {
		t.Fatalf("EstimateImageSize() = %v", err)
	}
	if want := int64(3072 * 1024); got != want {
		t.Errorf("EstimateImageSize() = %d, want %d", got, want)
	}
}

// Kernel threads have no VmRSS and contribute nothing, so their absence is not
// an error — treating it as one would disable the guard on any real tree.
func TestEstimateImageSizeToleratesAThreadWithoutRSS(t *testing.T) {
	procRoot := t.TempDir()
	writeProcStatusWithRSS(t, procRoot, 42, "Name:\tkworker\nState:\tS (sleeping)\n")

	got, err := EstimateImageSize(procRoot, []int{42})
	if err != nil {
		t.Fatalf("EstimateImageSize() = %v", err)
	}
	if got != 0 {
		t.Errorf("EstimateImageSize() = %d, want 0", got)
	}
}

func TestEstimateImageSizeReportsAnUnreadableProcess(t *testing.T) {
	if _, err := EstimateImageSize(t.TempDir(), []int{42}); err == nil {
		t.Error("a process that cannot be read should not estimate silently")
	}
}

func TestEstimateImageSizeRejectsAMalformedRSS(t *testing.T) {
	procRoot := t.TempDir()
	writeProcStatusWithRSS(t, procRoot, 42, "VmRSS:\tnot-a-number kB\n")

	if _, err := EstimateImageSize(procRoot, []int{42}); err == nil {
		t.Error("a malformed VmRSS should not be read as zero")
	}
}

func TestStagingShortfall(t *testing.T) {
	for _, tc := range []struct{ estimate, available, want int64 }{
		{estimate: 100, available: 40, want: 60},
		{estimate: 100, available: 100, want: 0},
		{estimate: 40, available: 100, want: 0},
	} {
		t.Run(fmt.Sprintf("%d/%d", tc.estimate, tc.available), func(t *testing.T) {
			if got := StagingShortfall(tc.estimate, tc.available); got != tc.want {
				t.Errorf("StagingShortfall() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestStagingCapacityReadsARealPath(t *testing.T) {
	available, err := StagingCapacity(t.TempDir())
	if err != nil {
		t.Fatalf("StagingCapacity() = %v", err)
	}
	if available <= 0 {
		t.Errorf("StagingCapacity() = %d, want a positive size", available)
	}
	if _, err := StagingCapacity("/nonexistent/staging"); err == nil {
		t.Error("an absent path should not report capacity")
	}
}
