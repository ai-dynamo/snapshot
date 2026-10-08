// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package pagebroker

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
)

const gpuTestTimeout = 5 * time.Second

func gpuListener(t *testing.T) (*net.UnixListener, *CustomStorageExecution) {
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
	if err := listener.SetDeadline(time.Now().Add(gpuTestTimeout)); err != nil {
		t.Fatal(err)
	}
	gpu, err := (Client{ControlSocketPath: listener.Addr().String()}).openCustomStorageExecution("restore", &GpuContext{CapturedPids: []uint32{12}}, openTestPidfd(t))
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

func openTestPidfd(t *testing.T) *os.File {
	t.Helper()
	fd, err := unix.PidfdOpen(os.Getpid(), 0)
	if err != nil {
		t.Fatal(err)
	}
	file := os.NewFile(uintptr(fd), "target-pidfd")
	t.Cleanup(func() { _ = file.Close() })
	return file
}

func readGPURequest(connection *net.UnixConn) (*Request, []int, error) {
	header := make([]byte, 4)
	control := make([]byte, unix.CmsgSpace(4*maxPassedFiles))
	n, controlSize, flags, _, err := connection.ReadMsgUnix(header, control)
	if err != nil {
		return nil, nil, err
	}
	messages, err := unix.ParseSocketControlMessage(control[:controlSize])
	if err != nil {
		return nil, nil, err
	}
	var descriptors []int
	for _, message := range messages {
		files, err := unix.ParseUnixRights(&message)
		if err != nil {
			return nil, descriptors, err
		}
		descriptors = append(descriptors, files...)
	}
	if flags&unix.MSG_CTRUNC != 0 {
		return nil, descriptors, fmt.Errorf("truncated GPU descriptors")
	}
	if _, err := io.ReadFull(connection, header[n:]); err != nil {
		return nil, descriptors, err
	}
	message := make([]byte, binary.BigEndian.Uint32(header))
	if _, err := io.ReadFull(connection, message); err != nil {
		return nil, descriptors, err
	}
	request := new(Request)
	err = proto.Unmarshal(message, request)
	return request, descriptors, err
}

func TestGPURequestCarriesHostPIDsAfterControlPathMoves(t *testing.T) {
	listener, gpu := gpuListener(t)
	pidfd := openTestPidfd(t)
	server := make(chan error, 1)
	go func() {
		connection, err := listener.AcceptUnix()
		if err != nil {
			server <- err
			return
		}
		defer connection.Close()
		if err := connection.SetDeadline(time.Now().Add(gpuTestTimeout)); err != nil {
			server <- err
			return
		}
		request, descriptors, err := readGPURequest(connection)
		defer func() {
			for _, fd := range descriptors {
				unix.Close(fd)
			}
		}()
		if err != nil {
			server <- err
			return
		}
		if len(descriptors) != 2 {
			server <- fmt.Errorf("received %d pidfds, want broker and target", len(descriptors))
			return
		}
		identity, err := os.ReadFile(fmt.Sprintf("/proc/self/fdinfo/%d", descriptors[1]))
		if err != nil || !strings.Contains(string(identity), fmt.Sprintf("Pid:\t%d\n", os.Getpid())) {
			server <- fmt.Errorf("received wrong process descriptor: %s, %v", identity, err)
			return
		}
		targets := request.GetRestoreGpu().GetTargets()
		context := request.GetRestoreGpu().GetContext()
		if len(context.GetCapturedPids()) != 1 || context.CapturedPids[0] != 12 || len(targets) != 1 || targets[0].CapturedPid != 12 || targets[0].TargetPid != uint32(os.Getpid()) || request.GetTransactionId() != "restore" {
			server <- fmt.Errorf("incorrect GPU restore request: %v", request)
			return
		}
		message, _ := proto.Marshal(&Response{RequestId: request.RequestId, TransactionId: request.TransactionId, Result: &Response_GpuRestoreComplete{GpuRestoreComplete: &GpuComplete{}}})
		server <- writeMessage(connection, message)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), gpuTestTimeout)
	defer cancel()
	if _, err := gpu.Restore(ctx, []int{12}, []int{os.Getpid()}, []*os.File{pidfd}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-server:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(gpuTestTimeout):
		t.Fatal("GPU request test server did not finish")
	}
}

