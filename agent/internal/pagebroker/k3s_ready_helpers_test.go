// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pagebroker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestReadyConfig(t *testing.T) {
	values := map[string]string{"PAGEBROKER_READY_RUN_ID": uuid.NewString(),
		"PAGEBROKER_READY_SOCKET": "/shared/pb.sock", "PAGEBROKER_READY_REPORT": "/results/readiness.json"}
	if _, _, err := readReadyConfig(func(name string) string { return values[name] }); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, value string }{
		{"PAGEBROKER_READY_RUN_ID", "not-a-uuid"}, {"PAGEBROKER_READY_SOCKET", "relative"},
		{"PAGEBROKER_READY_REPORT", "/"}, {"PAGEBROKER_READY_REPORT", "/shared/pb.sock"},
	} {
		t.Run(test.name+test.value, func(t *testing.T) {
			_, _, err := readReadyConfig(func(name string) string {
				if name == test.name {
					return test.value
				}
				return values[name]
			})
			if err == nil {
				t.Fatal("accepted invalid readiness configuration")
			}
		})
	}
}

func TestReadyProbeUsesOnlyCapabilities(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "pb.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := listener.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	server := make(chan error, 1)
	go func() {
		connection, err := listener.AcceptUnix()
		if err != nil {
			server <- err
			return
		}
		defer connection.Close()
		request, err := readRequest(connection)
		if err == nil && (request.GetCapabilities() == nil || request.GetTransactionId() != "") {
			err = fmt.Errorf("readiness issued a transaction: %v", request)
		}
		if err == nil {
			err = writeReply(connection, request, &Response_Capabilities{Capabilities: &Capabilities{CustomStorageAvailable: true}})
		}
		server <- err
	}()
	path := filepath.Join(t.TempDir(), "readiness.json")
	report := readyReport{RunID: uuid.NewString(), Socket: socket, Status: "RUNNING"}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := probeK3sReadiness(ctx, path, report, (Client{ControlSocketPath: socket}).Capabilities); err != nil {
		t.Fatal(err)
	}
	if err := <-server; err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved readyReport
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Status != "SUCCESS" || saved.RunID != report.RunID || saved.Socket != socket || !saved.CustomStorageAvailable {
		t.Fatalf("unexpected readiness report: %+v", saved)
	}
	if err := probeK3sReadiness(ctx, path, report, nil); !errors.Is(err, os.ErrExist) {
		t.Fatalf("old report was not preserved: %v", err)
	}
}

func TestReadyProbeFailureReport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "readiness.json")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := probeK3sReadiness(ctx, path, readyReport{RunID: uuid.NewString(), Status: "RUNNING"},
		func(ctx context.Context) (*Capabilities, error) { return nil, ctx.Err() })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want canceled probe", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved readyReport
	if err := json.Unmarshal(data, &saved); err != nil || saved.Status != "ERROR" || saved.Error == "" {
		t.Fatalf("missing failure evidence: %+v, %v", saved, err)
	}
}
