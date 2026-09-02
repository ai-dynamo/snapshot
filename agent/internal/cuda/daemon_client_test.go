/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

package cuda

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"

	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

func TestDaemonRequestMatchesSharedGoldenFixture(t *testing.T) {
	encodedFixture, err := os.ReadFile(filepath.Join("..", "..", "cmd", "cuda-checkpoint-helper", "testdata", "daemon_request_v6.hex"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := hex.DecodeString(strings.TrimSpace(string(encodedFixture)))
	if err != nil {
		t.Fatal(err)
	}
	identity := testDaemonIdentity(42)
	identity.StartTimeTicks = 12345
	identity.Cgroup = "0::/kubepods/test\n"
	got, err := daemonRequest(helperAction{
		PID: 42, Action: actionRestore,
		DeviceMap:   "GPU-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee=GPU-11111111-2222-3333-4444-555555555555",
		StorageMode: types.CUDAStorageModePOSIX,
		StorageDir:  "/checkpoints/process-nspid-42",
		JobFile:     "/host/proc/42/root/tmp/cuda-job",
		GPUUUIDs:    []string{"GPU-12345678-1234-1234-1234-123456789abc"},
		Transfer:    types.CUDATransferSettings{BufferCount: 2, ChunkBytes: 8 * 1024 * 1024},
		Identity:    identity,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("daemonRequest() bytes differ from shared v6 fixture\n got: %x\nwant: %x", got, want)
	}
}

func daemonTestResponse(status int32, flags uint32) []byte {
	packet := make([]byte, daemonResponseHeader)
	binary.LittleEndian.PutUint32(packet[0:4], daemonProtocolMagic)
	binary.LittleEndian.PutUint16(packet[4:6], daemonProtocolVersion)
	binary.LittleEndian.PutUint16(packet[6:8], daemonResponseHeader)
	binary.LittleEndian.PutUint32(packet[8:12], uint32(status))
	binary.LittleEndian.PutUint32(packet[12:16], flags)
	return packet
}

func testDaemonIdentity(pid int) snapshotruntime.ProcessDetails {
	return snapshotruntime.ProcessDetails{
		ObservedPID: pid, OutermostPID: pid, InnermostPID: pid,
		NamespacePIDs: []int{pid}, StartTimeTicks: 12345,
		Cgroup: "0::/kubepods/test\n",
	}
}

func withOperationServer(t *testing.T, handler func(*net.UnixConn)) {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "helper.sock")
	oldSocket := daemonSocketPath
	daemonSocketPath = socket
	t.Cleanup(func() { daemonSocketPath = oldSocket })
	listener, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: socket, Net: "unixpacket"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		conn, err := listener.AcceptUnix()
		if err != nil {
			return
		}
		defer conn.Close()
		handler(conn)
	}()
}

func TestCommandRunnerSendsDaemonOperations(t *testing.T) {
	identity := testDaemonIdentity(42)
	transfer := types.CUDATransferSettings{BufferCount: 2, ChunkBytes: 8 * 1024 * 1024}
	for _, test := range []struct {
		action, backend, deviceMap, storageDir string
		wantAction, wantBackend                uint16
		wantStorageDir                         string
		wantBufferCount                        uint32
		wantChunkBytes                         uint64
	}{
		{action: actionLock, backend: types.CUDAStorageModeLegacy, storageDir: "/ignored", wantAction: daemonActionLock, wantBackend: 1},
		{action: actionCheckpoint, backend: types.CUDAStorageModeLegacy, storageDir: "/ignored", wantAction: daemonActionCheckpoint, wantBackend: 1},
		{
			action: actionRestore, backend: types.CUDAStorageModePOSIX, deviceMap: "0=1",
			storageDir: "/checkpoints/process-0000", wantAction: daemonActionRestore, wantBackend: 2,
			wantStorageDir: "/checkpoints/process-0000", wantBufferCount: 2, wantChunkBytes: 8 * 1024 * 1024,
		},
		{action: actionUnlock, backend: types.CUDAStorageModePOSIX, storageDir: "/ignored", wantAction: daemonActionUnlock, wantBackend: 2},
	} {
		t.Run(test.action, func(t *testing.T) {
			var gpuUUIDs []string
			if test.backend == types.CUDAStorageModePOSIX &&
				(test.action == actionCheckpoint || test.action == actionRestore) {
				gpuUUIDs = []string{"GPU-12345678-1234-1234-1234-123456789abc"}
			}
			request := make(chan []byte, 1)
			withOperationServer(t, func(conn *net.UnixConn) {
				packet := make([]byte, daemonMaxRequest)
				n, err := conn.Read(packet)
				if err != nil {
					t.Error(err)
					return
				}
				request <- packet[:n]
				_, _ = conn.Write(daemonTestResponse(0, 0))
			})
			if err := (commandHelperActionRunner{}).run(
				context.Background(), helperAction{
					PID: 42, Action: test.action, DeviceMap: test.deviceMap,
					StorageMode: test.backend, StorageDir: test.storageDir,
					JobFile: "/host/proc/42/root/tmp/cuda-job", Transfer: transfer,
					GPUUUIDs: gpuUUIDs,
					Identity: identity,
				}, logr.Discard(),
			); err != nil {
				t.Fatalf("run() error = %v", err)
			}

			packet := <-request
			if got := binary.LittleEndian.Uint16(packet[8:10]); got != test.wantAction {
				t.Errorf("action = %d, want %d", got, test.wantAction)
			}
			if got := binary.LittleEndian.Uint16(packet[10:12]); got != test.wantBackend {
				t.Errorf("backend = %d, want %d", got, test.wantBackend)
			}
			if got := binary.LittleEndian.Uint32(packet[12:16]); got != 42 {
				t.Errorf("pid = %d, want 42", got)
			}
			if got := binary.LittleEndian.Uint32(packet[16:20]); got != test.wantBufferCount {
				t.Errorf("buffer count = %d, want %d", got, test.wantBufferCount)
			}
			if got := binary.LittleEndian.Uint64(packet[20:28]); got != test.wantChunkBytes {
				t.Errorf("chunk bytes = %d, want %d", got, test.wantChunkBytes)
			}
			deviceMapSize := int(binary.LittleEndian.Uint32(packet[28:32]))
			storageDirSize := int(binary.LittleEndian.Uint32(packet[32:36]))
			cgroupSize := int(binary.LittleEndian.Uint32(packet[36:40]))
			jobFileSize := int(binary.LittleEndian.Uint32(packet[48:52]))
			selectedDevicesSize := int(binary.LittleEndian.Uint32(packet[52:56]))
			if got := binary.LittleEndian.Uint64(packet[40:48]); got != identity.StartTimeTicks {
				t.Errorf("start time = %d, want %d", got, identity.StartTimeTicks)
			}
			payload := packet[daemonRequestHeader:]
			if got := string(payload[:deviceMapSize]); got != test.deviceMap {
				t.Errorf("device map = %q, want %q", got, test.deviceMap)
			}
			payload = payload[deviceMapSize:]
			if got := string(payload[:storageDirSize]); got != test.wantStorageDir {
				t.Errorf("storage directory = %q, want %q", got, test.wantStorageDir)
			}
			payload = payload[storageDirSize:]
			if got := string(payload[:cgroupSize]); got != identity.Cgroup {
				t.Errorf("cgroup = %q, want %q", got, identity.Cgroup)
			}
			payload = payload[cgroupSize:]
			if got := string(payload[:jobFileSize]); got != "/host/proc/42/root/tmp/cuda-job" {
				t.Errorf("job file = %q, want host-visible launch-job path", got)
			}
			payload = payload[jobFileSize:]
			wantSelectedDevices := ""
			if test.backend == types.CUDAStorageModePOSIX &&
				(test.action == actionCheckpoint || test.action == actionRestore) {
				wantSelectedDevices = "GPU-12345678-1234-1234-1234-123456789abc"
			}
			if got := string(payload[:selectedDevicesSize]); got != wantSelectedDevices {
				t.Errorf("selected devices = %q, want %q", got, wantSelectedDevices)
			}
		})
	}
}

func TestDaemonRequestRejectsBackendArgumentMismatch(t *testing.T) {
	identity := testDaemonIdentity(42)
	transfer := types.CUDATransferSettings{BufferCount: 1, ChunkBytes: types.DefaultCUDATransferChunkBytes}
	for _, test := range []struct {
		name, backend, storageDir string
	}{
		{name: "regular with directory", backend: types.CUDAStorageModeLegacy, storageDir: "/checkpoints/process-0000"},
		{name: "posix without directory", backend: types.CUDAStorageModePOSIX},
		{name: "unknown", backend: "auto"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := daemonRequest(helperAction{PID: 42, Action: actionCheckpoint, StorageMode: test.backend, StorageDir: test.storageDir, Transfer: transfer, Identity: identity}); err == nil {
				t.Fatal("daemonRequest() accepted mismatched backend arguments")
			}
		})
	}
}

