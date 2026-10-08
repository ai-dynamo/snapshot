// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pagebroker

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
)

const (
	// PageBroker control requests and responses are limited to 64 KiB.
	maxMessageSize    = 64 << 10
	messageHeaderSize = 4
	commitRetryDelay  = 100 * time.Millisecond
)

var (
	commitRetryLimit   = 30 * time.Second
	errMessageTooLarge = fmt.Errorf("message exceeds %d bytes", maxMessageSize)
)

// Client uses the deployment-wide filesystem/POSIX PageBroker plan.
type Client struct {
	ControlSocketPath string
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

func (c Client) PrepareCheckpoint(ctx context.Context, transactionID, destination string) (string, error) {
	response, err := c.request(ctx, transactionID, &Request_PrepareStagedCheckpoint{
		PrepareStagedCheckpoint: &PrepareStagedCheckpointRequest{Destination: filesystem(destination), IoEngine: posixCopy()},
	})
	if err != nil {
		return "", err
	}
	return imageDirectory(response.GetStagedCheckpointDirectory().GetImageDirectory())
}

// DirectRestore prepares a transaction against the original checkpoint directory.
// The caller must keep its contents available until Commit or Abort.
func (c Client) DirectRestore(ctx context.Context, transactionID, source string) error {
	response, err := c.request(ctx, transactionID, &Request_DirectRestore{
		DirectRestore: &DirectRestoreRequest{Source: filesystem(source), IoEngine: posixCopy()},
	})
	if err != nil {
		return err
	}
	if response.GetDirectRestoreReady() == nil {
		return fmt.Errorf("unexpected PageBroker direct restore response")
	}
	return nil
}

func (c Client) PrepareDirectCheckpoint(ctx context.Context, transactionID, destination string) (string, error) {
	response, err := c.request(ctx, transactionID, &Request_PrepareDirectCheckpoint{
		PrepareDirectCheckpoint: &PrepareDirectCheckpointRequest{Destination: filesystem(destination), IoEngine: posixCopy()},
	})
	if err != nil {
		return "", err
	}
	return imageDirectory(response.GetDirectCheckpointDirectory().GetImageDirectory())
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
	return c.abort(ctx, transactionID)
}

func (c Client) abort(ctx context.Context, transactionID string, files ...*os.File) error {
	response, err := c.request(ctx, transactionID, &Request_Abort{Abort: &AbortRequest{}}, files...)
	if err != nil {
		return err
	}
	if response.GetAbortComplete() == nil {
		return fmt.Errorf("unexpected PageBroker abort response")
	}
	return nil
}

func (c Client) request(ctx context.Context, transactionID string, command isRequest_Command, files ...*os.File) (*Response, error) {
	response, brokerProcess, err := c.requestWithProcess(ctx, transactionID, command, files...)
	if brokerProcess != nil {
		brokerProcess.Close()
	}
	return response, err
}

func (c Client) requestWithProcess(ctx context.Context, transactionID string, command isRequest_Command, files ...*os.File) (*Response, *os.File, error) {
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", c.ControlSocketPath)
	if err != nil {
		return nil, nil, transportError{cause: fmt.Errorf("dial PageBroker: %w", err)}
	}
	defer connection.Close()
	return exchangeWithProcess(ctx, connection.(*net.UnixConn), transactionID, command, files...)
}

func exchange(ctx context.Context, connection *net.UnixConn, transactionID string, command isRequest_Command, files ...*os.File) (*Response, error) {
	response, brokerProcess, err := exchangeWithProcess(ctx, connection, transactionID, command, files...)
	if brokerProcess != nil {
		brokerProcess.Close()
	}
	return response, err
}

func exchangeWithProcess(ctx context.Context, connection *net.UnixConn, transactionID string, command isRequest_Command, files ...*os.File) (*Response, *os.File, error) {
	stopCancel := context.AfterFunc(ctx, func() {
		_ = connection.CloseWrite()
		_ = connection.Close()
	})
	defer stopCancel()

	requestID := uuid.NewString()
	request := &Request{RequestId: &requestID, Command: command}
	if transactionID != "" {
		request.TransactionId = &transactionID
	}
	message, err := proto.Marshal(request)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal PageBroker request: %w", err)
	}
	if err := writeRequest(connection, message, files); err != nil {
		return nil, nil, transportError{cause: fmt.Errorf("write PageBroker request: %w", err)}
	}
	message, receivedFiles, err := readMessageWithFiles(connection)
	retained := false
	defer func() {
		if !retained {
			for _, fd := range receivedFiles {
				unix.Close(fd)
			}
		}
	}()
	if err != nil {
		if errors.Is(err, errMessageTooLarge) {
			return nil, nil, err
		}
		return nil, nil, transportError{cause: fmt.Errorf("read PageBroker response: %w", err)}
	}
	response := new(Response)
	if err := proto.Unmarshal(message, response); err != nil {
		return nil, nil, fmt.Errorf("unmarshal PageBroker response: %w", err)
	}
	if response.GetRequestId() != requestID || response.GetTransactionId() != transactionID {
		return nil, nil, fmt.Errorf("PageBroker response identifiers do not match request")
	}
	if failure := response.GetFailure(); failure != nil {
		return nil, nil, failureError{code: failureCode(failure.GetCode()), message: failure.GetMessage()}
	}
	if len(receivedFiles) > 1 {
		return nil, nil, fmt.Errorf("unexpected PageBroker response descriptors")
	}
	if len(receivedFiles) == 0 {
		return response, nil, nil
	}
	if response.GetDirectCheckpointDirectory() == nil && response.GetStagedCheckpointDirectory() == nil &&
		response.GetDirectRestoreReady() == nil && response.GetStagedRestoreDirectory() == nil {
		return nil, nil, fmt.Errorf("broker pidfd is only allowed on preparation responses")
	}
	if err := unix.PidfdSendSignal(receivedFiles[0], 0, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) && !errors.Is(err, unix.EPERM) {
		return nil, nil, fmt.Errorf("invalid PageBroker process descriptor: %w", err)
	}
	retained = true
	return response, os.NewFile(uintptr(receivedFiles[0]), "pagebroker-process"), nil
}

