// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cuda

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	podresourcesv1 "k8s.io/kubelet/pkg/apis/podresources/v1"

	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

func TestCUDAOperationSlotHonorsCancellation(t *testing.T) {
	if err := acquireCUDAOperation(context.Background()); err != nil {
		t.Fatalf("acquireCUDAOperation() = %v", err)
	}
	defer releaseCUDAOperation()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := acquireCUDAOperation(ctx); err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("acquireCUDAOperation(canceled) = %v, want context.Canceled", err)
	}

	_, err := LockAndCheckpointProcessTreeValidated(
		ctx, nil, "", "", "", nil, types.CUDATransferSettings{}, logr.Discard(),
	)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("LockAndCheckpointProcessTreeValidated(canceled) = %v, want context.Canceled", err)
	}
	if !FailedBeforeTargetMutation(err) {
		t.Fatalf("slot-acquisition failure = %v, want pre-mutation classification", err)
	}
}

func TestParseRestoreTIDProbeOutput(t *testing.T) {
	for _, test := range []struct {
		name    string
		output  string
		want    bool
		wantErr bool
	}{
		{name: "CUDA owner", output: "42\n", want: true},
		{name: "no CUDA context", output: "none\n", want: false},
		{name: "empty", output: "", wantErr: true},
		{name: "zero TID", output: "0\n", wantErr: true},
		{name: "diagnostic", output: "CUDA_ERROR_UNKNOWN\n", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseRestoreTIDProbeOutput([]byte(test.output))
			if (err != nil) != test.wantErr || got != test.want {
				t.Fatalf("parseRestoreTIDProbeOutput(%q) = %v, %v; want %v, error=%v", test.output, got, err, test.want, test.wantErr)
			}
		})
	}
}

func TestFailedBeforeTargetMutation(t *testing.T) {
	safe := &checkpointOperationError{
		err:                errDaemonUnavailable,
		targetMayBeMutated: false,
	}
	if !FailedBeforeTargetMutation(safe) {
		t.Fatal("unavailable helper before the first lock should be classified as pre-mutation")
	}
	unsafe := &checkpointOperationError{
		err:                errDaemonUnavailable,
		targetMayBeMutated: true,
	}
	if FailedBeforeTargetMutation(unsafe) || FailedBeforeTargetMutation(errors.New("unknown")) {
		t.Fatal("mutated or unclassified failures must not be classified as pre-mutation")
	}
}

func TestCheckpointRejectsShortBudgetBeforeMutation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	_, err := LockAndCheckpointProcessTreeValidated(
		ctx,
		[]snapshotruntime.ProcessDetails{{OutermostPID: 42, StartTimeTicks: 1, Cgroup: "0::/test\n"}},
		"", "", "", nil, types.CUDATransferSettings{}, logr.Discard(),
	)
	if err == nil || !FailedBeforeTargetMutation(err) {
		t.Fatalf("short-budget checkpoint error = %v, want pre-mutation classification", err)
	}
}

type helperActionRunnerFunc func(context.Context, helperAction, logr.Logger) error

func (f helperActionRunnerFunc) run(ctx context.Context, request helperAction, log logr.Logger) error {
	return f(ctx, request, log)
}

func TestIdentityValidatingRunnerRejectsMissingAndChangedIdentityBeforeDriver(t *testing.T) {
	for _, test := range []struct {
		name       string
		identities map[int]snapshotruntime.ProcessDetails
	}{
		{name: "missing", identities: map[int]snapshotruntime.ProcessDetails{}},
		{name: "changed", identities: map[int]snapshotruntime.ProcessDetails{42: {
			OutermostPID: 42, InnermostPID: 42, StartTimeTicks: 1, Cgroup: "0::/test\n",
		}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			called := false
			runner := identityValidatingRunner{
				runner: helperActionRunnerFunc(func(context.Context, helperAction, logr.Logger) error {
					called = true
					return nil
				}),
				procRoot:   t.TempDir(),
				identities: test.identities,
			}
			err := runner.run(context.Background(), helperAction{PID: 42, Action: actionLock}, logr.Discard())
			if !errors.Is(err, errProcessIdentityChangedBeforeCUDA) {
				t.Fatalf("identityValidatingRunner.run() error = %v, want identity-change sentinel", err)
			}
			if called {
				t.Fatal("identityValidatingRunner called the driver-facing runner after identity rejection")
			}
		})
	}
}

func TestCheckpointIdentityRaceBeforeFirstLockIsPreMutation(t *testing.T) {
	_, err := lockAndCheckpointProcessTree(
		context.Background(), []int{41}, nil, "", types.CUDAStorageModeLegacy, "", nil,
		types.CUDATransferSettings{},
		helperActionRunnerFunc(func(context.Context, helperAction, logr.Logger) error {
			return fmt.Errorf("%w: test race", errProcessIdentityChangedBeforeCUDA)
		}),
		logr.Discard(),
	)
	if err == nil || !FailedBeforeTargetMutation(err) {
		t.Fatalf("first-lock identity race = %v, want pre-mutation classification", err)
	}
}

