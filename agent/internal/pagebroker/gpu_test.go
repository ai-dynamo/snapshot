// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package pagebroker

import (
	"context"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
)

func gpuListener(t *testing.T) (*net.UnixListener, *GPUExecution) {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "control")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(directory, "broker.sock"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	gpu, err := (Client{ControlSocketPath: listener.Addr().String()}).OpenGPUExecution("restore", &GpuContext{CapturedPids: []uint32{12}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gpu.Close() })
	// Namespace entry can hide the original socket path. The inherited directory
	// must still let nsrestore connect when CRIU has finished.
	if err := os.Rename(directory, directory+"-moved"); err != nil {
		t.Fatal(err)
	}
	return listener, gpu
}

func TestGPURequestCarriesHostPIDsAfterControlPathMoves(t *testing.T) {
	listener, gpu := gpuListener(t)
	server := make(chan error, 1)
	go func() {
		connection, err := listener.AcceptUnix()
		if err != nil {
			server <- err
			return
		}
		defer connection.Close()
		message, err := readMessage(connection)
		if err != nil {
			server <- err
			return
		}
		request := new(Request)
		if err := proto.Unmarshal(message, request); err != nil {
			server <- err
			return
		}
		targets := request.GetRestoreGpu().GetTargets()
		context := request.GetRestoreGpu().GetContext()
		if len(context.GetCapturedPids()) != 1 || context.CapturedPids[0] != 12 || len(targets) != 1 || targets[0].CapturedPid != 12 || targets[0].TargetPid != 112 || request.GetTransactionId() != "restore" {
			server <- fmt.Errorf("incorrect GPU restore request: %v", request)
			return
		}
		message, _ = proto.Marshal(&Response{RequestId: request.RequestId, TransactionId: request.TransactionId, Result: &Response_GpuRestoreComplete{GpuRestoreComplete: &GpuComplete{}}})
		server <- writeMessage(connection, message)
	}()
	if _, err := gpu.Restore(context.Background(), []int{12}, []int{112}); err != nil {
		t.Fatal(err)
	}
	if err := <-server; err != nil {
		t.Fatal(err)
	}
}

func TestGPUCancellationClosesExecutionConnection(t *testing.T) {
	listener, gpu := gpuListener(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := gpu.Checkpoint(ctx, []int{12}, []int{112}); result <- err }()
	peer, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	message, err := readMessage(peer)
	if err != nil {
		t.Fatal(err)
	}
	request := new(Request)
	if err := proto.Unmarshal(message, request); err != nil {
		t.Fatal(err)
	}
	targets := request.GetCheckpointGpu().GetTargets()
	if len(targets) != 1 || targets[0].CapturedPid != 12 || targets[0].TargetPid != 112 {
		t.Fatalf("incorrect checkpoint targets: %v", targets)
	}
	cancel()
	if err := <-result; err == nil {
		t.Fatal("cancelled execution succeeded")
	}
	if err := peer.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var data [1]byte
	if _, err := peer.Read(data[:]); err == nil {
		t.Fatal("peer did not observe cancellation")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("cancelled execution connection remained open")
	}
	if _, err := gpu.Directory.Stat(); err != nil {
		t.Fatalf("socket directory unexpectedly closed: %v", err)
	}
}

func TestGPURejectsInvalidParticipantMappingBeforeSending(t *testing.T) {
	for _, targets := range [][]int{{}, {0}, {2, 2}} {
		if _, err := (&GPUExecution{}).Restore(context.Background(), []int{12}, targets); err == nil {
			t.Fatalf("accepted mapping %v", targets)
		}
	}
}

func TestGPUTargetPIDBounds(t *testing.T) {
	for _, test := range []struct {
		name               string
		captured, observed int64
		valid              bool
	}{
		{"minimum", 1, 1, true},
		{"maximum", math.MaxInt32, math.MaxInt32, true},
		{"zero-captured", 0, 1, false},
		{"zero-observed", 1, 0, false},
		{"negative-captured", -1, 1, false},
		{"negative-observed", 1, -1, false},
		{"above-max-captured", math.MaxInt32 + 1, 1, false},
		{"above-max-observed", 1, math.MaxInt32 + 1, false},
		{"wrap-captured", math.MaxUint32 + 2, 1, false},
		{"wrap-observed", 1, math.MaxUint32 + 2, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if strconv.IntSize == 32 && (test.captured > math.MaxInt32 || test.observed > math.MaxInt32) {
				t.Skip("PID does not fit in int on this platform")
			}
			targets, err := gpuTargets([]int{int(test.captured)}, []int{int(test.observed)})
			if !test.valid {
				if err == nil {
					t.Fatalf("accepted captured PID %d and observed PID %d as %v", test.captured, test.observed, targets)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(targets) != 1 || int64(targets[0].CapturedPid) != test.captured || int64(targets[0].TargetPid) != test.observed {
				t.Fatalf("participant identity changed: %v", targets)
			}
		})
	}
}
