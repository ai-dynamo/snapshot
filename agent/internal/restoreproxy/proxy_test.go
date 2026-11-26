// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package restoreproxy

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

// These tests are not parallel: the proxy waits on any child of the process,
// so two tests running at once could collect each other's children.

type proxyRun struct {
	dir     string
	signals chan os.Signal
	exit    chan int
}

func startProxy(t *testing.T, pollInterval time.Duration) *proxyRun {
	t.Helper()
	r := &proxyRun{dir: t.TempDir(), signals: make(chan os.Signal, 4), exit: make(chan int, 1)}
	cfg := Config{
		ControlDir:   r.dir,
		ProcRoot:     "/proc",
		Signals:      r.signals,
		PollInterval: pollInterval,
		Log:          testr.New(t),
	}
	go func() { r.exit <- Run(cfg) }()
	return r
}

func (r *proxyRun) complete(t *testing.T, contents string) {
	t.Helper()
	// Publish the way the agent does: write a temp file, then rename it.
	tmp := filepath.Join(r.dir, ".restore-complete.tmp")
	require.NoError(t, os.WriteFile(tmp, []byte(contents), 0o644))
	require.NoError(t, os.Rename(tmp, filepath.Join(r.dir, podcontract.RestoreCompleteFile)))
}

func (r *proxyRun) exitCode(t *testing.T) int {
	t.Helper()
	select {
	case code := <-r.exit:
		return code
	case <-time.After(10 * time.Second):
		t.Fatal("proxy did not exit")
		return 0
	}
}

func (r *proxyRun) assertRunning(t *testing.T) {
	t.Helper()
	select {
	case code := <-r.exit:
		t.Fatalf("proxy exited early with %d", code)
	case <-time.After(200 * time.Millisecond):
	}
}

// startChild starts a shell script as a child of the test process, which
// stands in for the restored process the proxy inherits.
func startChild(t *testing.T, script string) int {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd.Process.Pid
}

func pidFile(pid int) string {
	return string(podcontract.FormatRestoreComplete(podcontract.RestoreComplete{PID: pid}))
}

func TestRunRefusesSpentRestore(t *testing.T) {
	for _, name := range []string{podcontract.RestoreCompleteFile, podcontract.RestoreFailedFile} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, name)
			require.NoError(t, os.WriteFile(path, []byte("pid=1\n"), 0o644))

			code := Run(Config{ControlDir: dir, ProcRoot: "/proc", Log: testr.New(t)})

			assert.Equal(t, podcontract.RestoreNotRepeatableExitCode, code)
			assert.FileExists(t, path, "the proxy must never delete a control file")
		})
	}
}

func TestRunMirrorsWorkloadExit(t *testing.T) {
	r := startProxy(t, time.Hour)
	pid := startChild(t, "sleep 0.2; exit 7")
	r.assertRunning(t)

	r.complete(t, pidFile(pid))

	assert.Equal(t, 7, r.exitCode(t))
	assert.FileExists(t, filepath.Join(r.dir, podcontract.RestoreCompleteFile))
}

func TestRunWakesOnInotify(t *testing.T) {
	// An hour-long poll means only the inotify watch can wake the proxy.
	r := startProxy(t, time.Hour)
	pid := startChild(t, "exit 0")
	r.assertRunning(t)

	r.complete(t, pidFile(pid))

	assert.Equal(t, 0, r.exitCode(t))
}

// The agent normally kills the container on a failed restore. When it cannot,
// restore-failed is the proxy's only way to learn that no restore is coming.
func TestRunExitsWhenRestoreFailsWhileWaiting(t *testing.T) {
	r := startProxy(t, time.Hour)
	r.assertRunning(t)

	path := filepath.Join(r.dir, podcontract.RestoreFailedFile)
	require.NoError(t, os.WriteFile(path, []byte("failed\n"), 0o644))

	assert.Equal(t, podcontract.RestoreNotRepeatableExitCode, r.exitCode(t))
	assert.FileExists(t, path, "the proxy must never delete a control file")
}

func TestRunSignalBeforeRestore(t *testing.T) {
	r := startProxy(t, time.Hour)
	r.signals <- syscall.SIGUSR1
	r.assertRunning(t)

	r.signals <- syscall.SIGINT

	assert.Equal(t, 128+int(syscall.SIGINT), r.exitCode(t))
}

