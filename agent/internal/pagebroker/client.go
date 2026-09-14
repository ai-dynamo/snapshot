// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pagebroker

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

const (
	// PageBroker control requests and responses are limited to 64 KiB.
	maxMessageSize               = 64 << 10
	maxRestoreIdentityFieldBytes = 253
	commitRetryDelay             = 100 * time.Millisecond
)

var (
	commitRetryLimit   = 30 * time.Second
	errMessageTooLarge = fmt.Errorf("message exceeds %d bytes", maxMessageSize)
)

// Client uses the deployment-wide filesystem/POSIX PageBroker plan.
type Client struct {
	ControlSocketPath string
}

// CUDATarget is an identity-validated CUDA process. NamespacePID is the stable
// artifact key; HostPID is revalidated by PageBroker before every driver call.
type CUDATarget struct {
	HostPID         int
	NamespacePID    int
	StartTimeTicks  uint64
	Cgroup          string
	DeviceMap       string
	SelectedDevices []string
}

type CuinterposeStateMetadata struct {
	ProtocolVersion  uint32
	SizeBytes        uint64
	SHA256           string
	ParticipantCount uint32
}

// RegularRestoreIdentity binds an artifact to one destination container
// incarnation for PageBroker-owned restore coordination. The historical name
// is retained for Go API compatibility; both regular and direct CustomStorage
// staging require the same identity.
type RegularRestoreIdentity struct {
	PodUID               string
	DestinationContainer string
	ContentUID           string
	SourceContainer      string
	ContainerID          string
}

func (c Client) CUDACheckpoint(
	ctx context.Context,
	transactionID string,
	targets []CUDATarget,
	storageBackend CudaStorageBackend,
	usesJobFile bool,
	usesCuinterpose bool,
) (*CuinterposeStateMetadata, error) {
	if err := validateCUDAStorageBackend(storageBackend); err != nil {
		return nil, err
	}
	encoded, err := cudaTargets(targets, storageBackend)
	if err != nil {
		return nil, err
	}
	request := &CudaCheckpointRequest{
		Targets: encoded, UsesJobFile: &usesJobFile, UsesCuinterpose: &usesCuinterpose,
		StorageBackend: &storageBackend,
	}
	response, err := c.request(ctx, transactionID, &Request_CudaCheckpoint{CudaCheckpoint: request})
	if err != nil {
		return nil, err
	}
	if complete := response.GetCudaOperationComplete(); complete == nil ||
		complete.GetTargetCount() != uint32(len(targets)) {
		return nil, cudaResponseError{cause: errors.New("unexpected PageBroker CUDA checkpoint response")}
	}
	state := response.GetCudaOperationComplete().GetCuinterposeState()
	if !usesCuinterpose {
		if state != nil {
			return nil, cudaResponseError{cause: errors.New("unexpected cuinterpose state in native CUDA checkpoint response")}
		}
		return nil, nil
	}
	if state == nil || state.GetProtocolVersion() == 0 || state.GetSizeBytes() == 0 ||
		len(state.GetSha256()) != 64 || state.GetParticipantCount() != uint32(len(targets)) {
		return nil, cudaResponseError{cause: errors.New("incomplete PageBroker cuinterpose state metadata")}
	}
	return &CuinterposeStateMetadata{
		ProtocolVersion: state.GetProtocolVersion(), SizeBytes: state.GetSizeBytes(),
		SHA256: state.GetSha256(), ParticipantCount: state.GetParticipantCount(),
	}, nil
}