func TestGPUCancellationWaitsForAbortConfirmation(t *testing.T) {
	listener, gpu := gpuListener(t)
	pidfd := openTestPidfd(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := gpu.Checkpoint(ctx, []int{12}, []int{os.Getpid()}, []*os.File{pidfd})
		result <- err
	}()
	peer, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	if err := peer.SetDeadline(time.Now().Add(gpuTestTimeout)); err != nil {
		t.Fatal(err)
	}
	request, descriptors, err := readGPURequest(peer)
	defer func() {
		for _, fd := range descriptors {
			unix.Close(fd)
		}
	}()
	if err != nil || request.GetCheckpointGpu() == nil || len(descriptors) != 2 {
		t.Fatalf("GPU checkpoint request: %v, descriptors %v, %v", request, descriptors, err)
	}
	cancel()
	var data [1]byte
	if _, err := peer.Read(data[:]); !errors.Is(err, io.EOF) {
		t.Fatalf("execution socket did not shut down: %v", err)
	}
	// Lose one Abort reply, then hold the retry until return ordering is checked.
	first, _ := acceptGPUAbort(t, listener)
	first.Close()
	second, abort := acceptGPUAbort(t, listener)
	defer second.Close()
	select {
	case err := <-result:
		t.Fatalf("execution returned before drain: %v", err)
	default:
	}
	if err := pidfd.Close(); err != nil {
		t.Fatal(err)
	}
	if err := unix.PidfdSendSignal(descriptors[1], 0, nil, 0); err != nil {
		t.Fatalf("receiver lost target reference after sender closed it: %v", err)
	}
	message, _ := proto.Marshal(&Response{RequestId: abort.RequestId, TransactionId: abort.TransactionId,
		Result: &Response_AbortComplete{AbortComplete: &AbortComplete{}}})
	if err := writeMessage(second, message); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled execution: %v", err)
		}
	case <-time.After(gpuTestTimeout):
		t.Fatal("execution did not finish after confirmed drain")
	}
}

func acceptGPUAbort(t *testing.T, listener *net.UnixListener) (*net.UnixConn, *Request) {
	t.Helper()
	connection, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connection.Close() })
	if err := connection.SetDeadline(time.Now().Add(gpuTestTimeout)); err != nil {
		t.Fatal(err)
	}
	message, err := readMessage(connection)
	if err != nil {
		t.Fatal(err)
	}
	request := new(Request)
	if err := proto.Unmarshal(message, request); err != nil || request.GetAbort() == nil {
		t.Fatalf("Abort request: %v, %v", request, err)
	}
	return connection, request
}

