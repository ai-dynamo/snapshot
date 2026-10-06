// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/ai-dynamo/snapshot/agent/internal/cuda"
	"github.com/ai-dynamo/snapshot/agent/internal/nsmount"
	"github.com/ai-dynamo/snapshot/agent/internal/pagebroker"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/ai-dynamo/snapshot/api/compat"
	"github.com/ai-dynamo/snapshot/api/podcontract"
)

// readFakeRequest reads one length-prefixed PageBroker request frame.
func readFakeRequest(connection net.Conn) (*pagebroker.Request, error) {
	var size uint32
	if err := binary.Read(connection, binary.BigEndian, &size); err != nil {
		return nil, err
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(connection, data); err != nil {
		return nil, err
	}
	request := new(pagebroker.Request)
	if err := proto.Unmarshal(data, request); err != nil {
		return nil, err
	}
	return request, nil
}

// writeFakeResponse writes one length-prefixed PageBroker response frame.
func writeFakeResponse(connection net.Conn, response *pagebroker.Response) error {
	data, err := proto.Marshal(response)
	if err != nil {
		return err
	}
	if err := binary.Write(connection, binary.BigEndian, uint32(len(data))); err != nil {
		return err
	}
	_, err = connection.Write(data)
	return err
}

type checkpointPathRuntime struct{}

func (checkpointPathRuntime) ResolveContainer(context.Context, string) (int, *specs.Spec, error) {
	return 0, nil, errors.New("stop after path preparation")
}

func (checkpointPathRuntime) ResolveContainerIDByPod(context.Context, string, string, string) (string, error) {
	return "", errors.New("not implemented")
}

func (checkpointPathRuntime) ResolveContainerByPod(context.Context, string, string, string) (int, *specs.Spec, error) {
	return 0, nil, errors.New("not implemented")
}

func (checkpointPathRuntime) ResolveContainerImageID(context.Context, string) (string, error) {
	return "", errors.New("not implemented")
}

func (checkpointPathRuntime) TerminateContainer(context.Context, string) error {
	return errors.New("not implemented")
}

func (checkpointPathRuntime) Close() error { return nil }

type checkpointImageRuntime struct {
	checkpointPathRuntime
}

func (checkpointImageRuntime) ResolveContainer(context.Context, string) (int, *specs.Spec, error) {
	return 1, &specs.Spec{}, nil
}

func (checkpointImageRuntime) ResolveContainerImageID(context.Context, string) (string, error) {
	return "", errors.New("runtime image unavailable")
}

func TestCheckpointDoesNotCreateArtifactsBeforeInspection(t *testing.T) {
	cfg := &types.AgentConfig{Storage: types.StorageSpec{BasePath: t.TempDir()}}
	finalDir, err := nsmount.ResolveArtifactPath(cfg.Storage.BasePath, "content-uid", "main")
	require.NoError(t, err)

	err = Checkpoint(context.Background(), checkpointPathRuntime{}, logr.Discard(), CheckpointRequest{
		ContentUID:    "content-uid",
		ContainerName: "main",
	}, cfg)
	require.ErrorContains(t, err, "stop after path preparation")
	assert.NoDirExists(t, filepath.Dir(finalDir))
	assert.NoDirExists(t, filepath.Join(cfg.Storage.BasePath, "artifacts", "content-uid", ".tmp"))
}

func TestInspectContainerToleratesUnreadableRuntimeImageID(t *testing.T) {
	var logged []string
	log := funcr.New(func(_, args string) { logged = append(logged, args) }, funcr.Options{})

	_, _, err := inspectContainer(
		context.Background(),
		checkpointImageRuntime{},
		log,
		CheckpointRequest{ContainerID: "container-id"},
	)

	// The fake runtime has no rootfs to offer, so inspection still fails - but
	// after the image ID rather than on it.
	require.ErrorContains(t, err, "failed to get rootfs")
	require.Len(t, logged, 1)
	assert.Contains(t, logged[0], "this checkpoint will not record it")
	assert.Contains(t, logged[0], "runtime image unavailable")
}

func TestConfigureCheckpointRecordsRuntimeImageID(t *testing.T) {
	checkpointDir := t.TempDir()
	_, _, err := configureCheckpoint(
		logr.Discard(),
		&types.CheckpointContainerSnapshot{
			PID:        42,
			ImageID:    "sha256:runtime-content",
			RootFS:     "/",
			NetNSInode: 7,
		},
		CheckpointRequest{
			ContentUID:    "content-uid",
			ContainerID:   "container-id",
			ContainerName: "main",
			Pod: compat.Environment{
				Image:   "registry.example/workload:latest",
				ImageID: "sha256:kubelet-alias",
			},
		},
		&types.AgentConfig{},
		checkpointDir,
		false,
	)
	require.NoError(t, err)

	manifest, err := types.ReadManifest(checkpointDir)
	require.NoError(t, err)
	assert.Equal(t, "registry.example/workload:latest", manifest.K8s.Image)
	assert.Equal(t, "sha256:runtime-content", manifest.K8s.ImageID)
}

func TestGPUCheckpointCannotFallBackWhenExecutionIsMissing(t *testing.T) {
	_, err := captureCheckpoint(context.Background(), nil, &types.CRIUSettings{}, &types.CheckpointManifest{CUDA: types.CUDAManifest{CustomStorage: true}},
		&types.CheckpointContainerSnapshot{CUDAHostPIDs: []int{12}, CUDANSPIDs: []int{12}},
		t.TempDir(), "", logr.Discard(), nil)
	require.ErrorContains(t, err, "missing PageBroker CustomStorage execution context")
}

const pageBrokerTestTimeout = 5 * time.Second

func listenPageBroker(t *testing.T) *net.UnixListener {
	t.Helper()
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(t.TempDir(), "broker.sock"), Net: "unix"})
	require.NoError(t, err)
	require.NoError(t, listener.SetDeadline(time.Now().Add(pageBrokerTestTimeout)))
	t.Cleanup(func() { _ = listener.Close() })
	return listener
}

