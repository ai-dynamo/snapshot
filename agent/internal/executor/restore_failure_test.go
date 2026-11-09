// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/testr"
	"google.golang.org/protobuf/proto"

	"github.com/ai-dynamo/snapshot/agent/internal/nsmount"
	"github.com/ai-dynamo/snapshot/agent/internal/pagebroker"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

func TestRestoreFailureKeepsCauseAndCleanupApart(t *testing.T) {
	cause := pagebroker.NewFailureError(pagebroker.Failure_UNAVAILABLE, "busy")
	cleanup := pagebroker.NewFailureError(pagebroker.Failure_ARTIFACT_CORRUPT, "abort refused")
	failure := newRestoreFailure(failStep(StepPrepareRestore, cause), cleanup, false, false)

	if failure.Step != StepPrepareRestore || failure.Cause != error(cause) {
		t.Fatalf("failure = %+v, want the step and its cause", failure)
	}
	if want := "while preparing PageBroker restore: PageBroker UNAVAILABLE: busy\nPageBroker ARTIFACT_CORRUPT: abort refused"; failure.Error() != want {
		t.Fatalf("Error() = %q, want %q", failure.Error(), want)
	}
	if StepOf(failure) != StepPrepareRestore {
		t.Fatalf("StepOf = %q", StepOf(failure))
	}
	var code *pagebroker.FailureError
	if !errors.As(failure.Cause, &code) || code.Code() != pagebroker.Failure_UNAVAILABLE {
		t.Fatalf("cause code = %v, want UNAVAILABLE", code)
	}
	if !IsCleanupFailure(failure.Cleanup) || !errors.Is(failure, cleanup) {
		t.Fatalf("cleanup = %v, want a marked cleanup failure reachable from the failure", failure.Cleanup)
	}

	bare := newRestoreFailure(errors.New("restore mounter is required"), nil, false, false)
	if bare.Step != "" || bare.Error() != "restore mounter is required" || len(bare.Unwrap()) != 1 {
		t.Fatalf("failure without a step or cleanup = %+v (%q)", bare, bare.Error())
	}
}

func TestNewRestoreFailureReadsTheMarks(t *testing.T) {
	cause := errors.New("boom")
	tests := []struct {
		name        string
		err         error
		abortFailed bool
		mountLeft   bool
		wantFinal   bool
		wantChanged bool
	}{
		{name: "unmarked", err: failStep(StepRestoreCRIU, cause)},
		{name: "final inside the step", err: failStep(StepValidateManifest, final(cause)), wantFinal: true},
		{name: "changed outside the step", err: &changedContainerError{err: failStep(StepRestoreCRIU, cause)}, wantChanged: true},
		{name: "abort failed", err: failStep(StepRestoreCRIU, cause), abortFailed: true, wantFinal: true},
		{name: "a mount was left", err: failStep(StepRestoreCRIU, cause), mountLeft: true, wantChanged: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			failure := newRestoreFailure(tt.err, nil, tt.abortFailed, tt.mountLeft)
			if failure.Final != tt.wantFinal || failure.ContainerChanged != tt.wantChanged {
				t.Fatalf("failure = %+v, want Final %v and ContainerChanged %v", failure, tt.wantFinal, tt.wantChanged)
			}
			if !errors.Is(failure.Cause, cause) || failure.Error() != tt.err.Error() {
				t.Fatalf("failure = %q with cause %v, want the attempt's text and cause", failure, failure.Cause)
			}
		})
	}
}

