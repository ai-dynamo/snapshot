// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package cuda

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/go-logr/logr"
	"golang.org/x/sys/unix"
)

const helperCleanupTimeout = 5 * time.Minute

// Helper owns the persistent CUDA process. Its CLI implements the private IPC;
// Go only passes arguments and inherited descriptors and checks exit status.
type Helper struct {
	binary        string
	cmd           *exec.Cmd
	control       *os.File
	done          chan struct{}
	waitErr       error // published by closing done
	stop          sync.Once
	mu            sync.Mutex // serializes CLI access to the control socket
	nextSession   uint64
	sessionIDs    map[*os.File]uint64
	customStorage bool
}

// StartHelper waits for eager CUDA initialization before returning a usable
// helper. The agent starts its controller only after this succeeds.
func StartHelper(ctx context.Context, binary string, options types.CUDACheckpointSpec, log logr.Logger) (*Helper, error) {
	if err := options.Validate(); err != nil {
		return nil, err
	}
	control, child, err := helperSocketPair()
	if err != nil {
		return nil, err
	}
	defer child.Close()
	cmd := exec.Command(binary, "--daemon", "--cuda-storage-mode", options.StorageMode,
		"--transfer-buffer-count", strconv.FormatUint(options.TransferBufferCount, 10),
		"--transfer-chunk-bytes", strconv.FormatUint(options.TransferChunkBytes, 10),
		"--max-pinned-bytes", strconv.FormatUint(options.MaxPinnedBytes, 10))
	cmd.ExtraFiles = []*os.File{child}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		_ = control.Close()
		return nil, fmt.Errorf("start CUDA helper: %w", err)
	}
	_ = child.Close()
	h := &Helper{binary: binary, cmd: cmd, control: control, done: make(chan struct{}), sessionIDs: make(map[*os.File]uint64)}
	go func() {
		h.waitErr = cmd.Wait()
		close(h.done)
	}()
	readyCtx, cancel := context.WithTimeout(ctx, helperCleanupTimeout)
	defer cancel()
	output, err := runHelperCLI(readyCtx, binary, []*os.File{control}, "--wait-ready", "--control-fd", "3")
	var ready struct {
		CustomStorageAvailable bool `json:"custom_storage_available"`
		Metrics                struct {
			InitializationSeconds float64 `json:"initialization_seconds"`
			VisibleDevices        uint32  `json:"visible_devices"`
		} `json:"metrics"`
	}
	if err == nil {
		err = json.Unmarshal(output, &ready)
	}
	if err == nil && options.StorageMode == "custom" && !ready.CustomStorageAvailable {
		err = fmt.Errorf("CUDA helper does not support CustomStorage")
	}
	if err != nil {
		h.Stop()
		return nil, fmt.Errorf("initialize CUDA helper: %w", err)
	}
	h.customStorage = ready.CustomStorageAvailable
	log.Info("CUDA helper ready", "custom_storage_available", h.customStorage,
		"initialization_seconds", ready.Metrics.InitializationSeconds, "visible_devices", ready.Metrics.VisibleDevices)
	return h, nil
}

func (h *Helper) Done() <-chan struct{} { return h.done }

// Err is read after Done closes. An unexpected clean exit is still a loss of
// the CUDA contexts and must stop admission in the agent.
func (h *Helper) Err() error {
	if h.waitErr != nil {
		return fmt.Errorf("CUDA helper exited: %w", h.waitErr)
	}
	return fmt.Errorf("CUDA helper exited")
}

// Stop waits for process exit before the caller releases backing files. Killing
// without waiting is insufficient when the driver still owns imported memory.
func (h *Helper) Stop() {
	if h == nil {
		return
	}
	h.stop.Do(func() {
		_ = h.cmd.Process.Kill()
		_ = h.control.Close()
		<-h.done
	})
}

func helperSocketPair() (*os.File, *os.File, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	return os.NewFile(uintptr(fds[0]), "cuda-helper-client"), os.NewFile(uintptr(fds[1]), "cuda-helper-peer"), nil
}

func runHelperCLI(ctx context.Context, binary string, files []*os.File, args ...string) ([]byte, error) {
	// nsrestore opens the executable before CRIU removes its bundle mount.
	// Move it to a known inherited FD so session descriptors cannot overwrite
	// the original /proc/self/fd/N slot in the child.
	if strings.HasPrefix(binary, "/proc/self/fd/") {
		file, err := os.Open(binary)
		if err != nil {
			return nil, err
		}
		defer file.Close()
		binary = fmt.Sprintf("/proc/self/fd/%d", 3+len(files))
		files = append(append([]*os.File(nil), files...), file)
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.ExtraFiles = files
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return normalizeProcessGroupKillError(syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL))
	}
	cmd.WaitDelay = helperWaitDelay
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return output, fmt.Errorf("CUDA helper %s: %w (%s)", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return output, nil
}

// Drain is the parent-only lifetime barrier. It cancels every unfinished
// participant before asking the daemon to join them. nsrestore must have exited
// (or RunGPUSessions returned) before this is called.
func (h *Helper) Drain(sessions GPUSessions) error {
	if len(sessions) == 0 {
		return nil
	}
	sessions.halfClose()
	defer sessions.Close()
	if h == nil {
		return fmt.Errorf("cannot drain GPU sessions without their CUDA helper")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	args := []string{"--drain-batch", "--control-fd", "3"}
	for _, file := range sessions {
		id, ok := h.sessionIDs[file]
		if !ok {
			h.Stop()
			return fmt.Errorf("unknown CUDA helper session")
		}
		args = append(args, "--session", strconv.FormatUint(id, 10))
		delete(h.sessionIDs, file)
	}
	ctx, cancel := context.WithTimeout(context.Background(), helperCleanupTimeout)
	defer cancel()
	if _, err := runHelperCLI(ctx, h.binary, []*os.File{h.control}, args...); err != nil {
		h.Stop()
		return fmt.Errorf("drain CUDA helper (process reaped): %w", err)
	}
	return nil
}