func TestDaemonRequestRejectsRelativeJobFile(t *testing.T) {
	identity := testDaemonIdentity(42)
	_, err := daemonRequest(helperAction{PID: 42, Action: actionCheckpoint,
		StorageMode: types.CUDAStorageModeLegacy, JobFile: "tmp/cuda-job", Identity: identity})
	if err == nil || !strings.Contains(err.Error(), "job file") {
		t.Fatalf("daemonRequest() error = %v, want invalid job-file rejection", err)
	}
}

func TestDaemonRequestRejectsParserInvariantViolations(t *testing.T) {
	identity := testDaemonIdentity(42)
	validTransfer := types.CUDATransferSettings{BufferCount: 1, ChunkBytes: types.DefaultCUDATransferChunkBytes}
	validUUID := "GPU-12345678-1234-1234-1234-123456789abc"
	for _, test := range []struct {
		name            string
		action          string
		deviceMap       string
		storageDir      string
		jobFile         string
		selectedDevices []string
		transfer        types.CUDATransferSettings
		identity        snapshotruntime.ProcessDetails
	}{
		{name: "checkpoint device map", action: actionCheckpoint, deviceMap: "GPU-a=GPU-b", storageDir: "/checkpoints/process", selectedDevices: []string{validUUID}, transfer: validTransfer, identity: identity},
		{name: "zero transfer settings", action: actionRestore, storageDir: "/checkpoints/process", selectedDevices: []string{validUUID}, identity: identity},
		{name: "case-insensitive duplicate UUID", action: actionRestore, storageDir: "/checkpoints/process", selectedDevices: []string{validUUID, strings.ToUpper(validUUID)}, transfer: validTransfer, identity: identity},
		{name: "NUL in storage path", action: actionRestore, storageDir: "/checkpoints/process\x00other", selectedDevices: []string{validUUID}, transfer: validTransfer, identity: identity},
		{name: "NUL in job file", action: actionRestore, storageDir: "/checkpoints/process", jobFile: "/tmp/job\x00other", selectedDevices: []string{validUUID}, transfer: validTransfer, identity: identity},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := daemonRequest(helperAction{PID: 42, Action: test.action, DeviceMap: test.deviceMap,
				StorageMode: types.CUDAStorageModePOSIX, StorageDir: test.storageDir, JobFile: test.jobFile,
				GPUUUIDs: test.selectedDevices, Transfer: test.transfer, Identity: test.identity})
			if err == nil {
				t.Fatal("daemonRequest() accepted a request rejected by the daemon parser")
			}
		})
	}
}