// Each case fails one real Restore at a different point and checks what the
// failure says about the next attempt.
func TestRestoreFailureSaysWhatIsLeftForTheNextAttempt(t *testing.T) {
	changedCRIU := &changedContainerError{err: failStep(StepRestoreCRIU, errors.New("CRIU restore failed: exit 1"))}
	tests := []struct {
		name        string
		exchanges   []string
		mutate      func(*types.CheckpointManifest)
		initCmdline string
		nsErr       error
		validateErr error
		failMount   bool
		wantStep    Step
		wantFinal   bool
		wantChanged bool
		wantCleanup string
		wantCode    pagebroker.Failure_Code
	}{
		{
			name:      "nsrestore failed before changing the container",
			exchanges: []string{"stage", "abort"},
			nsErr:     errors.New("nsrestore exited with status 1 without reporting a cause"),
			wantStep:  StepRunNSRestore,
		},
		{
			name:        "nsrestore failed after changing the container",
			exchanges:   []string{"stage", "abort"},
			nsErr:       changedCRIU,
			wantStep:    StepRestoreCRIU,
			wantChanged: true,
		},
		{
			name:        "the restored process failed validation",
			exchanges:   []string{"stage", "commit"},
			validateErr: errors.New("process 4242 is a zombie"),
			wantStep:    StepValidateRestored,
			wantChanged: true,
		},
		{
			name:        "the abort failed",
			exchanges:   []string{"stage", "refuse-abort"},
			nsErr:       errors.New("nsrestore was killed (signal: killed)"),
			wantStep:    StepRunNSRestore,
			wantFinal:   true,
			wantCleanup: "abort PageBroker restore",
		},
		{
			name:        "a restore mount was left in the container",
			exchanges:   []string{"stage", "abort"},
			nsErr:       errors.New("nsrestore was killed (signal: killed)"),
			failMount:   true,
			wantStep:    StepRunNSRestore,
			wantChanged: true,
			wantCleanup: "unmount staging failed",
		},
		{
			name:      "PageBroker refused the artifact",
			exchanges: []string{"corrupt", "abort"},
			wantStep:  StepPrepareRestore,
			wantCode:  pagebroker.Failure_ARTIFACT_CORRUPT,
		},
		{
			name:      "the manifest names another artifact",
			mutate:    func(m *types.CheckpointManifest) { m.Artifact.ContentUID = "other" },
			wantStep:  StepValidateManifest,
			wantFinal: true,
		},
		{
			name:      "PageBroker cannot restore GPU memory it saved",
			mutate:    func(m *types.CheckpointManifest) { m.CUDA.CustomStorage = true },
			wantStep:  StepValidateManifest,
			wantFinal: true,
		},
		{
			name:      "the cuinterpose libraries cannot be verified",
			mutate:    func(m *types.CheckpointManifest) { m.CuInterpose = testCuInterposeIdentity() },
			wantStep:  StepVerifyCuInterpose,
			wantFinal: true,
		},
		{
			name:        "the container does not run the restore proxy",
			initCmdline: "/bin/sleep",
			wantStep:    StepInspectPlaceholder,
			wantFinal:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := t.TempDir()
			directory, err := nsmount.ResolveArtifactPath(base, "content", "main")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(directory, 0700); err != nil {
				t.Fatal(err)
			}
			manifest := types.NewCheckpointManifest("content", "main", types.CRIUDumpManifest{},
				types.SourcePodManifest{}, types.OverlayManifest{}, types.HostManifest{})
			if tt.mutate != nil {
				tt.mutate(manifest)
			}
			if err := types.WriteManifest(directory, manifest); err != nil {
				t.Fatal(err)
			}

			originalExec, originalValidate, originalInit := execNSRestoreFunc, validateRestoredProcessFunc, readInitCmdline
			t.Cleanup(func() {
				execNSRestoreFunc, validateRestoredProcessFunc, readInitCmdline = originalExec, originalValidate, originalInit
			})
			execNSRestoreFunc = func(context.Context, logr.Logger, RestoreRequest, *types.RestoreContainerSnapshot, nsmount.MountPoint, string, *pagebroker.CustomStorageExecution) (*RestoreInNamespaceResult, error) {
				if tt.nsErr != nil {
					return nil, tt.nsErr
				}
				return &RestoreInNamespaceResult{RestoredPID: 4242}, nil
			}
			validateRestoredProcessFunc = func(string, int, logr.Logger) error { return tt.validateErr }
			if tt.initCmdline != "" {
				readInitCmdline = func(int) (string, error) { return tt.initCmdline, nil }
			}

			listener := listenPageBroker(t)
			server := make(chan error, 1)
			go func() { server <- servePageBrokerExchanges(listener, directory, tt.exchanges) }()

			events := make(chan string, 16)
			var mounts RestoreMounter = &cleanupMounter{events: events}
			if tt.failMount {
				mounts = &failingStagingMounter{events: events}
			}
			ctx, cancel := context.WithTimeout(context.Background(), pageBrokerTestTimeout)
			defer cancel()
			_, err = Restore(ctx, &restoreFakeRuntime{}, testr.New(t), RestoreRequest{
				BasePath: base, ContentUID: "content", ArtifactContainerName: "main", ContainerID: "placeholder",
				PageBrokerControlSocketPath: listener.Addr().String(),
				PageBrokerRestoreMode:       "staged", SkipCompatCheck: true,
			}, mounts)

			var failure *RestoreFailure
			if !errors.As(err, &failure) {
				t.Fatalf("Restore() = %v, want a *RestoreFailure", err)
			}
			if failure.Step != tt.wantStep || failure.Final != tt.wantFinal || failure.ContainerChanged != tt.wantChanged {
				t.Fatalf("failure = step %q, Final %v, ContainerChanged %v; want %q, %v, %v (%v)",
					failure.Step, failure.Final, failure.ContainerChanged, tt.wantStep, tt.wantFinal, tt.wantChanged, err)
			}
			if tt.wantCleanup == "" && failure.Cleanup != nil {
				t.Fatalf("cleanup = %v, want none", failure.Cleanup)
			}
			if tt.wantCleanup != "" && (failure.Cleanup == nil || !strings.Contains(failure.Cleanup.Error(), tt.wantCleanup)) {
				t.Fatalf("cleanup = %v, want it to name %q", failure.Cleanup, tt.wantCleanup)
			}
			if tt.wantCleanup != "" && strings.Contains(failure.Cause.Error(), tt.wantCleanup) {
				t.Fatalf("cause = %v, want the cleanup kept out of it", failure.Cause)
			}
			var code *pagebroker.FailureError
			if found := errors.As(failure.Cause, &code); found != (tt.wantCode != pagebroker.Failure_UNSPECIFIED) ||
				(found && code.Code() != tt.wantCode) {
				t.Fatalf("cause code = %v, want %v", code, tt.wantCode)
			}
			waitPageBrokerTest(t, server)
		})
	}
}

