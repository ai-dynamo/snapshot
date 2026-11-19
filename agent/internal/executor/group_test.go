// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	criurpc "github.com/checkpoint-restore/go-criu/v8/rpc"
	"github.com/go-logr/logr"
	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmihailenco/msgpack/v5"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/ai-dynamo/snapshot/agent/internal/cuda"
	"github.com/ai-dynamo/snapshot/agent/internal/group"
	"github.com/ai-dynamo/snapshot/agent/internal/nsmount"
	"github.com/ai-dynamo/snapshot/agent/internal/pagebroker"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/ai-dynamo/snapshot/coordinator/coordinatortest"
	"github.com/ai-dynamo/snapshot/coordinator/server"
	"github.com/ai-dynamo/snapshot/coordinator/session"
)

func TestGroupRestoreRefusesAnotherMembersArtifact(t *testing.T) {
	otherVersion := capturedAs("worker", "leader", "worker")
	otherVersion.Version = types.PodGroupManifestVersion + 1
	for _, tc := range []struct {
		name     string
		captured *types.PodGroupManifest
		want     string
	}{
		{name: "the member's own artifact", captured: capturedAs("worker", "leader", "worker")},
		{name: "an artifact captured on its own", want: "not captured as a member of a pod group"},
		{
			name:     "another member's artifact",
			captured: capturedAs("leader", "leader", "worker"),
			want:     "the artifact is member leader at rank 0 of its capture, not member worker at rank 1",
		},
		{
			name:     "the member's artifact at another rank",
			captured: capturedAs("worker", "worker", "leader"),
			want:     "the artifact is member worker at rank 0 of its capture, not member worker at rank 1",
		},
		{
			name:     "an artifact this agent cannot read",
			captured: otherVersion,
			want:     "the artifact's pod group section has version 2, and this agent restores version 1",
		},
		{
			name:     "an artifact of a capture with more members",
			captured: capturedAs("worker", "leader", "worker", "spare"),
			want:     "the capture had 3 members, the restore has 2",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkGroupArtifact(tc.captured, "worker", 1, 2)
			if tc.want == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestGroupCheckpointRecordsEveryMember(t *testing.T) {
	member, peer := joinMembers(t)
	peerEnded := runPeer(t, peer, group.Admission{
		PodIP: netip.MustParseAddr("10.0.0.2"), PodUID: "pod-1", Containers: []string{"worker"},
	}, group.NetFreeze)
	socket := serveCheckpointStaging(t, t.TempDir())
	var recorded *types.PodGroupManifest
	stubCapture(t, func(data *types.CheckpointManifest) (*checkpointPhaseTimings, error) {
		recorded = data.PodGroup
		return nil, failStep(StepDumpCRIU, errors.New("stop after the manifest is recorded"))
	})
	cfg := &types.AgentConfig{
		Storage:    types.StorageSpec{BasePath: t.TempDir()},
		PageBroker: types.PageBrokerSpec{ControlSocketPath: socket},
	}
	req := CheckpointRequest{
		ContentUID: "content-uid", ContainerName: "main", PodUID: "pod-0", PodIP: "10.0.0.1", Group: member,
	}
	_, err := checkpoint(groupTestContext(t), nil, testr.New(t), req, cfg, inspectNothing(t))

	require.Equal(t, StepDumpCRIU, StepOf(err), "%v", err)
	<-peerEnded
	require.Equal(t, &types.PodGroupManifest{
		Version:    types.PodGroupManifestVersion,
		CaptureUID: "attempt-1",
		MemberID:   "member-0",
		Members: []types.PodGroupManifestMember{
			{ID: "member-0", PodUID: "pod-0", Containers: []string{"main"}, PodIP: netip.MustParseAddr("10.0.0.1")},
			{ID: "member-1", PodUID: "pod-1", Containers: []string{"worker"}, PodIP: netip.MustParseAddr("10.0.0.2")},
		},
	}, recorded)
}

func TestGroupCheckpointFailsTheAttemptAsRefusedUntilItCommits(t *testing.T) {
	for _, tc := range []struct {
		name  string
		podIP string
		// noStaging leaves PageBroker's staging directory missing, so writing
		// the manifest, the last step before admission, fails.
		noStaging     bool
		wantStep      Step
		wantDumped    bool
		wantCommitted bool
		wantReason    string
	}{
		{
			name:       "a step before admission fails",
			podIP:      "10.0.0.1",
			noStaging:  true,
			wantStep:   StepConfigureCheckpoint,
			wantReason: session.ReasonRefused,
		},
		{
			name:       "the Pod's IP is not IPv4",
			podIP:      "fd00::1",
			wantStep:   StepAdmitGroup,
			wantReason: session.ReasonRefused,
		},
		{
			name:          "the dump fails",
			podIP:         "10.0.0.1",
			wantStep:      StepDumpCRIU,
			wantDumped:    true,
			wantCommitted: true,
			wantReason:    session.ReasonMemberFailed,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			member, peer := joinMembers(t)
			peerEnded := runPeer(t, peer, group.Admission{PodIP: netip.MustParseAddr("10.0.0.2")}, group.NetFreeze)
			staging := t.TempDir()
			if tc.noStaging {
				staging = filepath.Join(staging, "missing")
			}
			socket := serveCheckpointStaging(t, staging)

			dumped := false
			stubCapture(t, func(*types.CheckpointManifest) (*checkpointPhaseTimings, error) {
				dumped = true
				return nil, failStep(StepDumpCRIU, errors.New("the dump failed"))
			})
			cfg := &types.AgentConfig{
				Storage:    types.StorageSpec{BasePath: t.TempDir()},
				PageBroker: types.PageBrokerSpec{ControlSocketPath: socket},
			}
			req := CheckpointRequest{ContentUID: "content-uid", ContainerName: "main", PodIP: tc.podIP, Group: member}
			_, err := checkpoint(groupTestContext(t), nil, testr.New(t), req, cfg, inspectNothing(t))

			assert.Equal(t, tc.wantStep, StepOf(err), "%v", err)
			assert.Equal(t, tc.wantDumped, dumped)
			outcome := <-peerEnded
			assert.Equal(t, tc.wantCommitted, outcome.committed, "whether the peer passed the admitted barrier")
			require.ErrorIs(t, outcome.err, &session.AbortError{Reason: tc.wantReason})
		})
	}
}

func TestGroupCheckpointThatCuInterposeRefusesNeverCommits(t *testing.T) {
	member, peer := joinMembers(t)
	peerEnded := runPeer(t, peer, group.Admission{PodIP: netip.MustParseAddr("10.0.0.2")}, group.NetFreeze)
	socket := serveCheckpointStaging(t, t.TempDir())
	// The relays' leader finds the group's topology invalid, and the member's
	// coordinator exits with an error.
	var inspected cuda.CuInterposeTarget
	stubCuInterpose(t, func(target cuda.CuInterposeTarget) error {
		inspected = target
		return errors.New("cuinterpose coordinator: exit status 1: the group's topology is invalid")
	})
	captured := false
	stubCapture(t, func(*types.CheckpointManifest) (*checkpointPhaseTimings, error) {
		captured = true
		return nil, errors.New("the capture started")
	})
	cfg := &types.AgentConfig{
		Storage:    types.StorageSpec{BasePath: t.TempDir()},
		PageBroker: types.PageBrokerSpec{ControlSocketPath: socket},
	}
	recorded := false
	req := CheckpointRequest{
		ContentUID: "content-uid", ContainerName: "main", PodIP: "10.0.0.1", Group: member,
		Committed: func(context.Context) error {
			recorded = true
			return nil
		},
	}
	_, err := checkpoint(groupTestContext(t), nil, testr.New(t), req, cfg, inspectCUDA(t))

	assert.Equal(t, StepAdmitGroup, StepOf(err), "%v", err)
	assert.ErrorContains(t, err, "the group's topology is invalid")
	require.NotNil(t, inspected.Relay, "the coordinator inspects as the member's relay")
	assert.Equal(t, []string{"--rank", "0", "--relay-ranks", "0"}, inspected.Relay.Args[:4])
	assert.False(t, recorded, "the commit was recorded")
	assert.False(t, captured, "the capture started")
	outcome := <-peerEnded
	assert.False(t, outcome.committed, "the peer passed the admitted barrier")
	require.ErrorIs(t, outcome.err, &session.AbortError{Reason: session.ReasonRefused})
}

func TestGroupCheckpointRecordsTheMembersItSharesGPUMemoryWith(t *testing.T) {
	for _, tc := range []struct {
		name string
		// record is what the relays' leader records at inspect, or nothing
		// if it is nil.
		record     []byte
		wantStep   Step
		wantErr    string
		wantPeers  []int
		wantReason string
	}{
		{
			name: "the leader records them",
			// rmp_serde's encoding of the leader's map {0: [1], 1: [0]}.
			record:     []byte{0x82, 0x00, 0x91, 0x01, 0x01, 0x91, 0x00},
			wantStep:   StepDumpCRIU,
			wantPeers:  []int{1},
			wantReason: session.ReasonMemberFailed,
		},
		{
			name:       "the leader records nothing",
			wantStep:   StepAdmitGroup,
			wantErr:    "the leader of the cuInterpose relays recorded no inspect.fabric-peers",
			wantReason: session.ReasonRefused,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			member, peer := joinMembers(t)
			peerEnded := runPeer(t, peer, group.Admission{PodIP: netip.MustParseAddr("10.0.0.2"), RunsRelay: true},
				group.NetFreeze)
			socket := serveCheckpointStaging(t, t.TempDir())
			// The member's coordinator inspects as the relays' leader.
			stubCuInterpose(t, func(target cuda.CuInterposeTarget) error {
				if tc.record == nil {
					return nil
				}
				return insertThroughRelay(target.Relay, "inspect.fabric-peers", tc.record)
			})
			var recorded *types.PodGroupManifest
			stubCapture(t, func(data *types.CheckpointManifest) (*checkpointPhaseTimings, error) {
				recorded = data.PodGroup
				return nil, failStep(StepDumpCRIU, errors.New("stop after the manifest is recorded"))
			})
			cfg := &types.AgentConfig{
				Storage:    types.StorageSpec{BasePath: t.TempDir()},
				PageBroker: types.PageBrokerSpec{ControlSocketPath: socket},
			}
			req := CheckpointRequest{ContentUID: "content-uid", ContainerName: "main", PodIP: "10.0.0.1", Group: member}
			_, err := checkpoint(groupTestContext(t), nil, testr.New(t), req, cfg, inspectCUDA(t))

			assert.Equal(t, tc.wantStep, StepOf(err), "%v", err)
			if tc.wantErr != "" {
				assert.ErrorContains(t, err, tc.wantErr)
				assert.Nil(t, recorded, "the capture started")
			} else {
				require.NotNil(t, recorded, "the capture started")
				assert.Equal(t, tc.wantPeers, recorded.FabricPeers)
			}
			require.ErrorIs(t, (<-peerEnded).err, &session.AbortError{Reason: tc.wantReason})
		})
	}
}

func TestGroupRestoreFailsTheAttemptAsRefusedUntilItCommits(t *testing.T) {
	for _, tc := range []struct {
		name          string
		captured      *types.PodGroupManifest
		targetPodIP   string
		wantStep      Step
		wantMounted   bool
		wantCommitted bool
		wantReason    string
	}{
		{
			name:        "the artifact is another member's",
			captured:    capturedAs("member-1", "member-0", "member-1"),
			targetPodIP: "10.0.0.1",
			wantStep:    StepAdmitGroup,
			wantReason:  session.ReasonRefused,
		},
		{
			name:        "the artifact lists another number of members",
			captured:    capturedAs("member-0", "member-0", "member-1", "member-2"),
			targetPodIP: "10.0.0.1",
			wantStep:    StepAdmitGroup,
			wantReason:  session.ReasonRefused,
		},
		{
			name:        "the restore Pod's IP is not IPv4",
			captured:    capturedAs("member-0", "member-0", "member-1"),
			targetPodIP: "fd00::1",
			wantStep:    StepAdmitGroup,
			wantReason:  session.ReasonRefused,
		},
		{
			name:          "the first step after the commit fails",
			captured:      capturedAs("member-0", "member-0", "member-1"),
			targetPodIP:   "10.0.0.1",
			wantStep:      StepMountBundle,
			wantMounted:   true,
			wantCommitted: true,
			wantReason:    session.ReasonMemberFailed,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			member, peer := joinMembers(t)
			peerEnded := runPeer(t, peer,
				group.Admission{PodIP: netip.MustParseAddr("10.0.0.2"), CaptureID: "capture-1"}, group.Release)
			base := writeGroupArtifact(t, tc.captured)

			mounted := false
			mounter := stoppingMounter{stop: func() error {
				mounted = true
				return errors.New("the bundle mount failed")
			}}
			req := RestoreRequest{
				BasePath: base, ContentUID: "content", ArtifactContainerName: "main", ContainerID: "placeholder",
				TargetPodIP: tc.targetPodIP, WithoutProxy: true, SkipCompatCheck: true, Group: member,
			}
			_, err := Restore(groupTestContext(t), &restoreFakeRuntime{}, testr.New(t), req, mounter)

			assert.Equal(t, tc.wantStep, StepOf(err), "%v", err)
			assert.Equal(t, tc.wantMounted, mounted)
			outcome := <-peerEnded
			assert.Equal(t, tc.wantCommitted, outcome.committed, "whether the peer passed the admitted barrier")
			require.ErrorIs(t, outcome.err, &session.AbortError{Reason: tc.wantReason})
		})
	}
}

