// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"sync"
	"testing"
	"time"

	criurpc "github.com/checkpoint-restore/go-criu/v8/rpc"
	"github.com/go-logr/logr"
	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	"github.com/ai-dynamo/snapshot/agent/internal/group"
	"github.com/ai-dynamo/snapshot/agent/internal/nsmount"
	"github.com/ai-dynamo/snapshot/agent/internal/pagebroker"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	agenttypes "github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/ai-dynamo/snapshot/coordinator/coordinatortest"
	"github.com/ai-dynamo/snapshot/coordinator/server"
	"github.com/ai-dynamo/snapshot/coordinator/session"
)

// steps records the order in which a test's steps ran, across goroutines.
type steps struct {
	mu   sync.Mutex
	seen []string
}

func (s *steps) add(step string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, step)
}

func (s *steps) list() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

// joinPair joins the two members of one attempt, member-0 and member-1, to a
// coordinator started for the test.
func joinPair(t *testing.T) (*group.Member, *group.Member) {
	t.Helper()
	srv := coordinatortest.Start(t)
	spec := server.Spec{UID: types.UID("attempt-1"), Deadline: time.Now().Add(time.Hour),
		Members: []session.Member{{ID: "member-0", Rank: 0}, {ID: "member-1", Rank: 1}}}
	ctx := withinTest(t)
	require.NoError(t, srv.Ensure(ctx, spec))
	members := make([]*group.Member, 2)
	for rank := range members {
		m, err := group.Join(ctx, srv.Config(spec.UID, fmt.Sprintf("member-%d", rank)))
		require.NoError(t, err)
		members[rank] = m
	}
	t.Cleanup(func() { _ = members[1].Fail(context.Background()) })
	return members[0], members[1]
}

// admitPeer runs the peer's admission, for the capture captureID, and then
// waits at the next barrier, as a peer that has started its own steps would,
// reporting how that wait ended.
func admitPeer(t *testing.T, peer *group.Member, captureID string) <-chan error {
	t.Helper()
	ended := make(chan error, 1)
	go func() {
		ctx := withinTest(t)
		if err := peer.Admit(ctx, group.Admission{PodIP: netip.MustParseAddr("10.0.0.2"), CaptureID: captureID}); err != nil {
			ended <- err
			return
		}
		if err := peer.Barrier(ctx, group.Admitted); err != nil {
			ended <- err
			return
		}
		ended <- peer.Barrier(ctx, group.NetFreeze)
	}()
	return ended
}

func withinTest(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestCheckpointRecordsTheCommitBeforeTheFirstIrreversibleStep(t *testing.T) {
	for _, tc := range []struct {
		name      string
		recordErr error
		wantSteps []string
		wantStep  Step
	}{
		{
			name:      "the record is written",
			wantSteps: []string{"record", "checkpoint GPU and dump"},
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
			t.Chdir(t.TempDir())
			member, peer := joinPair(t)
			peerEnded := admitPeer(t, peer, "")

			listener := listenPageBroker(t)
			imageDirectory := t.TempDir()
			served := make(chan error, 1)
			go func() {
				for {
					connection, err := listener.Accept()
					if err != nil {
						served <- nil
						return
					}
					request, err := readPageBrokerTestRequest(connection)
					if err != nil {
						_ = connection.Close()
						served <- err
						return
					}
					response := &pagebroker.Response{}
					if request.GetPrepareStagedCheckpoint() != nil {
						response.Result = &pagebroker.Response_StagedCheckpointDirectory{
							StagedCheckpointDirectory: &pagebroker.StagedCheckpointDirectory{ImageDirectory: &imageDirectory},
						}
					} else {
						response.Result = &pagebroker.Response_AbortComplete{AbortComplete: &pagebroker.AbortComplete{}}
					}
					err = replyPageBrokerTest(connection, request, response)
					_ = connection.Close()
					if err != nil {
						served <- err
						return
					}
				}
			}()

			var seen steps
			originalCapture := captureCheckpointFunc
			t.Cleanup(func() { captureCheckpointFunc = originalCapture })
			captureCheckpointFunc = func(context.Context, *criurpc.CriuOpts, *agenttypes.CRIUSettings, *agenttypes.CheckpointManifest,
				*agenttypes.CheckpointContainerSnapshot, string, string, logr.Logger, *pagebroker.CustomStorageExecution,
				int, func(error) (bool, string), func(int, error), *group.Member,
			) (*checkpointPhaseTimings, error) {
				seen.add("checkpoint GPU and dump")
				return nil, failStep(StepDumpCRIU, errors.New("stop after the first irreversible step"))
			}

			cfg := &agenttypes.AgentConfig{
				Storage:    agenttypes.StorageSpec{BasePath: t.TempDir()},
				PageBroker: agenttypes.PageBrokerSpec{ControlSocketPath: listener.Addr().String()},
			}
			req := CheckpointRequest{
				ContentUID: "content-uid", ContainerName: "main", PodIP: "10.0.0.1", Group: member,
				Committed: func(context.Context) error {
					seen.add("record")
					return tc.recordErr
				},
			}
			_, err := checkpoint(withinTest(t), nil, testr.New(t), req, cfg,
				func(context.Context, snapshotruntime.Runtime, logr.Logger, CheckpointRequest) (*agenttypes.CheckpointContainerSnapshot, time.Duration, error) {
					return &agenttypes.CheckpointContainerSnapshot{PID: -1, RootFS: t.TempDir()}, 0, nil
				})
			_ = listener.Close()
			waitPageBrokerTest(t, served)

			require.Error(t, err)
			assert.Equal(t, tc.wantStep, StepOf(err))
			assert.Equal(t, tc.wantSteps, seen.list())
			// The barrier had passed, so the attempt fails as committed.
			require.ErrorIs(t, <-peerEnded, &session.AbortError{Reason: session.ReasonMemberFailed})
		})
	}
}

