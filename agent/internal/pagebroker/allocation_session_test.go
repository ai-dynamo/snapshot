// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package pagebroker

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/proto"
)

func TestAbortDrainedRetriesActiveWorkerAdmission(t *testing.T) {
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "broker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		for attempt := 0; attempt < 2; attempt++ {
			connection, err := listener.Accept()
			if err != nil {
				done <- err
				return
			}
			body, err := readMessage(connection)
			request := new(Request)
			if err == nil {
				err = proto.Unmarshal(body, request)
			}
			if err == nil {
				response := &Response{RequestId: request.RequestId, TransactionId: request.TransactionId,
					Result: &Response_AbortComplete{AbortComplete: &AbortComplete{}}}
				if attempt == 0 {
					code := Failure_TRANSACTION_CONFLICT
					response.Result = &Response_Failure{Failure: &Failure{Code: &code}}
				}
				body, err = proto.Marshal(response)
				if err == nil {
					err = writeMessage(connection, body)
				}
			}
			_ = connection.Close()
			if err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	if err := (Client{ControlSocketPath: listener.Addr().String()}).AbortDrained(context.Background(), "transaction"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestBindAllocationsRetainsOnlyScopedConnection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broker.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer connection.Close()
		body, err := readMessage(connection)
		request := new(Request)
		if err == nil {
			err = proto.Unmarshal(body, request)
		}
		if err != nil {
			done <- err
			return
		}
		if request.GetBindAllocations().GetParticipantId() != "0123456789abcdef0123456789abcdef" ||
			request.GetBindAllocations().GetDirection() != BindAllocationSession_SAVE {
			done <- fmt.Errorf("unexpected allocation binding: %v", request)
			return
		}
		body, err = proto.Marshal(&Response{
			RequestId: request.RequestId, TransactionId: request.TransactionId,
			Result: &Response_AllocationSession{AllocationSession: &AllocationSessionReply{
				Result: &AllocationSessionReply_Completed{Completed: &AllocationBatch{}},
			}},
		})
		if err == nil {
			err = writeMessage(connection, body)
		}
		if err == nil {
			_, err = connection.Write([]byte("scoped"))
		}
		done <- err
	}()
	file, err := (Client{ControlSocketPath: path}).BindAllocations(context.Background(), "transaction", "0123456789abcdef0123456789abcdef", BindAllocationSession_SAVE)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	buffer := make([]byte, 6)
	if n, err := file.Read(buffer); err != nil || n != 6 || string(buffer) != "scoped" {
		t.Fatalf("scoped socket did not survive generic connection close: n=%d err=%v", n, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