func TestCheckpointIdentityRaceAfterEarlierLockIsMutating(t *testing.T) {
	calls := 0
	_, err := lockAndCheckpointProcessTree(
		context.Background(), []int{41, 42}, nil, "", types.CUDAStorageModeLegacy, "", nil,
		types.CUDATransferSettings{},
		helperActionRunnerFunc(func(context.Context, helperAction, logr.Logger) error {
			calls++
			if calls == 2 {
				return fmt.Errorf("%w: test race", errProcessIdentityChangedBeforeCUDA)
			}
			return nil
		}),
		logr.Discard(),
	)
	if err == nil || FailedBeforeTargetMutation(err) {
		t.Fatalf("second-lock identity race = %v, want possibly-mutating classification", err)
	}
}

func TestCheckpointRejectsInvalidIdentityBeforeMutation(t *testing.T) {
	_, err := LockAndCheckpointProcessTreeValidated(
		context.Background(),
		[]snapshotruntime.ProcessDetails{{OutermostPID: 42}},
		"", "", "", nil, types.CUDATransferSettings{}, logr.Discard(),
	)
	if err == nil || !FailedBeforeTargetMutation(err) {
		t.Fatalf("invalid-identity checkpoint error = %v, want pre-mutation classification", err)
	}
}

func TestCustomStorageProcessDirectoryUsesStableNamespacePID(t *testing.T) {
	got := customStorageProcessDir("/checkpoint", 42)
	if want := "/checkpoint/cuda-custom-storage/process-nspid-42"; got != want {
		t.Fatalf("customStorageProcessDir() = %q, want %q", got, want)
	}
}

func TestRestoreDerivesJobFilePathForEachTargetPID(t *testing.T) {
	var requests []helperAction
	runner := helperActionRunnerFunc(func(_ context.Context, request helperAction, _ logr.Logger) error {
		requests = append(requests, request)
		return nil
	})
	_, err := restoreAndUnlockProcessTree(
		context.Background(), []int{101, 202}, nil, "", types.CUDAStorageModeLegacy,
		"/checkpoint", "job-file-present", nil, types.CUDATransferSettings{}, runner, logr.Discard(),
	)
	if err != nil {
		t.Fatalf("restoreAndUnlockProcessTree() error = %v", err)
	}
	if len(requests) != 4 {
		t.Fatalf("request count = %d, want 4", len(requests))
	}
	for index, request := range requests {
		pid := []int{101, 202, 101, 202}[index]
		want, err := HostJobFilePath(pid)
		if err != nil {
			t.Fatal(err)
		}
		if request.PID != pid || request.JobFile != want {
			t.Fatalf("request[%d] = PID %d job %q, want PID %d job %q", index, request.PID, request.JobFile, pid, want)
		}
	}
}

