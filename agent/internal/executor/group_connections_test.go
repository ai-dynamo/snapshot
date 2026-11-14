// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

// startKept serves a group member's peer sync socket with no connections to
// hold, as keepConnections does for a member that keeps none, so the test
// exercises only the handshake with nsrestore.
func startKept(t *testing.T, ctx context.Context, socketsRestored func(context.Context) error) *keptConnections {
	t.Helper()
	k := newTestKept(t, socketsRestored)
	k.start(ctx)
	return k
}

func newTestKept(t *testing.T, socketsRestored func(context.Context) error) *keptConnections {
	t.Helper()
	agent, nsrestore, err := peerSyncSocket()
	if err != nil {
		t.Fatal(err)
	}
	k := &keptConnections{
		log:             logr.Discard(),
		attempt:         "attempt",
		socketsRestored: socketsRestored,
		agent:           agent,
		nsrestore:       nsrestore,
		served:          make(chan error, 1),
	}
	t.Cleanup(k.close)
	return k
}

// nsrestoreSide runs nsrestore's post-restore hook on k's socket.
func nsrestoreSide(k *keptConnections) <-chan error {
	done := make(chan error, 1)
	go func() { done <- awaitPeers(k.nsrestore)() }()
	return done
}

func await(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("timed out")
		return nil
	}
}

func TestKeptConnectionsLetTheRestoreContinueOnceEveryMemberRestored(t *testing.T) {
	calls := 0
	k := startKept(t, context.Background(), func(context.Context) error {
		calls++
		time.Sleep(10 * time.Millisecond)
		return nil
	})

	if err := await(t, nsrestoreSide(k)); err != nil {
		t.Fatalf("nsrestore's post-restore hook = %v, want the agent to let it continue", err)
	}
	if err := k.wait(nil); err != nil {
		t.Fatalf("wait = %v", err)
	}
	if calls != 1 {
		t.Fatalf("the barrier ran %d times, want once", calls)
	}
	if k.waited < 10*time.Millisecond {
		t.Fatalf("waited = %v, want the time the barrier took", k.waited)
	}
}

func TestKeptConnectionsAbortTheRestoreWhenTheBarrierFails(t *testing.T) {
	k := startKept(t, context.Background(), func(context.Context) error {
		return errors.New("session failed")
	})

	// nsrestore gets no answer and fails on its own, since the agent waits for
	// it only once it has exited.
	if err := await(t, nsrestoreSide(k)); err == nil {
		t.Fatal("nsrestore's post-restore hook succeeded, want it to abort the restore")
	}
	nsrestoreErr := failStep(StepRestoreCRIU, errors.New("post-restore hook failed"))
	err := k.wait(nsrestoreErr)
	if StepOf(err) != StepKeepPeerConnections || !strings.Contains(err.Error(), "session failed") ||
		!errors.Is(err, nsrestoreErr) {
		t.Fatalf("wait = %v, want the barrier's error first, then nsrestore's", err)
	}
}

func TestKeptConnectionsKeepTheStepOfAnNSRestoreThatFailedFirst(t *testing.T) {
	k := startKept(t, context.Background(), func(context.Context) error {
		t.Error("the barrier ran, want serve to stop before nsrestore reports")
		return nil
	})

	// nsrestore failed before CRIU restored the sockets, so it never reported.
	nsrestoreErr := failStep(StepRestoreCRIU, errors.New("criu failed"))
	err := k.wait(nsrestoreErr)
	if StepOf(err) != StepRestoreCRIU || !strings.Contains(err.Error(), "did not report its restored sockets") {
		t.Fatalf("wait = %v, want nsrestore's step first, then the unanswered peer sync", err)
	}
}

func TestKeptConnectionsStopWaitingOnceNSRestoreExits(t *testing.T) {
	waiting := make(chan struct{})
	k := startKept(t, context.Background(), func(ctx context.Context) error {
		close(waiting)
		<-ctx.Done()
		return ctx.Err()
	})
	// nsrestore reports its restored sockets, then exits before the other
	// members restore theirs.
	if _, err := k.nsrestore.Write([]byte{0}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-waiting:
	case <-time.After(10 * time.Second):
		t.Fatal("the barrier never ran")
	}

	nsrestoreErr := failStep(StepRestoreCRIU, errors.New("nsrestore was killed"))
	done := make(chan error, 1)
	go func() { done <- k.wait(nsrestoreErr) }()
	if err := await(t, done); StepOf(err) != StepRestoreCRIU {
		t.Fatalf("wait = %v, want nsrestore's failure first", err)
	}
}

func TestKeptConnectionsStopWhenTheirContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	k := startKept(t, ctx, func(context.Context) error {
		t.Error("the barrier ran, want serve to stop before nsrestore reports")
		return nil
	})

	cancel()
	if err := await(t, k.served); err == nil {
		t.Fatal("serve = nil, want an error once its context ended")
	}
}

func TestAwaitPeersAbortsTheRestoreWhenTheAgentIsGone(t *testing.T) {
	k := newTestKept(t, nil)
	_ = k.agent.Close()

	if err := awaitPeers(k.nsrestore)(); err == nil {
		t.Fatal("awaitPeers = nil, want an error when the agent is gone")
	}
}
