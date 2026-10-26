// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pagebroker

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
)

func readRequest(connection net.Conn) (*Request, error) {
	message, err := readMessage(connection)
	if err != nil {
		return nil, err
	}
	request := new(Request)
	if err := proto.Unmarshal(message, request); err != nil {
		return nil, err
	}
	return request, nil
}

func writeReply(connection net.Conn, request *Request, result isResponse_Result) error {
	message, err := proto.Marshal(&Response{
		RequestId:     request.RequestId,
		TransactionId: request.TransactionId,
		Result:        result,
	})
	if err != nil {
		return err
	}
	return writeMessage(connection, message)
}

func TestFailureCodeMapsUnknownValuesToUnspecified(t *testing.T) {
	if got := failureCode(Failure_Code(99)); got != Failure_UNSPECIFIED {
		t.Fatalf("failureCode(99) = %v, want %v", got, Failure_UNSPECIFIED)
	}
}

func TestCapabilitiesProbe(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(fmt.Sprint(valid), func(t *testing.T) {
			socket := filepath.Join(t.TempDir(), "broker.sock")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			done := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					done <- err
					return
				}
				defer conn.Close()
				r, err := readRequest(conn)
				if err != nil {
					done <- err
					return
				}
				if r.GetCapabilities() == nil || r.GetTransactionId() != "" {
					done <- errors.New("invalid probe request")
					return
				}
				if valid {
					done <- writeReply(conn, r, &Response_Capabilities{Capabilities: &Capabilities{}})
				} else {
					done <- writeReply(conn, r, &Response_CommitComplete{CommitComplete: &CommitComplete{}})
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, err = (Client{ControlSocketPath: socket}).Capabilities(ctx)
			if (err == nil) != valid {
				t.Fatalf("valid=%v: %v", valid, err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRequestStopsWhenContextIsCanceled(t *testing.T) {
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "pagebroker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		connection, err := listener.Accept()
		if err == nil {
			accepted <- connection
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- (Client{ControlSocketPath: listener.Addr().String()}).Abort(ctx, "transaction")
	}()

	connection := <-accepted
	defer connection.Close()
	if _, err := readMessage(connection); err != nil {
		t.Fatal(err)
	}

	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("request succeeded after its context was canceled")
		}
	case <-time.After(time.Second):
		t.Fatal("request did not stop after its context was canceled")
	}
}

func TestRequestWaitsForSocketToAppear(t *testing.T) {
	const limit = 5 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()

	socketPath := filepath.Join(t.TempDir(), "pagebroker.sock")
	result := make(chan error, 1)
	go func() {
		result <- (Client{ControlSocketPath: socketPath}).Abort(ctx, "transaction")
	}()

	time.Sleep(300 * time.Millisecond)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := listener.(*net.UnixListener).SetDeadline(time.Now().Add(limit)); err != nil {
		t.Fatal(err)
	}
	connection, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(limit)); err != nil {
		t.Fatal(err)
	}
	request, err := readRequest(connection)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeReply(connection, request, &Response_AbortComplete{AbortComplete: &AbortComplete{}}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Abort() = %v, want success after the socket appeared", err)
		}
	case <-time.After(limit):
		t.Fatal("Abort() did not return after the socket appeared")
	}
}

func TestRequestStopsWaitingForSocketWhenContextExpires(t *testing.T) {
	const limit = 200 * time.Millisecond
	socketPath := filepath.Join(t.TempDir(), "missing.sock")

	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()

	start := time.Now()
	err := (Client{ControlSocketPath: socketPath}).Abort(ctx, "transaction")
	// Caller's context expired, not just the local retry budget.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Abort() error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed < limit || elapsed > 5*time.Second {
		t.Fatalf("Abort() returned after %v, want about %v", elapsed, limit)
	}
}

func TestRequestReturnsAPermanentDialErrorImmediately(t *testing.T) {
	const limit = 350 * time.Millisecond

	regularFile := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(regularFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(regularFile, "pagebroker.sock")
	client := Client{ControlSocketPath: socketPath}

	requests := map[string]func(context.Context, string) error{
		"Abort":  client.Abort,
		"Commit": client.Commit,
	}
	for name, request := range requests {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), limit)
			defer cancel()

			start := time.Now()
			err := request(ctx, "transaction")
			if !errors.Is(err, unix.ENOTDIR) {
				t.Fatalf("%s() error = %v, want errors.Is(err, unix.ENOTDIR)", name, err)
			}
			if !IsDialError(err) {
				t.Fatalf("IsDialError(%v) = false, want true", err)
			}
			if elapsed := time.Since(start); elapsed >= limit {
				t.Fatalf("%s() returned after %v, want well under the %v caller deadline", name, elapsed, limit)
			}
		})
	}
}

