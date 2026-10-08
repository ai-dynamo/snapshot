// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package pagebroker

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/sys/unix"
)

const gpuControlTimeout = 30 * time.Second

func (c Client) Capabilities(ctx context.Context) (*Capabilities, error) {
	response, err := c.request(ctx, "", &Request_Capabilities{Capabilities: &CapabilitiesRequest{}})
	if err != nil {
		return nil, err
	}
	if response.GetCapabilities() == nil {
		return nil, fmt.Errorf("unexpected PageBroker capabilities response")
	}
	return response.GetCapabilities(), nil
}

// CustomStorageExecution owns one GPU request and waits for cleanup on failure.
type CustomStorageExecution struct {
	// SocketDirectory keeps the socket path accessible after entering the mount
	// namespace, so nsrestore can open separate control connections for Abort.
	SocketDirectory *os.File
	// Socket carries the restore request and is shared with the parent, which can
	// shut down the connection if nsrestore exits.
	Socket        *os.File
	SocketName    string
	GPUContext    *GpuContext
	TransactionID string
	BrokerProcess *os.File
	aborted       bool
	completed     bool
}

func (g *CustomStorageExecution) Close() error {
	if g == nil {
		return nil
	}
	var err error
	for _, file := range []*os.File{g.SocketDirectory, g.Socket, g.BrokerProcess} {
		if file != nil {
			err = errors.Join(err, file.Close())
		}
	}
	return err
}

func (c Client) openCustomStorageExecution(transactionID string, gpuContext *GpuContext, brokerProcess *os.File) (*CustomStorageExecution, error) {
	if gpuContext == nil {
		return nil, fmt.Errorf("CustomStorage execution requires GPU context")
	}
	directory, err := os.Open(filepath.Dir(c.ControlSocketPath))
	if err != nil {
		return nil, fmt.Errorf("open PageBroker socket directory: %w", err)
	}
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		directory.Close()
		return nil, fmt.Errorf("create PageBroker execution socket: %w", err)
	}
	execution := &CustomStorageExecution{
		SocketDirectory: directory,
		Socket:          os.NewFile(uintptr(fd), "pagebroker-execution"),
		SocketName:      filepath.Base(c.ControlSocketPath),
		GPUContext:      gpuContext,
		TransactionID:   transactionID,
		BrokerProcess:   brokerProcess,
	}
	return execution, nil
}

// PrepareGPUCheckpoint prepares storage and retains the broker that owns the transaction.
func (c Client) PrepareGPUCheckpoint(ctx context.Context, transactionID, destination string, gpuContext *GpuContext) (string, *CustomStorageExecution, error) {
	response, execution, err := c.prepareGPUExecution(ctx, transactionID, gpuContext, &Request_PrepareDirectCheckpoint{
		PrepareDirectCheckpoint: &PrepareDirectCheckpointRequest{Destination: filesystem(destination), IoEngine: posixCopy()},
	})
	if err != nil {
		return "", nil, err
	}
	directory, err := imageDirectory(response.GetDirectCheckpointDirectory().GetImageDirectory())
	if err != nil {
		execution.Close()
		return "", nil, err
	}
	return directory, execution, nil
}

// PrepareGPURestore prepares storage and retains the broker before PID namespace entry.
func (c Client) PrepareGPURestore(ctx context.Context, transactionID, source string, direct bool, gpuContext *GpuContext) (string, *CustomStorageExecution, error) {
	var command isRequest_Command = &Request_StagedRestore{
		StagedRestore: &StagedRestoreRequest{Source: filesystem(source), IoEngine: posixCopy()},
	}
	if direct {
		command = &Request_DirectRestore{DirectRestore: &DirectRestoreRequest{Source: filesystem(source), IoEngine: posixCopy()}}
	}
	response, execution, err := c.prepareGPUExecution(ctx, transactionID, gpuContext, command)
	if err != nil {
		return "", nil, err
	}
	if direct {
		if response.GetDirectRestoreReady() == nil {
			execution.Close()
			return "", nil, fmt.Errorf("unexpected PageBroker direct restore response")
		}
		return "", execution, nil
	}
	directory, err := imageDirectory(response.GetStagedRestoreDirectory().GetImageDirectory())
	if err != nil {
		execution.Close()
		return "", nil, err
	}
	return directory, execution, nil
}

func (c Client) prepareGPUExecution(ctx context.Context, transactionID string, gpuContext *GpuContext, command isRequest_Command) (*Response, *CustomStorageExecution, error) {
	if gpuContext == nil {
		return nil, nil, fmt.Errorf("CustomStorage execution requires GPU context")
	}
	response, brokerProcess, err := c.requestWithProcess(ctx, transactionID, command)
	if err != nil {
		return nil, nil, err
	}
	if brokerProcess == nil {
		return nil, nil, fmt.Errorf("PageBroker preparation response requires a broker pidfd")
	}
	execution, err := c.openCustomStorageExecution(transactionID, gpuContext, brokerProcess)
	if err != nil {
		brokerProcess.Close()
		return nil, nil, err
	}
	return response, execution, nil
}

