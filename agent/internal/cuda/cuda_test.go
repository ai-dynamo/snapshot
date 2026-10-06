// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cuda

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/sys/unix"
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

func TestResolveVisibleGPUsPreservesCompatibilityMetadata(t *testing.T) {
	const uuid = "GPU-aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	dir := t.TempDir()
	script := "#!/bin/sh\ncase \"$*\" in\n" +
		"*uuid,name,driver_version*) echo '" + uuid + ", NVIDIA B200, 595.58.03';;\n" +
		"*) echo '" + uuid + "';;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "nvidia-smi"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	got, err := resolveSelectedGPUs(context.Background(), "7")
	want := compat.GPUInfo{
		DriverVersion: "595.58.03",
		Devices:       []compat.GPUDevice{{UUID: uuid, ProductName: "NVIDIA B200"}},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("GPU metadata = %#v, %v; want %#v", got, err, want)
	}
}

func TestDisabledLegacySelectionUsesOnlyContainerVisibility(t *testing.T) {
	for _, value := range []string{"", "none", "void"} {
		for _, visible := range []string{"", "GPU-cdi, NVIDIA B200, 595.58.03"} {
			t.Run(value+"/"+visible, func(t *testing.T) {
				installFakeNSenter(t, fmt.Sprintf("printf '%%s\\n' '%s'\n", visible))
				// No Kubernetes or PodResources client: an allocation fallback
				// would fail instead of returning the container's actual view.
				got, err := DiscoverGPUs(context.Background(), nil, "", "", "", "/host/proc", 42,
					[]string{"NVIDIA_VISIBLE_DEVICES=" + value}, logr.Discard())
				if err != nil || !reflect.DeepEqual(got, parseNvidiaSmiGPUs(visible)) {
					t.Fatalf("container discovery = %#v, %v", got, err)
				}
			})
		}
	}
}

func TestSelectionLookupHonorsDeadline(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "nvidia-smi"), []byte("#!/bin/sh\nexec sleep 30\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := resolveSelectedGPUs(ctx, "0"); err == nil {
		t.Fatal("stalled lookup succeeded")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("lookup ignored deadline: %s", elapsed)
	}
}

func TestVisibleDevicesValueUsesLastAssignment(t *testing.T) {
	got := VisibleDevicesValue([]string{"NVIDIA_VISIBLE_DEVICES=0", "NVIDIA_VISIBLE_DEVICES="})
	if got == nil || *got != "" {
		t.Fatalf("selection = %v, want explicitly empty", got)
	}
	if VisibleDevicesValue(nil) != nil {
		t.Fatal("absent selection became explicit")
	}
}