func TestCommitRetriesLostResponses(t *testing.T) {
	const limit = 5 * time.Second
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "pagebroker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := listener.(*net.UnixListener).SetDeadline(time.Now().Add(limit)); err != nil {
		t.Fatal(err)
	}

	requests := make(chan *Request, 3)
	server := make(chan error, 1)
	go func() {
		for attempt := 0; attempt < 3; attempt++ {
			connection, err := listener.Accept()
			if err != nil {
				server <- err
				return
			}
			if err := connection.SetDeadline(time.Now().Add(limit)); err != nil {
				_ = connection.Close()
				server <- err
				return
			}
			request, err := readRequest(connection)
			if err != nil {
				_ = connection.Close()
				server <- err
				return
			}
			requests <- request
			if attempt == 2 {
				if err := writeReply(connection, request, &Response_CommitComplete{CommitComplete: &CommitComplete{}}); err != nil {
					_ = connection.Close()
					server <- err
					return
				}
			}
			_ = connection.Close()
		}
		server <- nil
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := (Client{ControlSocketPath: listener.Addr().String()}).Commit(ctx, "transaction"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-server:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(limit):
		t.Fatal("fake server did not observe all retry attempts")
	}
	for range 3 {
		request := <-requests
		if request.GetTransactionId() != "transaction" || request.GetCommit() == nil {
			t.Fatalf("unexpected retry request: %v", request)
		}
	}
}

func TestCommitStopsWhenRetryResponseHangs(t *testing.T) {
	const limit = 5 * time.Second

	listener := newBoundedUnixListener(t, limit)
	accepted, acceptErr := acceptConnections(listener, 2, limit)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- (Client{ControlSocketPath: listener.Addr().String()}).Commit(ctx, "transaction")
	}()

	first := acceptConnection(t, accepted, acceptErr, limit, "first")
	if _, err := readMessage(first); err != nil {
		t.Fatal(err)
	}
	_ = first.Close()

	second := acceptConnection(t, accepted, acceptErr, limit, "retry")
	defer second.Close()
	if _, err := readMessage(second); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) || IsDialError(err) {
			t.Fatalf("Commit() error = %v, want retry deadline after a lost response", err)
		}
	case <-time.After(limit):
		t.Fatal("Commit did not return after the retry deadline")
	}
	if _, err := readMessage(second); !errors.Is(err, io.EOF) {
		t.Fatalf("retry connection read error = %v, want io.EOF from peer close", err)
	}
}

func TestCommitStopsOnItsOwnRetryBudgetWithALongCallerDeadline(t *testing.T) {
	const limit = 5 * time.Second
	original := commitRetryLimit
	commitRetryLimit = 200 * time.Millisecond
	defer func() { commitRetryLimit = original }()

	listener := newBoundedUnixListener(t, limit)
	accepted, acceptErr := acceptConnections(listener, 2, limit)

	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- (Client{ControlSocketPath: listener.Addr().String()}).Commit(ctx, "transaction")
	}()

	first := acceptConnection(t, accepted, acceptErr, limit, "first")
	if _, err := readMessage(first); err != nil {
		t.Fatal(err)
	}
	_ = first.Close()

	second := acceptConnection(t, accepted, acceptErr, limit, "retry")
	defer second.Close()
	if _, err := readMessage(second); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) || IsDialError(err) {
			t.Fatalf("Commit() error = %v, want its own retry deadline, not the caller's", err)
		}
		if ctx.Err() != nil {
			t.Fatal("caller context already expired; Commit did not stop on its own retry budget")
		}
	case <-time.After(limit):
		t.Fatal("Commit did not return after its own retry budget expired")
	}
}

func newBoundedUnixListener(t *testing.T, limit time.Duration) net.Listener {
	t.Helper()
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "pagebroker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	if err := listener.(*net.UnixListener).SetDeadline(time.Now().Add(limit)); err != nil {
		t.Fatal(err)
	}
	return listener
}