func validateCapturedPIDs(capturedPIDs []int) error {
	if len(capturedPIDs) == 0 {
		return fmt.Errorf("GPU context requires captured PIDs")
	}
	seen := make(map[int]bool, len(capturedPIDs))
	for _, pid := range capturedPIDs {
		if pid <= 0 || pid > math.MaxInt32 {
			return fmt.Errorf("invalid captured GPU PID %d", pid)
		}
		if seen[pid] {
			return fmt.Errorf("duplicate captured GPU PID %d", pid)
		}
		seen[pid] = true
	}
	return nil
}

func gpuTargets(capturedPIDs, hostPIDs []int) ([]*GpuTarget, error) {
	if err := validateCapturedPIDs(capturedPIDs); err != nil {
		return nil, err
	}
	if len(capturedPIDs) != len(hostPIDs) {
		return nil, fmt.Errorf("GPU participant count mismatch: %d captured PIDs, %d host PIDs", len(capturedPIDs), len(hostPIDs))
	}
	seenHost := make(map[int]bool, len(hostPIDs))
	targets := make([]*GpuTarget, len(capturedPIDs))
	for i, capturedPID := range capturedPIDs {
		hostPID := hostPIDs[i]
		if hostPID <= 0 || hostPID > math.MaxInt32 || seenHost[hostPID] {
			return nil, fmt.Errorf("invalid or duplicate host GPU PID %d for captured PID %d", hostPID, capturedPID)
		}
		seenHost[hostPID] = true
		targets[i] = &GpuTarget{
			CapturedPid: uint32(capturedPID),
			TargetPid:   uint32(hostPID),
		}
	}
	return targets, nil
}

func (g *CustomStorageExecution) targets(capturedPIDs, hostPIDs []int, pidfds []*os.File) ([]*GpuTarget, error) {
	if g == nil || g.SocketDirectory == nil || g.Socket == nil || g.BrokerProcess == nil || g.GPUContext == nil || g.TransactionID == "" || g.SocketName == "" {
		return nil, fmt.Errorf("missing PageBroker CustomStorage execution context")
	}
	if len(pidfds) != len(hostPIDs) || len(pidfds) >= maxPassedFiles {
		return nil, fmt.Errorf("GPU request requires one pidfd per host PID, at most %d", maxPassedFiles-1)
	}
	for _, file := range pidfds {
		if file == nil {
			return nil, fmt.Errorf("missing GPU process descriptor")
		}
		if _, err := unix.FcntlInt(file.Fd(), unix.F_GETFD, 0); err != nil {
			return nil, fmt.Errorf("invalid GPU process descriptor: %w", err)
		}
	}
	return gpuTargets(capturedPIDs, hostPIDs)
}

func (g *CustomStorageExecution) Checkpoint(ctx context.Context, capturedPIDs, hostPIDs []int, pidfds []*os.File) (*GpuComplete, error) {
	targets, err := g.targets(capturedPIDs, hostPIDs, pidfds)
	if err != nil {
		return nil, err
	}
	response, err := g.execute(ctx, &Request_CheckpointGpu{
		CheckpointGpu: &CheckpointGpuRequest{
			Targets: targets,
			Context: g.GPUContext,
		},
	}, pidfds)
	if err != nil {
		return nil, err
	}
	if response.GetGpuCheckpointComplete() == nil {
		_ = g.Abort(ctx)
		return nil, fmt.Errorf("unexpected PageBroker GPU checkpoint response")
	}
	g.completed = true
	return response.GetGpuCheckpointComplete(), nil
}

func (g *CustomStorageExecution) Restore(ctx context.Context, capturedPIDs, hostPIDs []int, pidfds []*os.File) (*GpuComplete, error) {
	targets, err := g.targets(capturedPIDs, hostPIDs, pidfds)
	if err != nil {
		return nil, err
	}
	response, err := g.execute(ctx, &Request_RestoreGpu{
		RestoreGpu: &RestoreGpuRequest{
			Targets: targets,
			Context: g.GPUContext,
		},
	}, pidfds)
	if err != nil {
		return nil, err
	}
	if response.GetGpuRestoreComplete() == nil {
		_ = g.Abort(ctx)
		return nil, fmt.Errorf("unexpected PageBroker GPU restore response")
	}
	g.completed = true
	return response.GetGpuRestoreComplete(), nil
}

