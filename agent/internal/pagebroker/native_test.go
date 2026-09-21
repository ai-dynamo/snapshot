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

func TestBindNativeRetainsOnlyScopedConnection(t *testing.T) {
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
		if request.GetBindNative().GetNamespacePid() != 123 ||
			request.GetBindNative().GetDirection() != BindNativeSession_SAVE {
			done <- fmt.Errorf("unexpected native binding: %v", request)
			return
		}
		body, err = proto.Marshal(&Response{
			RequestId: request.RequestId, TransactionId: request.TransactionId,
			Result: &Response_NativeSession{NativeSession: &NativeSessionReply{}},
		})
		if err == nil {
			err = writeMessage(connection, body)
		}
		if err == nil {
			_, err = connection.Write([]byte("scoped"))
		}
		done <- err
	}()
	file, err := (Client{ControlSocketPath: path}).BindNative(context.Background(), "transaction", &BindNativeSession{Direction: BindNativeSession_SAVE, NamespacePid: 123})
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
