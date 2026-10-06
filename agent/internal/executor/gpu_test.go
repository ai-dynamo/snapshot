// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/ai-dynamo/snapshot/agent/internal/pagebroker"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
	"google.golang.org/protobuf/proto"
)

func TestGPUCaptureOnlyFallsBackOnExplicitNegativeCapability(t *testing.T) {
	for _, outcome := range []string{"supported", "unsupported", "initialization-error", "lost-reply"} {
		t.Run(outcome, func(t *testing.T) {
			listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "broker.sock"))
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			server := make(chan error, 1)
			go func() {
				connection, err := listener.Accept()
				if err != nil {
					server <- err
					return
				}
				defer connection.Close()
				var size uint32
				if err := binary.Read(connection, binary.BigEndian, &size); err != nil {
					server <- err
					return
				}
				message := make([]byte, size)
				if _, err := io.ReadFull(connection, message); err != nil {
					server <- err
					return
				}
				request := new(pagebroker.Request)
				if err := proto.Unmarshal(message, request); err != nil {
					server <- err
					return
				}
				if outcome == "lost-reply" {
					server <- nil
					return
				}
				response := &pagebroker.Response{RequestId: request.RequestId, TransactionId: request.TransactionId,
					Result: &pagebroker.Response_Capabilities{Capabilities: &pagebroker.Capabilities{CustomStorageAvailable: outcome == "supported"}}}
				if outcome == "initialization-error" {
					code := pagebroker.Failure_INTERNAL_ERROR
					response.Result = &pagebroker.Response_Failure{Failure: &pagebroker.Failure{Code: &code}}
				}
				message, err = proto.Marshal(response)
				if err == nil {
					err = binary.Write(connection, binary.BigEndian, uint32(len(message)))
				}
				if err == nil {
					_, err = connection.Write(message)
				}
				server <- err
			}()
			selected, err := selectGPUCheckpoint(context.Background(), types.PageBrokerSpec{Enabled: true, ControlSocketPath: listener.Addr().String()}, true)
			wantError := outcome == "initialization-error" || outcome == "lost-reply"
			if (err != nil) != wantError || selected != (outcome == "supported") {
				t.Fatalf("selected=%t err=%v", selected, err)
			}
			if err := <-server; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCPUAndDisabledPageBrokerDoNotProbeCUDA(t *testing.T) {
	for _, state := range []struct{ enabled, cuda bool }{{false, true}, {true, false}, {false, false}} {
		selected, err := selectGPUCheckpoint(context.Background(), types.PageBrokerSpec{Enabled: state.enabled, ControlSocketPath: "/absent/socket"}, state.cuda)
		if selected || err != nil {
			t.Fatalf("state=%+v selected=%t err=%v", state, selected, err)
		}
	}
}

func TestFailedRestoreWaitsForAbortBeforeTermination(t *testing.T) {
	for _, outcome := range []string{"drained", "lost-abort-reply", "termination-error"} {
		t.Run(outcome, func(t *testing.T) {
			listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "broker.sock"))
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			abortReceived, replyAllowed := make(chan struct{}), make(chan struct{})
			server := make(chan error, 1)
			go func() {
				connection, err := listener.Accept()
				if err != nil {
					server <- err
					return
				}
				defer connection.Close()
				var size uint32
				if err := binary.Read(connection, binary.BigEndian, &size); err != nil {
					server <- err
					return
				}
				message := make([]byte, size)
				if _, err := io.ReadFull(connection, message); err != nil {
					server <- err
					return
				}
				request := new(pagebroker.Request)
				if err := proto.Unmarshal(message, request); err != nil {
					server <- err
					return
				}
				if request.GetAbort() == nil || request.GetTransactionId() != "failed-restore" {
					server <- fmt.Errorf("expected restore Abort, got %v", request)
					return
				}
				close(abortReceived)
				<-replyAllowed
				if outcome == "lost-abort-reply" {
					server <- nil
					return
				}
				message, err = proto.Marshal(&pagebroker.Response{RequestId: request.RequestId, TransactionId: request.TransactionId,
					Result: &pagebroker.Response_AbortComplete{AbortComplete: &pagebroker.AbortComplete{}}})
				if err == nil {
					err = binary.Write(connection, binary.BigEndian, uint32(len(message)))
				}
				if err == nil {
					_, err = connection.Write(message)
				}
				server <- err
			}()
			terminated := make(chan struct{}, 1)
			type result struct {
				drained bool
				err     error
			}
			finished := make(chan result, 1)
			go func() {
				drained, err := abortRestoreTransaction(pagebroker.Client{ControlSocketPath: listener.Addr().String()}, "failed-restore", func(context.Context) error {
					terminated <- struct{}{}
					if outcome == "termination-error" {
						return fmt.Errorf("stop failed")
					}
					return nil
				})
				finished <- result{drained, err}
			}()
			select {
			case <-abortReceived:
			case err := <-server:
				t.Fatalf("Abort request failed: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("Abort did not arrive")
			}
			select {
			case <-terminated:
				t.Fatal("terminated before broker confirmed drain")
			default:
			}
			close(replyAllowed)
			var got result
			select {
			case got = <-finished:
			case <-time.After(5 * time.Second):
				t.Fatal("cleanup did not finish")
			}
			if got.drained != (outcome != "lost-abort-reply") || (got.err != nil) != (outcome != "drained") {
				t.Fatalf("cleanup = %+v", got)
			}
			if len(terminated) != 0 && outcome == "lost-abort-reply" {
				t.Fatal("terminated without confirmed drain")
			}
			if len(terminated) != 1 && outcome != "lost-abort-reply" {
				t.Fatal("did not terminate after drain")
			}
			if err := <-server; err != nil {
				t.Fatal(err)
			}
		})
	}
}