func (c Client) CUDARestore(
	ctx context.Context,
	transactionID string,
	targets []CUDATarget,
	storageBackend CudaStorageBackend,
	usesJobFile bool,
	usesCuinterpose bool,
	cuinterposeState *CuinterposeStateMetadata,
) error {
	if err := validateCUDAStorageBackend(storageBackend); err != nil {
		return err
	}
	encoded, err := cudaTargets(targets, storageBackend)
	if err != nil {
		return err
	}
	request := &CudaRestoreRequest{
		Targets: encoded, UsesJobFile: &usesJobFile, UsesCuinterpose: &usesCuinterpose,
		StorageBackend: &storageBackend,
	}
	if usesCuinterpose {
		if cuinterposeState == nil {
			return errors.New("cuinterpose restore requires state metadata")
		}
		request.CuinterposeState = &CuinterposeState{
			ProtocolVersion:  &cuinterposeState.ProtocolVersion,
			SizeBytes:        &cuinterposeState.SizeBytes,
			Sha256:           &cuinterposeState.SHA256,
			ParticipantCount: &cuinterposeState.ParticipantCount,
		}
	} else if cuinterposeState != nil {
		return errors.New("native CUDA restore must not carry cuinterpose state metadata")
	}
	response, err := c.request(ctx, transactionID, &Request_CudaRestore{CudaRestore: request})
	if err != nil {
		return err
	}
	if complete := response.GetCudaOperationComplete(); complete == nil ||
		complete.GetTargetCount() != uint32(len(targets)) {
		return cudaResponseError{cause: errors.New("unexpected PageBroker CUDA restore response")}
	}
	return nil
}

// BeginRestore reserves PageBroker-owned CUDA capacity for a staged restore.
// It does not inspect or mutate the target workload. The executor calls it
// before publishing its durable attempt proof and before CRIU.
func (c Client) BeginRestore(
	ctx context.Context,
	transactionID string,
	storageBackend CudaStorageBackend,
	targetCount int,
) error {
	if err := validateCUDAStorageBackend(storageBackend); err != nil {
		return err
	}
	if targetCount < 1 || targetCount > 64 {
		return fmt.Errorf("PageBroker restore target count must be between 1 and 64")
	}
	encodedTargetCount := uint32(targetCount)
	response, err := c.request(ctx, transactionID, &Request_BeginRestore{
		BeginRestore: &BeginRestoreRequest{
			StorageBackend: &storageBackend,
			TargetCount:    &encodedTargetCount,
		},
	})
	if err != nil {
		return err
	}
	if response.GetRestoreAdmissionGranted() == nil {
		return cudaResponseError{cause: errors.New("unexpected PageBroker restore admission response")}
	}
	return nil
}

// ActivateRestore durably opens the PageBroker-produced GMS stage-ready gate
// inside the deployment's trusted node/storage boundary. Callers must first
// publish their identity-bound mutation proof. A transport-ambiguous activation
// is an unknown restore outcome and must never be replayed.
func (c Client) ActivateRestore(
	ctx context.Context,
	transactionID string,
	identity RegularRestoreIdentity,
) error {
	if err := validateRestoreIdentity(identity); err != nil {
		return err
	}
	response, err := c.request(ctx, transactionID, &Request_ActivateRestore{
		ActivateRestore: &ActivateRestoreRequest{Identity: &RestoreIdentity{
			PodUid: &identity.PodUID, DestinationContainer: &identity.DestinationContainer,
			ContentUid: &identity.ContentUID, SourceContainer: &identity.SourceContainer,
			ContainerId: &identity.ContainerID,
		}},
	})
	if err != nil {
		return err
	}
	if response.GetRestoreActivationGranted() == nil {
		return cudaResponseError{cause: errors.New("unexpected PageBroker restore activation response")}
	}
	return nil
}

// BeginCheckpoint reserves PageBroker's exclusive CUDA checkpoint boundary.
// It does not inspect or mutate the source workload and is idempotent for the
// same staged transaction, backend, and exact target count.
func (c Client) BeginCheckpoint(
	ctx context.Context,
	transactionID string,
	storageBackend CudaStorageBackend,
	targetCount int,
) error {
	if err := validateCUDAStorageBackend(storageBackend); err != nil {
		return err
	}
	if targetCount < 1 || targetCount > 64 {
		return fmt.Errorf("PageBroker checkpoint target count must be between 1 and 64")
	}
	encodedTargetCount := uint32(targetCount)
	response, err := c.request(ctx, transactionID, &Request_BeginCheckpoint{
		BeginCheckpoint: &BeginCheckpointRequest{
			StorageBackend: &storageBackend,
			TargetCount:    &encodedTargetCount,
		},
	})
	if err != nil {
		return err
	}
	if response.GetCheckpointAdmissionGranted() == nil {
		return cudaResponseError{cause: errors.New("unexpected PageBroker checkpoint admission response")}
	}
	return nil
}

