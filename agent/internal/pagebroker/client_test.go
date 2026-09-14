// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pagebroker

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
)

func TestFailureCodeMapsUnknownValuesToUnspecified(t *testing.T) {
	if got := failureCode(Failure_Code(99)); got != Failure_UNSPECIFIED {
		t.Fatalf("failureCode(99) = %v, want %v", got, Failure_UNSPECIFIED)
	}
}

func TestCudaTargetsPreserveIdentityAndDeviceScope(t *testing.T) {
	targets, err := cudaTargets([]CUDATarget{{
		HostPID: 1001, NamespacePID: 42, StartTimeTicks: 987654,
		Cgroup: "0::/kubepods/pod/container\n", DeviceMap: "GPU-a=GPU-b",
		SelectedDevices: []string{"GPU-b", "GPU-c"},
	}}, CudaStorageBackend_CUDA_STORAGE_BACKEND_POSIX_CUSTOM_STORAGE)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 {
		t.Fatalf("encoded targets = %d, want 1", len(targets))
	}
	got := targets[0]
	if got.GetHostPid() != 1001 || got.GetNamespacePid() != 42 ||
		got.GetStartTimeTicks() != 987654 || got.GetCgroup() != "0::/kubepods/pod/container\n" ||
		got.GetDeviceMap() != "GPU-a=GPU-b" || len(got.GetSelectedDevices()) != 2 {
		t.Fatalf("encoded CUDA target = %#v", got)
	}
}

func TestCudaTargetsRejectIncompleteIdentityBeforeDial(t *testing.T) {
	valid := CUDATarget{
		HostPID: 1001, NamespacePID: 42, StartTimeTicks: 987654,
		Cgroup: "0::/kubepods/pod/container\n", SelectedDevices: []string{"GPU-a"},
	}
	tests := []struct {
		name   string
		mutate func(*CUDATarget)
	}{
		{name: "host pid", mutate: func(target *CUDATarget) { target.HostPID = 0 }},
		{name: "namespace pid", mutate: func(target *CUDATarget) { target.NamespacePID = 0 }},
		{name: "start time", mutate: func(target *CUDATarget) { target.StartTimeTicks = 0 }},
		{name: "cgroup", mutate: func(target *CUDATarget) { target.Cgroup = "" }},
		{name: "selected devices", mutate: func(target *CUDATarget) { target.SelectedDevices = nil }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			target := valid
			tc.mutate(&target)
			if _, err := cudaTargets(
				[]CUDATarget{target},
				CudaStorageBackend_CUDA_STORAGE_BACKEND_POSIX_CUSTOM_STORAGE,
			); err == nil {
				t.Fatal("cudaTargets() accepted incomplete identity")
			}
		})
	}
	if _, err := cudaTargets(nil, CudaStorageBackend_CUDA_STORAGE_BACKEND_REGULAR); err == nil {
		t.Fatal("cudaTargets() accepted an empty batch")
	}
	regular := valid
	regular.SelectedDevices = nil
	if _, err := cudaTargets([]CUDATarget{regular}, CudaStorageBackend_CUDA_STORAGE_BACKEND_REGULAR); err != nil {
		t.Fatalf("regular CUDA target unexpectedly required selected devices: %v", err)
	}
}

func TestCUDAOperationsRejectUnspecifiedStorageBackendBeforeDial(t *testing.T) {
	target := CUDATarget{
		HostPID: 1001, NamespacePID: 42, StartTimeTicks: 987654,
		Cgroup: "0::/kubepods/pod/container\n",
	}
	client := Client{ControlSocketPath: filepath.Join(t.TempDir(), "missing.sock")}
	if _, err := client.CUDACheckpoint(
		context.Background(), "transaction", []CUDATarget{target},
		CudaStorageBackend_CUDA_STORAGE_BACKEND_UNSPECIFIED, false, false,
	); err == nil {
		t.Fatal("CUDACheckpoint() accepted an unspecified storage backend")
	} else {
		var transport transportError
		if errors.As(err, &transport) {
			t.Fatalf("CUDACheckpoint() dialed before rejecting backend: %v", err)
		}
	}
	if err := client.CUDARestore(
		context.Background(), "transaction", []CUDATarget{target},
		CudaStorageBackend_CUDA_STORAGE_BACKEND_UNSPECIFIED, false, false, nil,
	); err == nil {
		t.Fatal("CUDARestore() accepted an unspecified storage backend")
	} else {
		var transport transportError
		if errors.As(err, &transport) {
			t.Fatalf("CUDARestore() dialed before rejecting backend: %v", err)
		}
	}
}

