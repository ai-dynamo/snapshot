// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package cuda

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"testing"

	"github.com/ai-dynamo/snapshot/agent/internal/pagebroker"
	"github.com/go-logr/logr"
	"google.golang.org/protobuf/proto"
)

func TestNativeSessionsGlobalTransferBarrier(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(fmt.Sprint("transfer-failure=", failure), func(t *testing.T) {
			var mutex sync.Mutex
			transferred, completed := 0, 0
			sessions := NativeSessions{}
			defer sessions.Close()
			for _, pid := range []int{10, 20} {
				listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: t.TempDir() + "/session", Net: "unix"})
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				connection, err := net.DialUnix("unix", nil, listener.Addr().(*net.UnixAddr))
				if err != nil {
					t.Fatal(err)
				}
				file, err := connection.File()
				if err != nil {
					t.Fatal(err)
				}
				connection.Close()
				sessions[strconv.Itoa(pid)] = file
				server, err := listener.AcceptUnix()
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { server.Close() })
				go func() {
					defer server.Close()
					for {
						var size uint32
						if binary.Read(server, binary.BigEndian, &size) != nil {
							return
						}
						data := make([]byte, size)
						if _, err := io.ReadFull(server, data); err != nil {
							return
						}
						var request pagebroker.NativeSessionRequest
						if proto.Unmarshal(data, &request) != nil {
							return
						}
						if request.TargetPid != uint32(pid+100) {
							t.Errorf("worker target PID = %d, want restored PID %d", request.TargetPid, pid+100)
						}
						reply := &pagebroker.NativeSessionReply{Report: "{}"}
						mutex.Lock()
						switch request.Operation {
						case pagebroker.NativeSessionRequest_TRANSFER:
							transferred++
							if failure {
								reply.Failure = &pagebroker.Failure{}
								message := "injected copy failure"
								reply.Failure.Message = &message
							}
						case pagebroker.NativeSessionRequest_COMPLETE:
							completed++
							if transferred != 2 {
								t.Error("COMPLETE before all transfers")
							}
						}
						mutex.Unlock()
						data, _ = proto.Marshal(reply)
						if binary.Write(server, binary.BigEndian, uint32(len(data))) != nil {
							return
						}
						if _, err := server.Write(data); err != nil {
							return
						}
					}
				}()
			}
			err := RunNativeSessions(context.Background(), sessions, []int{10, 20}, false, logr.Discard(), []int{110, 120})
			mutex.Lock()
			defer mutex.Unlock()
			if (err != nil) != failure {
				t.Fatalf("error = %v", err)
			}
			if failure && completed != 0 {
				t.Fatal("failed transfer was completed")
			}
			if !failure && completed != 2 {
				t.Fatalf("completed %d targets", completed)
			}
		})
	}
}

func TestNativeSessionsRejectMissingCapability(t *testing.T) {
	if err := RunNativeSessions(context.Background(), map[string]*os.File{}, []int{1}, false, logr.Discard()); err == nil {
		t.Fatal("accepted missing native capability")
	}
}