func withHealthServer(t *testing.T, flags uint32) {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "helper.sock")
	oldSocket := daemonSocketPath
	daemonSocketPath = socket
	t.Cleanup(func() { daemonSocketPath = oldSocket })
	listener, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: socket + ".health", Net: "unixpacket"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		conn, err := listener.AcceptUnix()
		if err != nil {
			return
		}
		defer conn.Close()
		request := make([]byte, daemonMaxRequest)
		_, _ = conn.Read(request)
		_, _ = conn.Write(daemonTestResponse(0, flags|daemonCapabilityDeferredCUDA))
	}()
}

func TestWaitForDaemonRetriesUntilHealthListenerIsReady(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "helper.sock")
	oldSocket := daemonSocketPath
	daemonSocketPath = socket
	t.Cleanup(func() { daemonSocketPath = oldSocket })
	serverErr := make(chan error, 1)
	go func() {
		time.Sleep(2 * daemonHealthRetryInterval)
		listener, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: socket + ".health", Net: "unixpacket"})
		if err != nil {
			serverErr <- err
			return
		}
		defer listener.Close()
		conn, err := listener.AcceptUnix()
		if err == nil {
			defer conn.Close()
			request := make([]byte, daemonMaxRequest)
			_, _ = conn.Read(request)
			_, err = conn.Write(daemonTestResponse(0, daemonCapabilityDeferredCUDA))
		}
		serverErr <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := WaitForDaemon(ctx, types.CUDAStorageModeLegacy); err != nil {
		t.Fatalf("WaitForDaemon() error = %v, want retry success", err)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("health server error = %v", err)
	}
}