func TestDirectRestoreRejectsInvalidNamespacePIDsBeforeDial(t *testing.T) {
	client := Client{ControlSocketPath: filepath.Join(t.TempDir(), "missing.sock")}
	identity := RegularRestoreIdentity{
		PodUID: "pod-uid", DestinationContainer: "engine",
		ContentUID: "content-uid", SourceContainer: "worker",
		ContainerID: "containerd://0123456789abcdef",
	}
	for _, pids := range [][]int{nil, {0}, {42, 42}} {
		if _, err := client.DirectRestore(
			context.Background(), "transaction", "/checkpoints/source", pids,
			identity,
		); err == nil {
			t.Fatalf("DirectRestore() accepted invalid namespace PIDs %v", pids)
		} else {
			var transport transportError
			if errors.As(err, &transport) {
				t.Fatalf("DirectRestore() dialed before rejecting %v: %v", pids, err)
			}
		}
	}
}

func TestDirectRestoreRejectsInvalidDestinationIdentityBeforeDial(t *testing.T) {
	client := Client{ControlSocketPath: filepath.Join(t.TempDir(), "missing.sock")}
	if _, err := client.DirectRestore(
		context.Background(), "transaction", "/checkpoints/source", []int{42},
		RegularRestoreIdentity{},
	); err == nil {
		t.Fatal("DirectRestore() accepted an empty destination identity")
	} else {
		var transport transportError
		if errors.As(err, &transport) {
			t.Fatalf("DirectRestore() dialed before identity validation: %v", err)
		}
	}
}

func TestDirectRestorePreservesDestinationIdentity(t *testing.T) {
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "pagebroker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	server := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			server <- err
			return
		}
		defer connection.Close()
		message, err := readMessage(connection)
		if err != nil {
			server <- err
			return
		}
		request := new(Request)
		if err = proto.Unmarshal(message, request); err != nil {
			server <- err
			return
		}
		operation := request.GetDirectRestore()
		identity := operation.GetIdentity()
		if operation == nil || len(operation.GetCudaNamespacePids()) != 1 ||
			operation.GetCudaNamespacePids()[0] != 42 ||
			identity.GetPodUid() != "pod-uid" ||
			identity.GetDestinationContainer() != "engine" ||
			identity.GetContentUid() != "content-uid" ||
			identity.GetSourceContainer() != "worker" ||
			identity.GetContainerId() != "containerd://0123456789abcdef" {
			server <- errors.New("direct restore destination identity was not preserved")
			return
		}
		imageDirectory := "/staged/direct"
		response, err := proto.Marshal(&Response{
			RequestId: request.RequestId, TransactionId: request.TransactionId,
			Result: &Response_StagedRestoreDirectory{StagedRestoreDirectory: &StagedRestoreDirectory{
				ImageDirectory: &imageDirectory,
			}},
		})
		if err == nil {
			err = writeMessage(connection, response)
		}
		server <- err
	}()

	got, err := (Client{ControlSocketPath: listener.Addr().String()}).DirectRestore(
		context.Background(), "transaction", "/checkpoints/source", []int{42},
		RegularRestoreIdentity{
			PodUID: "pod-uid", DestinationContainer: "engine",
			ContentUID: "content-uid", SourceContainer: "worker",
			ContainerID: "containerd://0123456789abcdef",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got != "/staged/direct" {
		t.Fatalf("staged path = %q, want /staged/direct", got)
	}
	if err := <-server; err != nil {
		t.Fatal(err)
	}
}

func TestReferenceRegularRestorePreservesDestinationIdentity(t *testing.T) {
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "pagebroker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	server := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			server <- err
			return
		}
		defer connection.Close()
		message, err := readMessage(connection)
		if err != nil {
			server <- err
			return
		}
		request := new(Request)
		if err = proto.Unmarshal(message, request); err != nil {
			server <- err
			return
		}
		operation := request.GetReferenceRegularRestore()
		if operation == nil || operation.GetPodUid() != "pod-uid" ||
			operation.GetDestinationContainer() != "engine" ||
			operation.GetContentUid() != "content-uid" ||
			operation.GetSourceContainer() != "worker" ||
			operation.GetContainerId() != "containerd://0123456789abcdef" {
			server <- errors.New("regular restore destination identity was not preserved")
			return
		}
		imageDirectory := "/staged/reference"
		response, err := proto.Marshal(&Response{
			RequestId: request.RequestId, TransactionId: request.TransactionId,
			Result: &Response_StagedRestoreDirectory{StagedRestoreDirectory: &StagedRestoreDirectory{
				ImageDirectory: &imageDirectory,
			}},
		})
		if err == nil {
			err = writeMessage(connection, response)
		}
		server <- err
	}()

	got, err := (Client{ControlSocketPath: listener.Addr().String()}).ReferenceRegularRestore(
		context.Background(), "transaction", "/checkpoints/source",
		RegularRestoreIdentity{
			PodUID: "pod-uid", DestinationContainer: "engine",
			ContentUID: "content-uid", SourceContainer: "worker",
			ContainerID: "containerd://0123456789abcdef",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got != "/staged/reference" {
		t.Fatalf("staged path = %q, want /staged/reference", got)
	}
	if err := <-server; err != nil {
		t.Fatal(err)
	}
}

func TestReferenceRegularRestoreRejectsInvalidDestinationIdentityBeforeDial(t *testing.T) {
	valid := []string{"pod-uid", "engine", "content-uid", "worker", "containerd://0123456789abcdef"}
	tests := []struct {
		name  string
		field int
		value string
	}{
		{name: "empty", field: 0, value: ""},
		{name: "too long", field: 1, value: strings.Repeat("x", maxRestoreIdentityFieldBytes+1)},
		{name: "control", field: 2, value: "content\nuid"},
		{name: "invalid UTF-8", field: 3, value: string([]byte{0xff})},
		{name: "empty container ID", field: 4, value: ""},
	}
	client := Client{ControlSocketPath: filepath.Join(t.TempDir(), "missing.sock")}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fields := append([]string(nil), valid...)
			fields[tc.field] = tc.value
			_, err := client.ReferenceRegularRestore(
				context.Background(), "transaction", "/checkpoints/source",
				RegularRestoreIdentity{
					PodUID: fields[0], DestinationContainer: fields[1],
					ContentUID: fields[2], SourceContainer: fields[3],
					ContainerID: fields[4],
				},
			)
			if err == nil {
				t.Fatal("ReferenceRegularRestore() accepted invalid destination identity")
			}
			var transport transportError
			if errors.As(err, &transport) {
				t.Fatalf("ReferenceRegularRestore() dialed before validation: %v", err)
			}
		})
	}
}