// servePageBrokerExchanges answers one request per exchange, in order.
func servePageBrokerExchanges(listener *net.UnixListener, directory string, exchanges []string) error {
	for _, exchange := range exchanges {
		connection, err := listener.Accept()
		if err != nil {
			return err
		}
		request, err := readPageBrokerTestRequest(connection)
		if err != nil {
			connection.Close()
			return err
		}
		response := &pagebroker.Response{}
		switch exchange {
		case "stage":
			response.Result = &pagebroker.Response_StagedRestoreDirectory{StagedRestoreDirectory: &pagebroker.StagedRestoreDirectory{ImageDirectory: &directory}}
		case "corrupt":
			response.Result = &pagebroker.Response_Failure{Failure: &pagebroker.Failure{
				Code: pagebroker.Failure_ARTIFACT_CORRUPT.Enum(), Message: proto.String("chunk 3 checksum mismatch"),
			}}
		case "commit":
			if request.GetCommit() == nil {
				connection.Close()
				return errors.New("expected Commit")
			}
			response.Result = &pagebroker.Response_CommitComplete{CommitComplete: &pagebroker.CommitComplete{}}
		case "abort":
			if request.GetAbort() == nil {
				connection.Close()
				return errors.New("expected Abort")
			}
			response.Result = &pagebroker.Response_AbortComplete{AbortComplete: &pagebroker.AbortComplete{}}
		case "refuse-abort":
			if request.GetAbort() == nil {
				connection.Close()
				return errors.New("expected Abort")
			}
			response.Result = &pagebroker.Response_Failure{Failure: &pagebroker.Failure{
				Code: pagebroker.Failure_STORAGE_ERROR.Enum(), Message: proto.String("transaction is still in use"),
			}}
		}
		err = replyPageBrokerTest(connection, request, response)
		connection.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