func readPageBrokerTestRequest(connection net.Conn) (*pagebroker.Request, error) {
	if err := connection.SetDeadline(time.Now().Add(pageBrokerTestTimeout)); err != nil {
		return nil, err
	}
	var size uint32
	if err := binary.Read(connection, binary.BigEndian, &size); err != nil {
		return nil, err
	}
	message := make([]byte, size)
	if _, err := io.ReadFull(connection, message); err != nil {
		return nil, err
	}
	request := new(pagebroker.Request)
	if err := proto.Unmarshal(message, request); err != nil {
		return nil, err
	}
	return request, nil
}

func replyPageBrokerTest(connection net.Conn, request *pagebroker.Request, response *pagebroker.Response) error {
	response.RequestId = request.RequestId
	response.TransactionId = request.TransactionId
	message, err := proto.Marshal(response)
	if err != nil {
		return err
	}
	if err := binary.Write(connection, binary.BigEndian, uint32(len(message))); err != nil {
		return err
	}
	_, err = connection.Write(message)
	return err
}

func waitPageBrokerTest(t *testing.T, result <-chan error) {
	t.Helper()
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(pageBrokerTestTimeout):
		t.Fatal("PageBroker test server did not finish")
	}
}

func TestCheckpointPreparationFailure(t *testing.T) {
	// Supply only inspection data. Preparation and deferred cleanup use the real
	// client with forced failures. No runtime, CRIU, or GPU execution is tested.
	workingDirectory := t.TempDir()
	t.Chdir(workingDirectory)
	for _, format := range []string{"staged", "custom-storage"} {
		for _, outcome := range []struct{ preparation, cleanup string }{
			{"rejected", "confirmed"},
			{"rejected", "not-found"},
			{"rejected", "lost-reply"},
			{"lost-reply", "confirmed"},
			{"lost-reply", "not-found"},
			{"lost-reply", "lost-reply"},
		} {
			preparation, cleanup := outcome.preparation, outcome.cleanup
			t.Run(format+"/"+preparation+"/"+cleanup, func(t *testing.T) {
				listener := listenPageBroker(t)
				cfg := &types.AgentConfig{
					Storage:                types.StorageSpec{BasePath: t.TempDir()},
					PageBroker:             types.PageBrokerSpec{ControlSocketPath: listener.Addr().String()},
					CustomStorageAvailable: format == "custom-storage",
				}
				req := CheckpointRequest{ContentUID: "content-uid", ContainerName: "main"}
				destination, err := nsmount.ResolveArtifactPath(cfg.Storage.BasePath, req.ContentUID, req.ContainerName)
				require.NoError(t, err)
				ctx, cancel := context.WithTimeout(context.Background(), pageBrokerTestTimeout)
				defer cancel()
				transactionID := ""
				exchange := func(phase string) error {
					connection, err := listener.Accept()
					if err != nil {
						return err
					}
					defer connection.Close()
					request, err := readPageBrokerTestRequest(connection)
					if err != nil {
						return err
					}
					response := &pagebroker.Response{}
					switch phase {
					case "prepare":
						received := request.GetPrepareStagedCheckpoint().GetDestination().GetFilesystem().GetDirectory()
						if format == "custom-storage" {
							received = request.GetPrepareDirectCheckpoint().GetDestination().GetFilesystem().GetDirectory()
						}
						transactionID = request.GetTransactionId()
						if received != destination || transactionID == "" {
							return fmt.Errorf("unexpected checkpoint preparation: %v", request)
						}
						if preparation == "lost-reply" {
							return nil
						}
						response.Result = &pagebroker.Response_Failure{Failure: &pagebroker.Failure{
							Code: pagebroker.Failure_STORAGE_ERROR.Enum(), Message: proto.String("storage unavailable"),
						}}
					case "abort":
						if request.GetAbort() == nil || request.GetTransactionId() != transactionID {
							return fmt.Errorf("expected Abort for %q, got %v", transactionID, request)
						}
						if cleanup == "lost-reply" {
							return nil
						}
						response.Result = &pagebroker.Response_AbortComplete{AbortComplete: &pagebroker.AbortComplete{}}
						if cleanup == "not-found" {
							response.Result = &pagebroker.Response_Failure{Failure: &pagebroker.Failure{Code: pagebroker.Failure_TRANSACTION_NOT_FOUND.Enum()}}
						}
					}
					return replyPageBrokerTest(connection, request, response)
				}
				server := make(chan error, 1)
				go func() {
					defer listener.Close()
					for _, phase := range []string{"prepare", "abort"} {
						if err := exchange(phase); err != nil {
							server <- err
							return
						}
					}
					server <- nil
				}()
				_, err = checkpoint(ctx, nil, logr.Discard(), req, cfg,
					func(context.Context, snapshotruntime.Runtime, logr.Logger, CheckpointRequest) (*types.CheckpointContainerSnapshot, time.Duration, error) {
						state := &types.CheckpointContainerSnapshot{PID: -1, RootFS: workingDirectory}
						if format == "custom-storage" {
							state.CUDAHostPIDs = []int{-1}
							state.CUDANSPIDs = []int{12}
							state.GPUs = compat.GPUInfo{Devices: []compat.GPUDevice{{UUID: "GPU-source"}}}
						}
						return state, 0, nil
					})
				waitPageBrokerTest(t, server)
				require.ErrorContains(t, err, "prepare PageBroker checkpoint")
				if preparation == "rejected" {
					assert.ErrorContains(t, err, "STORAGE_ERROR: storage unavailable")
				} else {
					assert.ErrorContains(t, err, "prepare PageBroker checkpoint: read PageBroker response: EOF")
					assert.ErrorIs(t, err, io.EOF)
				}
				if cleanup == "confirmed" {
					assert.NotContains(t, err.Error(), "abort PageBroker checkpoint")
				} else {
					assert.ErrorContains(t, err, "abort PageBroker checkpoint")
					if cleanup == "not-found" {
						assert.ErrorContains(t, err, "TRANSACTION_NOT_FOUND")
					} else {
						assert.ErrorContains(t, err, fmt.Sprintf("abort PageBroker checkpoint %q: read PageBroker response: EOF", transactionID))
						assert.ErrorIs(t, err, io.EOF)
					}
				}
				assert.False(t, CheckpointNeedsSourceKill(err))
				for _, path := range []string{workingDirectory, cfg.Storage.BasePath} {
					entries, err := os.ReadDir(path)
					require.NoError(t, err)
					assert.Empty(t, entries, "preparation failure wrote checkpoint files in %s", path)
				}
			})
		}
	}
}