func TestGroupCheckpointRecordsTheCommitBeforeItsFirstIrreversibleStep(t *testing.T) {
	for _, tc := range []struct {
		name      string
		recordErr error
		wantSteps []string
		wantStep  Step
	}{
		{
			name:      "the record is written",
			wantSteps: []string{"record", "dump"},
			wantStep:  StepDumpCRIU,
		},
		{
			name:      "the record cannot be written",
			recordErr: errors.New("the API server refused the write"),
			wantSteps: []string{"record"},
			wantStep:  StepRecordGroupStart,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			member, peer := joinMembers(t)
			peerEnded := runPeer(t, peer, group.Admission{PodIP: netip.MustParseAddr("10.0.0.2")}, group.NetFreeze)
			socket := serveCheckpointStaging(t, t.TempDir())

			var steps []string
			stubCapture(t, func(*types.CheckpointManifest) (*checkpointPhaseTimings, error) {
				steps = append(steps, "dump")
				return nil, failStep(StepDumpCRIU, errors.New("stop at the first irreversible step"))
			})
			cfg := &types.AgentConfig{
				Storage:    types.StorageSpec{BasePath: t.TempDir()},
				PageBroker: types.PageBrokerSpec{ControlSocketPath: socket},
			}
			req := CheckpointRequest{
				ContentUID: "content-uid", ContainerName: "main", PodIP: "10.0.0.1", Group: member,
				Committed: func(context.Context) error {
					steps = append(steps, "record")
					return tc.recordErr
				},
			}
			_, err := checkpoint(groupTestContext(t), nil, testr.New(t), req, cfg, inspectNothing(t))

			assert.Equal(t, tc.wantStep, StepOf(err), "%v", err)
			assert.Equal(t, tc.wantSteps, steps)
			outcome := <-peerEnded
			assert.True(t, outcome.committed, "whether the peer passed the admitted barrier")
			require.ErrorIs(t, outcome.err, &session.AbortError{Reason: session.ReasonMemberFailed})
		})
	}
}

