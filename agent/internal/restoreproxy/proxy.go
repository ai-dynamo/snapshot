// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package restoreproxy is the PID 1 of a restore destination container. It
// waits for the agent to finish the restore, then follows the restored process
// and exits with its status, so the container's lifecycle is the workload's.
package restoreproxy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/sys/unix"

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

// ForwardedSignals are passed on to the restored process. SIGKILL and SIGSTOP
// cannot be caught, so kubelet's final SIGKILL takes the whole container down.
var ForwardedSignals = []os.Signal{
	syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP,
	syscall.SIGQUIT, syscall.SIGUSR1, syscall.SIGUSR2,
}

// DefaultPollInterval backs up inotify. It only matters when a watch event is
// lost, so it is slow.
const DefaultPollInterval = 5 * time.Second

// Config is what Run needs from the process around it.
type Config struct {
	// ControlDir holds RestoreCompleteFile and RestoreFailedFile.
	ControlDir string
	// ProcRoot is normally /proc. The restored PID is read relative to it.
	ProcRoot string
	// Signals delivers the ForwardedSignals the caller subscribed to.
	Signals <-chan os.Signal
	// PollInterval backs up inotify while waiting for the restore.
	PollInterval time.Duration
	Log          logr.Logger
}

// ControlDirFromEnv resolves the control directory the restore Pod contract
// mounts into the container.
func ControlDirFromEnv() string {
	if dir := os.Getenv(podcontract.SnapshotControlDirEnv); dir != "" {
		return dir
	}
	return podcontract.SnapshotControlMountPath
}

// Run waits for the restore and follows the restored process. It returns the
// container's exit code. It never deletes the control files: an emptyDir
// survives a container restart, and a file left by an earlier container is
// how a restarted proxy knows the Pod's one restore is spent.
func Run(cfg Config) int {
	log := cfg.Log
	for _, name := range []string{podcontract.RestoreCompleteFile, podcontract.RestoreFailedFile} {
		exists, err := fileExists(filepath.Join(cfg.ControlDir, name))
		if err != nil {
			log.Error(err, "Cannot inspect control directory")
			return podcontract.RestoreProxyErrorExitCode
		}
		if exists {
			log.Info("Restore already used by an earlier container; restores are not repeatable", "file", name)
			return podcontract.RestoreNotRepeatableExitCode
		}
	}

	log.Info("Waiting for restore", "control_dir", cfg.ControlDir)
	data, sig, err := waitForRestore(cfg)
	if err != nil {
		log.Error(err, "Failed waiting for restore")
		return podcontract.RestoreProxyErrorExitCode
	}
	if sig != 0 {
		// Nothing to forward to yet. The runtime kills anything CRIU left.
		log.Info("Signal before restore completed; exiting", "signal", sig.String())
		return 128 + int(sig)
	}

	complete, err := podcontract.ParseRestoreComplete(data)
	if err != nil {
		log.Error(err, "Invalid restore-complete file")
		return podcontract.RestoreProxyErrorExitCode
	}
	pid := complete.PID
	log.Info("Restore complete", "pid", pid, "cmdline", readCmdline(cfg.ProcRoot, pid))

	waited := make(chan waitResult, 1)
	go func() { waited <- waitFor(pid) }()
	for {
		select {
		case s := <-cfg.Signals:
			sig, ok := s.(syscall.Signal)
			if !ok {
				continue
			}
			// Logged before sending, so the container log shows every signal
			// the proxy received, whatever the workload does with it.
			log.Info("Forwarding signal", "pid", pid, "signal", sig.String())
			// ESRCH means the process just exited; waitFor reports it.
			if err := unix.Kill(pid, sig); err != nil && !errors.Is(err, unix.ESRCH) {
				log.Error(err, "Failed to forward signal", "pid", pid, "signal", sig.String())
			}
		case r := <-waited:
			if r.err != nil {
				log.Error(r.err, "Cannot wait for restored process", "pid", pid)
				return podcontract.RestoreProxyErrorExitCode
			}
			code := exitCode(r.status)
			log.Info("Restored process exited", "pid", pid, "exit_code", code)
			return code
		}
	}
}