func TestCUDAOperationsEncodeStorageBackend(t *testing.T) {
	target := CUDATarget{
		HostPID: 1001, NamespacePID: 42, StartTimeTicks: 987654,
		Cgroup: "0::/kubepods/pod/container\n", SelectedDevices: []string{"GPU-a"},
	}
	tests := []struct {
		name       string
		backend    CudaStorageBackend
		restore    bool
		getBackend func(*Request) CudaStorageBackend
	}{
		{
			name: "checkpoint regular", backend: CudaStorageBackend_CUDA_STORAGE_BACKEND_REGULAR,
			getBackend: func(request *Request) CudaStorageBackend {
				return request.GetCudaCheckpoint().GetStorageBackend()
			},
		},
		{
			name: "restore POSIX CustomStorage", backend: CudaStorageBackend_CUDA_STORAGE_BACKEND_POSIX_CUSTOM_STORAGE,
			restore: true,
			getBackend: func(request *Request) CudaStorageBackend {
				return request.GetCudaRestore().GetStorageBackend()
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "pagebroker.sock"))
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()

			server := make(chan error, 1)
			go func() {
				connection, err := listener.Accept()
				if err != nil {
					server <- err
					return
				}
				defer connection.Close()
				message, err := readMessage(connection)
				if err != nil {
					server <- err
					return
				}
				request := new(Request)
				if err := proto.Unmarshal(message, request); err != nil {
					server <- err
					return
				}
				if got := tc.getBackend(request); got != tc.backend {
					server <- errors.New("request did not preserve the CUDA storage backend")
					return
				}
				response := &Response{
					RequestId: request.RequestId, TransactionId: request.TransactionId,
					Result: &Response_CudaOperationComplete{
						CudaOperationComplete: &CudaOperationComplete{TargetCount: proto.Uint32(1)},
					},
				}
				message, err = proto.Marshal(response)
				if err == nil {
					err = writeMessage(connection, message)
				}
				server <- err
			}()

			client := Client{ControlSocketPath: listener.Addr().String()}
			if tc.restore {
				err = client.CUDARestore(
					context.Background(), "transaction", []CUDATarget{target},
					tc.backend, false, false, nil,
				)
			} else {
				_, err = client.CUDACheckpoint(
					context.Background(), "transaction", []CUDATarget{target},
					tc.backend, false, false,
				)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := <-server; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBeginRestoreEncodesBackendAndRequiresAdmissionResponse(t *testing.T) {
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "pb.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			server <- err
			return
		}
		defer connection.Close()
		message, err := readMessage(connection)
		request := new(Request)
		if err == nil {
			err = proto.Unmarshal(message, request)
		}
		if err == nil && (request.GetBeginRestore().GetStorageBackend() != CudaStorageBackend_CUDA_STORAGE_BACKEND_REGULAR ||
			request.GetBeginRestore().GetTargetCount() != 4) {
			err = errors.New("begin restore backend or target count was not preserved")
		}
		if err == nil {
			message, err = proto.Marshal(&Response{
				RequestId: request.RequestId, TransactionId: request.TransactionId,
				Result: &Response_RestoreAdmissionGranted{RestoreAdmissionGranted: &RestoreAdmissionGranted{}},
			})
		}
		if err == nil {
			err = writeMessage(connection, message)
		}
		server <- err
	}()

	client := Client{ControlSocketPath: listener.Addr().String()}
	if err := client.BeginRestore(
		context.Background(), "transaction",
		CudaStorageBackend_CUDA_STORAGE_BACKEND_REGULAR, 4,
	); err != nil {
		t.Fatal(err)
	}
	if err := <-server; err != nil {
		t.Fatal(err)
	}
}

func TestBeginRestoreRejectsInvalidTargetCountBeforeDial(t *testing.T) {
	client := Client{ControlSocketPath: filepath.Join(t.TempDir(), "missing.sock")}
	for _, count := range []int{0, 65} {
		if err := client.BeginRestore(
			context.Background(), "transaction",
			CudaStorageBackend_CUDA_STORAGE_BACKEND_REGULAR, count,
		); err == nil {
			t.Fatalf("BeginRestore() accepted target count %d", count)
		}
	}
}

func TestActivateRestorePreservesIdentityAndRequiresGrantedResponse(t *testing.T) {
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "pb.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			server <- err
			return
		}
		defer connection.Close()
		message, err := readMessage(connection)
		request := new(Request)
		if err == nil {
			err = proto.Unmarshal(message, request)
		}
		identity := request.GetActivateRestore().GetIdentity()
		if err == nil && (identity.GetPodUid() != "pod-uid" ||
			identity.GetDestinationContainer() != "engine" ||
			identity.GetContentUid() != "content-uid" ||
			identity.GetSourceContainer() != "worker" ||
			identity.GetContainerId() != "containerd://0123456789abcdef") {
			err = errors.New("restore activation identity was not preserved")
		}
		if err == nil {
			message, err = proto.Marshal(&Response{
				RequestId: request.RequestId, TransactionId: request.TransactionId,
				Result: &Response_RestoreActivationGranted{
					RestoreActivationGranted: &RestoreActivationGranted{},
				},
			})
		}
		if err == nil {
			err = writeMessage(connection, message)
		}
		server <- err
	}()

	err = (Client{ControlSocketPath: listener.Addr().String()}).ActivateRestore(
		context.Background(), "transaction", RegularRestoreIdentity{
			PodUID: "pod-uid", DestinationContainer: "engine",
			ContentUID: "content-uid", SourceContainer: "worker",
			ContainerID: "containerd://0123456789abcdef",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-server; err != nil {
		t.Fatal(err)
	}
}

func TestActivateRestoreRejectsMissingContainerIDBeforeDial(t *testing.T) {
	err := (Client{ControlSocketPath: filepath.Join(t.TempDir(), "missing.sock")}).ActivateRestore(
		context.Background(), "transaction", RegularRestoreIdentity{
			PodUID: "pod-uid", DestinationContainer: "engine",
			ContentUID: "content-uid", SourceContainer: "worker",
		},
	)
	if err == nil {
		t.Fatal("ActivateRestore() accepted a missing container ID")
	}
	var transport transportError
	if errors.As(err, &transport) {
		t.Fatalf("ActivateRestore() dialed before validation: %v", err)
	}
}

func TestBeginCheckpointEncodesExactAdmission(t *testing.T) {
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "pb.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			server <- err
			return
		}
		defer connection.Close()
		message, err := readMessage(connection)
		request := new(Request)
		if err == nil {
			err = proto.Unmarshal(message, request)
		}
		if err != nil {
			server <- err
			return
		}
		if request.GetBeginCheckpoint().GetStorageBackend() != CudaStorageBackend_CUDA_STORAGE_BACKEND_REGULAR ||
			request.GetBeginCheckpoint().GetTargetCount() != 4 {
			server <- errors.New("begin checkpoint backend or target count was not preserved")
			return
		}
		message, err = proto.Marshal(&Response{
			RequestId: request.RequestId, TransactionId: request.TransactionId,
			Result: &Response_CheckpointAdmissionGranted{
				CheckpointAdmissionGranted: &CheckpointAdmissionGranted{},
			},
		})
		if err == nil {
			err = writeMessage(connection, message)
		}
		server <- err
	}()

	client := Client{ControlSocketPath: listener.Addr().String()}
	if err := client.BeginCheckpoint(
		context.Background(), "transaction",
		CudaStorageBackend_CUDA_STORAGE_BACKEND_REGULAR, 4,
	); err != nil {
		t.Fatal(err)
	}
	if err := <-server; err != nil {
		t.Fatal(err)
	}
}