func TestWaitForDaemonHonorsCallerCancellation(t *testing.T) {
	oldSocket := daemonSocketPath
	daemonSocketPath = filepath.Join(t.TempDir(), "missing.sock")
	t.Cleanup(func() { daemonSocketPath = oldSocket })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := WaitForDaemon(ctx, types.CUDAStorageModeLegacy)
	if err == nil || !strings.Contains(err.Error(), "wait for CUDA helper daemon") {
		t.Fatalf("WaitForDaemon() error = %v, want bounded readiness failure", err)
	}
}

func TestValidateCUDAStorageModeCapabilitiesRequiresConfiguredBackend(t *testing.T) {
	if err := validateCUDAStorageModeCapabilities(types.CUDAStorageModeLegacy, daemonCapabilityDeferredCUDA); err != nil {
		t.Fatalf("legacy capability validation failed: %v", err)
	}
	err := validateCUDAStorageModeCapabilities(types.CUDAStorageModePOSIX, daemonCapabilityDeferredCUDA)
	if err == nil || !strings.Contains(err.Error(), "requires daemon CustomStorage capability") {
		t.Fatalf("POSIX capability validation error = %v, want missing CustomStorage capability", err)
	}
	if err := validateCUDAStorageModeCapabilities(types.CUDAStorageModePOSIX, daemonCapabilityDeferredCUDA|daemonCapabilityCustomStorage); err != nil {
		t.Fatalf("POSIX capability validation failed: %v", err)
	}
}

func TestSelectCUDAStorageModeRequiresExplicitOptIn(t *testing.T) {
	for _, test := range []struct {
		name       string
		configured string
		flags      uint32
		want       string
		wantError  string
	}{
		{
			name:       "legacy does not auto-enable CustomStorage",
			configured: types.CUDAStorageModeLegacy,
			flags:      daemonCapabilityCustomStorage,
			want:       types.CUDAStorageModeLegacy,
		},
		{
			name:       "posix requires capability",
			configured: types.CUDAStorageModePOSIX,
			wantError:  "requires daemon CustomStorage capability",
		},
		{
			name:       "posix selects CustomStorage when capable",
			configured: types.CUDAStorageModePOSIX,
			flags:      daemonCapabilityCustomStorage,
			want:       types.CUDAStorageModePOSIX,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			oldWaitForDaemon := waitForDaemon
			waitForDaemon = func(_ context.Context, storageMode string) error {
				return validateCUDAStorageModeCapabilities(
					storageMode,
					test.flags|daemonCapabilityDeferredCUDA,
				)
			}
			t.Cleanup(func() { waitForDaemon = oldWaitForDaemon })
			got, err := SelectCUDAStorageMode(context.Background(), test.configured)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("SelectCUDAStorageMode() error = %v, want %q", err, test.wantError)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("SelectCUDAStorageMode() = %q, %v; want %q", got, err, test.want)
			}
			manifest := types.NewCUDAManifest([]int{42}, []string{"GPU-aaa"}, got)
			if manifest.StorageMode != test.want {
				t.Fatalf("checkpoint manifest mode = %q, want %q", manifest.StorageMode, test.want)
			}
		})
	}
}