func TestGroupRestoreRecordsTheCommitBeforeItsFirstIrreversibleStep(t *testing.T) {
	for _, tc := range []struct {
		name      string
		recordErr error
		wantSteps []string
		wantStep  Step
	}{
		{
			name:      "the record is written",
			wantSteps: []string{"record", "mount the bundle"},
			wantStep:  StepMountBundle,
		},
		{
			name:      "the record cannot be written",
			recordErr: errors.New("the API server refused the write"),
			wantSteps: []string{"record"},
			wantStep:  StepRecordGroupStart,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			member, peer := joinMembers(t)
			peerEnded := runPeer(t, peer,
				group.Admission{PodIP: netip.MustParseAddr("10.0.0.2"), CaptureID: "capture-1"}, group.Release)
			base := writeGroupArtifact(t, capturedAs("member-0", "member-0", "member-1"))

			var steps []string
			mounter := stoppingMounter{stop: func() error {
				steps = append(steps, "mount the bundle")
				return errors.New("stop at the first irreversible step")
			}}
			req := RestoreRequest{
				BasePath: base, ContentUID: "content", ArtifactContainerName: "main", ContainerID: "placeholder",
				TargetPodIP: "10.0.0.1", WithoutProxy: true, SkipCompatCheck: true, Group: member,
				Committed: func(context.Context) error {
					steps = append(steps, "record")
					return tc.recordErr
				},
			}
			_, err := Restore(groupTestContext(t), &restoreFakeRuntime{}, testr.New(t), req, mounter)

			assert.Equal(t, tc.wantStep, StepOf(err), "%v", err)
			assert.Equal(t, tc.wantSteps, steps)
			outcome := <-peerEnded
			assert.True(t, outcome.committed, "whether the peer passed the admitted barrier")
			require.ErrorIs(t, outcome.err, &session.AbortError{Reason: session.ReasonMemberFailed})
		})
	}
}