func TestRunForwardsSignals(t *testing.T) {
	t.Run("workload handles the signal", func(t *testing.T) {
		ready := filepath.Join(t.TempDir(), "ready")
		pid := startChild(t, `trap 'exit 42' TERM; touch `+ready+`; while :; do sleep 0.05; done`)
		require.Eventually(t, func() bool { _, err := os.Stat(ready); return err == nil }, 5*time.Second, 10*time.Millisecond)
		r := startProxy(t, time.Hour)
		r.assertRunning(t)
		r.complete(t, pidFile(pid))
		r.assertRunning(t)

		r.signals <- syscall.SIGTERM

		assert.Equal(t, 42, r.exitCode(t))
	})

	t.Run("workload has no handler", func(t *testing.T) {
		pid := startChild(t, "exec sleep 30")
		r := startProxy(t, time.Hour)
		r.assertRunning(t)
		r.complete(t, pidFile(pid))
		r.assertRunning(t)

		r.signals <- syscall.SIGTERM

		assert.Equal(t, 128+int(syscall.SIGTERM), r.exitCode(t))
	})
}

// A signal and restore-complete can arrive together. If the signal wins the
// wake-up, the proxy must still follow the restored workload and pass the
// signal on, not exit and leave the workload running.
func TestRunForwardsSignalThatRacesRestore(t *testing.T) {
	// No inotify and an hour-long poll: only the signal can wake the wait, so
	// it always finds restore-complete already on disk.
	watchDirFn = func(string, logr.Logger) (<-chan struct{}, func()) { return nil, func() {} }
	t.Cleanup(func() { watchDirFn = watchDir })

	ready := filepath.Join(t.TempDir(), "ready")
	pid := startChild(t, `trap 'exit 42' TERM; touch `+ready+`; while :; do sleep 0.05; done`)
	require.Eventually(t, func() bool { _, err := os.Stat(ready); return err == nil }, 5*time.Second, 10*time.Millisecond)
	r := startProxy(t, time.Hour)
	r.assertRunning(t)
	r.complete(t, pidFile(pid))

	r.signals <- syscall.SIGTERM

	assert.Equal(t, 42, r.exitCode(t), "the proxy must forward the signal, not exit with 143")
}

func TestRunPrefersRestoreFailedOverSignal(t *testing.T) {
	// As above: only the signal can wake the wait, so it finds restore-failed.
	watchDirFn = func(string, logr.Logger) (<-chan struct{}, func()) { return nil, func() {} }
	t.Cleanup(func() { watchDirFn = watchDir })

	r := startProxy(t, time.Hour)
	r.assertRunning(t)
	require.NoError(t, os.WriteFile(filepath.Join(r.dir, podcontract.RestoreFailedFile), []byte("failed\n"), 0o644))

	r.signals <- syscall.SIGTERM

	assert.Equal(t, podcontract.RestoreNotRepeatableExitCode, r.exitCode(t), "restore-failed must win over the signal, not exit with 143")
}

// coldStartFake stands in for the container's command. A real exec does not
// return, so the fake records what the proxy asked for and returns
// errExecReplaced: the exit code 126 the proxy then reports is only the fake's.
type coldStartFake struct {
	dir string

	execCalled bool
	execPath   string
	execArgv   []string
	// triggerGone is whether the cold-start file was already removed when exec
	// was called.
	triggerGone bool
	// execErr is what Exec returns. It defaults to errExecReplaced.
	execErr error
}

var errExecReplaced = errors.New("exec replaced the process")

func (f *coldStartFake) config(t *testing.T, args []string, signals <-chan os.Signal) Config {
	t.Helper()
	return Config{
		ControlDir:   f.dir,
		Args:         args,
		ProcRoot:     "/proc",
		Signals:      signals,
		PollInterval: time.Hour,
		Log:          testr.New(t),
		LookPath: func(file string) (string, error) {
			if file == "server" || file == "/usr/bin/server" {
				return "/usr/bin/server", nil
			}
			return "", exec.ErrNotFound
		},
		Exec: func(path string, argv, _ []string) error {
			f.execCalled, f.execPath, f.execArgv = true, path, argv
			_, err := os.Stat(filepath.Join(f.dir, podcontract.ColdStartFile))
			f.triggerGone = os.IsNotExist(err)
			if f.execErr != nil {
				return f.execErr
			}
			return errExecReplaced
		},
	}
}

func (f *coldStartFake) requestColdStart(t *testing.T) {
	t.Helper()
	// Publish the way the agent does: write a temp file, then rename it.
	tmp := filepath.Join(f.dir, ".cold-start.tmp")
	require.NoError(t, os.WriteFile(tmp, nil, 0o644))
	require.NoError(t, os.Rename(tmp, filepath.Join(f.dir, podcontract.ColdStartFile)))
}