func acceptConnections(listener net.Listener, count int, limit time.Duration) (<-chan net.Conn, <-chan error) {
	accepted := make(chan net.Conn, count)
	acceptErr := make(chan error, 1)
	go func() {
		for range count {
			connection, err := listener.Accept()
			if err != nil {
				acceptErr <- err
				return
			}
			if err := connection.SetDeadline(time.Now().Add(limit)); err != nil {
				acceptErr <- err
				return
			}
			accepted <- connection
		}
	}()
	return accepted, acceptErr
}

func acceptConnection(t *testing.T, accepted <-chan net.Conn, acceptErr <-chan error, limit time.Duration, label string) net.Conn {
	t.Helper()
	select {
	case connection := <-accepted:
		return connection
	case err := <-acceptErr:
		t.Fatalf("accept %s connection: %v", label, err)
	case <-time.After(limit):
		t.Fatalf("server did not observe the %s connection", label)
	}
	return nil
}

func TestAbortRequiresAbortComplete(t *testing.T) {
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "pagebroker.sock"))
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
		request, err := readRequest(connection)
		if err != nil {
			server <- err
			return
		}
		server <- writeReply(connection, request, &Response_CommitComplete{CommitComplete: &CommitComplete{}})
	}()

	if err := (Client{ControlSocketPath: listener.Addr().String()}).Abort(context.Background(), "transaction"); err == nil {
		t.Fatal("abort accepted a commit response")
	}
	if err := <-server; err != nil {
		t.Fatal(err)
	}
}

func TestImageDirectoryResponses(t *testing.T) {
	for _, tc := range []struct {
		name  string
		call  func(Client, context.Context) (string, error)
		reply func(string) isResponse_Result
	}{
		{"restore", func(c Client, ctx context.Context) (string, error) {
			return c.StagedRestore(ctx, "transaction", "/checkpoints/source")
		}, func(directory string) isResponse_Result {
			return &Response_StagedRestoreDirectory{StagedRestoreDirectory: &StagedRestoreDirectory{ImageDirectory: &directory}}
		}},
		{"checkpoint", func(c Client, ctx context.Context) (string, error) {
			return c.PrepareCheckpoint(ctx, "transaction", "/checkpoints/destination")
		}, func(directory string) isResponse_Result {
			return &Response_StagedCheckpointDirectory{StagedCheckpointDirectory: &StagedCheckpointDirectory{ImageDirectory: &directory}}
		}},
		{"direct-checkpoint", func(c Client, ctx context.Context) (string, error) {
			return c.PrepareDirectCheckpoint(ctx, "transaction", "/checkpoints/destination")
		}, func(directory string) isResponse_Result {
			return &Response_DirectCheckpointDirectory{DirectCheckpointDirectory: &DirectCheckpointDirectory{ImageDirectory: &directory}}
		}},
	} {
		for _, outcome := range []string{"valid", "empty", "wrong-response"} {
			t.Run(tc.name+"/"+outcome, func(t *testing.T) {
				listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "pagebroker.sock"))
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
					request, err := readRequest(connection)
					if err != nil {
						server <- err
						return
					}
					if request.GetRequestBrokerPidfd() {
						server <- fmt.Errorf("ordinary storage request asked for a broker pidfd")
						return
					}
					if tc.name == "direct-checkpoint" {
						prepare := request.GetPrepareDirectCheckpoint()
						if prepare == nil || prepare.GetDestination().GetFilesystem().GetDirectory() != "/checkpoints/destination" || prepare.GetIoEngine().GetPosixCopy() == nil {
							server <- fmt.Errorf("unexpected direct checkpoint request: %v", request)
							return
						}
					}
					directory := "/checkpoints/prepared"
					if outcome == "empty" {
						directory = ""
					}
					result := tc.reply(directory)
					if outcome == "wrong-response" {
						result = &Response_CommitComplete{CommitComplete: &CommitComplete{}}
					}
					server <- writeReply(connection, request, result)
				}()
				directory, err := tc.call(Client{ControlSocketPath: listener.Addr().String()}, context.Background())
				if outcome == "valid" {
					if err != nil || directory != "/checkpoints/prepared" {
						t.Fatalf("directory=%q, err=%v", directory, err)
					}
				} else if err == nil {
					t.Fatal("accepted invalid image directory response")
				}
				if err := <-server; err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestCommitDoesNotRetryInvalidFrame(t *testing.T) {
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "pagebroker.sock"))
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
		if _, err := readMessage(connection); err != nil {
			server <- err
			return
		}
		server <- binary.Write(connection, binary.BigEndian, uint32(maxMessageSize+1))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = (Client{ControlSocketPath: listener.Addr().String()}).Commit(ctx, "transaction")
	if !errors.Is(err, errMessageTooLarge) {
		t.Fatalf("Commit() error = %v, want invalid frame error", err)
	}
	if err := <-server; err != nil {
		t.Fatal(err)
	}
}