func TestValidateCUDAStorageModeObeysManifest(t *testing.T) {
	withHealthServer(t, daemonCapabilityCustomStorage)
	if err := ValidateCUDAStorageMode(context.Background(), types.CUDAStorageModeLegacy); err != nil {
		t.Fatalf("legacy restore on capable daemon failed: %v", err)
	}

	withHealthServer(t, 0)
	err := ValidateCUDAStorageMode(context.Background(), types.CUDAStorageModePOSIX)
	if err == nil || !strings.Contains(err.Error(), "requires daemon CustomStorage capability") {
		t.Fatalf("POSIX restore error = %v, want capability rejection", err)
	}
}

func TestRunDaemonActionMapsFatalResponse(t *testing.T) {
	withOperationServer(t, func(conn *net.UnixConn) {
		request := make([]byte, daemonMaxRequest)
		_, _ = conn.Read(request)
		_, _ = conn.Write(daemonTestResponse(2, daemonResponseFatal))
	})
	err := runDaemonAction(context.Background(), helperAction{PID: 42, Action: actionCheckpoint,
		StorageMode: types.CUDAStorageModeLegacy, Identity: testDaemonIdentity(42)}, logr.Discard())
	if !errors.Is(err, errDaemonFatal) {
		t.Fatalf("runDaemonAction() error = %v, want errDaemonFatal", err)
	}
}

func TestRunDaemonActionMapsLockTimeoutBeforeMutation(t *testing.T) {
	withOperationServer(t, func(conn *net.UnixConn) {
		request := make([]byte, daemonMaxRequest)
		_, _ = conn.Read(request)
		_, _ = conn.Write(daemonTestResponse(600, daemonResponseLockNotAcquired))
	})
	err := runDaemonAction(context.Background(), helperAction{PID: 42, Action: actionLock,
		StorageMode: types.CUDAStorageModeLegacy, Identity: testDaemonIdentity(42)}, logr.Discard())
	if !errors.Is(err, errCheckpointLockNotAcquired) {
		t.Fatalf("runDaemonAction() error = %v, want pre-mutation lock timeout", err)
	}
}

func TestRunDaemonActionPreservesFatalLockRejectionClassification(t *testing.T) {
	withOperationServer(t, func(conn *net.UnixConn) {
		request := make([]byte, daemonMaxRequest)
		_, _ = conn.Read(request)
		_, _ = conn.Write(daemonTestResponse(600, daemonResponseFatal|daemonResponseLockNotAcquired))
	})
	err := runDaemonAction(context.Background(), helperAction{PID: 42, Action: actionLock,
		StorageMode: types.CUDAStorageModeLegacy, Identity: testDaemonIdentity(42)}, logr.Discard())
	if !errors.Is(err, errCheckpointLockNotAcquired) || !errors.Is(err, errDaemonFatal) {
		t.Fatalf("runDaemonAction() error = %v, want both pre-mutation and fatal classifications", err)
	}
}

func TestValidateCUDAOperationBudgetRejectsShortCallerDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	err := validateCUDAOperationBudget(ctx, actionCheckpoint, 1)
	if err == nil || !strings.Contains(err.Error(), "before state-changing work") {
		t.Fatalf("validateCUDAOperationBudget() error = %v, want caller-budget rejection", err)
	}
}

func TestValidateCUDAOperationBudgetCoversWholeMultiTargetSequence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*daemonRPCTimeout)
	defer cancel()
	err := validateCUDAOperationBudget(ctx, actionCheckpoint, 2)
	if err == nil || !strings.Contains(err.Error(), "for 2 target(s)") {
		t.Fatalf("validateCUDAOperationBudget() error = %v, want whole-sequence rejection", err)
	}
}

func TestValidateCUDAOperationBudgetAllowsOneTargetRestoreAtDefaultDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	if err := validateCUDAOperationBudget(ctx, actionRestore, 1); err != nil {
		t.Fatalf("validateCUDAOperationBudget() error = %v, want default one-target restore to fit", err)
	}
}