func TestGPUCompletionDoesNotWaitForUnknownCommittedTransaction(t *testing.T) {
	listener, gpu := gpuListener(t)
	result := make(chan error, 1)
	pidfd := openTestPidfd(t)
	go func() {
		_, err := gpu.Restore(context.Background(), []int{12}, []int{os.Getpid()}, []*os.File{pidfd})
		if err == nil {
			err = (Client{ControlSocketPath: gpu.socketPath()}).Commit(context.Background(), gpu.TransactionID)
			err = errors.Join(err, gpu.Abort(context.Background()))
		}
		result <- err
	}()
	peer, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	request, descriptors, err := readGPURequest(peer)
	for _, fd := range descriptors {
		unix.Close(fd)
	}
	if err != nil {
		t.Fatal(err)
	}
	message, _ := proto.Marshal(&Response{RequestId: request.RequestId, TransactionId: request.TransactionId,
		Result: &Response_GpuRestoreComplete{GpuRestoreComplete: &GpuComplete{}}})
	if err := writeMessage(peer, message); err != nil {
		t.Fatal(err)
	}
	commitPeer, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer commitPeer.Close()
	message, err = readMessage(commitPeer)
	commit := new(Request)
	if err != nil || proto.Unmarshal(message, commit) != nil || commit.GetCommit() == nil {
		t.Fatalf("Commit request: %v, %v", commit, err)
	}
	message, _ = proto.Marshal(&Response{RequestId: commit.RequestId, TransactionId: commit.TransactionId,
		Result: &Response_Failure{Failure: &Failure{Code: Failure_STORAGE_ERROR.Enum()}}})
	if err := writeMessage(commitPeer, message); err != nil {
		t.Fatal(err)
	}
	abortPeer, abort := acceptGPUAbort(t, listener)
	message, _ = proto.Marshal(&Response{RequestId: abort.RequestId, TransactionId: abort.TransactionId,
		Result: &Response_Failure{Failure: &Failure{Code: Failure_TRANSACTION_NOT_FOUND.Enum()}}})
	if err := writeMessage(abortPeer, message); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "STORAGE_ERROR") || !strings.Contains(err.Error(), "TRANSACTION_NOT_FOUND") {
			t.Fatalf("storage cleanup failure was lost: %v", err)
		}
	case <-time.After(gpuTestTimeout):
		t.Fatal("completed GPU execution waited for broker exit")
	}
}

func TestGPURejectsInvalidParticipantMappingBeforeSending(t *testing.T) {
	for _, hostPIDs := range [][]int{{}, {0}, {2, 2}} {
		if _, err := gpuTargets([]int{12}, hostPIDs); err == nil {
			t.Fatalf("accepted mapping %v", hostPIDs)
		}
	}
	if _, err := gpuTargets([]int{12, 13}, []int{112, 112}); err == nil || !strings.Contains(err.Error(), "duplicate host GPU PID 112 for captured PID 13") {
		t.Fatalf("duplicate host PID error = %v", err)
	}
	_, execution := gpuListener(t)
	pidfd := openTestPidfd(t)
	if err := pidfd.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), gpuTestTimeout)
	defer cancel()
	if _, err := execution.Restore(ctx, []int{12}, []int{os.Getpid()}, []*os.File{pidfd}); !errors.Is(err, unix.EBADF) {
		t.Fatalf("closed descriptor error = %v, want EBADF", err)
	}
}

type gpuPIDBoundsCase struct {
	name               string
	captured, observed int64
	valid              bool
}

