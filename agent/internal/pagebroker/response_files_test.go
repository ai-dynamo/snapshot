// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package pagebroker

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
)

func countProcessDescriptors(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		if target, _ := os.Readlink("/proc/self/fd/" + entry.Name()); target == "anon_inode:[pidfd]" {
			count++
		}
	}
	return count
}

func TestGPUPreparationRetainsOnlyValidProcessDescriptor(t *testing.T) {
	for _, outcome := range []string{"valid", "missing", "multiple", "ordinary-file", "wrong-response", "mismatched-id", "partial-body"} {
		t.Run(outcome, func(t *testing.T) {
			listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(t.TempDir(), "broker.sock"), Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			process := openTestPidfd(t)
			ordinary, err := os.Open("/dev/null")
			if err != nil {
				t.Fatal(err)
			}
			defer ordinary.Close()
			before := countProcessDescriptors(t)
			server := make(chan error, 1)
			go func() {
				connection, err := listener.AcceptUnix()
				if err != nil {
					server <- err
					return
				}
				defer connection.Close()
				request, _, err := readGPURequest(connection)
				if err != nil {
					server <- err
					return
				}
				response := &Response{RequestId: request.RequestId, TransactionId: request.TransactionId,
					Result: &Response_DirectRestoreReady{DirectRestoreReady: &DirectRestoreReady{}}}
				files := []*os.File{process}
				switch outcome {
				case "missing":
					files = nil
				case "multiple":
					files = append(files, process)
				case "ordinary-file":
					files = []*os.File{ordinary}
				case "wrong-response":
					response.Result = &Response_CommitComplete{CommitComplete: &CommitComplete{}}
				case "mismatched-id":
					response.RequestId = proto.String("different-request")
				}
				message, _ := proto.Marshal(response)
				if outcome == "partial-body" {
					// Announce a longer body, transfer the pidfd, then disconnect.
					_, _, err = connection.WriteMsgUnix([]byte{0, 0, 0, 100}, unix.UnixRights(int(process.Fd())), nil)
				} else {
					err = writeRequest(connection, message, files)
				}
				server <- err
			}()
			ctx, cancel := context.WithTimeout(context.Background(), gpuTestTimeout)
			defer cancel()
			_, execution, err := (Client{ControlSocketPath: listener.Addr().String()}).PrepareGPURestore(ctx, "restore", "/checkpoint", true, &GpuContext{})
			if outcome == "valid" {
				if err != nil {
					t.Fatal(err)
				}
				identity, readErr := os.ReadFile(fmt.Sprintf("/proc/self/fdinfo/%d", execution.BrokerProcess.Fd()))
				if readErr != nil || !strings.Contains(string(identity), fmt.Sprintf("Pid:\t%d\n", os.Getpid())) {
					t.Fatalf("wrong retained broker: %s, %v", identity, readErr)
				}
				flags, flagErr := unix.FcntlInt(execution.BrokerProcess.Fd(), unix.F_GETFD, 0)
				if flagErr != nil || flags&unix.FD_CLOEXEC == 0 {
					t.Fatalf("retained broker descriptor can leak through exec: %v", flagErr)
				}
				execution.Close()
			} else if err == nil || execution != nil {
				t.Fatalf("accepted %s response: %v", outcome, execution)
			}
			select {
			case err := <-server:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(gpuTestTimeout):
				t.Fatal("preparation server did not finish")
			}
			if after := countProcessDescriptors(t); after != before {
				t.Fatalf("process descriptor leak: before %d, after %d", before, after)
			}
		})
	}
}