func TestGroupCheckpointDumpsOnlyOnceEveryMemberArrivesAtNetFreeze(t *testing.T) {
	for _, tc := range []struct {
		name      string
		peerFails bool
		wantSteps []string
		wantStep  Step
	}{
		{
			name:      "the peer arrives late",
			wantSteps: []string{"record", "the peer arrives at net-freeze", "dump"},
		},
		{
			name:      "the peer fails instead of arriving",
			peerFails: true,
			wantSteps: []string{"record", "the peer fails"},
			wantStep:  StepGroupBarrier,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			member, peer := joinMembers(t)
			steps := make(chan string, 4)
			letGo := make(chan struct{})
			peerEnded := runLatePeer(t, peer, group.Admission{PodIP: netip.MustParseAddr("10.0.0.2")},
				group.NetFreeze, tc.peerFails, letGo, steps)
			socket := serveCheckpointStaging(t, t.TempDir())
			// Only the dump is replaced. The GPU checkpoint before net-freeze
			// runs, with no CUDA process to checkpoint.
			originalDump := dumpCheckpointFunc
			t.Cleanup(func() { dumpCheckpointFunc = originalDump })
			dumpCheckpointFunc = func(context.Context, *criurpc.CriuOpts, *types.CRIUSettings, *types.CheckpointManifest,
				*types.CheckpointContainerSnapshot, string, *checkpointPhaseTimings, logr.Logger,
			) error {
				steps <- "dump"
				return nil
			}
			cfg := &types.AgentConfig{
				Storage:    types.StorageSpec{BasePath: t.TempDir()},
				PageBroker: types.PageBrokerSpec{ControlSocketPath: socket},
			}
			committed := make(chan struct{})
			req := CheckpointRequest{
				ContentUID: "content-uid", ContainerName: "main", PodIP: "10.0.0.1", Group: member,
				Committed: func(context.Context) error {
					steps <- "record"
					close(committed)
					return nil
				},
			}
			var err error
			returned := make(chan struct{})
			go func() {
				defer close(returned)
				_, err = checkpoint(groupTestContext(t), nil, testr.New(t), req, cfg, inspectNothing(t))
			}()

			holdPeer(t, committed, returned, letGo)
			<-returned

			assert.Equal(t, tc.wantStep, StepOf(err), "%v", err)
			if tc.peerFails {
				require.ErrorIs(t, err, &session.AbortError{Reason: session.ReasonMemberFailed})
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.wantSteps, received(steps))
			require.NoError(t, (<-peerEnded).err)
		})
	}
}

