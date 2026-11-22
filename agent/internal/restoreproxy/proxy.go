// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package restoreproxy is the PID 1 of a restore destination container. It
// waits for the agent to finish the restore, then follows the restored process
// and exits with its status, so the container's lifecycle is the workload's.
// If the agent asks for a cold start instead, it becomes the container's own
// command.
package restoreproxy

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
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
	// ControlDir holds ColdStartFile, RestoreCompleteFile and
	// RestoreFailedFile.
	ControlDir string
	// Args are the proxy's arguments. The container's own command follows a
	// "--": the NRI plugin passes it so that a cold start can run it.
	Args []string
	// ProcRoot is normally /proc. The restored PID is read relative to it.
	ProcRoot string
	// Signals delivers the ForwardedSignals the caller subscribed to.
	Signals <-chan os.Signal
	// PollInterval backs up inotify while waiting for the restore.
	PollInterval time.Duration
	Log          logr.Logger
	// Exec replaces the process with the container's command. It is a function
	// pointer so that tests can override it.
	Exec func(path string, argv, env []string) error
	// LookPath finds the container's command on PATH. It is a function pointer
	// so that tests can override it.
	LookPath func(file string) (string, error)
}

// withDefaults returns a copy of the config with what the caller left out
// filled in: the real Exec and LookPath.
func (cfg Config) withDefaults() Config {
	if cfg.Exec == nil {
		cfg.Exec = syscall.Exec
	}
	if cfg.LookPath == nil {
		cfg.LookPath = exec.LookPath
	}
	return cfg
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
// container's exit code, unless the agent asks for a cold start: then it does
// not return, because the process becomes the container's own command.
//
// It never deletes the restore result files: an emptyDir survives a container
// restart, and a file left by an earlier container is how a restarted proxy
// knows the Pod's one restore is spent. The cold-start file is the one it does
// delete, as soon as it acts on it, so that a restarted container does not
// find a request the agent made for an earlier one.
func Run(cfg Config) int {
	cfg = cfg.withDefaults()
	log := cfg.Log
	prior, done, err := checkRestore(cfg.ControlDir)
	if err != nil {
		log.Error(err, "Cannot inspect control directory")
		return podcontract.RestoreProxyErrorExitCode
	}
	if prior.coldStart {
		return startCold(cfg, 0)
	}
	if done {
		log.Info("Restore already used by an earlier container; restores are not repeatable", "restore_failed", prior.failed)
		return podcontract.RestoreNotRepeatableExitCode
	}

	log.Info("Waiting for restore", "control_dir", cfg.ControlDir)
	outcome, err := waitForRestore(cfg)
	if err != nil {
		log.Error(err, "Failed waiting for restore")
		return podcontract.RestoreProxyErrorExitCode
	}
	if outcome.coldStart {
		return startCold(cfg, outcome.signal)
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

// startCold runs the container's own command in place of the restore. It
// removes the cold-start file first, even if the command then cannot be run, so
// that a restarted container waits for a restore again instead of finding a
// stale request. sig is a SIGTERM or SIGINT that arrived with the request: the
// container is being stopped, so nothing is started.
func startCold(cfg Config, sig syscall.Signal) int {
	log := cfg.Log
	if err := os.Remove(filepath.Join(cfg.ControlDir, podcontract.ColdStartFile)); err != nil && !os.IsNotExist(err) {
		log.Error(err, "Cannot remove the cold-start file; not running the command")
		return podcontract.RestoreProxyErrorExitCode
	}
	if sig != 0 {
		log.Info("Signal with the cold-start request; exiting", "signal", sig.String())
		return 128 + int(sig)
	}
	argv := containerCommand(cfg.Args)
	if len(argv) == 0 {
		log.Error(nil, "Cold start requested, but the container has no command to run")
		return podcontract.RestoreProxyErrorExitCode
	}
	path, err := cfg.LookPath(argv[0])
	if err != nil {
		log.Error(err, "Cold start: command not found", "command", argv[0])
		return podcontract.ColdStartCommandNotFoundExitCode
	}
	log.Info("Cold start: running the container's command", "command", path)
	// On success Exec does not return.
	err = cfg.Exec(path, argv, os.Environ())
	log.Error(err, "Cold start: cannot run the command", "command", path)
	return podcontract.ColdStartExecFailedExitCode
}

// containerCommand returns what follows the first "--" in the proxy's
// arguments: the container's own command and its arguments. Without the "--"
// there is none, as in a container made before the NRI plugin passed it.
func containerCommand(args []string) []string {
	if len(args) == 0 || args[0] != "--" {
		return nil
	}
	return args[1:]
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
	// coldStart means ColdStartFile exists. It wins over the other two.
	coldStart bool
	// complete holds RestoreCompleteFile once it exists.
	complete []byte
	// failed means RestoreFailedFile appeared instead.
	failed bool
	// signal is a SIGTERM or SIGINT that ended the wait. When complete is
	// also set, it still has to be forwarded to the restored process.
	signal syscall.Signal
}

// waitForRestore waits until ColdStartFile, RestoreCompleteFile or
// RestoreFailedFile exists, or SIGTERM or SIGINT arrives. Other signals are
// ignored until there is a process to forward them to.
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
			// decides with its usual order: cold start, complete, then failed,
			// then the signal. If the directory cannot be read, the signal still wins:
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

// checkRestore reports whether the agent has published a restore result or a
// cold-start request. The request is checked first: a restore result left by an
// earlier container must not hide it.
func checkRestore(dir string) (restoreOutcome, bool, error) {
	coldStart, err := fileExists(filepath.Join(dir, podcontract.ColdStartFile))
	if err != nil {
		return restoreOutcome{}, false, err
	}
	if coldStart {
		return restoreOutcome{coldStart: true}, true, nil
	}
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