func TestConfigureCheckpointPreservesNativeAndCoordinatorParticipants(t *testing.T) {
	for _, coordinatorPIDs := range [][]int{{623}, {}} {
		t.Run(fmt.Sprint(coordinatorPIDs), func(t *testing.T) {
			directory := t.TempDir()
			identity := testCuInterposeIdentity()
			identity.PIDs = coordinatorPIDs
			state := &types.CheckpointContainerSnapshot{
				PID: 1001, RootFS: "/", NetNSInode: 7,
				CUDAHostPIDs: []int{1001, 1623}, CUDANSPIDs: []int{1, 623}, CuInterpose: identity,
			}
			_, _, err := configureCheckpoint(logr.Discard(), state,
				CheckpointRequest{ContentUID: "content", ContainerName: "main"}, &types.AgentConfig{}, directory, false)
			require.NoError(t, err)
			manifest, err := types.ReadManifest(directory)
			require.NoError(t, err)
			require.Equal(t, []int{1, 623}, manifest.CUDA.PIDs)
			require.Equal(t, identity, manifest.CuInterpose)
		})
	}
}

// Preparation changes shared state, so it runs only for coordinator participants and
// inside captureCheckpoint, whose errors Checkpoint treats as requiring source termination.
func TestCapturePreparesOnlyCoordinatorParticipants(t *testing.T) {
	for _, tc := range []struct {
		name            string
		coordinatorPIDs []int
		wantError       string
	}{
		// PID -1 makes preparation fail before any coordinator starts.
		{name: "coordinator", coordinatorPIDs: []int{1}, wantError: "prepare cuinterpose"},
		// The cancelled context stops at the native CUDA boundary without a helper.
		{name: "frontend only", coordinatorPIDs: []int{}, wantError: "CUDA checkpoint failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			identity := testCuInterposeIdentity()
			identity.PIDs = tc.coordinatorPIDs
			ctx, cancel := context.WithCancel(context.Background())
			if len(tc.coordinatorPIDs) == 0 {
				cancel()
			}
			defer cancel()
			_, err := captureCheckpoint(ctx, nil, &types.CRIUSettings{},
				&types.CheckpointManifest{CuInterpose: identity},
				&types.CheckpointContainerSnapshot{PID: -1, CUDAHostPIDs: []int{1}, CUDANSPIDs: []int{1}},
				t.TempDir(), "", logr.Discard(), nil)
			require.ErrorContains(t, err, tc.wantError)
		})
	}
}