func TestCUDATargetMayBeMutatedFailsClosed(t *testing.T) {
	if CUDATargetMayBeMutated(nil) {
		t.Fatal("nil error reported target mutation")
	}
	if !CUDATargetMayBeMutated(failureError{code: Failure_CUDA_ERROR}) {
		t.Fatal("CUDA failure without an explicit mutation bit did not fail closed")
	}
	if !CUDATargetMayBeMutated(failureError{code: Failure_BUSY}) {
		t.Fatal("BUSY without an explicit mutation bit did not fail closed")
	}
	if !CUDATargetMayBeMutated(failureError{code: Failure_STORAGE_ERROR}) {
		t.Fatal("STORAGE_ERROR without an explicit mutation bit did not fail closed")
	}
	if !CUDATargetMayBeMutated(failureError{code: Failure_INTERNAL_ERROR}) {
		t.Fatal("INTERNAL_ERROR without an explicit mutation bit did not fail closed")
	}
	if CUDATargetMayBeMutated(failureError{
		code: Failure_STORAGE_ERROR, targetMutationKnown: true,
	}) {
		t.Fatal("explicit non-mutating storage failure reported mutation")
	}
	for _, code := range []Failure_Code{
		Failure_INVALID_REQUEST,
		Failure_TRANSACTION_NOT_FOUND,
		Failure_TRANSACTION_CONFLICT,
		Failure_INSUFFICIENT_STORAGE,
	} {
		if CUDATargetMayBeMutated(failureError{code: code}) {
			t.Fatalf("pre-dispatch failure %s reported mutation", code)
		}
	}
	if CUDATargetMayBeMutated(failureError{
		code: Failure_BUSY, targetMutationKnown: true,
	}) {
		t.Fatal("explicit non-mutating restore admission busy reported mutation")
	}
	if !CUDATargetMayBeMutated(failureError{
		code: Failure_CUDA_ERROR, targetMayBeMutated: true, targetMutationKnown: true,
	}) {
		t.Fatal("driver-classified mutation was lost")
	}
	if CUDATargetMayBeMutated(transportError{cause: errors.New("dial")}) {
		t.Fatal("failed dial reported mutation")
	}
	if !CUDATargetMayBeMutated(transportError{cause: errors.New("read"), mayHaveDispatched: true}) {
		t.Fatal("dispatched transport error did not fail closed")
	}
	if !CUDATargetMayBeMutated(cudaResponseError{cause: errors.New("shape")}) {
		t.Fatal("invalid success response did not fail closed")
	}
}

