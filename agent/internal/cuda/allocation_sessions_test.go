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
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ai-dynamo/snapshot/agent/internal/pagebroker"
	"google.golang.org/protobuf/proto"
)

func TestLoadAdmissionIsConcurrentAndDrainsFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(strconv.FormatBool(fail), func(t *testing.T) {
			listener, err := net.Listen("unix", filepath.Join(shortTempDir(t), "broker"))
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				var connections []net.Conn
				defer func() {
					for _, c := range connections {
						_ = c.Close()
					}
				}()
				// No response until both connections arrive: sequential admission
				// would time out, while the successful capability must close on failure.
				for range 2 {
					c, err := listener.Accept()
					if err != nil {
						done <- err
						return
					}
					_ = c.SetDeadline(time.Now().Add(3 * time.Second))
					connections = append(connections, c)
				}
				for i, c := range connections {
					var size uint32
					if err := binary.Read(c, binary.BigEndian, &size); err != nil {
						done <- err
						return
					}
					data := make([]byte, size)
					if _, err := io.ReadFull(c, data); err != nil {
						done <- err
						return
					}
					var request pagebroker.Request
					if err := proto.Unmarshal(data, &request); err != nil {
						done <- err
						return
					}
					if fail && i == 1 {
						_ = c.Close()
						continue
					}
					response := &pagebroker.Response{RequestId: request.RequestId, TransactionId: request.TransactionId,
						Result: &pagebroker.Response_AllocationSession{AllocationSession: &pagebroker.AllocationSessionReply{
							Result: &pagebroker.AllocationSessionReply_Completed{Completed: &pagebroker.AllocationBatch{}}}}}
					data, err := proto.Marshal(response)
					if err != nil {
						done <- err
						return
					}
					if err := binary.Write(c, binary.BigEndian, uint32(len(data))); err != nil {
						done <- err
						return
					}
					if _, err := c.Write(data); err != nil {
						done <- err
						return
					}
				}
				var one [1]byte
				_, err := connections[0].Read(one[:])
				if err != io.EOF {
					done <- fmt.Errorf("capability not closed: %v", err)
					return
				}
				done <- nil
			}()
			sessions, err := BindAllocationSessions(ctx, pagebroker.Client{ControlSocketPath: listener.Addr().String()}, "test",
				[]string{strings.Repeat("01", 16), strings.Repeat("02", 16)}, pagebroker.BindAllocationSession_LOAD)
			if (err != nil) != fail {
				t.Fatalf("bind error=%v", err)
			}
			sessions.Close()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAllocationSessionsAppendAndClose(t *testing.T) {
	session, err := os.Open("/dev/null")
	if err != nil {
		t.Fatal(err)
	}
	existing, err := os.Open("/dev/null")
	if err != nil {
		t.Fatal(err)
	}
	defer existing.Close()
	id := "0123456789abcdef0123456789abcdef"
	sessions := AllocationSessions{id: session}
	cmd := exec.Command("coordinator", "--prepare")
	// Capture reserves eight descriptors, so the first capability must be fd11.
	cmd.ExtraFiles = make([]*os.File, 8)
	cmd.ExtraFiles[0] = existing
	sessions.AppendTo(cmd)
	want := []string{"coordinator", "--prepare", "--content-storage", "pagebroker", "--allocation-session", id, "11"}
	if !slices.Equal(cmd.Args, want) || len(cmd.ExtraFiles) != 9 || cmd.ExtraFiles[0] != existing || cmd.ExtraFiles[8] != session {
		t.Fatalf("unexpected capability mapping: %v, files=%v", cmd.Args, cmd.ExtraFiles)
	}
	sessions.Close()
	sessions.Close()
	if len(sessions) != 0 {
		t.Fatal("capability map not drained")
	}
	if _, err := session.Stat(); err == nil {
		t.Fatal("session descriptor remained open")
	}
	if _, err := existing.Stat(); err != nil {
		t.Fatalf("namespace descriptor was closed: %v", err)
	}
}