func (g *CustomStorageExecution) execute(ctx context.Context, command isRequest_Command, pidfds []*os.File) (*Response, error) {
	connectCtx, cancel := context.WithTimeout(ctx, gpuControlTimeout)
	defer cancel()
	for {
		if err := connectCtx.Err(); err != nil {
			return nil, err
		}
		err := g.socketControl(func(fd int) error {
			// ExtraFiles and File.Fd can put a shared socket into blocking mode.
			if err := unix.SetNonblock(fd, true); err != nil {
				return err
			}
			return unix.Connect(fd, &unix.SockaddrUnix{Name: g.socketPath()})
		})
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EINTR) {
			return nil, fmt.Errorf("connect PageBroker execution socket: %w", err)
		}
		select {
		case <-connectCtx.Done():
			return nil, connectCtx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	connection, err := net.FileConn(g.Socket)
	if err != nil {
		return nil, err
	}
	defer connection.Close()
	// The receiver validates its own identity before admitting GPU work.
	files := append([]*os.File{g.BrokerProcess}, pidfds...)
	response, err := exchange(ctx, connection.(*net.UnixConn), g.TransactionID, command, files...)
	if err != nil {
		_ = g.Abort(ctx)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return response, err
}

func (g *CustomStorageExecution) socketPath() string {
	return fmt.Sprintf("/proc/self/fd/%d/%s", g.SocketDirectory.Fd(), g.SocketName)
}

func (g *CustomStorageExecution) socketControl(call func(int) error) error {
	raw, err := g.Socket.SyscallConn()
	if err != nil {
		return err
	}
	var callErr error
	err = raw.Control(func(fd uintptr) { callErr = call(int(fd)) })
	return errors.Join(err, callErr)
}

// Abort retains the execution until PageBroker confirms cleanup or exits.
// Caller cancellation stops GPU work but does not shorten this cleanup wait.
func (g *CustomStorageExecution) Abort(ctx context.Context) error {
	if g.BrokerProcess == nil {
		return fmt.Errorf("missing retained PageBroker process descriptor")
	}
	if g.aborted {
		return nil
	}
	neverConnected := false
	shutdownErr := g.socketControl(func(fd int) error {
		if err := unix.Shutdown(fd, unix.SHUT_RDWR); err != nil {
			return err
		}
		// Inspect after shutdown: a child racing to connect cannot submit work.
		_, err := unix.Getpeername(fd)
		neverConnected = errors.Is(err, unix.ENOTCONN)
		if neverConnected {
			return nil
		}
		return err
	})
	log := logr.FromContextOrDiscard(ctx)
	var nextLog time.Time
	for {
		poll := []unix.PollFd{{Fd: int32(g.BrokerProcess.Fd()), Events: unix.POLLIN}}
		if _, err := unix.Poll(poll, 0); err == nil && poll[0].Revents&(unix.POLLIN|unix.POLLHUP) != 0 {
			g.aborted = true
			return nil
		}
		attempt, cancel := context.WithTimeout(context.WithoutCancel(ctx), gpuControlTimeout)
		err := (Client{ControlSocketPath: g.socketPath()}).abort(attempt, g.TransactionID, g.BrokerProcess)
		cancel()
		if err == nil {
			g.aborted = true
			return nil
		}
		// The pinned receiver can forget a transaction only after GPU work has
		// drained. A replacement rejects our broker pidfd before checking the ID.
		var failure failureError
		if errors.As(err, &failure) && failure.code == Failure_TRANSACTION_NOT_FOUND {
			g.aborted = true
			return err
		}
		if g.completed || (shutdownErr == nil && neverConnected) {
			return err
		}
		if time.Now().After(nextLog) {
			log.Error(errors.Join(err, shutdownErr), "Waiting for PageBroker GPU cleanup", "transaction", g.TransactionID)
			nextLog = time.Now().Add(30 * time.Second)
		}
		time.Sleep(time.Second)
	}
}

// NewGPUContext converts participant and device mappings into a PageBroker request context.
func NewGPUContext(capturedPIDs []int, visibleGPUUUIDs []string, deviceMap string) (*GpuContext, error) {
	if err := validateCapturedPIDs(capturedPIDs); err != nil {
		return nil, err
	}
	if len(visibleGPUUUIDs) == 0 {
		return nil, fmt.Errorf("GPU context requires visible GPU UUIDs")
	}
	mapping, err := parseDeviceMap(deviceMap)
	if err != nil {
		return nil, err
	}
	gpuContext := &GpuContext{
		VisibleDevices: visibleGPUUUIDs,
		DeviceMap:      mapping,
	}
	for _, pid := range capturedPIDs {
		gpuContext.CapturedPids = append(gpuContext.CapturedPids, uint32(pid))
	}
	return gpuContext, nil
}

func parseDeviceMap(deviceMap string) ([]*GpuDeviceMapping, error) {
	if deviceMap == "" {
		return nil, nil
	}
	var mappings []*GpuDeviceMapping
	for _, pair := range strings.Split(deviceMap, ",") {
		source, target, ok := strings.Cut(pair, "=")
		if !ok || source == "" || target == "" || strings.Contains(target, "=") {
			return nil, fmt.Errorf("invalid GPU device mapping %q", pair)
		}
		mappings = append(mappings, &GpuDeviceMapping{
			SourceUuid: source,
			TargetUuid: target,
		})
	}
	return mappings, nil
}