func TestTransactionNotFoundClassification(t *testing.T) {
	if !TransactionNotFound(failureError{code: Failure_TRANSACTION_NOT_FOUND}) {
		t.Fatal("transaction-not-found response was not recognized")
	}
	if TransactionNotFound(failureError{code: Failure_TRANSACTION_CONFLICT}) {
		t.Fatal("transaction conflict was classified as missing")
	}
	if TransactionNotFound(transportError{cause: errors.New("dial")}) {
		t.Fatal("transport failure was classified as missing")
	}
}

func TestRestoreAdmissionRetryableRequiresExactProof(t *testing.T) {
	busy := failureError{
		code: Failure_BUSY, targetMutationKnown: true,
	}
	if !RestoreAdmissionBusy(busy) {
		t.Fatal("correlated BUSY(false) was not classified as resumable")
	}
	if !RestoreAdmissionRetryable(busy) {
		t.Fatal("correlated BUSY(false) was not retryable")
	}
	for _, err := range []error{
		failureError{code: Failure_BUSY},
		failureError{code: Failure_BUSY, targetMutationKnown: true, targetMayBeMutated: true},
		failureError{code: Failure_CUDA_ERROR, targetMutationKnown: true},
		cudaResponseError{cause: errors.New("mismatched identifiers")},
	} {
		if RestoreAdmissionRetryable(err) {
			t.Fatalf("RestoreAdmissionRetryable(%v) = true", err)
		}
		if RestoreAdmissionBusy(err) {
			t.Fatalf("RestoreAdmissionBusy(%v) = true", err)
		}
	}
	if !RestoreAdmissionRetryable(transportError{cause: errors.New("dial")}) {
		t.Fatal("pre-dispatch PageBroker unavailability was not retryable")
	}
	if !RestoreAdmissionRetryable(transportError{cause: errors.New("read response"), mayHaveDispatched: true}) {
		t.Fatal("ambiguous BeginRestore transport outcome was not recoverable")
	}
	if RestoreAdmissionBusy(transportError{cause: errors.New("read response"), mayHaveDispatched: true}) {
		t.Fatal("ambiguous BeginRestore transport outcome was classified as correlated BUSY")
	}
}