func TestRunColdStartAtStart(t *testing.T) {
	for name, tc := range map[string]struct {
		args     []string
		wantArgv []string
	}{
		"command":                 {args: []string{"--", "server"}, wantArgv: []string{"server"}},
		"command and arguments":   {args: []string{"--", "server", "--port", "8000"}, wantArgv: []string{"server", "--port", "8000"}},
		"a path":                  {args: []string{"--", "/usr/bin/server", "-v"}, wantArgv: []string{"/usr/bin/server", "-v"}},
		"later -- is not special": {args: []string{"--", "server", "--", "--x"}, wantArgv: []string{"server", "--", "--x"}},
		"empty argument":          {args: []string{"--", "server", ""}, wantArgv: []string{"server", ""}},
	} {
		t.Run(name, func(t *testing.T) {
			f := &coldStartFake{dir: t.TempDir()}
			f.requestColdStart(t)

			code := Run(f.config(t, tc.args, nil))

			assert.True(t, f.execCalled)
			assert.Equal(t, "/usr/bin/server", f.execPath)
			assert.Equal(t, tc.wantArgv, f.execArgv)
			assert.True(t, f.triggerGone, "the cold-start file must be removed before the command runs")
			assert.Equal(t, podcontract.ColdStartExecFailedExitCode, code, "only the fake's exec returns")
		})
	}
}

// The request wins over restore results an earlier container left behind, and
// those results are not touched.
func TestRunColdStartWinsOverRestoreFiles(t *testing.T) {
	for _, name := range []string{podcontract.RestoreCompleteFile, podcontract.RestoreFailedFile} {
		t.Run(name, func(t *testing.T) {
			f := &coldStartFake{dir: t.TempDir()}
			stale := filepath.Join(f.dir, name)
			require.NoError(t, os.WriteFile(stale, []byte("pid=1\n"), 0o644))
			f.requestColdStart(t)

			Run(f.config(t, []string{"--", "server"}, nil))

			assert.True(t, f.execCalled)
			assert.FileExists(t, stale, "the proxy must never delete a restore result file")
		})
	}
}

func TestRunColdStartWhileWaiting(t *testing.T) {
	// An hour-long poll means only the inotify watch can wake the proxy.
	f := &coldStartFake{dir: t.TempDir()}
	exit := make(chan int, 1)
	cfg := f.config(t, []string{"--", "server", "--port", "8000"}, nil)
	go func() { exit <- Run(cfg) }()
	select {
	case code := <-exit:
		t.Fatalf("proxy exited early with %d", code)
	case <-time.After(200 * time.Millisecond):
	}

	f.requestColdStart(t)

	select {
	case <-exit:
	case <-time.After(10 * time.Second):
		t.Fatal("proxy did not act on the cold-start request")
	}
	assert.True(t, f.execCalled)
	assert.Equal(t, []string{"server", "--port", "8000"}, f.execArgv)
	assert.True(t, f.triggerGone)
}

func TestRunColdStartExitCodes(t *testing.T) {
	for name, tc := range map[string]struct {
		args    []string
		execErr error
		want    int
	}{
		"no arguments":             {args: nil, want: podcontract.RestoreProxyErrorExitCode},
		"-- and no command":        {args: []string{"--"}, want: podcontract.RestoreProxyErrorExitCode},
		"arguments without the --": {args: []string{"server"}, want: podcontract.RestoreProxyErrorExitCode},
		"command not found":        {args: []string{"--", "missing"}, want: podcontract.ColdStartCommandNotFoundExitCode},
		"exec fails":               {args: []string{"--", "server"}, execErr: syscall.EACCES, want: podcontract.ColdStartExecFailedExitCode},
	} {
		t.Run(name, func(t *testing.T) {
			f := &coldStartFake{dir: t.TempDir(), execErr: tc.execErr}
			f.requestColdStart(t)

			code := Run(f.config(t, tc.args, nil))

			assert.Equal(t, tc.want, code)
			// Whatever went wrong, a restarted container must not find the request.
			assert.NoFileExists(t, filepath.Join(f.dir, podcontract.ColdStartFile))
			if tc.want != podcontract.ColdStartExecFailedExitCode {
				assert.False(t, f.execCalled)
			}
		})
	}
}

func TestRunColdStartDoesNotRunWhenTheRequestCannotBeRemoved(t *testing.T) {
	f := &coldStartFake{dir: t.TempDir()}
	// A directory with something in it exists, but cannot be removed.
	trigger := filepath.Join(f.dir, podcontract.ColdStartFile)
	require.NoError(t, os.MkdirAll(filepath.Join(trigger, "child"), 0o755))

	code := Run(f.config(t, []string{"--", "server"}, nil))

	assert.Equal(t, podcontract.RestoreProxyErrorExitCode, code)
	assert.False(t, f.execCalled, "a request that cannot be removed would run the command again on every restart")
}