func TestDirectRestoreResponse(t *testing.T) {
	for _, ready := range []bool{true, false} {
		t.Run(map[bool]string{true: "ready", false: "wrong response"}[ready], func(t *testing.T) {
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
				request, err := readRequest(connection)
				if err != nil {
					server <- err
					return
				}
				if request.GetTransactionId() != "transaction" || request.GetDirectRestore().GetSource().GetFilesystem().GetDirectory() != "/checkpoints/source" || request.GetDirectRestore().GetIoEngine().GetPosixCopy() == nil {
					server <- errors.New("unexpected direct restore request")
					return
				}
				var result isResponse_Result
				if ready {
					result = &Response_DirectRestoreReady{DirectRestoreReady: &DirectRestoreReady{}}
				} else {
					result = &Response_CommitComplete{CommitComplete: &CommitComplete{}}
				}
				server <- writeReply(connection, request, result)
			}()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err = (Client{ControlSocketPath: listener.Addr().String()}).DirectRestore(ctx, "transaction", "/checkpoints/source")
			if (err == nil) != ready {
				t.Fatalf("ready=%v: %v", ready, err)
			}
			if err := <-server; err != nil {
				t.Fatal(err)
			}
		})
	}
}

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

func TestTruncatedPreparationClosesReceivedProcessDescriptor(t *testing.T) {
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(t.TempDir(), "broker.sock"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	process := openTestPidfd(t)
	before := countProcessDescriptors(t)
	server := make(chan error, 1)
	go func() {
		connection, err := listener.AcceptUnix()
		if err != nil {
			server <- err
			return
		}
		defer connection.Close()
		// Announce a body, transfer the pidfd, then disconnect without the body.
		_, _, err = connection.WriteMsgUnix([]byte{0, 0, 0, 100}, unix.UnixRights(int(process.Fd())), nil)
		server <- err
	}()
	connection, err := net.DialUnix("unix", nil, listener.Addr().(*net.UnixAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_, files, err := readMessageWithFiles(connection)
	if !errors.Is(err, io.EOF) || len(files) != 0 {
		t.Fatalf("truncated preparation: files=%v, error=%v", files, err)
	}
	if err := <-server; err != nil {
		t.Fatal(err)
	}
	if after := countProcessDescriptors(t); after != before {
		t.Fatalf("process descriptor leak: before %d, after %d", before, after)
	}
}

func TestUnrequestedProcessDescriptorIsRejectedAndClosed(t *testing.T) {
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(t.TempDir(), "broker.sock"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	process := openTestPidfd(t)
	before := countProcessDescriptors(t)
	server := make(chan error, 1)
	go func() {
		connection, err := listener.AcceptUnix()
		if err != nil {
			server <- err
			return
		}
		defer connection.Close()
		request, err := readRequest(connection)
		if err != nil {
			server <- err
			return
		}
		directory := "/checkpoints/prepared"
		message, err := proto.Marshal(&Response{
			RequestId:     request.RequestId,
			TransactionId: request.TransactionId,
			Result:        &Response_DirectCheckpointDirectory{DirectCheckpointDirectory: &DirectCheckpointDirectory{ImageDirectory: &directory}},
		})
		if err != nil {
			server <- err
			return
		}
		header := binary.BigEndian.AppendUint32(nil, uint32(len(message)))
		if _, _, err := connection.WriteMsgUnix(header, unix.UnixRights(int(process.Fd())), nil); err != nil {
			server <- err
			return
		}
		_, err = connection.Write(message)
		server <- err
	}()
	_, err = (Client{ControlSocketPath: listener.Addr().String()}).PrepareDirectCheckpoint(context.Background(), "transaction", "/checkpoints/destination")
	if err == nil || !strings.Contains(err.Error(), "unexpected PageBroker response descriptors") {
		t.Fatalf("unrequested broker pidfd: %v", err)
	}
	if err := <-server; err != nil {
		t.Fatal(err)
	}
	if after := countProcessDescriptors(t); after != before {
		t.Fatalf("process descriptor leak: before %d, after %d", before, after)
	}
}