func TestRestoreStagingFailureClassification(t *testing.T) {
	for _, err := range []error{
		transportError{cause: errors.New("dial")},
		transportError{cause: errors.New("read response"), mayHaveDispatched: true},
	} {
		if !RestoreStagingTransportRetryable(err) {
			t.Fatalf("staging transport failure %v was not retryable", err)
		}
	}
	if !RestoreStagingCapacityPending(failureError{code: Failure_INSUFFICIENT_STORAGE}) {
		t.Fatal("insufficient staging capacity was not pending")
	}
	if !RestoreStagingCapacityPending(failureError{
		code: Failure_BUSY, targetMutationKnown: true,
	}) {
		t.Fatal("proven non-mutating descriptor capacity was not pending")
	}
	for _, err := range []error{
		failureError{code: Failure_BUSY},
		failureError{code: Failure_BUSY, targetMutationKnown: true, targetMayBeMutated: true},
	} {
		if RestoreStagingCapacityPending(err) {
			t.Fatalf("unproven descriptor capacity %v was pending", err)
		}
	}
	if !RestoreStagingCleanupRequired(failureError{code: Failure_STORAGE_ERROR}) {
		t.Fatal("staging storage error did not require cleanup")
	}
	for _, err := range []error{
		failureError{code: Failure_INVALID_REQUEST},
		failureError{code: Failure_TRANSACTION_CONFLICT},
		failureError{code: Failure_INTERNAL_ERROR},
		cudaResponseError{cause: errors.New("invalid response")},
	} {
		if RestoreStagingTransportRetryable(err) || RestoreStagingCapacityPending(err) || RestoreStagingCleanupRequired(err) {
			t.Fatalf("terminal staging failure %v was reclassified", err)
		}
	}
}

func TestCppDaemonConnectionSaturationIsNonMutatingBusy(t *testing.T) {
	binaryPath := os.Getenv("PAGEBROKER_TEST_BINARY")
	if binaryPath == "" {
		t.Skip("PAGEBROKER_TEST_BINARY is not set")
	}
	root := t.TempDir()
	socketPath := filepath.Join(root, "pagebroker.sock")
	storagePath := filepath.Join(root, "storage")
	if err := os.Mkdir(storagePath, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(
		binaryPath, socketPath, filepath.Join(root, "staging"),
		storagePath, "--max-concurrent-requests", "1",
		"--max-staging-bytes", "1048576",
	)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			<-waited
		}
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case err := <-waited:
			cmd.Process = nil
			t.Fatalf("PageBroker exited before creating its socket: %v: %s", err, stderr.String())
		default:
		}
		if _, err := os.Stat(socketPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			err := <-waited
			cmd.Process = nil
			t.Fatalf("PageBroker socket was not created: %v: %s", err, stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	blocker, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	// Supply the frame size but not its body so the sole normal request handler
	// remains occupied without dispatching anything to Broker.
	if err := binary.Write(blocker, binary.BigEndian, uint32(1)); err != nil {
		t.Fatal(err)
	}

	client := Client{ControlSocketPath: socketPath}
	err = client.Abort(context.Background(), "saturated-transaction")
	var failure failureError
	if !errors.As(err, &failure) || failure.code != Failure_BUSY {
		t.Fatalf("Client.Abort() error = %v, want PageBroker BUSY", err)
	}
	if CUDATargetMayBeMutated(err) {
		t.Fatalf("connection saturation reported target mutation: %v", err)
	}

	// A saturated peer that connects but sends no frame must not hold the
	// daemon's accept loop through its normal 30-second request timeout or make
	// SIGINT shutdown unresponsive.
	slow, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	if err := slow.Close(); err != nil {
		t.Fatal(err)
	}
	if err := blocker.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("PageBroker exit: %v: %s", err, stderr.String())
		}
	case <-time.After(time.Second):
		t.Fatal("PageBroker shutdown blocked on a slow saturated connection")
	}
	cmd.Process = nil
}