func TestGroupRestoreCompletesOnlyOnceEveryMemberArrivesAtRelease(t *testing.T) {
	for _, tc := range []struct {
		name      string
		peerFails bool
		wantSteps []string
		wantStep  Step
		want      RestoreResult
	}{
		{
			name:      "the peer arrives late",
			wantSteps: []string{"unmount staging", "validate", "the peer arrives at release", "unmount bundle"},
			want:      RestoreResult{PlaceholderHostPID: 123, RestoredPID: 4242},
		},
		{
			name:      "the peer fails instead of arriving",
			peerFails: true,
			// A failed restore unmounts the bundle too, in its cleanup.
			wantSteps: []string{"unmount staging", "validate", "the peer fails", "unmount bundle"},
			wantStep:  StepGroupBarrier,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			member, peer := joinMembers(t)
			steps := make(chan string, 8)
			letGo := make(chan struct{})
			peerEnded := runLatePeer(t, peer, group.Admission{PodIP: netip.MustParseAddr("10.0.0.2"), CaptureID: "capture-1"},
				group.Release, tc.peerFails, letGo, steps)
			base := writeGroupArtifact(t, capturedAs("member-0", "member-0", "member-1"))
			socket := serveCheckpointStaging(t, t.TempDir())
			// nsrestore, which runs CRIU in the placeholder, and the check of
			// the process it restored are replaced.
			originalExec, originalValidate := execNSRestoreFunc, validateRestoredProcessFunc
			t.Cleanup(func() { execNSRestoreFunc, validateRestoredProcessFunc = originalExec, originalValidate })
			execNSRestoreFunc = func(context.Context, logr.Logger, RestoreRequest, *types.RestoreContainerSnapshot,
				nsmount.MountPoint, string, *pagebroker.CustomStorageExecution,
			) (*RestoreInNamespaceResult, error) {
				return &RestoreInNamespaceResult{RestoredPID: 4242}, nil
			}
			validated := make(chan struct{})
			validateRestoredProcessFunc = func(string, int, logr.Logger) error {
				steps <- "validate"
				close(validated)
				return nil
			}
			req := RestoreRequest{
				BasePath: base, ContentUID: "content", ArtifactContainerName: "main", ContainerID: "placeholder",
				TargetPodIP: "10.0.0.1", WithoutProxy: true, SkipCompatCheck: true, Group: member,
				PageBrokerControlSocketPath: socket, PageBrokerRestoreMode: "staged",
			}
			var restored RestoreResult
			var err error
			returned := make(chan struct{})
			go func() {
				defer close(returned)
				restored, err = Restore(groupTestContext(t), &restoreFakeRuntime{}, testr.New(t), req,
					&cleanupMounter{events: steps})
			}()

			holdPeer(t, validated, returned, letGo)
			<-returned

			assert.Equal(t, tc.wantStep, StepOf(err), "%v", err)
			if tc.peerFails {
				require.ErrorIs(t, err, &session.AbortError{Reason: session.ReasonMemberFailed})
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.want, restored)
			assert.Equal(t, tc.wantSteps, received(steps))
			require.NoError(t, (<-peerEnded).err)
		})
	}
}

func TestRestoreRefusesAPodGroupMembersArtifactOnItsOwn(t *testing.T) {
	base := writeGroupArtifact(t, capturedAs("member-0", "member-0", "member-1"))
	mounter := stoppingMounter{stop: func() error {
		t.Error("a refused restore mounted into its placeholder")
		return errors.New("refused")
	}}
	req := RestoreRequest{
		BasePath: base, ContentUID: "content", ArtifactContainerName: "main", ContainerID: "placeholder",
		TargetPodIP: "10.0.0.1", WithoutProxy: true, SkipCompatCheck: true,
	}
	_, err := Restore(groupTestContext(t), &restoreFakeRuntime{}, testr.New(t), req, mounter)

	assert.Equal(t, StepValidateManifest, StepOf(err), "%v", err)
	require.ErrorContains(t, err,
		"the checkpoint is member member-0 of pod group capture capture-1, and restores only with its group")
}

