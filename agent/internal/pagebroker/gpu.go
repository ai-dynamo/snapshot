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
	// shut down the connection and identify the original broker if nsrestore exits.
	Socket        *os.File
	SocketName    string
	GPUContext    *GpuContext
	TransactionID string
	broker        *os.File
	aborted       bool
	completed     bool
}

func (g *CustomStorageExecution) Close() error {
	if g == nil {
		return nil
	}
	var err error
	for _, file := range []*os.File{g.SocketDirectory, g.Socket, g.broker} {
		if file != nil {
			err = errors.Join(err, file.Close())
		}
	}
	return err
}

func (c Client) OpenCustomStorageExecution(transactionID string, gpuContext *GpuContext) (*CustomStorageExecution, error) {
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
	}
	// An unconnected socket has no peer. Require this option before GPU work.
	if peer, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_PEERPIDFD); !errors.Is(err, unix.ENODATA) {
		if err == nil {
			unix.Close(peer)
			err = errors.New("unconnected socket has a peer")
		}
		execution.Close()
		return nil, fmt.Errorf("PageBroker GPU execution requires SO_PEERPIDFD: %w", err)
	}
	return execution, nil
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
		invalidID := hostPID <= 0 || hostPID > math.MaxInt32
		duplicateID := seenHost[hostPID]
		if invalidID || duplicateID {
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
	if g == nil || g.SocketDirectory == nil || g.Socket == nil || g.GPUContext == nil || g.TransactionID == "" || g.SocketName == "" {
		return nil, fmt.Errorf("missing PageBroker CustomStorage execution context")
	}
	if len(pidfds) != len(hostPIDs) || len(pidfds) > maxPassedFiles {
		return nil, fmt.Errorf("GPU request requires one pidfd per host PID, at most %d", maxPassedFiles)
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
	var err error
	g.broker, err = g.peerProcess()
	if err != nil {
		return nil, fmt.Errorf("identify PageBroker process: %w", err)
	}
	connection, err := net.FileConn(g.Socket)
	if err != nil {
		return nil, err
	}
	defer connection.Close()
	response, err := exchange(ctx, connection.(*net.UnixConn), g.TransactionID, command, pidfds...)
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

func (g *CustomStorageExecution) peerProcess() (*os.File, error) {
	var fd int
	err := g.socketControl(func(socket int) error {
		var err error
		fd, err = unix.GetsockoptInt(socket, unix.SOL_SOCKET, unix.SO_PEERPIDFD)
		return err
	})
	// This fixed-size SO_PEERPIDFD call uses the socket's stored process identity.
	// Older kernels report EINVAL after that process is reaped. Newer kernels
	// report ESRCH or return a pidfd that is already readable.
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ESRCH) {
		return nil, os.ErrProcessDone
	}
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), "pagebroker-process"), nil
}

// Abort retains the execution until PageBroker confirms cleanup or exits.
// Caller cancellation stops GPU work but does not shorten this cleanup wait.
func (g *CustomStorageExecution) Abort(ctx context.Context) error {
	if g.aborted {
		return nil
	}
	shutdownErr := g.socketControl(func(fd int) error { return unix.Shutdown(fd, unix.SHUT_RDWR) })
	log := logr.FromContextOrDiscard(ctx)
	var nextLog time.Time
	for {
		var peerErr error
		if g.broker == nil {
			g.broker, peerErr = g.peerProcess()
		}
		if errors.Is(peerErr, os.ErrProcessDone) {
			g.aborted = true
			return nil
		}
		if g.broker != nil {
			poll := []unix.PollFd{{Fd: int32(g.broker.Fd()), Events: unix.POLLIN}}
			if _, err := unix.Poll(poll, 0); err == nil && poll[0].Revents&(unix.POLLIN|unix.POLLHUP) != 0 {
				g.aborted = true
				return nil
			}
		}
		attempt, cancel := context.WithTimeout(context.WithoutCancel(ctx), gpuControlTimeout)
		err := (Client{ControlSocketPath: g.socketPath()}).Abort(attempt, g.TransactionID)
		cancel()
		if err == nil {
			g.aborted = true
			return nil
		}
		// Shutdown also prevents a surviving child from submitting work through
		// this socket after it connects. ENODATA alone does not establish that.
		if g.completed || (shutdownErr == nil && errors.Is(peerErr, unix.ENODATA)) {
			return err
		}
		if time.Now().After(nextLog) {
			log.Error(errors.Join(err, peerErr, shutdownErr), "Waiting for PageBroker GPU cleanup", "transaction", g.TransactionID)
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
