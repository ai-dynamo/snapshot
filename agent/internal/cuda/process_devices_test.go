// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cuda

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/go-logr/logr"

	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

const (
	testGPUA = "GPU-aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	testGPUB = "GPU-bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
)

type recordingHelperActionRunner struct {
	requests   []helperAction
	batchCalls int
}

func (r *recordingHelperActionRunner) run(_ context.Context, request helperAction, _ logr.Logger) error {
	r.requests = append(r.requests, request)
	return nil
}

func (r *recordingHelperActionRunner) runRestoreBatch(_ context.Context, requests []helperAction, _ logr.Logger) error {
	r.batchCalls++
	r.requests = append(r.requests, requests...)
	return nil
}

func TestParseProcessGPUUUIDsFiltersAndPartitions(t *testing.T) {
	got, err := parseProcessGPUUUIDs(
		"101, "+testGPUA+"\n202, "+testGPUB+"\n999, "+testGPUA+"\n",
		[]int{101, 202},
		[]string{testGPUA, testGPUB},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int][]string{101: {testGPUA}, 202: {testGPUB}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("process GPU mapping = %#v, want %#v", got, want)
	}
}

func TestParseProcessGPUUUIDsFallsBackForVMMOwnerAndRejectsForeign(t *testing.T) {
	got, err := parseProcessGPUUUIDs(
		"101, "+testGPUA+"\n",
		[]int{101, 202},
		[]string{testGPUA, testGPUB},
	)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{testGPUA, testGPUB}; !reflect.DeepEqual(got[202], want) {
		t.Fatalf("VMM-only owner fallback = %#v, want %#v", got[202], want)
	}
	if _, err := parseProcessGPUUUIDs("101, "+testGPUB+"\n", []int{101}, []string{testGPUA}); err == nil {
		t.Fatal("expected foreign GPU mapping to fail")
	}
}

func TestCheckpointProcessTreeUsesPerProcessGPUUUIDs(t *testing.T) {
	runner := &recordingHelperActionRunner{}
	_, err := lockAndCheckpointProcessTree(
		context.Background(),
		[]int{101, 202},
		[]int{11, 22},
		"",
		types.CUDAStorageModePOSIX,
		t.TempDir(),
		[]string{testGPUA, testGPUB},
		map[int][]string{101: {testGPUA}, 202: {testGPUB}},
		types.CUDATransferSettings{}.WithDefaults(),
		runner,
		logr.Discard(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.requests) != 4 {
		t.Fatalf("helper requests = %d, want 4", len(runner.requests))
	}
	if got := runner.requests[2].GPUUUIDs; !reflect.DeepEqual(got, []string{testGPUA}) {
		t.Fatalf("PID 101 target GPUs = %#v, want %s", got, testGPUA)
	}
	if got := runner.requests[3].GPUUUIDs; !reflect.DeepEqual(got, []string{testGPUB}) {
		t.Fatalf("PID 202 target GPUs = %#v, want %s", got, testGPUB)
	}
}

func TestCheckpointProcessTreeRejectsIncompleteGPUMapBeforeLock(t *testing.T) {
	runner := &recordingHelperActionRunner{}
	_, err := lockAndCheckpointProcessTree(
		context.Background(),
		[]int{101, 202},
		[]int{11, 22},
		"",
		types.CUDAStorageModePOSIX,
		t.TempDir(),
		[]string{testGPUA, testGPUB},
		map[int][]string{101: {testGPUA}},
		types.CUDATransferSettings{}.WithDefaults(),
		runner,
		logr.Discard(),
	)
	if err == nil || !strings.Contains(err.Error(), "missing CustomStorage GPU mapping for CUDA PID 202") {
		t.Fatalf("lockAndCheckpointProcessTree() error = %v, want missing-map rejection", err)
	}
	if len(runner.requests) != 0 {
		t.Fatalf("helper requests = %d, want no target mutation", len(runner.requests))
	}
}

func TestRestoreProcessTreeUsesManifestGPUUUIDs(t *testing.T) {
	checkpointDir := t.TempDir()
	for index, uuid := range []string{testGPUA, testGPUB} {
		namespacePID := (index + 1) * 11
		processDir := customStorageProcessDir(checkpointDir, namespacePID)
		if err := os.MkdirAll(processDir, 0o700); err != nil {
			t.Fatal(err)
		}
		manifest := "version 2\ndevice_count 1\ndevice 0 " + uuid + " 4096 device-0000.bin\n"
		if err := os.WriteFile(filepath.Join(processDir, "manifest.txt"), []byte(manifest), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runner := &recordingHelperActionRunner{}
	_, err := restoreAndUnlockProcessTree(
		context.Background(),
		[]int{303, 404},
		[]int{11, 22},
		"",
		types.CUDAStorageModePOSIX,
		checkpointDir,
		"",
		[]string{testGPUA, testGPUB},
		types.CUDATransferSettings{}.WithDefaults(),
		runner,
		logr.Discard(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.requests) != 4 {
		t.Fatalf("helper requests = %d, want 4", len(runner.requests))
	}
	if runner.batchCalls != 1 {
		t.Fatalf("restore batch calls = %d, want 1", runner.batchCalls)
	}
	if got := runner.requests[0]; got.PID != 303 || !reflect.DeepEqual(got.GPUUUIDs, []string{testGPUA}) {
		t.Fatalf("first restore request = %#v, want PID 303 on %s", got, testGPUA)
	}
	if got := runner.requests[1]; got.PID != 404 || !reflect.DeepEqual(got.GPUUUIDs, []string{testGPUB}) {
		t.Fatalf("second restore request = %#v, want PID 404 on %s", got, testGPUB)
	}
}

func TestCustomStorageTargetGPUUUIDs(t *testing.T) {
	dir := t.TempDir()
	manifest := "version 2\ndevice_count 1\ndevice 0 " + testGPUA + " 4096 device-0000.bin\n"
	if err := os.WriteFile(filepath.Join(dir, "manifest.txt"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := customStorageTargetGPUUUIDs(dir, testGPUA+"="+testGPUB, []string{testGPUB})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{testGPUB}) {
		t.Fatalf("restore target GPUs = %#v, want %s", got, testGPUB)
	}
}

func TestCustomStorageTargetGPUUUIDsAllowsZeroDeviceProcess(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifest.txt"), []byte("version 2\ndevice_count 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := customStorageTargetGPUUUIDs(dir, "", []string{testGPUA})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{testGPUA}) {
		t.Fatalf("zero-device process target GPUs = %#v, want validated allocation", got)
	}
}

func TestReadCustomStorageSourceUUIDsRejectsTrailingData(t *testing.T) {
	dir := t.TempDir()
	manifest := "version 2\ndevice_count 1\ndevice 0 " + testGPUA + " 4096 device-0000.bin\nextra\n"
	if err := os.WriteFile(filepath.Join(dir, "manifest.txt"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readCustomStorageSourceUUIDs(filepath.Join(dir, "manifest.txt")); err == nil {
		t.Fatal("expected trailing manifest data to fail")
	}
}
