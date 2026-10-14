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
	if prior, done, err := checkRestore(cfg.ControlDir); err != nil {
		log.Error(err, "Cannot inspect control directory")
		return podcontract.RestoreProxyErrorExitCode
	} else if done {
		log.Info("Restore already used by an earlier container; restores are not repeatable", "restore_failed", prior.failed)
		return podcontract.RestoreNotRepeatableExitCode
	}

	log.Info("Waiting for restore", "control_dir", cfg.ControlDir)
	outcome, err := waitForRestore(cfg)
	if err != nil {
		log.Error(err, "Failed waiting for restore")
		return podcontract.RestoreProxyErrorExitCode
	}
	if outcome.complete == nil {
		if outcome.failed {
			// Normally the agent kills this container first. If it could not,
			// the marker is the only way out.
			log.Info("Restore failed; this Pod's restore is spent")
			return podcontract.RestoreNotRepeatableExitCode
		}
		// Nothing to forward to yet. The runtime kills anything CRIU left.
		log.Info("Signal before restore completed; exiting", "signal", outcome.signal.String())
		return 128 + int(outcome.signal)
	}

	complete, err := podcontract.ParseRestoreComplete(outcome.complete)
	if err != nil {
		log.Error(err, "Invalid restore-complete file")
		return podcontract.RestoreProxyErrorExitCode
	}
	pid := complete.PID
	log.Info("Restore complete", "pid", pid, "cmdline", readCmdline(cfg.ProcRoot, pid))
	if outcome.signal != 0 {
		// It arrived as the restore landed; the workload is running, so it
		// gets the signal.
		forwardSignal(log, pid, outcome.signal)
	}

	waited := make(chan waitResult, 1)
	go func() { waited <- waitForPid(pid) }()
	for {
		select {
		case s := <-cfg.Signals:
			if sig, ok := s.(syscall.Signal); ok {
				forwardSignal(log, pid, sig)
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

// forwardSignal passes sig on to the restored process. It logs before
// sending, so the container log shows every signal the proxy received,
// whatever the workload does with it.
func forwardSignal(log logr.Logger, pid int, sig syscall.Signal) {
	log.Info("Forwarding signal", "pid", pid, "signal", sig.String())
	// ESRCH means the process just exited; waitForPid reports it.
	if err := unix.Kill(pid, sig); err != nil && !errors.Is(err, unix.ESRCH) {
		log.Error(err, "Failed to forward signal", "pid", pid, "signal", sig.String())
	}
}

// restoreOutcome is how the wait for the restore ended.
type restoreOutcome struct {
	// complete holds RestoreCompleteFile once it exists.
	complete []byte
	// failed means RestoreFailedFile appeared instead.
	failed bool
	// signal is a SIGTERM or SIGINT that ended the wait. When complete is
	// also set, it still has to be forwarded to the restored process.
	signal syscall.Signal
}

// waitForRestore waits until RestoreCompleteFile or RestoreFailedFile exists,
// or SIGTERM or SIGINT arrives. Other signals are ignored until there is a
// process to forward them to.
//
// On a restore error the agent normally writes RestoreFailedFile and kills
// this container, but it cannot kill it when it fails to find the container,
// so the marker is honored here too.
func waitForRestore(cfg Config) (restoreOutcome, error) {
	changed, stop := watchDirFn(cfg.ControlDir, cfg.Log)
	defer stop()
	interval := cfg.PollInterval
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// The watch is in place before the first check, so a file that appears
	// in between still produces an event.
	for {
		outcome, done, err := checkRestore(cfg.ControlDir)
		if err != nil || done {
			return outcome, err
		}
		select {
		case s := <-cfg.Signals:
			sig, ok := s.(syscall.Signal)
			if !ok || (sig != syscall.SIGTERM && sig != syscall.SIGINT) {
				continue
			}
			// The restore may have landed or failed in the same instant. Run
			// decides with its usual order: complete, then failed, then the
			// signal. If the directory cannot be read, the signal still wins:
			// the container is being stopped either way.
			next, _, err := checkRestore(cfg.ControlDir)
			if err != nil {
				cfg.Log.Error(err, "Cannot recheck control directory after signal")
			}
			next.signal = sig
			return next, nil
		case <-changed:
		case <-ticker.C:
		}
	}
}

// checkRestore reports whether the agent has published a restore result.
func checkRestore(dir string) (restoreOutcome, bool, error) {
	data, err := os.ReadFile(filepath.Join(dir, podcontract.RestoreCompleteFile))
	if err == nil {
		return restoreOutcome{complete: data}, true, nil
	}
	if !os.IsNotExist(err) {
		return restoreOutcome{}, false, err
	}
	failed, err := fileExists(filepath.Join(dir, podcontract.RestoreFailedFile))
	return restoreOutcome{failed: failed}, failed, err
}

// watchDirFn is watchDir; tests replace it to take inotify out of a race.
var watchDirFn = watchDir

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

// waitForPid blocks until pid exits and returns its status. As PID 1 the proxy
// inherits orphans from the container's PID namespace, such as background
// children left by `kubectl exec` or exec probes. Waiting on pid alone would
// leave them as zombies for the workload's lifetime, slowly using up the Pod's
// PID limit, so it waits on any child and cleans up the others, the way tini
// does. It checks the PID of every child that exits, so the restored process's
// status is never lost. A pid that is not a child fails at once with ECHILD
// instead of hanging.
func waitForPid(pid int) waitResult {
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
