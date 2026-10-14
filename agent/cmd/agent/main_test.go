// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/ai-dynamo/snapshot/agent/internal/pagebroker"
	"google.golang.org/protobuf/proto"
)

func TestWaitForPageBrokerRetriesUntilSocketReady(t *testing.T) {
	for _, staleSocket := range []bool{false, true} {
		name := "missing"
		if staleSocket {
			name = "refused"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "broker.sock")
			if staleSocket {
				listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
				if err != nil {
					t.Fatal(err)
				}
				listener.SetUnlinkOnClose(false)
				if err := listener.Close(); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			client := pagebroker.Client{ControlSocketPath: path}
			_, err := client.Capabilities(ctx)
			want := syscall.ENOENT
			if staleSocket {
				want = syscall.ECONNREFUSED
			}
			if !errors.Is(err, want) {
				t.Fatalf("initial error = %v, want %v", err, want)
			}
			finished := make(chan error, 1)
			started := time.Now()
			go func() {
				capabilities, err := waitForPageBroker(ctx, client)
				if err == nil && !capabilities.GetCustomStorageAvailable() {
					err = errors.New("GPU capability missing")
				}
				finished <- err
			}()
			// Keep the endpoint unavailable until the readiness loop has retried.
			time.Sleep(200 * time.Millisecond)
			if staleSocket {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			if err := listener.SetDeadline(time.Now().Add(4 * time.Second)); err != nil {
				t.Fatal(err)
			}
			connection, err := listener.AcceptUnix()
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			if err := connection.SetDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			var size uint32
			if err := binary.Read(connection, binary.BigEndian, &size); err != nil {
				t.Fatal(err)
			}
			message := make([]byte, size)
			if _, err := io.ReadFull(connection, message); err != nil {
				t.Fatal(err)
			}
			request := new(pagebroker.Request)
			if err := proto.Unmarshal(message, request); err != nil {
				t.Fatal(err)
			}
			response := &pagebroker.Response{RequestId: request.RequestId, Result: &pagebroker.Response_Capabilities{Capabilities: &pagebroker.Capabilities{CustomStorageAvailable: true}}}
			message, err = proto.Marshal(response)
			if err != nil {
				t.Fatal(err)
			}
			if err := binary.Write(connection, binary.BigEndian, uint32(len(message))); err != nil {
				t.Fatal(err)
			}
			if _, err := connection.Write(message); err != nil {
				t.Fatal(err)
			}
			if err := <-finished; err != nil {
				t.Fatal(err)
			}
			if time.Since(started) < time.Second {
				t.Fatal("readiness did not exercise retry delay")
			}
		})
	}
}

func TestWaitForPageBrokerCancellationDuringRetry(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := waitForPageBroker(ctx, pagebroker.Client{ControlSocketPath: filepath.Join(t.TempDir(), "missing.sock")})
	if !errors.Is(err, syscall.ENOENT) || ctx.Err() == nil {
		t.Fatalf("wait returned %v, context %v", err, ctx.Err())
	}
	if time.Since(started) >= time.Second {
		t.Fatal("cancellation waited for the retry timer")
	}
}

func TestWaitForPageBrokerReturnsPermanentError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, err := waitForPageBroker(ctx, pagebroker.Client{ControlSocketPath: filepath.Join(path, "broker.sock")})
	if !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("wait returned %v, want ENOTDIR", err)
	}
	if ctx.Err() != nil {
		t.Fatal("permanent error was retried until cancellation")
	}
}
