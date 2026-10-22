// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pagebroker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSmokeTreeRejectsDamagedRestores(t *testing.T) {
	for _, mutation := range []string{"contents", "truncated", "mode", "missing", "extra", "symlink", "stale-cycle"} {
		t.Run(mutation, func(t *testing.T) {
			directory := t.TempDir()
			if err := writeSmokeFixture(t.Context(), directory, (1<<20)+37, "cycle-1"); err != nil {
				t.Fatal(err)
			}
			expected, err := readSmokeTree(directory)
			if err != nil {
				t.Fatal(err)
			}
			if err := verifySmokeTree(directory, expected); err != nil {
				t.Fatal(err)
			}
			payload := filepath.Join(directory, "nested/data.bin")
			switch mutation {
			case "contents":
				file, openErr := os.OpenFile(payload, os.O_WRONLY, 0)
				if openErr != nil {
					t.Fatal(openErr)
				}
				_, err = file.WriteAt([]byte("changed"), 1<<20)
				err = errors.Join(err, file.Close())
			case "truncated":
				err = os.Truncate(payload, 1<<20)
			case "mode":
				err = os.Chmod(payload, 0644)
			case "missing":
				err = os.Remove(filepath.Join(directory, "empty"))
			case "extra":
				err = os.WriteFile(filepath.Join(directory, "extra"), nil, 0600)
			case "symlink":
				err = os.Remove(payload)
				if err == nil {
					err = os.Symlink("small", payload)
				}
			case "stale-cycle":
				directory = t.TempDir()
				err = writeSmokeFixture(t.Context(), directory, (1<<20)+37, "cycle-2")
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := verifySmokeTree(directory, expected); err == nil {
				t.Fatal("damaged restore was accepted")
			}
		})
	}
}

func TestSmokePayloadStopsAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := writeSmokePayload(ctx, filepath.Join(t.TempDir(), "payload"), smokePayloadBytes, "cancelled"); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
}

func TestSmokeConfigRequiresExplicitPathsAndStore(t *testing.T) {
	valid := map[string]string{
		"PAGEBROKER_SMOKE_MODE": "filesystem", "PAGEBROKER_SMOKE_SOCKET": "/shared/pb.sock",
		"PAGEBROKER_SMOKE_STAGING_ROOT": "/shared/staging", "PAGEBROKER_SMOKE_STORAGE_ROOT": "/storage",
		"PAGEBROKER_SMOKE_REPORT": "/results/smoke.json",
	}
	for _, change := range []struct{ key, value string }{
		{"PAGEBROKER_SMOKE_MODE", "unknown"}, {"PAGEBROKER_SMOKE_MODE", "s3"},
		{"PAGEBROKER_SMOKE_SOCKET", ""}, {"PAGEBROKER_SMOKE_REPORT", "relative.json"},
		{"PAGEBROKER_SMOKE_STAGING_ROOT", "/shared/../staging"}, {"PAGEBROKER_SMOKE_STORAGE_ROOT", "/"},
	} {
		t.Run(change.key+"="+change.value, func(t *testing.T) {
			getenv := func(name string) string {
				if name == change.key {
					return change.value
				}
				return valid[name]
			}
			if _, err := readSmokeConfig(getenv); err == nil {
				t.Fatal("invalid smoke configuration was accepted")
			}
		})
	}
	for _, mode := range []string{"filesystem", "s3"} {
		valid["PAGEBROKER_SMOKE_MODE"] = mode
		valid["PAGEBROKER_SMOKE_STORE_ID"] = "smoke-store"
		if _, err := readSmokeConfig(func(name string) string { return valid[name] }); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSmokeStagingRejectsEscapes(t *testing.T) {
	root := t.TempDir()
	directory, err := os.MkdirTemp(root, "transaction-")
	if err != nil {
		t.Fatal(err)
	}
	if err := validateSmokeStaging(root, directory); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	escape := filepath.Join(root, "escape")
	if err := os.Symlink(outside, escape); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{root, outside, escape, "relative", root + "/../outside"} {
		if err := validateSmokeStaging(root, invalid); err == nil {
			t.Errorf("invalid staging directory accepted: %s", invalid)
		}
	}
}

func TestSmokeReportRejectsReuseAndRecordsFailedSteps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	report := smokeReport{Status: "RUNNING"}
	if err := createSmokeReport(path, &report); err != nil {
		t.Fatal(err)
	}
	if err := createSmokeReport(path, &smokeReport{Status: "SUCCESS"}); !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing report was not rejected: %v", err)
	}
	runner := smokeRunner{config: smokeConfig{Report: path}, report: &report, logf: t.Logf}
	failure := errors.New("hash mismatch")
	if err := runner.step(t.Context(), "restore", func(context.Context) error { return failure }); !errors.Is(err, failure) {
		t.Fatalf("lost step failure: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved smokeReport
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Status == "SUCCESS" || len(saved.Steps) != 1 || saved.Steps[0].Status != "ERROR" || saved.Steps[0].Error != failure.Error() {
		t.Fatalf("failed step was not recorded: %+v", saved)
	}
}