func TestValidateCUDAOperationBudgetAllowsQualifiedTwoTargetRestoreAtChartDefault(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 135*time.Minute)
	defer cancel()
	if err := validateCUDAOperationBudget(ctx, actionRestore, 2); err != nil {
		t.Fatalf("validateCUDAOperationBudget() error = %v, want qualified two-PID restore to fit chart default", err)
	}
}

func TestValidateCUDAOperationBudgetCoversAllRestoreTargets(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*daemonRPCTimeout)
	defer cancel()
	err := validateCUDAOperationBudget(ctx, actionRestore, 4)
	if err == nil || !strings.Contains(err.Error(), "for 4 target(s)") {
		t.Fatalf("validateCUDAOperationBudget() error = %v, want whole restore-sequence rejection", err)
	}
}

func TestRunDaemonActionAllowsUnlockWithShortCallerDeadline(t *testing.T) {
	withOperationServer(t, func(conn *net.UnixConn) {
		request := make([]byte, daemonMaxRequest)
		_, _ = conn.Read(request)
		_, _ = conn.Write(daemonTestResponse(0, 0))
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := runDaemonAction(ctx, helperAction{PID: 42, Action: actionUnlock,
		StorageMode: types.CUDAStorageModeLegacy, Identity: testDaemonIdentity(42)}, logr.Discard()); err != nil {
		t.Fatalf("runDaemonAction(unlock) error = %v", err)
	}
}

func TestRunDaemonActionRejectsInvalidResponses(t *testing.T) {
	for _, test := range []struct {
		name     string
		response func() []byte
		want     string
	}{
		{
			name: "malformed payload lengths",
			response: func() []byte {
				packet := daemonTestResponse(0, 0)
				binary.LittleEndian.PutUint32(packet[16:20], 1)
				return packet
			},
			want: "payload lengths",
		},
		{
			name: "oversized packet",
			response: func() []byte {
				return make([]byte, daemonMaxResponse+1)
			},
			want: "exceeded",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			withOperationServer(t, func(conn *net.UnixConn) {
				request := make([]byte, daemonMaxRequest)
				_, _ = conn.Read(request)
				_, _ = conn.Write(test.response())
			})
			err := runDaemonAction(context.Background(), helperAction{PID: 42, Action: actionCheckpoint,
				StorageMode: types.CUDAStorageModeLegacy, Identity: testDaemonIdentity(42)}, logr.Discard())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("runDaemonAction() error = %v, want error containing %q", err, test.want)
			}
		})
	}
}

func TestRunDaemonActionDisconnectAfterSendIsNotReplayed(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "helper.sock")
	oldSocket := daemonSocketPath
	daemonSocketPath = socket
	t.Cleanup(func() { daemonSocketPath = oldSocket })
	listener, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: socket, Net: "unixpacket"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	accepted := make(chan int, 1)
	go func() {
		count := 0
		conn, acceptErr := listener.AcceptUnix()
		if acceptErr != nil {
			accepted <- count
			return
		}
		count++
		request := make([]byte, daemonMaxRequest)
		_, _ = conn.Read(request)
		_ = conn.Close()
		_ = listener.SetDeadline(time.Now().Add(100 * time.Millisecond))
		conn, acceptErr = listener.AcceptUnix()
		if acceptErr == nil {
			count++
			_ = conn.Close()
		}
		accepted <- count
	}()
	err = runDaemonAction(context.Background(), helperAction{PID: 42, Action: actionCheckpoint,
		StorageMode: types.CUDAStorageModeLegacy, Identity: testDaemonIdentity(42)}, logr.Discard())
	if err == nil || !strings.Contains(err.Error(), "outcome is unknown and will not be replayed") {
		t.Fatalf("runDaemonAction() error = %v, want ambiguous non-replayable operation error", err)
	}
	if errors.Is(err, errDaemonUnavailable) {
		t.Fatalf("disconnect after request must not be treated as pre-send unavailability: %v", err)
	}
	if count := <-accepted; count != 1 {
		t.Fatalf("daemon accepted %d operation requests after an ambiguous disconnect, want 1", count)
	}
}
