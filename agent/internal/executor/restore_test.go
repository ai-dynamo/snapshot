// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

type restoreFakeRuntime struct {
	resolvedID      string
	resolveByPodHit bool
}

func (r *restoreFakeRuntime) ResolveContainer(ctx context.Context, id string) (int, *specs.Spec, error) {
	r.resolvedID = id
	return 123, &specs.Spec{}, nil
}

func (r *restoreFakeRuntime) ResolveContainerIDByPod(ctx context.Context, pod, ns, ctr string) (string, error) {
	return "", errors.New("pod lookup should not be used")
}

func (r *restoreFakeRuntime) ResolveContainerByPod(ctx context.Context, pod, ns, ctr string) (int, *specs.Spec, error) {
	r.resolveByPodHit = true
	return 0, nil, errors.New("pod lookup should not be used")
}

func (r *restoreFakeRuntime) Close() error { return nil }

func TestExecNSRestoreRejectsRelativeContainerCheckpointLocation(t *testing.T) {
	_, err := execNSRestore(
		context.Background(),
		testr.New(t),
		RestoreRequest{
			ContainerCheckpointLocation: "relative/checkpoint",
			NSRestorePath:               "/usr/local/bin/nsrestore",
		},
		&types.RestoreContainerSnapshot{
			CheckpointPath: "/host/checkpoints/abc123",
			PlaceholderPID: 1,
		},
		nil)
	if err == nil {
		t.Fatal("expected relative container checkpoint location to be rejected")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("expected absolute-path validation error, got: %v", err)
	}
}

func TestNSRestoreInheritedFDLayout(t *testing.T) {
	if got, want := nsrestoreCUDAHelperFD, 5; got != want {
		t.Fatalf("CUDA helper FD = %d, want %d", got, want)
	}
	want := []string{
		"--pagebroker-image-fd", "6",
		"--pagebroker-work-fd", "7",
		"--pagebroker-provider-fd", "8",
		"--pagebroker-control-fd", "9",
		"--pagebroker-transaction-id", "tx-test",
	}
	got := nsrestorePageBrokerArgs(nsrestoreCUDAHelperFD+1, "tx-test")
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("PageBroker nsrestore args = %q, want %q", got, want)
	}
}

func TestRestorePreResumeWaitsForPageBroker(t *testing.T) {
	control, err := os.CreateTemp(t.TempDir(), "pagebroker-control")
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()

	var calls []string
	timings := &nsrestorePhaseTimings{}
	preResume := restorePreResume(control, "tx-test", func(gotControl *os.File, transactionID string) error {
		calls = append(calls, "ready")
		if gotControl != control {
			t.Fatal("readiness wait received a different control file")
		}
		if transactionID != "tx-test" {
			t.Fatalf("transaction ID = %q, want tx-test", transactionID)
		}
		return nil
	}, time.Now(), timings, testr.New(t))
	if err := preResume(42); err != nil {
		t.Fatalf("pre-resume callback: %v", err)
	}
	if got, want := strings.Join(calls, ","), "ready"; got != want {
		t.Fatalf("pre-resume calls = %q, want %q", got, want)
	}
}

func TestRestorePreResumePropagatesReadinessFailure(t *testing.T) {
	want := errors.New("host memory fill failed")
	control, err := os.CreateTemp(t.TempDir(), "pagebroker-control")
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	preResume := restorePreResume(control, "tx-test", func(*os.File, string) error {
		return want
	}, time.Now(), &nsrestorePhaseTimings{}, testr.New(t))
	if err := preResume(42); !errors.Is(err, want) {
		t.Fatalf("pre-resume error = %v, want %v", err, want)
	}
}

func TestInspectRestoreUsesContainerIDWhenProvided(t *testing.T) {
	checkpointDir := t.TempDir()
	manifest := types.NewCheckpointManifest(
		"checkpoint-123",
		types.CRIUDumpManifest{},
		types.NewSourcePodManifest("source-id", 456, "node-1", "source-pod", "default", "10.0.0.11", nil),
		types.OverlayManifest{},
	)
	if err := types.WriteManifest(checkpointDir, manifest); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}

	rt := &restoreFakeRuntime{}
	_, err := inspectRestore(
		context.Background(),
		rt,
		testr.New(t),
		RestoreRequest{
			CheckpointID:       "checkpoint-123",
			CheckpointLocation: checkpointDir,
			ContainerID:        "placeholder-id",
			PodName:            "virtual-pod-name",
			PodNamespace:       "default",
			ContainerName:      "main",
		},
	)
	if err != nil {
		t.Fatalf("inspectRestore: %v", err)
	}
	if rt.resolvedID != "placeholder-id" {
		t.Fatalf("ResolveContainer called with %q, want placeholder-id", rt.resolvedID)
	}
	if rt.resolveByPodHit {
		t.Fatal("ResolveContainerByPod should not be used when ContainerID is provided")
	}
}
