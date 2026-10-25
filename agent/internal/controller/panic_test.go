// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ai-dynamo/snapshot/agent/internal/executor"
	"github.com/ai-dynamo/snapshot/agent/internal/nsmount"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	snapshottypes "github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/ai-dynamo/snapshot/api/podcontract"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

func TestRecoverPanicHandsTheCauseToTheHandler(t *testing.T) {
	var got error
	func() {
		defer recoverPanic(logr.Discard(), "test", func(err error) { got = err })
		panic("boom")
	}()

	if got == nil {
		t.Fatal("recoverPanic did not invoke the handler")
	}
	if !strings.Contains(got.Error(), "boom") {
		t.Errorf("error %q does not name the panic value", got)
	}
	var panicErr *recoveredPanicError
	if !errors.As(got, &panicErr) {
		t.Fatalf("error %T is not a *recoveredPanicError", got)
	}
}

// This text reaches a status condition readable by anyone who can read the
// work order, so the exact form is the point: a stack must not creep into it.
func TestRecoveredPanicErrorCarriesNoStack(t *testing.T) {
	err := &recoveredPanicError{value: "boom"}
	if got := err.Error(); got != "panic: boom" {
		t.Errorf("Error() = %q, want %q", got, "panic: boom")
	}
}

func TestRecoverPanicIsInertWithoutAPanic(t *testing.T) {
	called := false
	func() {
		defer recoverPanic(logr.Discard(), "test", func(error) { called = true })
	}()
	if called {
		t.Error("handler ran without a panic")
	}
}

func TestRecoverPanicContainsThePanic(t *testing.T) {
	survived := false
	func() {
		defer func() { survived = true }()
		defer recoverPanic(logr.Discard(), "test", nil)
		panic(errors.New("work order is malformed"))
	}()
	if !survived {
		t.Error("panic escaped recoverPanic")
	}
}

// restorePodContainers runs each destination under sync.WaitGroup.Go, which
// recovers only to re-panic, so a deferred recover inside the worker is what
// keeps a panicking restore from killing the agent. This pins that the idiom
// contains the panic; the wiring in restorePodContainers is not reachable here
// without a plan, a pod and CRIU.
func TestRecoverPanicContainsAWaitGroupWorker(t *testing.T) {
	var workers sync.WaitGroup
	workers.Go(func() {
		defer recoverPanic(logr.Discard(), "restore destination", nil)
		panic("restore blew up")
	})
	workers.Wait() // re-panics, and so fails the run, without the recover above
}

// failCaptureAfterPanicOn runs the cleanup against a work order that has not
// reached a terminal condition, as it would be while its capture is running.
func failCaptureAfterPanicOn(t *testing.T, committed bool) *snapshotv1alpha1.PodSnapshotContent {
	t.Helper()
	content := makeWorkOrder("content-a", "node-a", "a")
	w := makeNodeController(t, &fakeCheckpointer{}, content)
	if committed {
		path, err := nsmount.ResolveArtifactPath(w.config.Storage.BasePath, string(content.UID), "main")
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(path, 0o700))
		require.NoError(t, snapshottypes.WriteManifest(path, &snapshottypes.CheckpointManifest{
			Artifact: snapshottypes.ArtifactManifest{ContentUID: string(content.UID), ContainerName: "main"},
		}))
	}

	w.failCaptureAfterPanic(context.Background(), content.Name, errors.New("panic: boom"))

	return getContent(t, w, content.Name)
}

func TestFailCaptureAfterPanicPromotesACommittedCapture(t *testing.T) {
	got := failCaptureAfterPanicOn(t, true)

	assert.True(t, isContentReady(got), "a committed capture must survive the panic")
	assert.False(t, isContentFailed(got))
}

func TestFailCaptureAfterPanicFailsWhenNothingCommitted(t *testing.T) {
	got := failCaptureAfterPanicOn(t, false)

	require.True(t, isContentFailed(got))
	assert.Equal(t, "AgentPanic",
		meta.FindStatusCondition(got.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed).Reason)
}

func TestFailRestoreAfterPanicRecordsTheFailureOnThePod(t *testing.T) {
	pod := restorePod(map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"})
	w := makeTestController(t, pod)

	w.failRestoreAfterPanic(context.Background(), client.ObjectKeyFromObject(pod), pod, errors.New("panic: boom"))

	payload := string(lastPodStatusApply(t, w).GetPatch())
	assert.Contains(t, payload, `"reason":"RestoreFailed"`)
	assert.Contains(t, payload, "panic: boom")
	assert.True(t, w.restoreHandled(pod), "a recorded panic must not be replayed")
}

func TestFailRestoreAfterPanicWithoutAPodDoesNothing(t *testing.T) {
	w := makeTestController(t, nil)

	w.failRestoreAfterPanic(context.Background(),
		client.ObjectKey{Namespace: "inference", Name: "restore-worker"}, nil, errors.New("panic: boom"))

	assert.False(t, hasPodStatusApply(w))
}

// A panic during the restore must stop the placeholder, not leave the container
// waiting for a restore that will never arrive.
func TestRunRestorePanicFailsTheDestination(t *testing.T) {
	pod := restorePod(map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"})
	w := makeTestController(t, pod)
	w.runtime = &fakeRuntime{resolveContainerPID: 4242}
	w.restoreFn = func(context.Context, snapshotruntime.Runtime, logr.Logger, executor.RestoreRequest, executor.RestoreMounter) (executor.RestoreResult, error) {
		panic("boom")
	}
	var sentinels []string
	w.writeControlSentinelFn = func(_ int, name string, _ []byte) error {
		sentinels = append(sentinels, name)
		return nil
	}
	killed := 0
	w.sendSignalFn = func(logr.Logger, int, syscall.Signal, string) error {
		killed++
		return nil
	}
	plan := &restorePlan{
		artifact: &restoreArtifact{SnapshotName: "snapshot-a", ContentUID: "content-uid", SourceContainerName: "main"},
		mappings: []podcontract.ContainerMapping{{Source: "main", Destination: "main"}},
	}

	err := w.runRestore(context.Background(), pod, plan, "main", "containerd://abc", time.Now(), false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "panic: boom")
	assert.Equal(t, []string{podcontract.RestoreFailedFile}, sentinels)
	assert.Equal(t, 1, killed, "the placeholder must be killed")
}