func TestGroupCheckpointThatPanicsFailsTheAttempt(t *testing.T) {
	member, peer := joinMembers(t)
	peerEnded := runPeer(t, peer, group.Admission{PodIP: netip.MustParseAddr("10.0.0.2")}, group.NetFreeze)
	socket := serveCheckpointStaging(t, t.TempDir())
	stubCapture(t, func(*types.CheckpointManifest) (*checkpointPhaseTimings, error) {
		panic("the dump panicked")
	})
	cfg := &types.AgentConfig{
		Storage:    types.StorageSpec{BasePath: t.TempDir()},
		PageBroker: types.PageBrokerSpec{ControlSocketPath: socket},
	}
	req := CheckpointRequest{ContentUID: "content-uid", ContainerName: "main", PodIP: "10.0.0.1", Group: member}

	require.PanicsWithValue(t, "the dump panicked", func() {
		_, _ = checkpoint(groupTestContext(t), nil, testr.New(t), req, cfg, inspectNothing(t))
	})
	require.ErrorIs(t, (<-peerEnded).err, &session.AbortError{Reason: session.ReasonMemberFailed})
}

func TestGroupRestoreThatPanicsFailsTheAttempt(t *testing.T) {
	member, peer := joinMembers(t)
	peerEnded := runPeer(t, peer,
		group.Admission{PodIP: netip.MustParseAddr("10.0.0.2"), CaptureID: "capture-1"}, group.Release)
	base := writeGroupArtifact(t, capturedAs("member-0", "member-0", "member-1"))
	mounter := stoppingMounter{stop: func() error { panic("the bundle mount panicked") }}
	req := RestoreRequest{
		BasePath: base, ContentUID: "content", ArtifactContainerName: "main", ContainerID: "placeholder",
		TargetPodIP: "10.0.0.1", WithoutProxy: true, SkipCompatCheck: true, Group: member,
	}

	require.PanicsWithValue(t, "the bundle mount panicked", func() {
		_, _ = Restore(groupTestContext(t), &restoreFakeRuntime{}, testr.New(t), req, mounter)
	})
	require.ErrorIs(t, (<-peerEnded).err, &session.AbortError{Reason: session.ReasonMemberFailed})
}

// joinMembers starts a coordinator with the session of one attempt of two
// members, member-0 and member-1, and joins both.
func joinMembers(t *testing.T) (*group.Member, *group.Member) {
	t.Helper()
	srv := coordinatortest.Start(t)
	spec := server.Spec{UID: k8stypes.UID("attempt-1"), Deadline: time.Now().Add(time.Hour),
		Members: []session.Member{{ID: "member-0", Rank: 0}, {ID: "member-1", Rank: 1}}}
	require.NoError(t, srv.Ensure(groupTestContext(t), spec))
	members := make([]*group.Member, 2)
	for rank := range members {
		m, err := group.Join(groupTestContext(t), srv.Config(spec.UID, fmt.Sprintf("member-%d", rank)))
		require.NoError(t, err)
		// Closes the connection, if the test has not.
		t.Cleanup(func() { _ = m.Fail(context.Background()) })
		members[rank] = m
	}
	return members[0], members[1]
}

// peerOutcome is how a peer's part ended: whether it passed the admitted
// barrier, and the error it stopped with.
type peerOutcome struct {
	committed bool
	err       error
}

// runPeer runs a peer's admission with own, then waits at the admitted barrier
// and at next, as a peer whose own steps all succeed would, and closes its
// connection once the session ends.
func runPeer(t *testing.T, peer *group.Member, own group.Admission, next string) <-chan peerOutcome {
	t.Helper()
	ctx := groupTestContext(t)
	ended := make(chan peerOutcome, 1)
	go func() {
		outcome := peerOutcome{err: peer.Admit(ctx, own)}
		if outcome.err == nil {
			outcome.err = peer.Barrier(ctx, group.Admitted)
		}
		if outcome.err == nil {
			outcome.committed = true
			outcome.err = peer.Barrier(ctx, next)
		}
		_ = peer.Fail(ctx)
		ended <- outcome
	}()
	return ended
}

// peerDelay is how long holdPeer holds a late peer back, which gives a member
// that does not wait for it the time to go on without it.
const peerDelay = 50 * time.Millisecond