func TestConfigureCheckpointRecordsCuInterposeSocketExclusion(t *testing.T) {
	cfg := &types.AgentConfig{Overlay: types.OverlaySettings{Exclusions: []string{"/var/cache"}}}
	_, data, err := configureCheckpoint(logr.Discard(), &types.CheckpointContainerSnapshot{PID: 42, RootFS: "/", NetNSInode: 7},
		CheckpointRequest{ContentUID: "content", ContainerName: "main"}, cfg, t.TempDir(), false)
	require.NoError(t, err)
	require.Equal(t, []string{"/var/cache", cuda.CuInterposeSocketPattern}, data.Overlay.Exclusions.Exclusions)
}

func TestCheckCuInterposeMountReadOnly(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mounts    []types.MountInfo
		wantError bool
	}{
		{name: "read-only", mounts: []types.MountInfo{{MountPoint: podcontract.CuInterposeMountPath, ReadOnly: true}}},
		{name: "writable", mounts: []types.MountInfo{{MountPoint: podcontract.CuInterposeMountPath}}, wantError: true},
		{name: "writable nested", mounts: []types.MountInfo{{MountPoint: podcontract.CuInterposeMountPath + "/libcuinterpose.so"}}, wantError: true},
		{name: "delivered in image", mounts: []types.MountInfo{{MountPoint: "/models"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkCuInterposeMountReadOnly(tc.mounts)
			if tc.wantError {
				require.ErrorContains(t, err, "must be read-only")
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestCheckpointPageBrokerPrepareFailureDoesNotMutate(t *testing.T) {
	cfg := &types.AgentConfig{
		Storage:    types.StorageSpec{BasePath: t.TempDir()},
		PageBroker: types.PageBrokerSpec{ControlSocketPath: t.TempDir() + "/pagebroker.sock"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	_, err := checkpoint(ctx, checkpointPathRuntime{}, logr.Discard(), CheckpointRequest{
		ContentUID:    "content-uid",
		ContainerName: "main",
	}, cfg, func(context.Context, snapshotruntime.Runtime, logr.Logger, CheckpointRequest) (*types.CheckpointContainerSnapshot, time.Duration, error) {
		return &types.CheckpointContainerSnapshot{}, 0, nil
	})
	require.ErrorContains(t, err, "prepare PageBroker checkpoint")
	assert.NotContains(t, err.Error(), "abort PageBroker checkpoint")
	assert.Less(t, time.Since(start), 3*time.Second)
	assert.False(t, CheckpointNeedsSourceKill(err))
	assert.NoDirExists(t, filepath.Join(cfg.Storage.BasePath, "artifacts"))
}

func TestCheckpointBoundRejectsMalformedStoreIDBeforeDialing(t *testing.T) {
	cfg := &types.AgentConfig{
		Storage:    types.StorageSpec{BasePath: t.TempDir()},
		PageBroker: types.PageBrokerSpec{ControlSocketPath: filepath.Join(t.TempDir(), "pagebroker.sock")},
	}

	_, err := Checkpoint(context.Background(), checkpointPathRuntime{}, logr.Discard(), CheckpointRequest{
		ContentUID:    "content-uid",
		ContainerName: "main",
		StoreID:       "not-a-valid-store-id",
	}, cfg)
	require.ErrorContains(t, err, "validate storage binding")
	assert.NotContains(t, err.Error(), "prepare PageBroker checkpoint")
}

func TestCheckpointBoundSendsArtifactTarget(t *testing.T) {
	storeID := "store-v1-" + fmt.Sprintf("%064x", 1)
	target := &pagebroker.ArtifactTarget{
		StoreId:  storeID,
		Artifact: &pagebroker.ArtifactIdentity{ArtifactUid: "content-uid", ContainerName: "main"},
	}
	listener := listenPageBroker(t)
	server := make(chan error, 1)
	go func() {
		for i := 0; i < 2; i++ {
			connection, err := listener.Accept()
			if err != nil {
				server <- err
				return
			}
			request, err := readPageBrokerTestRequest(connection)
			if err != nil {
				connection.Close()
				server <- err
				return
			}
			response := &pagebroker.Response{}
			if i == 0 {
				prepare := request.GetPrepareStagedCheckpoint()
				if prepare == nil || !proto.Equal(prepare.GetTarget(), target) || prepare.GetDestination() != nil {
					connection.Close()
					server <- fmt.Errorf("wrong bound preparation: %v", request)
					return
				}
				response.Result = &pagebroker.Response_Failure{Failure: &pagebroker.Failure{
					Code: pagebroker.Failure_INVALID_REQUEST.Enum(), Message: proto.String("stop after target validation"),
				}}
			} else {
				if request.GetAbort() == nil {
					connection.Close()
					server <- fmt.Errorf("expected Abort: %v", request)
					return
				}
				response.Result = &pagebroker.Response_AbortComplete{AbortComplete: &pagebroker.AbortComplete{}}
			}
			err = replyPageBrokerTest(connection, request, response)
			connection.Close()
			if err != nil {
				server <- err
				return
			}
		}
		server <- nil
	}()
	cfg := &types.AgentConfig{PageBroker: types.PageBrokerSpec{ControlSocketPath: listener.Addr().String()}}
	ctx, cancel := context.WithTimeout(context.Background(), pageBrokerTestTimeout)
	defer cancel()
	// Inspection precedes preparation; stop at the broker before CUDA or CRIU executes.
	_, err := checkpoint(ctx, nil, logr.Discard(), CheckpointRequest{
		ContentUID: "content-uid", ContainerName: "main", StoreID: storeID,
	}, cfg, func(context.Context, snapshotruntime.Runtime, logr.Logger, CheckpointRequest) (*types.CheckpointContainerSnapshot, time.Duration, error) {
		return &types.CheckpointContainerSnapshot{}, 0, nil
	})
	require.ErrorContains(t, err, "stop after target validation")
	waitPageBrokerTest(t, server)
}

func TestCheckpointNeedsSourceKill(t *testing.T) {
	assert.True(t, CheckpointNeedsSourceKill(checkpointNeedsSourceKill(errors.New("capture failed"))))
	assert.False(t, CheckpointNeedsSourceKill(errors.New("prepare failed")))
	assert.False(t, CheckpointNeedsSourceKill(fmt.Errorf("commit PageBroker checkpoint: %w", errors.New("failed"))))
}