func TestGPUTargetPIDBounds(t *testing.T) {
	for _, test := range []gpuPIDBoundsCase{
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

func TestNewGPUContext(t *testing.T) {
	for _, mapping := range []string{"missing-separator", "=target", "source=", "source=target=other", "source=target,"} {
		t.Run(mapping, func(t *testing.T) {
			_, err := NewGPUContext([]int{12}, []string{"GPU-target"}, mapping)
			if err == nil || !strings.Contains(err.Error(), "invalid GPU device mapping") {
				t.Fatalf("mapping %q error = %v", mapping, err)
			}
		})
	}
	for _, capturedPIDs := range [][]int{nil, {0}, {-1}, {12, 12}} {
		if _, err := NewGPUContext(capturedPIDs, []string{"GPU-target"}, ""); err == nil {
			t.Fatalf("accepted captured PIDs %v", capturedPIDs)
		}
	}
	if _, err := NewGPUContext([]int{12}, nil, ""); err == nil {
		t.Fatal("accepted missing GPU UUIDs")
	}
	gpuContext, err := NewGPUContext([]int{12, 13}, []string{"GPU-target"}, "GPU-source=GPU-target")
	if err != nil {
		t.Fatal(err)
	}
	if len(gpuContext.CapturedPids) != 2 || len(gpuContext.DeviceMap) != 1 || gpuContext.DeviceMap[0].SourceUuid != "GPU-source" || gpuContext.DeviceMap[0].TargetUuid != "GPU-target" {
		t.Fatalf("unexpected GPU context: %v", gpuContext)
	}
}

func TestGPUParentRetainsOriginalPeerAfterChildExit(t *testing.T) {
	switch os.Getenv("SNAPSHOT_TEST_GPU_PROCESS") {
	case "broker":
		listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: os.Getenv("SNAPSHOT_TEST_GPU_SOCKET"), Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		_, _ = os.Stdout.WriteString("ready\n")
		for {
			connection, err := listener.AcceptUnix()
			if err != nil {
				t.Fatal(err)
			}
			request, descriptors, err := readGPURequest(connection)
			for _, fd := range descriptors {
				unix.Close(fd)
			}
			if err == nil && request.GetDirectRestore() != nil {
				message, marshalErr := proto.Marshal(&Response{RequestId: request.RequestId, TransactionId: request.TransactionId,
					Result: &Response_DirectRestoreReady{DirectRestoreReady: &DirectRestoreReady{}}})
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				if err := writeRequest(connection, message, []*os.File{openTestPidfd(t)}); err != nil {
					t.Fatal(err)
				}
			}
			connection.Close()
			if err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
					continue
				}
				t.Fatal(err)
			}
			if request.GetRestoreGpu() != nil {
				_, _ = os.Stdout.WriteString("submitted\n")
			}
		}
	case "client":
		gpu := &CustomStorageExecution{
			Socket: os.NewFile(3, "execution"), SocketDirectory: os.NewFile(4, "directory"), BrokerProcess: os.NewFile(5, "broker"),
			SocketName: "broker.sock", TransactionID: "restore", GPUContext: &GpuContext{CapturedPids: []uint32{12}},
		}
		defer gpu.Close()
		_, err := gpu.Restore(context.Background(), []int{12}, []int{os.Getpid()}, []*os.File{openTestPidfd(t)})
		t.Fatalf("client returned before cleanup: %v", err)
	}
	for _, reaped := range []bool{false, true} {
		t.Run(fmt.Sprintf("peer-already-reaped-%t", reaped), func(t *testing.T) {
			directory, err := os.MkdirTemp("", "gpu-owner-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(directory) })
			socketPath := filepath.Join(directory, "broker.sock")
			broker := exec.Command(os.Args[0], "-test.run=^TestGPUParentRetainsOriginalPeerAfterChildExit$")
			broker.Env = append(os.Environ(), "SNAPSHOT_TEST_GPU_PROCESS=broker", "SNAPSHOT_TEST_GPU_SOCKET="+socketPath)
			stdout, err := broker.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := broker.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = broker.Process.Kill()
				_ = broker.Wait()
			})
			lines := make(chan string, 2)
			go func() {
				scanner := bufio.NewScanner(stdout)
				for scanner.Scan() {
					lines <- scanner.Text()
				}
			}()
			await := func(want string) {
				t.Helper()
				select {
				case got := <-lines:
					if got != want {
						t.Fatalf("broker reported %q, want %q", got, want)
					}
				case <-time.After(gpuTestTimeout):
					t.Fatalf("broker did not report %q", want)
				}
			}
			await("ready")
			_, gpu, err := (Client{ControlSocketPath: socketPath}).PrepareGPURestore(context.Background(), "restore", "/checkpoint", true, &GpuContext{})
			if err != nil {
				t.Fatal(err)
			}
			defer gpu.Close()
			child := exec.Command(os.Args[0], "-test.run=^TestGPUParentRetainsOriginalPeerAfterChildExit$")
			child.Env = append(os.Environ(), "SNAPSHOT_TEST_GPU_PROCESS=client")
			child.ExtraFiles = []*os.File{gpu.Socket, gpu.SocketDirectory, gpu.BrokerProcess}
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = child.Process.Kill()
				_ = child.Wait()
			})
			await("submitted")
			if err := child.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			_ = child.Wait()
			if reaped {
				_ = broker.Process.Kill()
				_ = broker.Wait()
			} else {
				poll := []unix.PollFd{{Fd: int32(gpu.BrokerProcess.Fd()), Events: unix.POLLIN}}
				n, err := unix.Poll(poll, 0)
				if err != nil || n != 0 {
					t.Fatalf("original broker exited with the client: %v, %v", poll, err)
				}
			}
			if err := os.Remove(socketPath); err != nil {
				t.Fatal(err)
			}
			replacement, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			defer replacement.Close()
			if err := replacement.SetDeadline(time.Now().Add(gpuTestTimeout)); err != nil {
				t.Fatal(err)
			}
			finished := make(chan error, 1)
			go func() { finished <- gpu.Abort(context.Background()) }()
			if !reaped {
				peer, abort := acceptGPUAbort(t, replacement)
				message, _ := proto.Marshal(&Response{RequestId: abort.RequestId, TransactionId: abort.TransactionId,
					Result: &Response_Failure{Failure: &Failure{Code: Failure_TRANSACTION_NOT_FOUND.Enum()}}})
				if err := writeMessage(peer, message); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-finished:
					t.Fatalf("replacement broker authorized cleanup: %v", err)
				case <-time.After(100 * time.Millisecond):
				}
				_ = broker.Process.Kill()
				_ = broker.Wait()
			}
			select {
			case err := <-finished:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(gpuTestTimeout):
				t.Fatal("parent did not finish cleanup after original broker exit")
			}
		})
	}
}