// runLatePeer runs a peer as runPeer does, but holds it after the admitted
// barrier until letGo is closed. The peer then arrives at next or, if fails
// is set, fails the attempt instead, as a peer whose own step before next
// failed would. It records which on steps, before it does it.
func runLatePeer(t *testing.T, peer *group.Member, own group.Admission, next string, fails bool,
	letGo <-chan struct{}, steps chan<- string,
) <-chan peerOutcome {
	t.Helper()
	ctx := groupTestContext(t)
	ended := make(chan peerOutcome, 1)
	go func() {
		outcome := peerOutcome{err: peer.Admit(ctx, own)}
		if outcome.err == nil {
			outcome.err = peer.Barrier(ctx, group.Admitted)
		}
		if outcome.err == nil {
			outcome.committed = true
			select {
			case <-letGo:
				if fails {
					steps <- "the peer fails"
				} else {
					steps <- "the peer arrives at " + next
					outcome.err = peer.Barrier(ctx, next)
				}
			case <-ctx.Done():
				outcome.err = ctx.Err()
			}
		}
		_ = peer.Fail(ctx)
		ended <- outcome
	}()
	return ended
}

// holdPeer waits until the member has reached its last step before a barrier,
// then holds the late peer back for peerDelay before it closes letGo. It fails
// the test if the member returns before then.
func holdPeer(t *testing.T, reached, returned <-chan struct{}, letGo chan<- struct{}) {
	t.Helper()
	select {
	case <-reached:
	case <-returned:
		t.Fatal("the member returned before it reached the barrier")
	}
	select {
	case <-time.After(peerDelay):
	case <-returned:
		t.Fatal("the member returned while its peer was held back")
	}
	close(letGo)
}

// received closes steps, once nothing sends on it any more, and returns what
// it holds.
func received(steps chan string) []string {
	close(steps)
	var all []string
	for step := range steps {
		all = append(all, step)
	}
	return all
}

// serveCheckpointStaging answers the PageBroker requests of a capture or a
// staged restore until the test ends: its preparation with staging, its
// commit, and its abort.
func serveCheckpointStaging(t *testing.T, staging string) string {
	t.Helper()
	listener := listenPageBroker(t)
	served := make(chan error, 1)
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				served <- nil
				return
			}
			request, err := readPageBrokerTestRequest(connection)
			if err == nil {
				response := &pagebroker.Response{}
				switch {
				case request.GetPrepareStagedCheckpoint() != nil:
					response.Result = &pagebroker.Response_StagedCheckpointDirectory{
						StagedCheckpointDirectory: &pagebroker.StagedCheckpointDirectory{ImageDirectory: &staging},
					}
				case request.GetStagedRestore() != nil:
					response.Result = &pagebroker.Response_StagedRestoreDirectory{
						StagedRestoreDirectory: &pagebroker.StagedRestoreDirectory{ImageDirectory: &staging},
					}
				case request.GetCommit() != nil:
					response.Result = &pagebroker.Response_CommitComplete{CommitComplete: &pagebroker.CommitComplete{}}
				default:
					response.Result = &pagebroker.Response_AbortComplete{AbortComplete: &pagebroker.AbortComplete{}}
				}
				err = replyPageBrokerTest(connection, request, response)
			}
			_ = connection.Close()
			if err != nil {
				served <- err
				return
			}
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		waitPageBrokerTest(t, served)
	})
	return listener.Addr().String()
}

// inspectNothing inspects a source without CUDA processes, and without
// anything else a capture's later steps would read.
func inspectNothing(t *testing.T) func(context.Context, snapshotruntime.Runtime, logr.Logger, CheckpointRequest) (*types.CheckpointContainerSnapshot, time.Duration, error) {
	return func(context.Context, snapshotruntime.Runtime, logr.Logger, CheckpointRequest) (*types.CheckpointContainerSnapshot, time.Duration, error) {
		return &types.CheckpointContainerSnapshot{PID: -1, RootFS: t.TempDir()}, 0, nil
	}
}

// inspectCUDA inspects a source with one CUDA process, and without anything
// else a capture's later steps would read.
func inspectCUDA(t *testing.T) func(context.Context, snapshotruntime.Runtime, logr.Logger, CheckpointRequest) (*types.CheckpointContainerSnapshot, time.Duration, error) {
	return func(context.Context, snapshotruntime.Runtime, logr.Logger, CheckpointRequest) (*types.CheckpointContainerSnapshot, time.Duration, error) {
		return &types.CheckpointContainerSnapshot{PID: -1, RootFS: t.TempDir(), CUDAHostPIDs: []int{1001}, CUDANSPIDs: []int{1}}, 0, nil
	}
}