func TestResolveDevicePathsValidatesPhysicalMinor(t *testing.T) {
	root := t.TempDir()
	infoDir := filepath.Join(root, "driver/nvidia/gpus/0000:41:00.0")
	deviceDir := filepath.Join(root, "100/root/dev")
	for _, dir := range []string{infoDir, deviceDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(infoDir, "information"),
		[]byte("GPU UUID: GPU-A\nDevice Minor: 7\n"), 0644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(deviceDir, "nvidia7")
	if err := unix.Mknod(path, unix.S_IFCHR|0600, int(unix.Mkdev(195, 7))); err != nil {
		if errors.Is(err, unix.EPERM) {
			t.Skip("requires permission to create a test character device")
		}
		t.Fatal(err)
	}
	got, err := ResolveDevicePaths(root, 100, []string{"GPU-A"})
	if err != nil || got["GPU-A"] != "/dev/nvidia7" {
		t.Fatalf("paths = %v, error = %v", got, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveDevicePaths(root, 100, []string{"GPU-A"}); err == nil {
		t.Fatal("accepted a regular file in place of the allocated GPU")
	}
}

func TestResolveVisibleDevices(t *testing.T) {
	dir := t.TempDir()
	const a = "GPU-11111111-1111-1111-1111-111111111111"
	const b = "GPU-22222222-2222-2222-2222-222222222222"
	script := "#!/bin/sh\ncase \"$2\" in\n0|" + a + ") echo " + a + ";;\n2|" + b + ") echo " + b + ";;\n*) exit 1;;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "nvidia-smi"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	for _, tc := range []struct {
		value   string
		want    []string
		wantErr bool
	}{
		{"0,2", []string{a, b}, false},
		{b + "," + a, []string{b, a}, false},
		{"0," + a, nil, true},
		{"MIG-invalid", nil, true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			gpus, err := resolveSelectedGPUs(context.Background(), tc.value)
			got := gpuUUIDsOf(gpus)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v", err)
			}
			if !tc.wantErr && !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("UUIDs = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseNvidiaSmiGPUs(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   compat.GPUInfo
	}{
		{
			name:   "two GPUs on one driver",
			output: "GPU-aaa, NVIDIA A100-SXM4-40GB, 580.65.06\nGPU-bbb, NVIDIA A100-SXM4-40GB, 580.65.06\n",
			want: compat.GPUInfo{
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
			want: compat.GPUInfo{
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
			want: compat.GPUInfo{
				DriverVersion: "580.65.06",
				Devices:       []compat.GPUDevice{{UUID: "GPU-aaa", ProductName: "NVIDIA L4"}},
			},
		},
		{
			name:   "rows without UUIDs are not GPUs",
			output: ", NVIDIA L4, 580.65.06\nGPU-aaa, NVIDIA L4, 580.65.06\n",
			want: compat.GPUInfo{
				DriverVersion: "580.65.06",
				Devices:       []compat.GPUDevice{{UUID: "GPU-aaa", ProductName: "NVIDIA L4"}},
			},
		},
		{
			name:   "unsupported values are unknown env",
			output: "N/A, NVIDIA L4, 580.65.06\nGPU-aaa, N/A, N/A\nGPU-bbb, [Not Supported], Not Supported\n",
			want: compat.GPUInfo{
				Devices: []compat.GPUDevice{
					{UUID: "GPU-aaa"},
					{UUID: "GPU-bbb"},
				},
			},
		},
		{
			name:   "a node with no GPUs reports nothing",
			output: "\n",
			want:   compat.GPUInfo{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseNvidiaSmiGPUs(tc.output); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseNvidiaSmiGPUs() = %#v, want %#v", got, tc.want)
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

func TestDiscoverVisibleGPUs(t *testing.T) {
	installFakeNSenter(t, `
test "$1" = "--mount=/host/proc/42/ns/mnt"
test "$2" = "--pid=/host/proc/42/ns/pid"
test "$3" = "--"
test "$4" = "nvidia-smi"
case "$5" in
"--query-gpu=gpu_uuid,name,driver_version")
	test "$#" = 6
	test "$6" = "--format=csv,noheader"
	printf '%s\n' 'GPU-a, NVIDIA L4, 580.65.06'
	;;
"-L")
	test "$#" = 5
	printf '%s\n' 'GPU 0: NVIDIA L4 (UUID: GPU-a)'
	;;
*)
	exit 64
	;;
esac
`)

	got, err := DiscoverVisibleGPUs(context.Background(), "/host/proc/", 42, nvidiaSMITimeout, logr.Discard())
	if err != nil {
		t.Fatalf("DiscoverVisibleGPUs: %v", err)
	}
	want := compat.GPUInfo{
		DriverVersion: "580.65.06",
		Devices:       []compat.GPUDevice{{UUID: "GPU-a", ProductName: "NVIDIA L4"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DiscoverVisibleGPUs() = %#v, want %#v", got, want)
	}
}

// --query-gpu reports the parent card for a container holding a slice, so the
// slice's own UUID and shape can only come from the listing.
func TestDiscoverVisibleGPUsRecordsTheMIGProfile(t *testing.T) {
	installFakeNSenter(t, `
case "$5" in
"--query-gpu=gpu_uuid,name,driver_version")
	printf '%s\n' 'GPU-b1c4, NVIDIA H100 80GB HBM3, 580.65.06'
	;;
"-L")
	printf '%s\n' 'GPU 0: NVIDIA H100 80GB HBM3 (UUID: GPU-b1c4)' '  MIG 3g.40gb     Device  0: (UUID: MIG-7089d0f3-293f-58c9-8f8c-5ea666eedbde)'
	;;
esac
`)

	got, err := DiscoverVisibleGPUs(context.Background(), "/host/proc", 42, nvidiaSMITimeout, logr.Discard())
	if err != nil {
		t.Fatalf("DiscoverVisibleGPUs: %v", err)
	}
	want := compat.GPUInfo{
		DriverVersion: "580.65.06",
		Devices: []compat.GPUDevice{{
			UUID:        "MIG-7089d0f3-293f-58c9-8f8c-5ea666eedbde",
			ProductName: "NVIDIA H100 80GB HBM3",
			MIGProfile:  "3g.40gb",
		}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DiscoverVisibleGPUs() = %#v, want %#v", got, want)
	}
}

func TestDiscoverVisibleGPUsExpandsEverySliceOfAParent(t *testing.T) {
	installFakeNSenter(t, `
case "$5" in
"--query-gpu=gpu_uuid,name,driver_version")
	printf '%s\n' 'GPU-b1c4, NVIDIA H100 80GB HBM3, 580.65.06'
	;;
"-L")
	printf '%s\n' 'GPU 0: NVIDIA H100 80GB HBM3 (UUID: GPU-b1c4)' '  MIG 3g.40gb     Device  0: (UUID: MIG-aaa)' '  MIG 1g.10gb     Device  1: (UUID: MIG-bbb)'
	;;
esac
`)

	got, err := DiscoverVisibleGPUs(context.Background(), "/host/proc", 42, nvidiaSMITimeout, logr.Discard())
	if err != nil {
		t.Fatalf("DiscoverVisibleGPUs: %v", err)
	}
	want := compat.GPUInfo{
		DriverVersion: "580.65.06",
		Devices: []compat.GPUDevice{
			{UUID: "MIG-aaa", ProductName: "NVIDIA H100 80GB HBM3", MIGProfile: "3g.40gb"},
			{UUID: "MIG-bbb", ProductName: "NVIDIA H100 80GB HBM3", MIGProfile: "1g.10gb"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DiscoverVisibleGPUs() = %#v, want %#v", got, want)
	}
}

// The listing is a description, so losing it leaves the parent card recorded
// rather than costing the checkpoint.
func TestDiscoverVisibleGPUsSurvivesAFailedListing(t *testing.T) {
	installFakeNSenter(t, `
case "$5" in
"--query-gpu=gpu_uuid,name,driver_version")
	printf '%s\n' 'GPU-aaa, NVIDIA H100 80GB HBM3, 580.65.06'
	;;
*)
	exit 17
	;;
esac
`)

	got, err := DiscoverVisibleGPUs(context.Background(), "/host/proc", 42, nvidiaSMITimeout, logr.Discard())
	if err != nil {
		t.Fatalf("DiscoverVisibleGPUs: %v", err)
	}
	want := compat.GPUInfo{
		DriverVersion: "580.65.06",
		Devices:       []compat.GPUDevice{{UUID: "GPU-aaa", ProductName: "NVIDIA H100 80GB HBM3"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DiscoverVisibleGPUs() = %#v, want %#v", got, want)
	}
}

func TestParseNvidiaSmiMIGDevices(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   map[string][]migDevice
	}{
		{
			name: "every slice under one parent",
			output: `GPU 0: NVIDIA H100 80GB HBM3 (UUID: GPU-b1c4)
  MIG 3g.40gb     Device  0: (UUID: MIG-7089d0f3-293f-58c9-8f8c-5ea666eedbde)
  MIG 2g.20gb     Device  1: (UUID: MIG-56c30729-317f-5dd6-8da0-c3cc59e969e0)
  MIG 1g.10gb     Device  2: (UUID: MIG-9d14fb21-4ae1-546f-a636-011582899c39)
`,
			want: map[string][]migDevice{"GPU-b1c4": {
				{uuid: "MIG-7089d0f3-293f-58c9-8f8c-5ea666eedbde", profile: "3g.40gb"},
				{uuid: "MIG-56c30729-317f-5dd6-8da0-c3cc59e969e0", profile: "2g.20gb"},
				{uuid: "MIG-9d14fb21-4ae1-546f-a636-011582899c39", profile: "1g.10gb"},
			}},
		},
		{
			// Each slice belongs to the card listed above it, which is the
			// only thing tying it to the UUID --query-gpu reports.
			name: "slices split across two parents",
			output: `GPU 0: NVIDIA H100 80GB HBM3 (UUID: GPU-aaa)
  MIG 3g.40gb     Device  0: (UUID: MIG-000)
GPU 1: NVIDIA H100 80GB HBM3 (UUID: GPU-bbb)
  MIG 1g.10gb     Device  0: (UUID: MIG-111)
`,
			want: map[string][]migDevice{
				"GPU-aaa": {{uuid: "MIG-000", profile: "3g.40gb"}},
				"GPU-bbb": {{uuid: "MIG-111", profile: "1g.10gb"}},
			},
		},
		{
			// A parent GPU line carries no profile, so a node with MIG off
			// yields nothing rather than failing.
			name: "no MIG anywhere",
			output: `GPU 0: NVIDIA A100-SXM4-40GB (UUID: GPU-aaa)
GPU 1: NVIDIA A100-SXM4-40GB (UUID: GPU-bbb)
`,
			want: map[string][]migDevice{},
		},
		{
			name:   "nothing listed at all",
			output: "\n",
			want:   map[string][]migDevice{},
		},
		{
			// The listing from the cluster this was validated on.
			name:   "a whole GPU and nothing else",
			output: "GPU 0: Tesla T4 (UUID: GPU-698b3416-207f-8e8f-99b8-533f00103ca9)\n",
			want:   map[string][]migDevice{},
		},
		{
			// Without a parent there is nothing to attach the slice to.
			name:   "a slice with no parent line above it",
			output: "  MIG 1g.10gb     Device  0: (UUID: MIG-abc)\n",
			want:   map[string][]migDevice{},
		},
		{
			name:   "a slice named the older MIG-GPU-<parent>/<gi>/<ci> way",
			output: "GPU 0: NVIDIA A100-SXM4-40GB (UUID: GPU-6ecb3c0f-aaa)\n  MIG 1g.5gb      Device  0: (UUID: MIG-GPU-6ecb3c0f-aaa/1/0)\n",
			want:   map[string][]migDevice{"GPU-6ecb3c0f-aaa": {{uuid: "MIG-GPU-6ecb3c0f-aaa/1/0", profile: "1g.5gb"}}},
		},
		{
			name:   "a profile carrying the media-extension suffix",
			output: "GPU 0: NVIDIA A100-SXM4-40GB (UUID: GPU-aaa)\n  MIG 1g.10gb+me  Device  0: (UUID: MIG-abc)\n",
			want:   map[string][]migDevice{"GPU-aaa": {{uuid: "MIG-abc", profile: "1g.10gb+me"}}},
		},
		{
			// Padding only aligns the columns, so its width and whether it is
			// spaces or tabs leaves every value in the same column.
			name:   "padded with single spaces",
			output: "GPU 0: NVIDIA A100-SXM4-40GB (UUID: GPU-aaa)\nMIG 1g.10gb Device 0: (UUID: MIG-abc)\n",
			want:   map[string][]migDevice{"GPU-aaa": {{uuid: "MIG-abc", profile: "1g.10gb"}}},
		},
		{
			name:   "padded with tabs",
			output: "GPU\t0:\tNVIDIA A100-SXM4-40GB\t(UUID:\tGPU-aaa)\n\tMIG\t1g.10gb\tDevice\t0:\t(UUID:\tMIG-abc)\n",
			want:   map[string][]migDevice{"GPU-aaa": {{uuid: "MIG-abc", profile: "1g.10gb"}}},
		},
		{
			name:   "trailing text after the UUID",
			output: "GPU 0: NVIDIA A100-SXM4-40GB (UUID: GPU-aaa)\n  MIG 1g.10gb     Device  0: (UUID: MIG-abc)  extra\n",
			want:   map[string][]migDevice{"GPU-aaa": {{uuid: "MIG-abc", profile: "1g.10gb"}}},
		},
		{
			name:   "a MIG line whose device ordinal is not a number",
			output: "GPU 0: NVIDIA A100-SXM4-40GB (UUID: GPU-aaa)\n  MIG 1g.10gb     Device  X: (UUID: MIG-abc)\n",
			want:   map[string][]migDevice{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseNvidiaSmiMIGDevices(tc.output); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseNvidiaSmiMIGDevices() = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestDiscoverVisibleGPUsReturnCommandFailure(t *testing.T) {
	installFakeNSenter(t, "exit 17\n")

	_, err := DiscoverVisibleGPUs(context.Background(), "/host/proc", 42, nvidiaSMITimeout, logr.Discard())
	if err == nil {
		t.Fatal("DiscoverVisibleGPUs succeeded after nsenter failed")
	}
	if !strings.Contains(err.Error(), "pid 42") {
		t.Fatalf("DiscoverVisibleGPUs error = %q, want pid", err)
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

	got, err := discoverGPUs(
		ctx,
		client,
		podName,
		namespace,
		"main",
		"/proc",
		123,
		nvidiaSMITimeout,
		func(context.Context, string, int, time.Duration, logr.Logger) (compat.GPUInfo, error) {
			return compat.GPUInfo{
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
		t.Fatalf("discoverGPUs: %v", err)
	}
	// Ordered by the runtime, and still described: the DRA path used to reduce
	// nvidia-smi's answer to an ordering and throw the rest away.
	want := compat.GPUInfo{
		DriverVersion: "580.65.06",
		Devices: []compat.GPUDevice{
			{UUID: uuid0, ProductName: "NVIDIA A100-SXM4-40GB"},
			{UUID: uuid1, ProductName: "NVIDIA A100-SXM4-40GB"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("discoverGPUs() = %#v, want %#v", got, want)
	}
}

// The kubelet path has its GPUs without nvidia-smi, so it never used to run it.
// It runs it now for the model and driver version, and a failure there costs
// those two and nothing else.
func TestDiscoverGPUsDescribePodResourcesGPUs(t *testing.T) {
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
		visible func(context.Context, string, int, time.Duration, logr.Logger) (compat.GPUInfo, error)
		want    compat.GPUInfo
	}{
		{
			name: "described in the kubelet's order",
			visible: func(context.Context, string, int, time.Duration, logr.Logger) (compat.GPUInfo, error) {
				return compat.GPUInfo{
					DriverVersion: "580.65.06",
					Devices: []compat.GPUDevice{
						{UUID: "GPU-b", ProductName: "NVIDIA L4"},
						{UUID: "GPU-a", ProductName: "NVIDIA L4"},
					},
				}, nil
			},
			want: compat.GPUInfo{
				DriverVersion: "580.65.06",
				Devices: []compat.GPUDevice{
					{UUID: "GPU-a", ProductName: "NVIDIA L4"},
					{UUID: "GPU-b", ProductName: "NVIDIA L4"},
				},
			},
		},
		{
			name: "undescribed when nvidia-smi cannot be reached",
			visible: func(context.Context, string, int, time.Duration, logr.Logger) (compat.GPUInfo, error) {
				return compat.GPUInfo{}, errors.New("nsenter unavailable")
			},
			want: compat.GPUInfo{
				Devices: []compat.GPUDevice{{UUID: "GPU-a"}, {UUID: "GPU-b"}},
			},
		},
		{
			name: "undescribed when nvidia-smi reports other GPUs",
			visible: func(context.Context, string, int, time.Duration, logr.Logger) (compat.GPUInfo, error) {
				return compat.GPUInfo{
					DriverVersion: "580.65.06",
					Devices:       []compat.GPUDevice{{UUID: "GPU-z", ProductName: "NVIDIA L4"}},
				}, nil
			},
			want: compat.GPUInfo{
				DriverVersion: "580.65.06",
				Devices:       []compat.GPUDevice{{UUID: "GPU-a"}, {UUID: "GPU-b"}},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			got, err := discoverGPUs(
				ctx, nil, "test-pod", "default", "main", "/proc", 123, nvidiaSMITimeout, tc.visible, logr.Discard(),
			)
			if err != nil {
				t.Fatalf("discoverGPUs: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("discoverGPUs() = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestDiscoverGPUsUseVisibleGPUDescriptions(t *testing.T) {
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

	got, err := DiscoverGPUs(
		context.Background(), nil, "test-pod", "default", "main", "/host/proc", 42, nil, logr.Discard(),
	)
	if err != nil {
		t.Fatalf("DiscoverGPUs: %v", err)
	}
	want := compat.GPUInfo{
		DriverVersion: "580.65.06",
		Devices:       []compat.GPUDevice{{UUID: "GPU-a", ProductName: "NVIDIA L4"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DiscoverGPUs() = %#v, want %#v", got, want)
	}
}

func TestDiscoverGPUsFallBackToVisibleGPUs(t *testing.T) {
	installTestPodResourcesServer(t, &podresourcesv1.ListPodResourcesResponse{})
	want := compat.GPUInfo{
		DriverVersion: "580.65.06",
		Devices:       []compat.GPUDevice{{UUID: "GPU-a", ProductName: "NVIDIA L4"}},
	}

	got, err := discoverGPUs(
		context.Background(),
		nil,
		"test-pod",
		"default",
		"main",
		"/host/proc",
		42,
		nvidiaSMITimeout,
		func(context.Context, string, int, time.Duration, logr.Logger) (compat.GPUInfo, error) {
			return want, nil
		},
		logr.Discard(),
	)
	if err != nil {
		t.Fatalf("discoverGPUs: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("discoverGPUs() = %#v, want %#v", got, want)
	}
}

func TestDescribeGPUs(t *testing.T) {
	visible := compat.GPUInfo{
		DriverVersion: "580.65.06",
		Devices: []compat.GPUDevice{
			{UUID: "GPU-b", ProductName: "NVIDIA H100"},
			{UUID: "GPU-extra", ProductName: "NVIDIA L4"},
			{UUID: "GPU-a", ProductName: "NVIDIA A100"},
		},
	}

	got := describeGPUs([]string{"GPU-a", "GPU-b", "GPU-missing"}, visible)
	want := compat.GPUInfo{
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
	env := compat.GPUInfo{
		Devices: []compat.GPUDevice{
			{UUID: "GPU-a"},
			{ProductName: "NVIDIA L4"},
			{UUID: "GPU-b"},
		},
	}

	got := gpuUUIDsOf(env)
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