// RestoreAdmissionBusy reports a correlated capacity refusal from BeginRestore.
// PageBroker leaves the transaction STAGED and explicitly proves that the
// target was not mutated, so the controller may preserve and retry that exact
// transaction without rebuilding its staging state.
func RestoreAdmissionBusy(err error) bool {
	var failure failureError
	return errors.As(err, &failure) && failure.code == Failure_BUSY &&
		failure.targetMutationKnown && !failure.targetMayBeMutated
}

// RestoreAdmissionRetryable reports failures that are safe to recover through
// the controller's durable admission-lease proof. BeginRestore cannot mutate
// the target, so a transport loss after dispatch may leave only a
// RESTORE_ADMITTED transaction; recovery must abort that exact transaction
// before retrying with a fresh identity. A correlated BUSY response is also
// safe to defer and can be distinguished with RestoreAdmissionBusy.
func RestoreAdmissionRetryable(err error) bool {
	if RestoreAdmissionBusy(err) {
		return true
	}
	var transport transportError
	return errors.As(err, &transport)
}

// RestoreStagingTransportRetryable reports transport ambiguity while staging a
// restore transaction. StagedRestore and DirectRestore cannot mutate the
// workload, so even a request that may have reached PageBroker is safe to retry
// with a fresh transaction identity. PageBroker bounds any orphaned
// pre-mutation transaction with its staging expiry.
func RestoreStagingTransportRetryable(err error) bool {
	return isTransportError(err)
}

// TransactionNotFound reports that PageBroker no longer retains a transaction.
// Recovery treats this as successful release: there is no admission or staging
// state left for the controller to clean up.
func TransactionNotFound(err error) bool {
	var failure failureError
	return errors.As(err, &failure) && failure.code == Failure_TRANSACTION_NOT_FOUND
}

// RestoreStagingCapacityPending reports a correlated staging refusal that
// created no staging reservation and cannot have touched the workload. A new
// executor invocation may use a fresh transaction identity immediately.
func RestoreStagingCapacityPending(err error) bool {
	var failure failureError
	if !errors.As(err, &failure) {
		return false
	}
	return failure.code == Failure_INSUFFICIENT_STORAGE ||
		(failure.code == Failure_BUSY && failure.targetMutationKnown && !failure.targetMayBeMutated)
}

// RestoreStagingCleanupRequired reports a staging I/O failure whose
// transaction must be successfully aborted before a fresh transaction may be
// attempted. It is intentionally narrower than RestoreAdmissionRetryable:
// contract and transaction failures remain terminal.
func RestoreStagingCleanupRequired(err error) bool {
	var failure failureError
	return errors.As(err, &failure) && failure.code == Failure_STORAGE_ERROR
}

func validateCUDAStorageBackend(backend CudaStorageBackend) error {
	switch backend {
	case CudaStorageBackend_CUDA_STORAGE_BACKEND_REGULAR,
		CudaStorageBackend_CUDA_STORAGE_BACKEND_POSIX_CUSTOM_STORAGE:
		return nil
	default:
		return fmt.Errorf("unsupported PageBroker CUDA storage backend %v", backend)
	}
}

func cudaTargets(targets []CUDATarget, storageBackend CudaStorageBackend) ([]*CudaProcessTarget, error) {
	if len(targets) == 0 || len(targets) > 64 {
		return nil, fmt.Errorf("PageBroker CUDA operation requires between 1 and 64 targets")
	}
	encoded := make([]*CudaProcessTarget, 0, len(targets))
	for _, target := range targets {
		if target.HostPID <= 0 || uint64(target.HostPID) > math.MaxUint32 ||
			target.NamespacePID <= 0 || uint64(target.NamespacePID) > math.MaxUint32 ||
			target.StartTimeTicks == 0 || target.Cgroup == "" ||
			(storageBackend == CudaStorageBackend_CUDA_STORAGE_BACKEND_POSIX_CUSTOM_STORAGE &&
				len(target.SelectedDevices) == 0) {
			return nil, fmt.Errorf("invalid PageBroker CUDA target identity")
		}
		hostPID := uint32(target.HostPID)
		namespacePID := uint32(target.NamespacePID)
		startTime := target.StartTimeTicks
		cgroup := target.Cgroup
		deviceMap := target.DeviceMap
		encoded = append(encoded, &CudaProcessTarget{
			HostPid: &hostPID, NamespacePid: &namespacePID, StartTimeTicks: &startTime,
			Cgroup: &cgroup, DeviceMap: &deviceMap,
			SelectedDevices: append([]string(nil), target.SelectedDevices...),
		})
	}
	return encoded, nil
}