// waitForRestore returns the contents of RestoreCompleteFile once it exists.
// It ignores RestoreFailedFile: on a restore error the agent kills this
// process, so it never gets to choose an exit code. SIGTERM and SIGINT end the
// wait and are returned; other signals are ignored until there is a process to
// forward them to.
func waitForRestore(cfg Config) ([]byte, syscall.Signal, error) {
	path := filepath.Join(cfg.ControlDir, podcontract.RestoreCompleteFile)
	changed, stop := watchDir(cfg.ControlDir, cfg.Log)
	defer stop()
	interval := cfg.PollInterval
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// The watch is in place before the first read, so a file that appears
	// in between still produces an event.
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			return data, 0, nil
		}
		if !os.IsNotExist(err) {
			return nil, 0, err
		}
		select {
		case s := <-cfg.Signals:
			if sig, ok := s.(syscall.Signal); ok && (sig == syscall.SIGTERM || sig == syscall.SIGINT) {
				return nil, sig, nil
			}
		case <-changed:
		case <-ticker.C:
		}
	}
}

// watchDir signals on the returned channel whenever an entry in dir is
// created or renamed into place, which is how the agent publishes sentinels.
// If inotify is unavailable the channel never fires and polling takes over.
func watchDir(dir string, log logr.Logger) (<-chan struct{}, func()) {
	changed := make(chan struct{}, 1)
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		log.Error(err, "inotify unavailable; polling for restore")
		return changed, func() {}
	}
	if _, err := unix.InotifyAddWatch(fd, dir, unix.IN_CREATE|unix.IN_MOVED_TO|unix.IN_CLOSE_WRITE); err != nil {
		_ = unix.Close(fd)
		log.Error(err, "inotify watch failed; polling for restore", "dir", dir)
		return changed, func() {}
	}
	// Through os.File the read goes via the runtime poller, so Close
	// unblocks it.
	f := os.NewFile(uintptr(fd), "inotify")
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := f.Read(buf); err != nil {
				return
			}
			select {
			case changed <- struct{}{}:
			default:
			}
		}
	}()
	return changed, func() { _ = f.Close() }
}

type waitResult struct {
	status unix.WaitStatus
	err    error
}

// waitFor blocks until pid exits and returns its status. As PID 1 the proxy
// inherits orphans from the container's PID namespace, such as background
// children left by `kubectl exec` or exec probes. Waiting on pid alone would
// leave them as zombies for the workload's lifetime, slowly using up the Pod's
// PID limit, so it waits on any child and cleans up the others, the way tini
// does. It checks the PID of every child that exits, so the restored process's
// status is never lost. A pid that is not a child fails at once with ECHILD
// instead of hanging.
func waitFor(pid int) waitResult {
	var status unix.WaitStatus
	got, err := wait4(pid, &status, unix.WNOHANG)
	if err != nil {
		return waitResult{err: fmt.Errorf("pid %d is not a child of the proxy: %w", pid, err)}
	}
	for got != pid {
		if got, err = wait4(-1, &status, 0); err != nil {
			return waitResult{err: err}
		}
	}
	return waitResult{status: status}
}

func wait4(pid int, status *unix.WaitStatus, options int) (int, error) {
	for {
		got, err := unix.Wait4(pid, status, options, nil)
		if !errors.Is(err, unix.EINTR) {
			return got, err
		}
	}
}

// exitCode mirrors the workload: its own code on a normal exit, and 128 plus
// the signal number when a signal killed it, the way a shell reports it. That
// keeps kubelet's Error and OOMKilled reporting intact.
func exitCode(status unix.WaitStatus) int {
	switch {
	case status.Exited():
		return status.ExitStatus()
	case status.Signaled():
		return 128 + int(status.Signal())
	default:
		return podcontract.RestoreProxyErrorExitCode
	}
}

// readCmdline returns the restored process's command line for the container
// log, so `kubectl logs` shows what is actually running.
func readCmdline(procRoot string, pid int) string {
	data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return fmt.Sprintf("<unreadable: %v>", err)
	}
	return strings.TrimSpace(strings.ReplaceAll(string(data), "\x00", " "))
}

func fileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}