type failureError struct {
	code    Failure_Code
	message string
}

func failureCode(code Failure_Code) Failure_Code {
	switch code {
	case Failure_UNSPECIFIED, Failure_INVALID_REQUEST, Failure_TRANSACTION_NOT_FOUND, Failure_TRANSACTION_CONFLICT,
		Failure_INSUFFICIENT_STORAGE, Failure_STORAGE_ERROR, Failure_INTERNAL_ERROR, Failure_UNAVAILABLE:
		return code
	default:
		return Failure_UNSPECIFIED
	}
}

type transportError struct {
	cause error
}

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

// Linux permits at most 253 descriptors in one SCM_RIGHTS message.
const maxPassedFiles = 253

func writeRequest(connection *net.UnixConn, message []byte, files []*os.File) error {
	if len(files) == 0 {
		return writeMessage(connection, message)
	}
	if len(message) > maxMessageSize || len(files) > maxPassedFiles {
		return fmt.Errorf("PageBroker request exceeds message or descriptor limit")
	}
	descriptors := make([]int, len(files))
	for i, file := range files {
		if file == nil {
			return fmt.Errorf("missing GPU target descriptor %d", i)
		}
		descriptors[i] = int(file.Fd())
	}
	header := make([]byte, messageHeaderSize)
	binary.BigEndian.PutUint32(header, uint32(len(message)))
	rights := unix.UnixRights(descriptors...)
	written, controlWritten, err := connection.WriteMsgUnix(header, rights, nil)
	runtime.KeepAlive(files)
	if err != nil {
		return err
	}
	if controlWritten != len(rights) || written == 0 {
		return io.ErrShortWrite
	}
	if _, err := connection.Write(header[written:]); err != nil {
		return err
	}
	_, err = connection.Write(message)
	return err
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