func TestCUDAPostDispatchResponseErrorsFailClosed(t *testing.T) {
	target := CUDATarget{
		HostPID: 1001, NamespacePID: 42, StartTimeTicks: 987654,
		Cgroup: "0::/kubepods/pod/container\n", SelectedDevices: []string{"GPU-a"},
	}
	tests := []struct {
		name  string
		reply func(net.Conn, *Request) error
	}{
		{
			name: "oversized frame",
			reply: func(connection net.Conn, _ *Request) error {
				return binary.Write(connection, binary.BigEndian, uint32(maxMessageSize+1))
			},
		},
		{
			name: "malformed protobuf",
			reply: func(connection net.Conn, _ *Request) error {
				return writeMessage(connection, []byte{0xff})
			},
		},
		{
			name: "mismatched identifiers",
			reply: func(connection net.Conn, request *Request) error {
				otherRequestID := "other-request"
				message, err := proto.Marshal(&Response{
					RequestId: &otherRequestID, TransactionId: request.TransactionId,
					Result: &Response_CudaOperationComplete{
						CudaOperationComplete: &CudaOperationComplete{TargetCount: proto.Uint32(1)},
					},
				})
				if err != nil {
					return err
				}
				return writeMessage(connection, message)
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "pagebroker.sock"))
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()

			server := make(chan error, 1)
			go func() {
				connection, err := listener.Accept()
				if err != nil {
					server <- err
					return
				}
				defer connection.Close()
				message, err := readMessage(connection)
				if err != nil {
					server <- err
					return
				}
				request := new(Request)
				if err := proto.Unmarshal(message, request); err != nil {
					server <- err
					return
				}
				server <- tc.reply(connection, request)
			}()

			_, err = (Client{ControlSocketPath: listener.Addr().String()}).CUDACheckpoint(
				context.Background(), "transaction", []CUDATarget{target},
				CudaStorageBackend_CUDA_STORAGE_BACKEND_REGULAR, false, false,
			)
			if err == nil {
				t.Fatal("CUDACheckpoint() unexpectedly succeeded")
			}
			if !CUDATargetMayBeMutated(err) {
				t.Fatalf("CUDATargetMayBeMutated(%v) = false, want true", err)
			}
			if err := <-server; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRequestStopsWhenContextIsCanceled(t *testing.T) {
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "pagebroker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		connection, err := listener.Accept()
		if err == nil {
			accepted <- connection
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- (Client{ControlSocketPath: listener.Addr().String()}).Abort(ctx, "transaction")
	}()

	connection := <-accepted
	defer connection.Close()
	if _, err := readMessage(connection); err != nil {
		t.Fatal(err)
	}

	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("request succeeded after its context was canceled")
		}
	case <-time.After(time.Second):
		t.Fatal("request did not stop after its context was canceled")
	}
}

func TestCommitRetriesLostResponses(t *testing.T) {
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "pagebroker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	requests := make(chan *Request, 3)
	server := make(chan error, 1)
	go func() {
		for attempt := 0; attempt < 3; attempt++ {
			connection, err := listener.Accept()
			if err != nil {
				server <- err
				return
			}
			message, err := readMessage(connection)
			if err != nil {
				_ = connection.Close()
				server <- err
				return
			}
			request := new(Request)
			if err := proto.Unmarshal(message, request); err != nil {
				_ = connection.Close()
				server <- err
				return
			}
			requests <- request
			if attempt == 2 {
				response := &Response{
					RequestId:     request.RequestId,
					TransactionId: request.TransactionId,
					Result:        &Response_CommitComplete{CommitComplete: &CommitComplete{}},
				}
				message, err = proto.Marshal(response)
				if err == nil {
					err = writeMessage(connection, message)
				}
				if err != nil {
					_ = connection.Close()
					server <- err
					return
				}
			}
			_ = connection.Close()
		}
		server <- nil
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := (Client{ControlSocketPath: listener.Addr().String()}).Commit(ctx, "transaction"); err != nil {
		t.Fatal(err)
	}
	if err := <-server; err != nil {
		t.Fatal(err)
	}
	for range 3 {
		request := <-requests
		if request.GetTransactionId() != "transaction" || request.GetCommit() == nil {
			t.Fatalf("unexpected retry request: %v", request)
		}
	}
}

func TestCommitStopsWhenRetryResponseHangs(t *testing.T) {
	previousLimit := commitRetryLimit
	commitRetryLimit = 200 * time.Millisecond
	t.Cleanup(func() { commitRetryLimit = previousLimit })

	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "pagebroker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	accepted := make(chan net.Conn, 2)
	go func() {
		for range 2 {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			accepted <- connection
		}
	}()

	result := make(chan error, 1)
	go func() {
		result <- (Client{ControlSocketPath: listener.Addr().String()}).Commit(context.Background(), "transaction")
	}()

	first := <-accepted
	if _, err := readMessage(first); err != nil {
		t.Fatal(err)
	}
	_ = first.Close()

	second := <-accepted
	defer second.Close()
	if _, err := readMessage(second); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Commit() error = %v, want retry deadline", err)
	}
	if _, err := readMessage(second); err == nil {
		t.Fatal("retry connection did not close")
	}
}