// CUDATargetMayBeMutated reports whether a failed CUDA request may have
// reached PageBroker or a CUDA driver call. Unknown failures fail closed.
func CUDATargetMayBeMutated(err error) bool {
	if err == nil {
		return false
	}
	var failure failureError
	if errors.As(err, &failure) {
		switch failure.code {
		case Failure_INVALID_REQUEST, Failure_TRANSACTION_NOT_FOUND,
			Failure_TRANSACTION_CONFLICT, Failure_INSUFFICIENT_STORAGE:
			// These responses are emitted before PageBroker dispatches a CUDA
			// operation and are therefore canonical pre-mutation proof.
			return false
		}
		// Every other response either carries explicit mutation proof or is
		// an unknown CUDA-command outcome. Absence of the optional bit must
		// fail closed for legacy brokers and generic failures.
		return !failure.targetMutationKnown || failure.targetMayBeMutated
	}
	var transport transportError
	if errors.As(err, &transport) {
		return transport.mayHaveDispatched
	}
	var response cudaResponseError
	return errors.As(err, &response)
}

func (c Client) StagedRestore(ctx context.Context, transactionID, source string) (string, error) {
	response, err := c.request(ctx, transactionID, &Request_StagedRestore{
		StagedRestore: &StagedRestoreRequest{Source: filesystem(source), IoEngine: posixCopy()},
	})
	if err != nil {
		return "", err
	}
	return imageDirectory(response.GetStagedRestoreDirectory().GetImageDirectory())
}

// ReferenceRegularRestore asks PageBroker to retain a transaction-private,
// same-filesystem hardlink tree for a regular-backend artifact. No checkpoint
// payload bytes are copied into node-local staging.
func (c Client) ReferenceRegularRestore(
	ctx context.Context,
	transactionID, source string,
	identity RegularRestoreIdentity,
) (string, error) {
	if err := validateRestoreIdentity(identity); err != nil {
		return "", err
	}
	response, err := c.request(ctx, transactionID, &Request_ReferenceRegularRestore{
		ReferenceRegularRestore: &ReferenceRegularRestoreRequest{
			Source: filesystem(source), IoEngine: posixCopy(),
			PodUid: &identity.PodUID, DestinationContainer: &identity.DestinationContainer,
			ContentUid: &identity.ContentUID, SourceContainer: &identity.SourceContainer,
			ContainerId: &identity.ContainerID,
		},
	})
	if err != nil {
		return "", err
	}
	return imageDirectory(response.GetStagedRestoreDirectory().GetImageDirectory())
}

func validateRestoreIdentity(identity RegularRestoreIdentity) error {
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "pod UID", value: identity.PodUID},
		{name: "destination container", value: identity.DestinationContainer},
		{name: "content UID", value: identity.ContentUID},
		{name: "source container", value: identity.SourceContainer},
		{name: "container ID", value: identity.ContainerID},
	} {
		if err := validateRestoreIdentityField(field.name, field.value); err != nil {
			return err
		}
	}
	return nil
}

func validateRestoreIdentityField(name, value string) error {
	if value == "" {
		return fmt.Errorf("PageBroker restore %s is required", name)
	}
	if len(value) > maxRestoreIdentityFieldBytes {
		return fmt.Errorf("PageBroker restore %s exceeds %d bytes", name, maxRestoreIdentityFieldBytes)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("PageBroker restore %s is not valid UTF-8", name)
	}
	for _, valueByte := range []byte(value) {
		if valueByte < 0x20 || valueByte == 0x7f {
			return fmt.Errorf("PageBroker restore %s contains a control character", name)
		}
	}
	return nil
}