func TestGPUConnectCancellationWithFullListener(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "broker.sock")
	listener, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(listener)
	address := &unix.SockaddrUnix{Name: socketPath}
	if err := unix.Bind(listener, address); err != nil {
		t.Fatal(err)
	}
	if err := unix.Listen(listener, 0); err != nil {
		t.Fatal(err)
	}
	queued, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(queued)
	if err := unix.Connect(queued, address); err != nil {
		t.Fatal(err)
	}
	gpu, err := (Client{ControlSocketPath: socketPath}).openCustomStorageExecution("restore", &GpuContext{}, openTestPidfd(t))
	if err != nil {
		t.Fatal(err)
	}
	defer gpu.Close()
	// ExtraFiles obtains Fd before exec. Reproduce that change in descriptor mode.
	_ = gpu.Socket.Fd()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pidfd := openTestPidfd(t)
	finished := make(chan error, 1)
	go func() {
		_, err := gpu.Restore(ctx, []int{12}, []int{os.Getpid()}, []*os.File{pidfd})
		finished <- err
	}()
	select {
	case err := <-finished:
		t.Fatalf("request did not wait on the full listener: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("connection cancellation: %v", err)
		}
	case <-time.After(gpuTestTimeout):
		t.Fatal("full listener prevented connection cancellation")
	}
}

func TestGPUAbortPreventsSubmissionAfterLateConnect(t *testing.T) {
	listener, gpu := gpuListener(t)
	// A surviving nsrestore process retains its own descriptor for this socket.
	childSocket, err := unix.FcntlInt(gpu.Socket.Fd(), unix.F_DUPFD_CLOEXEC, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(childSocket)
	finished := make(chan error, 1)
	go func() { finished <- gpu.Abort(context.Background()) }()
	peer, _ := acceptGPUAbort(t, listener)
	peer.Close()
	select {
	case err := <-finished:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("lost Abort reply: %v", err)
		}
	case <-time.After(gpuTestTimeout):
		t.Fatal("Abort did not finish before GPU submission")
	}
	if err := unix.SetNonblock(childSocket, true); err != nil {
		t.Fatal(err)
	}
	if err := unix.Connect(childSocket, &unix.SockaddrUnix{Name: gpu.socketPath()}); err != nil {
		t.Fatal(err)
	}
	if err := unix.Sendmsg(childSocket, []byte("late request"), nil, nil, unix.MSG_NOSIGNAL); !errors.Is(err, unix.EPIPE) {
		t.Fatalf("late child could submit after Abort: %v", err)
	}
}
