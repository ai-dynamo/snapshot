// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pagebroker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

type readyReport struct {
	RunID                  string `json:"runId"`
	Socket                 string `json:"socket"`
	Status                 string `json:"status"`
	CustomStorageAvailable bool   `json:"customStorageAvailable"`
	Error                  string `json:"error,omitempty"`
}

// TestPageBrokerK3sReady probes protocol readiness without creating a transaction.
func TestPageBrokerK3sReady(t *testing.T) {
	if os.Getenv("PAGEBROKER_READY_RUN_ID") == "" {
		t.Skip("requires PAGEBROKER_READY_RUN_ID, PAGEBROKER_READY_SOCKET and PAGEBROKER_READY_REPORT")
	}
	report, path, err := readReadyConfig(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	client := Client{ControlSocketPath: report.Socket}
	if err := probeK3sReadiness(ctx, path, report, client.Capabilities); err != nil {
		t.Fatal(err)
	}
}

// readReadyConfig validates the caller's identity and fresh report location.
func readReadyConfig(getenv func(string) string) (readyReport, string, error) {
	report := readyReport{RunID: getenv("PAGEBROKER_READY_RUN_ID"), Socket: getenv("PAGEBROKER_READY_SOCKET"), Status: "RUNNING"}
	id, err := uuid.Parse(report.RunID)
	if err != nil || id.String() != report.RunID {
		return report, "", errors.New("PAGEBROKER_READY_RUN_ID must be a canonical UUID")
	}
	path := getenv("PAGEBROKER_READY_REPORT")
	for name, value := range map[string]string{"SOCKET": report.Socket, "REPORT": path} {
		if !filepath.IsAbs(value) || filepath.Clean(value) != value || value == "/" {
			return report, path, fmt.Errorf("PAGEBROKER_READY_%s must be a normalized absolute path other than /", name)
		}
	}
	if path == report.Socket {
		return report, path, errors.New("readiness report must differ from the socket")
	}
	return report, path, nil
}

// probeK3sReadiness preserves a failure report and refuses to overwrite old evidence.
func probeK3sReadiness(ctx context.Context, path string, report readyReport, capabilities func(context.Context) (*Capabilities, error)) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if err := errors.Join(json.NewEncoder(file).Encode(report), file.Close()); err != nil {
		return err
	}
	response, err := capabilities(ctx)
	report.Status = "SUCCESS"
	if err != nil {
		report.Status, report.Error = "ERROR", err.Error()
	} else if response == nil {
		err = errors.New("missing capabilities response")
		report.Status, report.Error = "ERROR", err.Error()
	} else {
		report.CustomStorageAvailable = response.GetCustomStorageAvailable()
	}
	temporary, saveErr := os.CreateTemp(filepath.Dir(path), ".ready-report-")
	if saveErr != nil {
		return errors.Join(err, saveErr)
	}
	defer func() { _ = os.Remove(temporary.Name()) }()
	if saveErr := errors.Join(json.NewEncoder(temporary).Encode(report), temporary.Close()); saveErr != nil {
		return errors.Join(err, saveErr)
	}
	return errors.Join(err, os.Rename(temporary.Name(), path))
}