// failingBundleMounter fails the first mount of a restore, the first step
// after admission on a node without PageBroker's GPU path, and records it.
type failingBundleMounter struct{ seen *steps }

func (m failingBundleMounter) MountBundle(context.Context, int) (nsmount.MountPoint, error) {
	m.seen.add("mount the bundle")
	return nil, errors.New("stop at the first step after admission")
}

func (failingBundleMounter) MountCuInterpose(context.Context, nsmount.MountPoint) (nsmount.MountPoint, error) {
	return nil, errors.New("not reached")
}

func (failingBundleMounter) MountArtifact(context.Context, nsmount.MountPoint, string) (nsmount.MountPoint, error) {
	return nil, errors.New("not reached")
}

func (failingBundleMounter) MountPageBroker(context.Context, nsmount.MountPoint, string) (nsmount.MountPoint, error) {
	return nil, errors.New("not reached")
}

func TestRestoreRecordsTheCommitBeforeTheFirstIrreversibleStep(t *testing.T) {
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
			member, peer := joinPair(t)
			peerEnded := admitPeer(t, peer, "capture-1")

			base := t.TempDir()
			directory, err := nsmount.ResolveArtifactPath(base, "content", "main")
			require.NoError(t, err)
			require.NoError(t, os.MkdirAll(directory, 0o700))
			manifest := agenttypes.NewCheckpointManifest("content", "main", agenttypes.CRIUDumpManifest{},
				agenttypes.SourcePodManifest{}, agenttypes.OverlayManifest{}, agenttypes.HostManifest{})
			manifest.Group = &agenttypes.GroupManifest{CaptureID: "capture-1", Member: "member-0", Rank: 0,
				PodIPs: []netip.Addr{netip.MustParseAddr("10.1.0.1"), netip.MustParseAddr("10.1.0.2")}}
			require.NoError(t, agenttypes.WriteManifest(directory, manifest))

			var seen steps
			req := RestoreRequest{
				BasePath: base, ContentUID: "content", ArtifactContainerName: "main", ContainerID: "placeholder",
				TargetPodIP: "10.0.0.1", WithoutProxy: true, SkipCompatCheck: true, Group: member,
				Committed: func(context.Context) error {
					seen.add("record")
					return tc.recordErr
				},
			}
			_, err = Restore(withinTest(t), &restoreFakeRuntime{}, testr.New(t), req, failingBundleMounter{seen: &seen})

			require.Error(t, err)
			assert.Equal(t, tc.wantStep, StepOf(err))
			assert.Equal(t, tc.wantSteps, seen.list())
			require.ErrorIs(t, <-peerEnded, &session.AbortError{Reason: session.ReasonMemberFailed})
		})
	}
}
