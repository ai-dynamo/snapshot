// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cuda

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
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

	"github.com/ai-dynamo/snapshot/api/compat"
)

func TestParseNvidiaSmiGPUFacts(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   compat.GPUFacts
	}{
		{
			name:   "two GPUs on one driver",
			output: "GPU-aaa, NVIDIA A100-SXM4-40GB, 580.65.06\nGPU-bbb, NVIDIA A100-SXM4-40GB, 580.65.06\n",
			want: compat.GPUFacts{
				DriverVersion: "580.65.06",
				Devices: []compat.GPUDevice{
					{UUID: "GPU-aaa", ProductName: "NVIDIA A100-SXM4-40GB"},
					{UUID: "GPU-bbb", ProductName: "NVIDIA A100-SXM4-40GB"},
				},
			},
		},
		{
			// The device map is built from UUIDs, so a row that loses its model
			// still has to count as a GPU.
			name:   "a row without a model still reports its GPU",
			output: "GPU-aaa\nGPU-bbb, NVIDIA H100 80GB HBM3, 580.65.06\n",
			want: compat.GPUFacts{
				DriverVersion: "580.65.06",
				Devices: []compat.GPUDevice{
					{UUID: "GPU-aaa"},
					{UUID: "GPU-bbb", ProductName: "NVIDIA H100 80GB HBM3"},
				},
			},
		},
		{
			name:   "blank lines are not GPUs",
			output: "\n\nGPU-aaa, NVIDIA L4, 580.65.06\n\n",
			want: compat.GPUFacts{
				DriverVersion: "580.65.06",
				Devices:       []compat.GPUDevice{{UUID: "GPU-aaa", ProductName: "NVIDIA L4"}},
			},
		},
		{
			name:   "rows without UUIDs are not GPUs",
			output: ", NVIDIA L4, 580.65.06\nGPU-aaa, NVIDIA L4, 580.65.06\n",
			want: compat.GPUFacts{
				DriverVersion: "580.65.06",
				Devices:       []compat.GPUDevice{{UUID: "GPU-aaa", ProductName: "NVIDIA L4"}},
			},
		},
		{
			name:   "a node with no GPUs reports nothing",
			output: "\n",
			want:   compat.GPUFacts{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseNvidiaSmiGPUFacts(tc.output); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseNvidiaSmiGPUFacts() = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func installFakeNSenter(t *testing.T, body string) {
	t.Helper()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "nsenter"), []byte("#!/bin/sh\nset -eu\n"+body), 0o755); err != nil {
		t.Fatalf("write fake nsenter: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestDiscoverVisibleGPUFacts(t *testing.T) {
	installFakeNSenter(t, `
test "$#" = 6
test "$1" = "--mount=/host/proc/42/ns/mnt"
test "$2" = "--pid=/host/proc/42/ns/pid"
test "$3" = "--"
test "$4" = "nvidia-smi"
test "$5" = "--query-gpu=gpu_uuid,name,driver_version"
test "$6" = "--format=csv,noheader"
printf '%s\n' 'GPU-a, NVIDIA L4, 580.65.06'
`)

	got, err := DiscoverVisibleGPUFacts(context.Background(), "/host/proc/", 42)
	if err != nil {
		t.Fatalf("DiscoverVisibleGPUFacts: %v", err)
	}
	want := compat.GPUFacts{
		DriverVersion: "580.65.06",
		Devices:       []compat.GPUDevice{{UUID: "GPU-a", ProductName: "NVIDIA L4"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DiscoverVisibleGPUFacts() = %#v, want %#v", got, want)
	}
}

func TestDiscoverVisibleGPUFactsReturnsCommandFailure(t *testing.T) {
	installFakeNSenter(t, "exit 17\n")

	_, err := DiscoverVisibleGPUFacts(context.Background(), "/host/proc", 42)
	if err == nil {
		t.Fatal("DiscoverVisibleGPUFacts succeeded after nsenter failed")
	}
	if !strings.Contains(err.Error(), "pid 42") {
		t.Fatalf("DiscoverVisibleGPUFacts error = %q, want pid", err)
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

func TestDiscoverGPUUUIDsReturnsDiscoveryError(t *testing.T) {
	previousSocketPath := podResourcesSocketPath
	podResourcesSocketPath = filepath.Join(t.TempDir(), "missing-kubelet.sock")
	t.Cleanup(func() {
		podResourcesSocketPath = previousSocketPath
	})

	_, err := DiscoverGPUUUIDs(
		context.Background(), nil, "test-pod", "default", "main", "/host/proc", 42, logr.Discard(),
	)
	if err == nil {
		t.Fatal("DiscoverGPUUUIDs succeeded after PodResources lookup failed")
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

	got, err := discoverGPUFacts(
		ctx,
		client,
		podName,
		namespace,
		"main",
		"/proc",
		123,
		func(context.Context, string, int) (compat.GPUFacts, error) {
			return compat.GPUFacts{
				DriverVersion: "580.65.06",
				Devices: []compat.GPUDevice{
					{UUID: uuid0, ProductName: "NVIDIA A100-SXM4-40GB"},
					{UUID: uuid1, ProductName: "NVIDIA A100-SXM4-40GB"},
				},
			}, nil
		},
		logr.Discard(),
	)
	if err != nil {
		t.Fatalf("discoverGPUFacts: %v", err)
	}
	// Ordered by the runtime, and still described: the DRA path used to reduce
	// nvidia-smi's answer to an ordering and throw the rest away.
	want := compat.GPUFacts{
		DriverVersion: "580.65.06",
		Devices: []compat.GPUDevice{
			{UUID: uuid0, ProductName: "NVIDIA A100-SXM4-40GB"},
			{UUID: uuid1, ProductName: "NVIDIA A100-SXM4-40GB"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("discoverGPUFacts() = %#v, want %#v", got, want)
	}
}

// The kubelet path has its GPUs without nvidia-smi, so it never used to run it.
// It runs it now for the model and driver version, and a failure there costs
// those facts and nothing else.
func TestDiscoverGPUFactsDescribesPodResourcesGPUs(t *testing.T) {
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

	tests := []struct {
		name    string
		visible func(context.Context, string, int) (compat.GPUFacts, error)
		want    compat.GPUFacts
	}{
		{
			name: "described in the kubelet's order",
			visible: func(context.Context, string, int) (compat.GPUFacts, error) {
				return compat.GPUFacts{
					DriverVersion: "580.65.06",
					Devices: []compat.GPUDevice{
						{UUID: "GPU-b", ProductName: "NVIDIA L4"},
						{UUID: "GPU-a", ProductName: "NVIDIA L4"},
					},
				}, nil
			},
			want: compat.GPUFacts{
				DriverVersion: "580.65.06",
				Devices: []compat.GPUDevice{
					{UUID: "GPU-a", ProductName: "NVIDIA L4"},
					{UUID: "GPU-b", ProductName: "NVIDIA L4"},
				},
			},
		},
		{
			name: "undescribed when nvidia-smi cannot be reached",
			visible: func(context.Context, string, int) (compat.GPUFacts, error) {
				return compat.GPUFacts{}, errors.New("nsenter unavailable")
			},
			want: compat.GPUFacts{
				Devices: []compat.GPUDevice{{UUID: "GPU-a"}, {UUID: "GPU-b"}},
			},
		},
		{
			name: "undescribed when nvidia-smi reports other GPUs",
			visible: func(context.Context, string, int) (compat.GPUFacts, error) {
				return compat.GPUFacts{
					DriverVersion: "580.65.06",
					Devices:       []compat.GPUDevice{{UUID: "GPU-z", ProductName: "NVIDIA L4"}},
				}, nil
			},
			want: compat.GPUFacts{
				DriverVersion: "580.65.06",
				Devices:       []compat.GPUDevice{{UUID: "GPU-a"}, {UUID: "GPU-b"}},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			got, err := discoverGPUFacts(
				ctx, nil, "test-pod", "default", "main", "/proc", 123, tc.visible, logr.Discard(),
			)
			if err != nil {
				t.Fatalf("discoverGPUFacts: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("discoverGPUFacts() = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestDiscoverGPUFactsUsesVisibleGPUDescriptions(t *testing.T) {
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
	installFakeNSenter(t, "printf '%s\\n' 'GPU-a, NVIDIA L4, 580.65.06'\n")

	got, err := DiscoverGPUFacts(
		context.Background(), nil, "test-pod", "default", "main", "/host/proc", 42, logr.Discard(),
	)
	if err != nil {
		t.Fatalf("DiscoverGPUFacts: %v", err)
	}
	want := compat.GPUFacts{
		DriverVersion: "580.65.06",
		Devices:       []compat.GPUDevice{{UUID: "GPU-a", ProductName: "NVIDIA L4"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DiscoverGPUFacts() = %#v, want %#v", got, want)
	}
}

func TestDiscoverGPUFactsFallsBackToVisibleGPUs(t *testing.T) {
	installTestPodResourcesServer(t, &podresourcesv1.ListPodResourcesResponse{})
	want := compat.GPUFacts{
		DriverVersion: "580.65.06",
		Devices:       []compat.GPUDevice{{UUID: "GPU-a", ProductName: "NVIDIA L4"}},
	}

	got, err := discoverGPUFacts(
		context.Background(),
		nil,
		"test-pod",
		"default",
		"main",
		"/host/proc",
		42,
		func(context.Context, string, int) (compat.GPUFacts, error) {
			return want, nil
		},
		logr.Discard(),
	)
	if err != nil {
		t.Fatalf("discoverGPUFacts: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("discoverGPUFacts() = %#v, want %#v", got, want)
	}
}

func TestDescribeGPUs(t *testing.T) {
	visible := compat.GPUFacts{
		DriverVersion: "580.65.06",
		Devices: []compat.GPUDevice{
			{UUID: "GPU-b", ProductName: "NVIDIA H100"},
			{UUID: "GPU-extra", ProductName: "NVIDIA L4"},
			{UUID: "GPU-a", ProductName: "NVIDIA A100"},
		},
	}

	got := describeGPUs([]string{"GPU-a", "GPU-b", "GPU-missing"}, visible)
	want := compat.GPUFacts{
		DriverVersion: "580.65.06",
		Devices: []compat.GPUDevice{
			{UUID: "GPU-a", ProductName: "NVIDIA A100"},
			{UUID: "GPU-b", ProductName: "NVIDIA H100"},
			{UUID: "GPU-missing"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("describeGPUs() = %#v, want %#v", got, want)
	}
}

func TestGPUUUIDsOf(t *testing.T) {
	facts := compat.GPUFacts{
		Devices: []compat.GPUDevice{
			{UUID: "GPU-a"},
			{ProductName: "NVIDIA L4"},
			{UUID: "GPU-b"},
		},
	}

	got := gpuUUIDsOf(facts)
	want := []string{"GPU-a", "GPU-b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("gpuUUIDsOf() = %v, want %v", got, want)
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