// SIGTERM and a cold-start request can arrive together. The container is being
// stopped, so the command is not started, and the request is still consumed.
func TestRunColdStartLosesToSignal(t *testing.T) {
	// No inotify and an hour-long poll: only the signal can wake the wait, so
	// it always finds the request already on disk.
	watchDirFn = func(string, logr.Logger) (<-chan struct{}, func()) { return nil, func() {} }
	t.Cleanup(func() { watchDirFn = watchDir })

	f := &coldStartFake{dir: t.TempDir()}
	signals := make(chan os.Signal, 1)
	exit := make(chan int, 1)
	cfg := f.config(t, []string{"--", "server"}, signals)
	go func() { exit <- Run(cfg) }()
	select {
	case code := <-exit:
		t.Fatalf("proxy exited early with %d", code)
	case <-time.After(200 * time.Millisecond):
	}
	f.requestColdStart(t)

	signals <- syscall.SIGTERM

	select {
	case code := <-exit:
		assert.Equal(t, 128+int(syscall.SIGTERM), code)
	case <-time.After(10 * time.Second):
		t.Fatal("proxy did not exit")
	}
	assert.False(t, f.execCalled)
	assert.NoFileExists(t, filepath.Join(f.dir, podcontract.ColdStartFile))
}

func TestContainerCommand(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		want []string
	}{
		"none":                  {args: nil, want: nil},
		"only the --":           {args: []string{"--"}, want: []string{}},
		"command and arguments": {args: []string{"--", "a", "b"}, want: []string{"a", "b"}},
		"no --":                 {args: []string{"a", "b"}, want: nil},
		"a flag first":          {args: []string{"--x", "--", "a"}, want: nil},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, containerCommand(tc.args))
		})
	}
}

func TestRunProxyErrors(t *testing.T) {
	for name, contents := range map[string]string{
		"malformed":   "done\n",
		"missing pid": "future=1\n",
		"not a child": pidFile(os.Getppid()),
	} {
		t.Run(name, func(t *testing.T) {
			r := startProxy(t, time.Hour)
			r.assertRunning(t)
			r.complete(t, contents)
			assert.Equal(t, podcontract.RestoreProxyErrorExitCode, r.exitCode(t))
		})
	}
}

func TestWaitForPidCleansUpOtherChildren(t *testing.T) {
	other := startChild(t, "exit 3")
	target := startChild(t, "sleep 0.3; exit 5")

	r := waitForPid(target)

	require.NoError(t, r.err)
	assert.Equal(t, 5, exitCode(r.status))
	var status unix.WaitStatus
	_, err := unix.Wait4(other, &status, unix.WNOHANG, nil)
	assert.ErrorIs(t, err, unix.ECHILD, "the other child should already be cleaned up")
}

func TestExitCode(t *testing.T) {
	for name, tc := range map[string]struct {
		status unix.WaitStatus
		want   int
	}{
		"success":   {status: 0, want: 0},
		"exit 1":    {status: 1 << 8, want: 1},
		"exit 255":  {status: 255 << 8, want: 255},
		"SIGKILL":   {status: unix.WaitStatus(syscall.SIGKILL), want: 137},
		"SIGTERM":   {status: unix.WaitStatus(syscall.SIGTERM), want: 143},
		"stopped":   {status: unix.WaitStatus(syscall.SIGSTOP)<<8 | 0x7f, want: podcontract.RestoreProxyErrorExitCode},
		"core dump": {status: unix.WaitStatus(syscall.SIGSEGV) | 0x80, want: 128 + int(syscall.SIGSEGV)},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, exitCode(tc.status))
		})
	}
}

func TestControlDirFromEnv(t *testing.T) {
	t.Setenv(podcontract.SnapshotControlDirEnv, "")
	assert.Equal(t, podcontract.SnapshotControlMountPath, ControlDirFromEnv())

	t.Setenv(podcontract.SnapshotControlDirEnv, "/custom")
	assert.Equal(t, "/custom", ControlDirFromEnv())
}

func TestReadCmdline(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "42"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "42", "cmdline"), []byte("python\x00-m\x00server\x00"), 0o644))

	assert.Equal(t, "python -m server", readCmdline(root, 42))
	assert.Contains(t, readCmdline(root, 7), "unreadable")
}
