// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cuda

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
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
		for _, visible := range []string{"", "GPU 0: NVIDIA B200 (UUID: GPU-cdi)"} {
			t.Run(value+"/"+visible, func(t *testing.T) {
				installFakeNSenter(t, fmt.Sprintf("printf '%%s\\n' '%s'\n", visible))
				// No Kubernetes or PodResources client: an allocation fallback
				// would fail instead of returning the container's actual view.
				got, err := DiscoverGPUs(context.Background(), nil, "", "", "",
					[]string{"NVIDIA_VISIBLE_DEVICES=" + value}, ListVisibleGPUs(context.Background(), "/host/proc", 42), logr.Discard())
				if err != nil || !reflect.DeepEqual(got, parseNvidiaSmiList(visible).GPUs) {
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
			got := GPUUUIDs(gpus)
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

func TestNsenterNvidiaSMI(t *testing.T) {
	installFakeNSenter(t, `
test "$#" = 6
test "$1" = "--mount=/host/proc/42/ns/mnt"
test "$2" = "--pid=/host/proc/42/ns/pid"
test "$3" = "--"
test "$4" = "nvidia-smi"
test "$5" = "-i"
test "$6" = "0"
printf 'raw output\n'
`)

	got, err := nsenterNvidiaSMI(context.Background(), "/host/proc/", 42, "-i", "0")
	if err != nil {
		t.Fatalf("nsenterNvidiaSMI: %v", err)
	}
	if string(got) != "raw output\n" {
		t.Fatalf("nsenterNvidiaSMI() = %q, want the command's stdout unchanged", got)
	}
}

func TestNsenterNvidiaSMIReturnsCommandFailure(t *testing.T) {
	installFakeNSenter(t, "exit 17\n")

	_, err := nsenterNvidiaSMI(context.Background(), "/host/proc", 42, "-L")
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 17 || !strings.Contains(err.Error(), "pid 42") {
		t.Fatalf("nsenterNvidiaSMI error = %v, want exit status 17 wrapped with pid 42", err)
	}
}

// fakeHostProc returns a host procfs holding only the NVIDIA kernel module's
// version file.
func fakeHostProc(t *testing.T, driverVersion string) string {
	t.Helper()
	hostProc := t.TempDir()
	dir := filepath.Join(hostProc, "driver", "nvidia")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	banner := "NVRM version: NVIDIA UNIX Open Kernel Module for x86_64  " + driverVersion +
		"  Release Build  (dvs-builder@U22-I3-F05-2-4)  Fri Sep  4 22:33:33 UTC 2026\nGCC version:  gcc version 12.3.0\n"
	if err := os.WriteFile(filepath.Join(dir, "version"), []byte(banner), 0o644); err != nil {
		t.Fatal(err)
	}
	return hostProc
}

func TestListVisibleGPUs(t *testing.T) {
	hostProc := fakeHostProc(t, "615.71.09")
	calls := filepath.Join(t.TempDir(), "calls")
	installFakeNSenter(t, fmt.Sprintf(`
echo call >> %s
test "$#" = 5
test "$1" = "--mount=%s/42/ns/mnt"
test "$2" = "--pid=%s/42/ns/pid"
test "$3" = "--"
test "$4" = "nvidia-smi"
test "$5" = "-L"
printf '%%s\n' 'GPU 0: NVIDIA A100-SXM4-40GB (UUID: GPU-5c89852c-d268-c3f3-1b07-005d5ae1dc3f)'
printf '%%s\n' '  MIG 1g.5gb      Device  0: (UUID: MIG-c6d4f1ef-42e4-5de3-91c7-45d71c87eb3f)'
`, calls, hostProc, hostProc))

	visible := ListVisibleGPUs(context.Background(), hostProc+"/", 42)
	want := VisibleGPUs{
		GPUs: compat.GPUInfo{
			DriverVersion: "615.71.09",
			Devices: []compat.GPUDevice{{
				UUID:        "GPU-5c89852c-d268-c3f3-1b07-005d5ae1dc3f",
				ProductName: "NVIDIA A100-SXM4-40GB",
			}},
		},
		MIGSlices: []string{"1g.5gb MIG-c6d4f1ef-42e4-5de3-91c7-45d71c87eb3f"},
	}
	// The MIG check and GPU discovery both ask, and nvidia-smi runs once.
	for range 2 {
		got, err := visible()
		if err != nil {
			t.Fatalf("ListVisibleGPUs: %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("ListVisibleGPUs() = %#v, want %#v", got, want)
		}
	}
	if recorded, err := os.ReadFile(calls); err != nil || string(recorded) != "call\n" {
		t.Fatalf("nvidia-smi ran %q times, want once (%v)", recorded, err)
	}
}

func TestListVisibleGPUsReturnsCommandFailure(t *testing.T) {
	installFakeNSenter(t, "exit 17\n")

	if _, err := ListVisibleGPUs(context.Background(), "/host/proc", 42)(); err == nil || !strings.Contains(err.Error(), "pid 42") {
		t.Fatalf("ListVisibleGPUs error = %v, want the nsenter failure for pid 42", err)
	}
}

// Without the module's version file, the GPUs are still listed and the
// driver version is unknown.
func TestListVisibleGPUsWithoutDriverVersion(t *testing.T) {
	installFakeNSenter(t, "printf '%s\\n' 'GPU 0: NVIDIA L4 (UUID: GPU-a)'\n")

	got, err := ListVisibleGPUs(context.Background(), t.TempDir(), 42)()
	want := VisibleGPUs{GPUs: compat.GPUInfo{Devices: []compat.GPUDevice{{UUID: "GPU-a", ProductName: "NVIDIA L4"}}}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("ListVisibleGPUs() = %#v, %v, want %#v", got, err, want)
	}
}

func TestParseDriverVersion(t *testing.T) {
	tests := []struct {
		name   string
		banner string
		want   string
	}{
		{
			name:   "open kernel module",
			banner: "NVRM version: NVIDIA UNIX Open Kernel Module for x86_64  615.71.09  Release Build  (dvs-builder@U22-I3-F05-2-4)  Fri Sep  4 22:33:33 UTC 2026\n",
			want:   "615.71.09",
		},
		{
			name:   "open kernel module on Arm",
			banner: "NVRM version: NVIDIA UNIX Open Kernel Module for aarch64  580.65.06  Release Build  (dvs-builder@U16-I2-C03-12-2)  Sun Jul 27 06:54:38 UTC 2025\n",
			want:   "580.65.06",
		},
		{
			name:   "proprietary kernel module",
			banner: "NVRM version: NVIDIA UNIX x86_64 Kernel Module  550.54.15  Tue Mar  5 22:23:56 UTC 2024\nGCC version:  gcc version 12.3.0\n",
			want:   "550.54.15",
		},
		{name: "not a banner", banner: "garbage\n"},
		{name: "empty"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseDriverVersion(tc.banner); got != tc.want {
				t.Fatalf("parseDriverVersion() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseNvidiaSmiList(t *testing.T) {
	const (
		card0 = "GPU-5c89852c-d268-c3f3-1b07-005d5ae1dc3f"
		card1 = "GPU-0d1a5b2c-3e4f-5a6b-7c8d-9e0f1a2b3c4d"
	)
	tests := []struct {
		name   string
		output string
		want   VisibleGPUs
	}{
		{
			name:   "a whole GPU is no slice",
			output: "GPU 0: NVIDIA H100 80GB HBM3 (UUID: " + card0 + ")\n",
			want: VisibleGPUs{GPUs: compat.GPUInfo{Devices: []compat.GPUDevice{
				{UUID: card0, ProductName: "NVIDIA H100 80GB HBM3"},
			}}},
		},
		{
			name: "every card in order and every slice of every card",
			output: "GPU 0: NVIDIA A100-SXM4-40GB (UUID: " + card0 + ")\n" +
				"  MIG 3g.20gb     Device  0: (UUID: MIG-11111111-2222-3333-4444-555555555555)\n" +
				"  MIG 1g.5gb      Device  1: (UUID: MIG-66666666-7777-8888-9999-000000000000)\n" +
				"GPU 1: NVIDIA A100-SXM4-40GB (UUID: " + card1 + ")\n",
			want: VisibleGPUs{
				GPUs: compat.GPUInfo{Devices: []compat.GPUDevice{
					{UUID: card0, ProductName: "NVIDIA A100-SXM4-40GB"},
					{UUID: card1, ProductName: "NVIDIA A100-SXM4-40GB"},
				}},
				MIGSlices: []string{
					"3g.20gb MIG-11111111-2222-3333-4444-555555555555",
					"1g.5gb MIG-66666666-7777-8888-9999-000000000000",
				},
			},
		},
		{
			name: "the R450 UUID form",
			output: "GPU 0: A100-SXM4-40GB (UUID: " + card0 + ")\n" +
				"  MIG 1g.5gb Device 0: (UUID: MIG-" + card0 + "/7/0)\n",
			want: VisibleGPUs{
				GPUs:      compat.GPUInfo{Devices: []compat.GPUDevice{{UUID: card0, ProductName: "A100-SXM4-40GB"}}},
				MIGSlices: []string{"1g.5gb MIG-" + card0 + "/7/0"},
			},
		},
		{
			// The device map is built from UUIDs, so a card without one is no GPU.
			name:   "a card line without a UUID is not a GPU",
			output: "GPU 0: NVIDIA L4\n",
		},
		{
			name:   "a slice line without a UUID is still a slice",
			output: "  MIG 1g.5gb      Device  0:\n",
			want:   VisibleGPUs{MIGSlices: []string{"MIG 1g.5gb Device 0:"}},
		},
		{
			name:   "no devices",
			output: "\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseNvidiaSmiList(tc.output); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseNvidiaSmiList() = %#v, want %#v", got, tc.want)
			}
		})
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
			// A TP4 restore onto GPUs 2-5 of a node, checkpointed on GPUs 0-3:
			// every process keeps its position, so GPUs 2 and 3 move too.
			name:   "overlapping GPUs pair by position",
			source: []string{"GPU-0", "GPU-1", "GPU-2", "GPU-3"},
			target: []string{"GPU-2", "GPU-3", "GPU-4", "GPU-5"},
			want:   "GPU-0=GPU-2,GPU-1=GPU-3,GPU-2=GPU-4,GPU-3=GPU-5",
		},
		{
			name:   "a GPU at the same position stays",
			source: []string{"GPU-aaa", "GPU-bbb"},
			target: []string{"GPU-aaa", "GPU-ccc"},
			want:   "GPU-aaa=GPU-aaa,GPU-bbb=GPU-ccc",
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

	installFakeNSenter(t, "printf '%s\\n' 'GPU 0: NVIDIA L4 (UUID: GPU-a)' 'GPU 1: NVIDIA L4 (UUID: GPU-b)'\n")
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

	installFakeNSenter(t, "printf '%s\\n' 'GPU 0: NVIDIA L4 (UUID: GPU-a)'\n")
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
		func() (VisibleGPUs, error) {
			return VisibleGPUs{GPUs: compat.GPUInfo{
				DriverVersion: "580.65.06",
				Devices: []compat.GPUDevice{
					{UUID: uuid0, ProductName: "NVIDIA A100-SXM4-40GB"},
					{UUID: uuid1, ProductName: "NVIDIA A100-SXM4-40GB"},
				},
			}}, nil
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
		visible VisibleGPULister
		want    compat.GPUInfo
		wantErr string
	}{
		{
			name: "described in the container's bus order, not the kubelet's",
			visible: func() (VisibleGPUs, error) {
				return VisibleGPUs{GPUs: compat.GPUInfo{
					DriverVersion: "580.65.06",
					Devices: []compat.GPUDevice{
						{UUID: "GPU-b", ProductName: "NVIDIA L4"},
						{UUID: "GPU-a", ProductName: "NVIDIA L4"},
					},
				}}, nil
			},
			want: compat.GPUInfo{
				DriverVersion: "580.65.06",
				Devices: []compat.GPUDevice{
					{UUID: "GPU-b", ProductName: "NVIDIA L4"},
					{UUID: "GPU-a", ProductName: "NVIDIA L4"},
				},
			},
		},
		{
			name: "fails when the container cannot list its GPUs",
			visible: func() (VisibleGPUs, error) {
				return VisibleGPUs{}, errors.New("nsenter unavailable")
			},
			wantErr: "nsenter unavailable",
		},
		{
			name: "fails when the container lists other GPUs",
			visible: func() (VisibleGPUs, error) {
				return VisibleGPUs{GPUs: compat.GPUInfo{
					DriverVersion: "580.65.06",
					Devices:       []compat.GPUDevice{{UUID: "GPU-z", ProductName: "NVIDIA L4"}},
				}}, nil
			},
			wantErr: "does not list allocated GPUs [GPU-a GPU-b]",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			got, err := discoverGPUs(
				ctx, nil, "test-pod", "default", "main", tc.visible, logr.Discard(),
			)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("discoverGPUs() error = %v, want %q", err, tc.wantErr)
				}
				return
			}
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
	installFakeNSenter(t, "printf '%s\\n' 'GPU 0: NVIDIA L4 (UUID: GPU-a)'\n")
	hostProc := fakeHostProc(t, "580.65.06")

	got, err := DiscoverGPUs(
		context.Background(), nil, "test-pod", "default", "main", nil, ListVisibleGPUs(context.Background(), hostProc, 42), logr.Discard(),
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
		func() (VisibleGPUs, error) {
			return VisibleGPUs{GPUs: want}, nil
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

	// Listing order wins over allocated order, and GPUs not allocated drop out.
	got, err := describeGPUs([]string{"GPU-a", "GPU-b"}, visible)
	want := compat.GPUInfo{
		DriverVersion: "580.65.06",
		Devices: []compat.GPUDevice{
			{UUID: "GPU-b", ProductName: "NVIDIA H100"},
			{UUID: "GPU-a", ProductName: "NVIDIA A100"},
		},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("describeGPUs() = %#v, %v; want %#v", got, err, want)
	}

	// An allocated GPU the container does not list has no bus position.
	if _, err := describeGPUs([]string{"GPU-a", "GPU-missing"}, visible); err == nil ||
		!strings.Contains(err.Error(), "GPU-missing") {
		t.Fatalf("describeGPUs() with an unlisted GPU: error = %v", err)
	}
}

// The case GPU Memory Service depends on: kubelet allocates the checkpoint's and
// the restore's GPUs in an order other than bus order, and the restore lands on
// a partly different set. Each process must keep its bus position, because GMS
// loads the weights saved from the N-th GPU in bus order onto the N-th one.
func TestDeviceMapKeepsBusPositionWhenAllocationOrderDiffers(t *testing.T) {
	listing := func(uuids ...string) compat.GPUInfo {
		var gpus compat.GPUInfo
		for _, uuid := range uuids {
			gpus.Devices = append(gpus.Devices, compat.GPUDevice{UUID: uuid})
		}
		return gpus
	}
	// Bus order 0-7 on the node; the checkpoint ran on GPUs 0-3, the restore on 2-5.
	source, err := describeGPUs(
		[]string{"GPU-3", "GPU-1", "GPU-0", "GPU-2"},
		listing("GPU-0", "GPU-1", "GPU-2", "GPU-3"),
	)
	if err != nil {
		t.Fatal(err)
	}
	target, err := describeGPUs(
		[]string{"GPU-5", "GPU-2", "GPU-4", "GPU-3"},
		listing("GPU-2", "GPU-3", "GPU-4", "GPU-5"),
	)
	if err != nil {
		t.Fatal(err)
	}
	got, err := BuildDeviceMap(GPUUUIDs(source), GPUUUIDs(target), logr.Discard())
	if err != nil {
		t.Fatal(err)
	}
	if want := "GPU-0=GPU-2,GPU-1=GPU-3,GPU-2=GPU-4,GPU-3=GPU-5"; got != want {
		t.Fatalf("device map = %q, want %q", got, want)
	}
}

func TestDiscoverGPUsListSelectedGPUsInBusOrder(t *testing.T) {
	const a = "GPU-11111111-1111-1111-1111-111111111111"
	const b = "GPU-22222222-2222-2222-2222-222222222222"
	dir := t.TempDir()
	script := "#!/bin/sh\ncase \"$2\" in\n0) echo '" + a + ", NVIDIA B200, 595.58.03';;\n2) echo '" + b + ", NVIDIA B200, 595.58.03';;\n*) exit 1;;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "nvidia-smi"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	installFakeNSenter(t, "printf '%s\\n' 'GPU 0: NVIDIA B200 (UUID: "+a+")' 'GPU 1: NVIDIA B200 (UUID: "+b+")'\n")

	got, err := DiscoverGPUs(context.Background(), nil, "", "", "",
		[]string{"NVIDIA_VISIBLE_DEVICES=2,0"},
		ListVisibleGPUs(context.Background(), fakeHostProc(t, "595.58.03"), 42), logr.Discard())
	if err != nil {
		t.Fatalf("DiscoverGPUs: %v", err)
	}
	if uuids := GPUUUIDs(got); !reflect.DeepEqual(uuids, []string{a, b}) {
		t.Fatalf("UUIDs = %v, want bus order [%s %s]", uuids, a, b)
	}
}

func TestGPUUUIDs(t *testing.T) {
	env := compat.GPUInfo{
		Devices: []compat.GPUDevice{
			{UUID: "GPU-a"},
			{ProductName: "NVIDIA L4"},
			{UUID: "GPU-b"},
		},
	}

	got := GPUUUIDs(env)
	want := []string{"GPU-a", "GPU-b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GPUUUIDs() = %v, want %v", got, want)
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

func TestListProcfsGPUs(t *testing.T) {
	writeCard := func(t *testing.T, root, busID, uuid, minor string) {
		t.Helper()
		dir := filepath.Join(root, "proc", "driver", "nvidia", "gpus", busID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		information := "Model: \t\t NVIDIA B200\nGPU UUID: \t " + uuid + "\nBus Location: \t " + busID + "\nDevice Minor: \t " + minor + "\n"
		if err := os.WriteFile(filepath.Join(dir, "information"), []byte(information), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	setup := func(t *testing.T) (string, string) {
		t.Helper()
		hostProc := t.TempDir()
		root := filepath.Join(hostProc, "42", "root")
		if err := os.MkdirAll(filepath.Join(root, "dev"), 0o755); err != nil {
			t.Fatal(err)
		}
		// Minors do not follow bus order, as on real nodes.
		writeCard(t, root, "0000:29:00.0", "GPU-b", "0")
		writeCard(t, root, "0000:18:00.0", "GPU-a", "6")
		writeCard(t, root, "0000:3a:00.0", "GPU-hidden", "3")
		for _, minor := range []string{"0", "6"} {
			if err := os.WriteFile(filepath.Join(root, "dev", "nvidia"+minor), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return hostProc, root
	}

	t.Run("cards the container can open, in bus order", func(t *testing.T) {
		hostProc, _ := setup(t)
		visible, ok := listProcfsGPUs(hostProc, 42)
		if !ok {
			t.Fatal("expected a procfs answer")
		}
		var uuids []string
		for _, device := range visible.GPUs.Devices {
			uuids = append(uuids, device.UUID)
			if device.ProductName != "NVIDIA B200" {
				t.Fatalf("product name = %q", device.ProductName)
			}
		}
		if strings.Join(uuids, ",") != "GPU-a,GPU-b" {
			t.Fatalf("uuids = %v", uuids)
		}
	})

	t.Run("MIG capability nodes defer to nvidia-smi", func(t *testing.T) {
		hostProc, root := setup(t)
		if err := os.MkdirAll(filepath.Join(root, "dev", "nvidia-caps"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "dev", "nvidia-caps", "nvidia-cap1"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, ok := listProcfsGPUs(hostProc, 42); ok {
			t.Fatal("expected no procfs answer with MIG capability nodes")
		}
	})

	t.Run("no readable procfs defers to nvidia-smi", func(t *testing.T) {
		if _, ok := listProcfsGPUs(t.TempDir(), 42); ok {
			t.Fatal("expected no procfs answer")
		}
	})
}