// stubCuInterpose has every CUDA process of the source load cuInterpose's
// core, so its coordinator runs, and replaces that coordinator's inspection,
// in the group's admission, with inspect.
func stubCuInterpose(t *testing.T, inspect func(cuda.CuInterposeTarget) error) {
	t.Helper()
	originalLibraries, originalCoordinator := inspectCuInterposeFunc, inspectWithRelayFunc
	t.Cleanup(func() { inspectCuInterposeFunc, inspectWithRelayFunc = originalLibraries, originalCoordinator })
	inspectCuInterposeFunc = func(_ context.Context, state *types.CheckpointContainerSnapshot, _, _ bool) (*types.CuInterposeManifest, error) {
		manifest := testCuInterposeIdentity()
		manifest.PIDs = state.CUDANSPIDs
		return manifest, nil
	}
	inspectWithRelayFunc = func(_ context.Context, target cuda.CuInterposeTarget) error { return inspect(target) }
}

// insertThroughRelay inserts value under key in the session's cuinterpose
// namespace, through relay's session socket, as the relay's coordinator would.
func insertThroughRelay(relay *cuda.CuInterposeRelay, key string, value []byte) error {
	body, err := msgpack.Marshal(map[string]any{"op": "insert", "id": 1, "key": key, "value": value})
	if err != nil {
		return err
	}
	if _, err := relay.Session.Write(append(binary.LittleEndian.AppendUint32(nil, uint32(len(body))), body...)); err != nil {
		return err
	}
	var header [4]byte
	if _, err := io.ReadFull(relay.Session, header[:]); err != nil {
		return err
	}
	body = make([]byte, binary.LittleEndian.Uint32(header[:]))
	if _, err := io.ReadFull(relay.Session, body); err != nil {
		return err
	}
	var answer struct {
		OK   bool   `msgpack:"ok"`
		Code string `msgpack:"code"`
	}
	if err := msgpack.Unmarshal(body, &answer); err != nil {
		return err
	}
	if !answer.OK {
		return fmt.Errorf("insert %s: %s", key, answer.Code)
	}
	return nil
}

// stubCapture replaces the GPU checkpoint and the dump, the first steps after
// a capture's admission, with capture, which receives the manifest.
func stubCapture(t *testing.T, capture func(*types.CheckpointManifest) (*checkpointPhaseTimings, error)) {
	t.Helper()
	original := captureCheckpointFunc
	t.Cleanup(func() { captureCheckpointFunc = original })
	captureCheckpointFunc = func(_ context.Context, _ *criurpc.CriuOpts, _ *types.CRIUSettings, data *types.CheckpointManifest,
		_ *types.CheckpointContainerSnapshot, _, _ string, _ logr.Logger, _ *pagebroker.CustomStorageExecution,
		_ int, _ func(error) (bool, string), _ func(int, error), _ *group.Member,
	) (*checkpointPhaseTimings, error) {
		return capture(data)
	}
}

// writeGroupArtifact writes the artifact of content "content" and container
// "main" under a new base path, which it returns, as a member of the group
// capture captured.
func writeGroupArtifact(t *testing.T, captured *types.PodGroupManifest) string {
	t.Helper()
	base := t.TempDir()
	directory, err := nsmount.ResolveArtifactPath(base, "content", "main")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(directory, 0o700))
	manifest := types.NewCheckpointManifest("content", "main", types.CRIUDumpManifest{},
		types.SourcePodManifest{}, types.OverlayManifest{}, types.HostManifest{})
	manifest.PodGroup = captured
	require.NoError(t, types.WriteManifest(directory, manifest))
	return base
}

// stoppingMounter stops a restore at its first mount, the first step after
// admission on a node without PageBroker's GPU path, with what stop returns.
type stoppingMounter struct{ stop func() error }

func (m stoppingMounter) MountBundle(context.Context, int) (nsmount.MountPoint, error) {
	return nil, m.stop()
}

func (stoppingMounter) MountCuInterpose(context.Context, nsmount.MountPoint) (nsmount.MountPoint, error) {
	return nil, errors.New("not reached")
}

func (stoppingMounter) MountArtifact(context.Context, nsmount.MountPoint, string) (nsmount.MountPoint, error) {
	return nil, errors.New("not reached")
}

func (stoppingMounter) MountPageBroker(context.Context, nsmount.MountPoint, string) (nsmount.MountPoint, error) {
	return nil, errors.New("not reached")
}

func groupTestContext(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// capturedAs returns the pod group section of member's artifact of the capture
// "capture-1", whose members have ids, by rank.
func capturedAs(member string, ids ...string) *types.PodGroupManifest {
	captured := &types.PodGroupManifest{Version: types.PodGroupManifestVersion, CaptureUID: "capture-1", MemberID: member}
	for rank, id := range ids {
		captured.Members = append(captured.Members, types.PodGroupManifestMember{
			ID: id, PodUID: "pod-" + id, Containers: []string{"main"}, PodIP: netip.AddrFrom4([4]byte{10, 0, 0, byte(rank + 1)}),
		})
	}
	return captured
}