func TestBuildDeviceMap(t *testing.T) {
	tests := []struct {
		name    string
		source  []string
		target  []string
		want    string
		wantErr bool
	}{
		{
			name:   "single GPU",
			source: []string{"GPU-aaa"},
			target: []string{"GPU-bbb"},
			want:   "GPU-aaa=GPU-bbb",
		},
		{
			name:   "single GPU identity returns no map",
			source: []string{"GPU-aaa"},
			target: []string{"GPU-aaa"},
			want:   "",
		},
		{
			name:   "multiple GPUs",
			source: []string{"GPU-aaa", "GPU-bbb"},
			target: []string{"GPU-ccc", "GPU-ddd"},
			want:   "GPU-aaa=GPU-ccc,GPU-bbb=GPU-ddd",
		},
		{
			name:   "multiple GPU identity returns no map",
			source: []string{"GPU-aaa", "GPU-bbb"},
			target: []string{"GPU-bbb", "GPU-aaa"},
			want:   "",
		},
		{
			name:    "mismatched lengths",
			source:  []string{"GPU-aaa", "GPU-bbb"},
			target:  []string{"GPU-ccc"},
			wantErr: true,
		},
		{
			name:    "both empty",
			source:  []string{},
			target:  []string{},
			wantErr: true,
		},
		{
			name:    "source empty target non-empty",
			source:  []string{},
			target:  []string{"GPU-aaa"},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := BuildDeviceMap(tc.source, tc.target, logr.Discard())
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

type testPodResourcesServer struct {
	podresourcesv1.UnimplementedPodResourcesListerServer
	resp *podresourcesv1.ListPodResourcesResponse
}

func (s *testPodResourcesServer) List(context.Context, *podresourcesv1.ListPodResourcesRequest) (*podresourcesv1.ListPodResourcesResponse, error) {
	return s.resp, nil
}

func (s *testPodResourcesServer) GetAllocatableResources(context.Context, *podresourcesv1.AllocatableResourcesRequest) (*podresourcesv1.AllocatableResourcesResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented in test")
}

func (s *testPodResourcesServer) Get(context.Context, *podresourcesv1.GetPodResourcesRequest) (*podresourcesv1.GetPodResourcesResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented in test")
}

func installTestPodResourcesServer(t *testing.T, resp *podresourcesv1.ListPodResourcesResponse) {
	socketDir := t.TempDir()
	socketPath := filepath.Join(socketDir, "kubelet.sock")

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix socket: %v", err)
	}

	server := grpc.NewServer()
	podresourcesv1.RegisterPodResourcesListerServer(server, &testPodResourcesServer{
		resp: resp,
	})

	go func() {
		if serveErr := server.Serve(listener); serveErr != nil {
			if errors.Is(serveErr, grpc.ErrServerStopped) || strings.Contains(serveErr.Error(), "use of closed network connection") {
				return
			}
			t.Errorf("serve test pod-resources gRPC server: %v", serveErr)
		}
	}()
	t.Cleanup(server.Stop)
	t.Cleanup(func() {
		_ = listener.Close()
	})

	previousSocketPath := podResourcesSocketPath
	podResourcesSocketPath = socketPath
	t.Cleanup(func() {
		podResourcesSocketPath = previousSocketPath
	})
}

func TestGetPodGPUUUIDs(t *testing.T) {
	installTestPodResourcesServer(t, &podresourcesv1.ListPodResourcesResponse{
		PodResources: []*podresourcesv1.PodResources{
			{
				Name:      "other-pod",
				Namespace: "default",
				Containers: []*podresourcesv1.ContainerResources{
					{
						Name: "main",
						Devices: []*podresourcesv1.ContainerDevices{
							{
								ResourceName: nvidiaGPUResource,
								DeviceIds:    []string{"GPU-ignore"},
							},
						},
					},
				},
			},
			{
				Name:      "test-pod",
				Namespace: "default",
				Containers: []*podresourcesv1.ContainerResources{
					{
						Name: "sidecar",
						Devices: []*podresourcesv1.ContainerDevices{
							{
								ResourceName: nvidiaGPUResource,
								DeviceIds:    []string{"GPU-sidecar"},
							},
						},
					},
					{
						Name: "main",
						Devices: []*podresourcesv1.ContainerDevices{
							{
								ResourceName: nvidiaGPUResource,
								DeviceIds:    []string{"GPU-a", "GPU-b"},
							},
							{
								ResourceName: "example.com/fpga",
								DeviceIds:    []string{"FPGA-ignore"},
							},
							{
								ResourceName: nvidiaGPUResource,
								DeviceIds:    []string{"GPU-c"},
							},
						},
					},
				},
			},
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	got, err := GetPodGPUUUIDs(ctx, "test-pod", "default", "main")
	if err != nil {
		t.Fatalf("GetPodGPUUUIDs: %v", err)
	}

	want := []string{"GPU-a", "GPU-b", "GPU-c"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestDiscoverGPUUUIDsUsesPodResourcesForClassicPod(t *testing.T) {
	installTestPodResourcesServer(t, &podresourcesv1.ListPodResourcesResponse{
		PodResources: []*podresourcesv1.PodResources{
			{
				Name:      "test-pod",
				Namespace: "default",
				Containers: []*podresourcesv1.ContainerResources{
					{
						Name: "main",
						Devices: []*podresourcesv1.ContainerDevices{
							{
								ResourceName: nvidiaGPUResource,
								DeviceIds:    []string{"GPU-a", "GPU-b"},
							},
						},
					},
				},
			},
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	got, err := DiscoverGPUUUIDs(
		ctx,
		nil,
		"test-pod",
		"default",
		"main",
		"/proc",
		123,
		logr.Discard(),
	)
	if err != nil {
		t.Fatalf("DiscoverGPUUUIDs: %v", err)
	}

	want := []string{"GPU-a", "GPU-b"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestDiscoverGPUUUIDsFallsBackToPodResourcesAfterDRAAPILookupError(t *testing.T) {
	installTestPodResourcesServer(t, &podresourcesv1.ListPodResourcesResponse{
		PodResources: []*podresourcesv1.PodResources{
			{
				Name:      "test-pod",
				Namespace: "default",
				Containers: []*podresourcesv1.ContainerResources{
					{
						Name: "main",
						Devices: []*podresourcesv1.ContainerDevices{
							{
								ResourceName: nvidiaGPUResource,
								DeviceIds:    []string{"GPU-a"},
							},
						},
					},
				},
			},
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	got, err := DiscoverGPUUUIDs(
		ctx,
		fake.NewSimpleClientset(),
		"test-pod",
		"default",
		"main",
		"/proc",
		123,
		logr.Discard(),
	)
	if err != nil {
		t.Fatalf("DiscoverGPUUUIDs: %v", err)
	}
	if len(got) != 1 || got[0] != "GPU-a" {
		t.Fatalf("got %v, want [GPU-a]", got)
	}
}

func TestDiscoverGPUUUIDsOrdersDRAPodByContainerOrdinal(t *testing.T) {
	previousSocketPath := podResourcesSocketPath
	podResourcesSocketPath = filepath.Join(t.TempDir(), "missing-kubelet.sock")
	t.Cleanup(func() {
		podResourcesSocketPath = previousSocketPath
	})

	nodeName := "node-1"
	poolName := "pool-node-1"
	namespace := "default"
	podName := "test-pod"
	claimName := "gpu-claim"
	uuid0 := "GPU-aaaaaaaa-1111-2222-3333-444444444444"
	uuid1 := "GPU-bbbbbbbb-5555-6666-7777-888888888888"

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: namespace},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			Containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Claims: []corev1.ResourceClaim{{Name: "gpu"}},
					},
				},
			},
			ResourceClaims: []corev1.PodResourceClaim{
				{
					Name:              "gpu",
					ResourceClaimName: &claimName,
				},
			},
		},
	}
	claim := &resourcev1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: namespace},
		Status: resourcev1.ResourceClaimStatus{
			Allocation: &resourcev1.AllocationResult{
				Devices: resourcev1.DeviceAllocationResult{
					Results: []resourcev1.DeviceRequestAllocationResult{
						{Driver: nvidiaGPUDRADriver, Pool: poolName, Device: "gpu-1", Request: "gpu"},
						{Driver: nvidiaGPUDRADriver, Pool: poolName, Device: "gpu-0", Request: "gpu"},
					},
				},
			},
		},
	}
	slice := &resourcev1.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: poolName + "-gpu.nvidia.com-xxx"},
		Spec: resourcev1.ResourceSliceSpec{
			Driver:   nvidiaGPUDRADriver,
			NodeName: &nodeName,
			Pool:     resourcev1.ResourcePool{Name: poolName},
			Devices: []resourcev1.Device{
				{
					Name: "gpu-0",
					Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
						resourcev1.QualifiedName("uuid"): {StringValue: &uuid0},
					},
				},
				{
					Name: "gpu-1",
					Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
						resourcev1.QualifiedName("uuid"): {StringValue: &uuid1},
					},
				},
			},
		},
	}

	client := fake.NewSimpleClientset(pod, claim, slice)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	got, err := discoverGPUUUIDs(
		ctx,
		client,
		podName,
		namespace,
		"main",
		"/proc",
		123,
		func(context.Context, string, int) ([]string, error) {
			return []string{uuid0, uuid1}, nil
		},
		logr.Discard(),
	)
	if err != nil {
		t.Fatalf("DiscoverGPUUUIDs: %v", err)
	}
	want := []string{uuid0, uuid1}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestOrderDRAUUIDsByRuntimeRejectsMismatches(t *testing.T) {
	uuid0 := "GPU-aaaaaaaa-1111-2222-3333-444444444444"
	uuid1 := "GPU-bbbbbbbb-5555-6666-7777-888888888888"
	uuid2 := "GPU-cccccccc-9999-aaaa-bbbb-cccccccccccc"

	tests := []struct {
		name      string
		allocated []string
		visible   []string
	}{
		{
			name:      "count mismatch",
			allocated: []string{uuid0, uuid1},
			visible:   []string{uuid0},
		},
		{
			name:      "different set",
			allocated: []string{uuid0, uuid1},
			visible:   []string{uuid0, uuid2},
		},
		{
			name:      "duplicate allocation",
			allocated: []string{uuid0, uuid0},
			visible:   []string{uuid0, uuid1},
		},
		{
			name:      "invalid allocation UUID",
			allocated: []string{uuid0, "not-a-gpu-uuid"},
			visible:   []string{uuid0, uuid1},
		},
		{
			name:      "duplicate visible",
			allocated: []string{uuid0, uuid1},
			visible:   []string{uuid0, uuid0},
		},
		{
			name:      "invalid visible UUID",
			allocated: []string{uuid0, uuid1},
			visible:   []string{uuid0, "not-a-gpu-uuid"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := orderDRAUUIDsByRuntime(tc.allocated, tc.visible); err == nil {
				t.Fatalf("expected error, got %v", got)
			}
		})
	}
}