func TestAbortRequiresAbortComplete(t *testing.T) {
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "pagebroker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	server := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			server <- err
			return
		}
		defer connection.Close()
		message, err := readMessage(connection)
		if err != nil {
			server <- err
			return
		}
		request := new(Request)
		if err := proto.Unmarshal(message, request); err != nil {
			server <- err
			return
		}
		message, err = proto.Marshal(&Response{
			RequestId:     request.RequestId,
			TransactionId: request.TransactionId,
			Result:        &Response_CommitComplete{CommitComplete: &CommitComplete{}},
		})
		if err == nil {
			err = writeMessage(connection, message)
		}
		server <- err
	}()

	if err := (Client{ControlSocketPath: listener.Addr().String()}).Abort(context.Background(), "transaction"); err == nil {
		t.Fatal("abort accepted a commit response")
	}
	if err := <-server; err != nil {
		t.Fatal(err)
	}
}

func TestStagingRequestsRejectEmptyDirectory(t *testing.T) {
	for _, tc := range []struct {
		name  string
		call  func(Client, context.Context) error
		reply func(*Request) *Response
	}{
		{
			name: "staged restore",
			call: func(client Client, ctx context.Context) error {
				_, err := client.StagedRestore(ctx, "transaction", "/checkpoints/source")
				return err
			},
			reply: func(request *Request) *Response {
				return &Response{RequestId: request.RequestId, TransactionId: request.TransactionId,
					Result: &Response_StagedRestoreDirectory{StagedRestoreDirectory: &StagedRestoreDirectory{}}}
			},
		},
		{
			name: "regular restore reference",
			call: func(client Client, ctx context.Context) error {
				_, err := client.ReferenceRegularRestore(
					ctx, "transaction", "/checkpoints/source",
					RegularRestoreIdentity{
						PodUID: "pod-uid", DestinationContainer: "main",
						ContentUID: "content-uid", SourceContainer: "main",
						ContainerID: "containerd://0123456789abcdef",
					},
				)
				return err
			},
			reply: func(request *Request) *Response {
				return &Response{RequestId: request.RequestId, TransactionId: request.TransactionId,
					Result: &Response_StagedRestoreDirectory{StagedRestoreDirectory: &StagedRestoreDirectory{}}}
			},
		},
		{
			name: "checkpoint",
			call: func(client Client, ctx context.Context) error {
				_, err := client.PrepareCheckpoint(ctx, "transaction", "/checkpoints/destination")
				return err
			},
			reply: func(request *Request) *Response {
				return &Response{RequestId: request.RequestId, TransactionId: request.TransactionId,
					Result: &Response_StagedCheckpointDirectory{StagedCheckpointDirectory: &StagedCheckpointDirectory{}}}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "pagebroker.sock"))
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()

			server := make(chan error, 1)
			go func() {
				connection, err := listener.Accept()
				if err != nil {
					server <- err
					return
				}
				defer connection.Close()
				message, err := readMessage(connection)
				if err != nil {
					server <- err
					return
				}
				request := new(Request)
				if err := proto.Unmarshal(message, request); err != nil {
					server <- err
					return
				}
				message, err = proto.Marshal(tc.reply(request))
				if err == nil {
					err = writeMessage(connection, message)
				}
				server <- err
			}()

			if err := tc.call(Client{ControlSocketPath: listener.Addr().String()}, context.Background()); err == nil {
				t.Fatal("staging request accepted an empty directory")
			}
			if err := <-server; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCommitDoesNotRetryInvalidFrame(t *testing.T) {
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "pagebroker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	server := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			server <- err
			return
		}
		defer connection.Close()
		if _, err := readMessage(connection); err != nil {
			server <- err
			return
		}
		server <- binary.Write(connection, binary.BigEndian, uint32(maxMessageSize+1))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = (Client{ControlSocketPath: listener.Addr().String()}).Commit(ctx, "transaction")
	if !errors.Is(err, errMessageTooLarge) {
		t.Fatalf("Commit() error = %v, want invalid frame error", err)
	}
	if err := <-server; err != nil {
		t.Fatal(err)
	}
}
