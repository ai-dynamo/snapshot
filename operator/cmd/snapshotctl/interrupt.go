// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// interruptRepeatWindow is how long after the first signal another signal counts as the same
// interrupt. One action of a user can arrive as two signals: GNU timeout sends its signal to the
// command and then to the command's process group, and a wrapper can pass on a Ctrl+C that the
// terminal also sent. A second signal that a user sends on purpose comes later than this.
const interruptRepeatWindow = 250 * time.Millisecond

// interruptError says that SIGINT or SIGTERM arrived before the command finished. main ends the
// process by that signal after the command has cleaned up and reported its error.
type interruptError struct {
	signal syscall.Signal
	// failure is the command's own error when the command had already failed before the signal
	// arrived. The report stays that error.
	failure error
}

func (e *interruptError) Error() string {
	if e.failure != nil {
		return e.failure.Error()
	}
	return e.signal.String() + " signal received"
}

func (e *interruptError) Unwrap() error { return e.failure }

// interruptWatch answers SIGINT and SIGTERM while a checkpoint runs. The first signal cancels the
// checkpoint, and the command then deletes the Job and the source Pod it created. A second signal
// that comes after interruptRepeatWindow ends the process at once and reports what the command
// leaves.
//
// The checkpoint tells the watch which Job it creates and when that Job is gone, so that both
// answers name what is at stake at that moment. A nil watch accepts every call and does nothing.
type interruptWatch struct {
	signals chan os.Signal
	done    chan struct{}
	stopped sync.Once

	mu       sync.Mutex
	first    syscall.Signal // the signal that interrupted the command, 0 for none
	finished bool           // the command ended its work before a signal arrived
	jobNS    string         // the Job that the command asked for and has not deleted
	jobName  string
	created  bool  // the API server answered the Job create
	failure  error // the checkpoint's own error, while the command cleans up after it
}

type interruptWatchKey struct{}

// watchInterrupts returns a context that the first SIGINT or SIGTERM cancels, with an
// *interruptError as its cause, and the watch that the context carries.
func watchInterrupts(parent context.Context) (context.Context, *interruptWatch) {
	ctx, cancel := context.WithCancelCause(parent)
	w := &interruptWatch{signals: make(chan os.Signal, 2), done: make(chan struct{})}
	// A signal that was ignored when the command started stays ignored. A shell starts a
	// background command with SIGINT ignored, and Notify would make the command take it.
	var handled []os.Signal
	for _, sig := range []os.Signal{os.Interrupt, syscall.SIGTERM} {
		if !signal.Ignored(sig) {
			handled = append(handled, sig)
		}
	}
	if len(handled) > 0 { // Notify with no signal relays every signal.
		signal.Notify(w.signals, handled...)
	}
	go w.answer(cancel)
	return context.WithValue(ctx, interruptWatchKey{}, w), w
}

func interruptWatchFrom(ctx context.Context) *interruptWatch {
	w, _ := ctx.Value(interruptWatchKey{}).(*interruptWatch)
	return w
}

func (w *interruptWatch) answer(cancel context.CancelCauseFunc) {
	var sig os.Signal
	select {
	case sig = <-w.signals:
	case <-w.done:
		return
	}
	received := time.Now()
	if !w.interrupt(sig.(syscall.Signal), cancel) {
		return
	}
	for {
		select {
		case sig = <-w.signals:
			if time.Since(received) >= interruptRepeatWindow {
				w.stopAtOnce(sig.(syscall.Signal))
			}
		case <-w.done:
			return
		}
	}
}

// interrupt acknowledges the first signal and cancels the checkpoint. It reports false when the
// command had already ended its work: the signal then changes nothing.
func (w *interruptWatch) interrupt(sig syscall.Signal, cancel context.CancelCauseFunc) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.finished {
		return false
	}
	w.first = sig
	// Ctrl+C also ends the other commands of a pipeline, such as "snapshotctl ... 2>&1 | tee log".
	// A write to that closed pipe must not end the command before it has cleaned up.
	signal.Ignore(syscall.SIGPIPE)
	switch {
	case w.jobName == "":
		snapshotctlLog.Info("Interrupted, stopping; interrupt again to stop at once", "signal", sig.String())
	case !w.created:
		snapshotctlLog.Info("Interrupted while the checkpoint Job is created; the command deletes the Job when the create is answered; interrupt again to stop at once",
			"signal", sig.String(), "job", w.jobNS+"/"+w.jobName)
	default:
		snapshotctlLog.Info("Interrupted, deleting the checkpoint Job and its source Pod; interrupt again to stop at once",
			"signal", sig.String(), "job", w.jobNS+"/"+w.jobName)
	}
	cancel(&interruptError{signal: sig})
	return true
}