// DirectRestore stages only mutable/CRIU bytes and asks PageBroker to retain
// descriptor-pinned POSIX CustomStorage carrier directories for the listed
// CUDA process identities. It preserves the same staged path contract used by
// nsrestore; only the immutable carrier read path remains at the source.
func (c Client) DirectRestore(
	ctx context.Context,
	transactionID, source string,
	namespacePIDs []int,
	identity RegularRestoreIdentity,
) (string, error) {
	if len(namespacePIDs) == 0 || len(namespacePIDs) > 64 {
		return "", fmt.Errorf("direct PageBroker restore requires between 1 and 64 CUDA namespace PIDs")
	}
	if err := validateRestoreIdentity(identity); err != nil {
		return "", err
	}
	encoded := make([]uint32, 0, len(namespacePIDs))
	seen := make(map[uint32]struct{}, len(namespacePIDs))
	for _, pid := range namespacePIDs {
		if pid <= 0 || uint64(pid) > math.MaxUint32 {
			return "", fmt.Errorf("invalid direct PageBroker CUDA namespace PID %d", pid)
		}
		value := uint32(pid)
		if _, exists := seen[value]; exists {
			return "", fmt.Errorf("duplicate direct PageBroker CUDA namespace PID %d", pid)
		}
		seen[value] = struct{}{}
		encoded = append(encoded, value)
	}
	response, err := c.request(ctx, transactionID, &Request_DirectRestore{
		DirectRestore: &DirectRestoreRequest{
			Source: filesystem(source), IoEngine: posixCopy(), CudaNamespacePids: encoded,
			Identity: &RestoreIdentity{
				PodUid: &identity.PodUID, DestinationContainer: &identity.DestinationContainer,
				ContentUid: &identity.ContentUID, SourceContainer: &identity.SourceContainer,
				ContainerId: &identity.ContainerID,
			},
		},
	})
	if err != nil {
		return "", err
	}
	return imageDirectory(response.GetStagedRestoreDirectory().GetImageDirectory())
}

func (c Client) PrepareCheckpoint(ctx context.Context, transactionID, destination string) (string, error) {
	response, err := c.request(ctx, transactionID, &Request_PrepareStagedCheckpoint{
		PrepareStagedCheckpoint: &PrepareStagedCheckpointRequest{Destination: filesystem(destination), IoEngine: posixCopy()},
	})
	if err != nil {
		return "", err
	}
	return imageDirectory(response.GetStagedCheckpointDirectory().GetImageDirectory())
}

func imageDirectory(directory string) (string, error) {
	if directory == "" {
		return "", fmt.Errorf("unexpected PageBroker staging response")
	}
	return directory, nil
}

func (c Client) Commit(ctx context.Context, transactionID string) error {
	err := c.commit(ctx, transactionID)
	if !isTransportError(err) {
		return err
	}
	return c.retryCommit(ctx, transactionID)
}

func (c Client) retryCommit(ctx context.Context, transactionID string) error {
	retryCtx, cancel := context.WithTimeout(ctx, commitRetryLimit)
	defer cancel()
	for {
		select {
		case <-retryCtx.Done():
			return retryCtx.Err()
		case <-time.After(commitRetryDelay):
		}
		if err := c.commit(retryCtx, transactionID); !isTransportError(err) {
			return err
		}
	}
}

func (c Client) commit(ctx context.Context, transactionID string) error {
	response, err := c.request(ctx, transactionID, &Request_Commit{Commit: &CommitRequest{}})
	if err != nil {
		return err
	}
	if response.GetCommitComplete() == nil {
		return fmt.Errorf("unexpected PageBroker commit response")
	}
	return nil
}

func isTransportError(err error) bool {
	var transport transportError
	return errors.As(err, &transport)
}

func (c Client) Abort(ctx context.Context, transactionID string) error {
	response, err := c.request(ctx, transactionID, &Request_Abort{Abort: &AbortRequest{}})
	if err != nil {
		return err
	}
	if response.GetAbortComplete() == nil {
		return fmt.Errorf("unexpected PageBroker abort response")
	}
	return nil
}

