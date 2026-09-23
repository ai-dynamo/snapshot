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
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

const (
	// PageBroker control requests and responses are limited to 64 KiB.
	maxMessageSize        = 64 << 10
	maxFailureMessageSize = 1024
	commitRetryDelay      = 100 * time.Millisecond
	commitRetryLimit      = 30 * time.Second
	dialRetryDelay        = 100 * time.Millisecond
	dialRetryLimit        = 30 * time.Second
)

// Client speaks the local PageBroker protocol. Artifact-addressed operations
// require backend support; existing filesystem callers can migrate separately.
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

func imageDirectory(directory string) (string, error) {
	if directory == "" {
		return "", fmt.Errorf("unexpected PageBroker staging response")
	}
	return directory, nil
}

func (c Client) Commit(ctx context.Context, transactionID string) error {
	_, err := c.commitWithRetry(ctx, transactionID)
	return err
}

func (c Client) commitWithRetry(ctx context.Context, transactionID string) (*CommitComplete, error) {
	result, err := c.commit(ctx, transactionID)
	if !isTransportError(err) {
		return result, err
	}
	return c.retryCommit(ctx, transactionID)
}

func (c Client) retryCommit(ctx context.Context, transactionID string) (*CommitComplete, error) {
	retryCtx, cancel := context.WithTimeout(ctx, commitRetryLimit)
	defer cancel()
	for {
		select {
		case <-retryCtx.Done():
			return nil, retryCtx.Err()
		case <-time.After(commitRetryDelay):
		}
		if result, err := c.commit(retryCtx, transactionID); !isTransportError(err) {
			return result, err
		}
	}
}

func (c Client) commit(ctx context.Context, transactionID string) (*CommitComplete, error) {
	response, err := c.request(ctx, transactionID, &Request_Commit{Commit: &CommitRequest{}})
	if err != nil {
		return nil, err
	}
	if response.GetCommitComplete() == nil {
		return nil, fmt.Errorf("unexpected PageBroker commit response")
	}
	return response.GetCommitComplete(), nil
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

func (c Client) dial(ctx context.Context) (net.Conn, error) {
	dialer := &net.Dialer{}
	connection, err := dialer.DialContext(ctx, "unix", c.ControlSocketPath)
	if err == nil || ctx.Err() != nil {
		return connection, err
	}
	retryCtx, cancel := context.WithTimeout(ctx, dialRetryLimit)
	defer cancel()
	for {
		select {
		case <-retryCtx.Done():
			return nil, err
		case <-time.After(dialRetryDelay):
		}
		connection, err = dialer.DialContext(retryCtx, "unix", c.ControlSocketPath)
		if err == nil {
			return connection, nil
		}
	}
}

func (c Client) request(ctx context.Context, transactionID string, command isRequest_Command) (*Response, error) {
	requestID := uuid.NewString()
	request := &Request{RequestId: &requestID, TransactionId: &transactionID, Command: command}
	message, err := proto.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("marshal PageBroker request: %w", err)
	}
	// Reject local framing errors before connecting: retrying cannot fix them.
	if len(message) > maxMessageSize {
		return nil, errMessageTooLarge
	}
	connection, err := c.dial(ctx)
	if err != nil {
		return nil, transportError{cause: dialError{cause: fmt.Errorf("dial PageBroker: %w", err)}}
	}
	defer connection.Close()
	stopCancel := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stopCancel()

	if err := writeMessage(connection, message); err != nil {
		return nil, transportError{cause: fmt.Errorf("write PageBroker request: %w", err)}
	}
	message, err = readMessage(connection)
	if err != nil {
		if errors.Is(err, errMessageTooLarge) {
			return nil, err
		}
		return nil, transportError{cause: fmt.Errorf("read PageBroker response: %w", err)}
	}
	response := new(Response)
	if err := proto.Unmarshal(message, response); err != nil {
		return nil, fmt.Errorf("unmarshal PageBroker response: %w", err)
	}
	if response.GetRequestId() != requestID || response.GetTransactionId() != transactionID {
		return nil, fmt.Errorf("PageBroker response identifiers do not match request")
	}
	if failure := response.GetFailure(); failure != nil {
		// Legacy handlers return filesystem exception text, which can include long
		// paths. Only the storage-extension codes promise the smaller wire bound;
		// legacy and unknown codes retain the existing frame-size limit.
		if (isStorageFailure(failure.GetCode()) && len(failure.GetMessage()) > maxFailureMessageSize) || !utf8.ValidString(failure.GetMessage()) {
			return nil, fmt.Errorf("invalid PageBroker failure message")
		}
		return nil, &FailureError{code: failureCode(failure.GetCode()), message: failure.GetMessage()}
	}
	return response, nil
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