// stopAtOnce answers the second signal: it reports what the command leaves, and ends the process
// by that signal.
func (w *interruptWatch) stopAtOnce(sig syscall.Signal) {
	// The report must not delay the end, also when nothing reads the command's output.
	time.AfterFunc(time.Second, func() { endBySignal(sig) })
	w.mu.Lock()
	var left error
	if w.jobName != "" {
		left = fmt.Errorf("checkpoint Job %s/%s can still exist, and its source Pod can hold a GPU; to delete both, run: kubectl delete job %s --namespace %s",
			w.jobNS, w.jobName, w.jobName, w.jobNS)
		if w.failure != nil {
			left = fmt.Errorf("%w; %w", w.failure, left)
		}
	}
	w.mu.Unlock()
	if left != nil {
		snapshotctlLog.Error(left, "Stopped by a second signal before the cleanup finished", "signal", sig.String())
	} else {
		snapshotctlLog.Info("Stopped by a second signal", "signal", sig.String())
	}
	endBySignal(sig)
}

// jobRequested records the Job that the command is about to create. It reports false when the
// command was interrupted before: the command must not create the Job then.
func (w *interruptWatch) jobRequested(namespace, name string) bool {
	if w == nil {
		return true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.first != 0 {
		return false
	}
	w.jobNS, w.jobName = namespace, name
	return true
}

// jobCreated records that the API server answered the Job create: the Job exists.
func (w *interruptWatch) jobCreated() {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.created = true
}

// cleaningUp records the error of a checkpoint that failed by itself, while the command deletes
// the Job. A second signal in that time reports the error, which nothing has printed yet.
func (w *interruptWatch) cleaningUp(failure error) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failure = failure
}

// jobDeleted records that the Job is gone: a second signal leaves nothing behind.
func (w *interruptWatch) jobDeleted() {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.jobNS, w.jobName = "", ""
}

// finish records that the checkpoint completed. It reports false when a signal interrupted the
// command before: the command then ends as interrupted. After it reports true, a signal changes
// nothing, so that it cannot cut off the result of a checkpoint that succeeded.
func (w *interruptWatch) finish() bool {
	if w == nil {
		return true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.first != 0 {
		return false
	}
	w.finished = true
	return true
}

// stop ends the watch of a command that failed, and puts the default action of the signals back.
// When a signal arrived, the returned error carries it, also when the command had already failed
// by itself, so that main ends the process by that signal.
func (w *interruptWatch) stop(err error) error {
	w.stopped.Do(func() {
		signal.Stop(w.signals)
		close(w.done)
	})
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.first == 0 {
		w.finished = true // a signal that is answered only now is too late to change the ending
		return err
	}
	var interrupt *interruptError
	if errors.As(err, &interrupt) {
		return err
	}
	return &interruptError{signal: w.first, failure: err}
}

// endBySignal ends the process the way sig ends a command that does not handle it. The caller of
// the command then sees an interrupted command: a shell reports 128 plus the signal number, and
// stops the script that ran the command after Ctrl+C. An exit with a status of its own would let
// the script go on. The Go runtime ends a program that has no handler in the same way.
func endBySignal(sig syscall.Signal) {
	signal.Reset(sig)
	if self, err := os.FindProcess(os.Getpid()); err == nil {
		_ = self.Signal(sig)
	}
	// The signal ends the process before this wait is over. Where it cannot, for example in a
	// process that runs as PID 1 of a container, use the status that a shell reports for it.
	time.Sleep(time.Second)
	os.Exit(128 + int(sig))
}