func (c Client) request(ctx context.Context, transactionID string, command isRequest_Command) (*Response, error) {
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", c.ControlSocketPath)
	if err != nil {
		return nil, transportError{cause: fmt.Errorf("dial PageBroker: %w", err)}
	}
	defer connection.Close()
	stopCancel := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stopCancel()

	requestID := uuid.NewString()
	request := &Request{RequestId: &requestID, TransactionId: &transactionID, Command: command}
	message, err := proto.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("marshal PageBroker request: %w", err)
	}
	if err := writeMessage(connection, message); err != nil {
		return nil, transportError{cause: fmt.Errorf("write PageBroker request: %w", err), mayHaveDispatched: true}
	}
	message, err = readMessage(connection)
	if err != nil {
		if errors.Is(err, errMessageTooLarge) {
			return nil, cudaResponseError{cause: err}
		}
		return nil, transportError{cause: fmt.Errorf("read PageBroker response: %w", err), mayHaveDispatched: true}
	}
	response := new(Response)
	if err := proto.Unmarshal(message, response); err != nil {
		return nil, cudaResponseError{cause: fmt.Errorf("unmarshal PageBroker response: %w", err)}
	}
	if response.GetRequestId() != requestID || response.GetTransactionId() != transactionID {
		return nil, cudaResponseError{cause: errors.New("PageBroker response identifiers do not match request")}
	}
	if failure := response.GetFailure(); failure != nil {
		return nil, failureError{
			code: failureCode(failure.GetCode()), message: failure.GetMessage(),
			targetMayBeMutated:  failure.GetTargetMayBeMutated(),
			targetMutationKnown: failure.TargetMayBeMutated != nil,
		}
	}
	return response, nil
}

type failureError struct {
	code                Failure_Code
	message             string
	targetMayBeMutated  bool
	targetMutationKnown bool
}

func failureCode(code Failure_Code) Failure_Code {
	switch code {
	case Failure_UNSPECIFIED, Failure_INVALID_REQUEST, Failure_TRANSACTION_NOT_FOUND, Failure_TRANSACTION_CONFLICT,
		Failure_INSUFFICIENT_STORAGE, Failure_STORAGE_ERROR, Failure_INTERNAL_ERROR, Failure_CUDA_ERROR, Failure_BUSY:
		return code
	default:
		return Failure_UNSPECIFIED
	}
}

type transportError struct {
	cause             error
	mayHaveDispatched bool
}

type cudaResponseError struct{ cause error }

func (e cudaResponseError) Error() string { return e.cause.Error() }
func (e cudaResponseError) Unwrap() error { return e.cause }

func (e transportError) Error() string { return e.cause.Error() }

func (e transportError) Unwrap() error { return e.cause }

func (e failureError) Error() string {
	return fmt.Sprintf("PageBroker %s: %s", e.code, e.message)
}

func filesystem(directory string) *StorageBackend {
	return &StorageBackend{Kind: &StorageBackend_Filesystem{Filesystem: &FilesystemStorage{Directory: &directory}}}
}

func posixCopy() *IOEngine {
	return &IOEngine{Kind: &IOEngine_PosixCopy{PosixCopy: &PosixCopyIOEngine{}}}
}

func writeMessage(writer io.Writer, message []byte) error {
	if len(message) > maxMessageSize {
		return fmt.Errorf("message exceeds %d bytes", maxMessageSize)
	}
	if err := binary.Write(writer, binary.BigEndian, uint32(len(message))); err != nil {
		return err
	}
	for len(message) > 0 {
		written, err := writer.Write(message)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		message = message[written:]
	}
	return nil
}

func readMessage(reader io.Reader) ([]byte, error) {
	var size uint32
	if err := binary.Read(reader, binary.BigEndian, &size); err != nil {
		return nil, err
	}
	if size > maxMessageSize {
		return nil, errMessageTooLarge
	}
	message := make([]byte, size)
	_, err := io.ReadFull(reader, message)
	return message, err
}
