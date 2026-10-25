// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pagebroker

import (
	"bytes"
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
	maxMessageSize        = 64 << 10
	maxFailureMessageSize = 1024
	messageHeaderSize     = 4
	commitRetryDelay      = 100 * time.Millisecond
	dialRetryDelay        = 100 * time.Millisecond
	dialRetryLimit        = 30 * time.Second
)

var commitRetryLimit = 30 * time.Second

// Client speaks the local PageBroker protocol. Filesystem operations run against
// the current broker; artifact-addressed operations require a storage backend.
type Client struct {
	ControlSocketPath string
}

func (c Client) StagedRestore(ctx context.Context, transactionID, source string) (string, error) {
	response, err := c.request(ctx, transactionID, stagedRestore(source))
	if err != nil {
		return "", err
	}
	return imageDirectory(response.GetStagedRestoreDirectory().GetImageDirectory())
}

func (c Client) PrepareCheckpoint(ctx context.Context, transactionID, destination string) (string, error) {
	response, err := c.request(ctx, transactionID, &Request_PrepareStagedCheckpoint{
		PrepareStagedCheckpoint: &PrepareStagedCheckpointRequest{
			Selector: &PrepareStagedCheckpointRequest_Destination{Destination: filesystem(destination)},
			IoEngine: posixCopy(),
		},
	})
	if err != nil {
		return "", err
	}
	return imageDirectory(response.GetStagedCheckpointDirectory().GetImageDirectory())
}

// DirectRestore prepares a transaction against the original checkpoint directory.
// The caller must keep its contents available until Commit or Abort.
func (c Client) DirectRestore(ctx context.Context, transactionID, source string) error {
	response, err := c.request(ctx, transactionID, directRestore(source))
	if err != nil {
		return err
	}
	return directRestoreReady(response)
}

func (c Client) PrepareDirectCheckpoint(ctx context.Context, transactionID, destination string) (string, error) {
	response, err := c.request(ctx, transactionID, prepareDirectCheckpoint(destination))
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

func directRestoreReady(response *Response) error {
	if response.GetDirectRestoreReady() == nil {
		return fmt.Errorf("unexpected PageBroker direct restore response")
	}
	return nil
}

func (c Client) Commit(ctx context.Context, transactionID string) error {
	_, err := c.commitWithRetry(ctx, transactionID)
	return err
}

func (c Client) commitWithRetry(ctx context.Context, transactionID string) (*CommitComplete, error) {
	result, err := c.commit(ctx, transactionID)
	if !isRetryableTransportError(err) {
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
		if result, err := c.commit(retryCtx, transactionID); !isRetryableTransportError(err) {
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

func transientDialError(err error) bool {
	return errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ECONNREFUSED)
}

func (c Client) dial(ctx context.Context) (net.Conn, error) {
	dialer := &net.Dialer{}
	retryCtx, cancel := context.WithTimeout(ctx, dialRetryLimit)
	defer cancel()
	for {
		connection, err := dialer.DialContext(retryCtx, "unix", c.ControlSocketPath)
		if err == nil {
			return connection, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !transientDialError(err) {
			return nil, err
		}
		select {
		case <-retryCtx.Done():
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		case <-time.After(dialRetryDelay):
		}
	}
}

func (c Client) request(ctx context.Context, transactionID string, command isRequest_Command, files ...*os.File) (*Response, error) {
	response, _, err := c.requestResponse(ctx, newRequest(transactionID, command), files...)
	return response, err
}

func (c Client) requestResponse(ctx context.Context, request *Request, files ...*os.File) (*Response, *os.File, error) {
	// Check framing before dialing. GPU exchanges bypass this path; writeRequest checks their size.
	if proto.Size(request) > maxMessageSize {
		return nil, nil, errMessageTooLarge
	}
	var connection net.Conn
	var err error
	if request.GetCapabilities() != nil {
		// Startup owns its retry budget and must see permanent endpoint errors immediately.
		connection, err = (&net.Dialer{}).DialContext(ctx, "unix", c.ControlSocketPath)
	} else {
		connection, err = c.dial(ctx)
	}
	if err != nil {
		return nil, nil, transportError{cause: fmt.Errorf("dial PageBroker: %w", err), dial: true}
	}
	defer connection.Close()
	return exchangeResponse(ctx, connection.(*net.UnixConn), request, files...)
}

func newRequest(transactionID string, command isRequest_Command) *Request {
	requestID := uuid.NewString()
	request := &Request{RequestId: &requestID, Command: command}
	if transactionID != "" {
		request.TransactionId = &transactionID
	}
	return request
}

func exchange(ctx context.Context, connection *net.UnixConn, request *Request, files ...*os.File) (*Response, error) {
	response, _, err := exchangeResponse(ctx, connection, request, files...)
	return response, err
}

// exchangeResponse returns a broker pidfd only when the request sets RequestBrokerPidfd.
func exchangeResponse(ctx context.Context, connection *net.UnixConn, request *Request, files ...*os.File) (*Response, *os.File, error) {
	stopCancel := context.AfterFunc(ctx, func() {
		_ = connection.CloseWrite()
		_ = connection.Close()
	})
	defer stopCancel()

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
				_ = unix.Close(fd)
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
	if response.GetRequestId() != request.GetRequestId() || response.GetTransactionId() != request.GetTransactionId() {
		return nil, nil, fmt.Errorf("PageBroker response identifiers do not match request")
	}
	if failure := response.GetFailure(); failure != nil {
		// Legacy handlers return filesystem exception text, which can include long
		// paths; those codes are bounded by the frame size alone. Storage-extension
		// codes promise the smaller 1024-byte bound. proto.Unmarshal above already
		// rejects any string field that is not valid UTF-8.
		message := failure.GetMessage()
		if isStorageFailure(failure.GetCode()) && len(message) > maxFailureMessageSize {
			message = "message omitted: exceeds the storage failure bound"
		}
		return nil, nil, &FailureError{code: failureCode(failure.GetCode()), message: message}
	}
	if len(receivedFiles) > 1 || (len(receivedFiles) == 1 && !request.GetRequestBrokerPidfd()) {
		return nil, nil, fmt.Errorf("unexpected PageBroker response descriptors")
	}
	if len(receivedFiles) == 0 {
		return response, nil, nil
	}
	if err := unix.PidfdSendSignal(receivedFiles[0], 0, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) && !errors.Is(err, unix.EPERM) {
		return nil, nil, fmt.Errorf("invalid PageBroker process descriptor: %w", err)
	}
	retained = true
	return response, os.NewFile(uintptr(receivedFiles[0]), "pagebroker-process"), nil
}

func stagedRestore(source string) isRequest_Command {
	return &Request_StagedRestore{StagedRestore: &StagedRestoreRequest{
		Selector: &StagedRestoreRequest_Source{Source: filesystem(source)},
		IoEngine: posixCopy(),
	}}
}

func directRestore(source string) isRequest_Command {
	return &Request_DirectRestore{DirectRestore: &DirectRestoreRequest{Source: filesystem(source), IoEngine: posixCopy()}}
}

func prepareDirectCheckpoint(destination string) isRequest_Command {
	return &Request_PrepareDirectCheckpoint{
		PrepareDirectCheckpoint: &PrepareDirectCheckpointRequest{Destination: filesystem(destination), IoEngine: posixCopy()},
	}
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

// Rights arrive with the first header bytes. Close every adopted descriptor on error.
func readMessageWithFiles(connection *net.UnixConn) (message []byte, files []int, err error) {
	defer func() {
		if err != nil {
			for _, fd := range files {
				_ = unix.Close(fd)
			}
			files = nil
		}
	}()
	header := make([]byte, messageHeaderSize)
	control := make([]byte, unix.CmsgSpace(maxPassedFiles*4))
	n, controlSize, flags, _, err := connection.ReadMsgUnix(header, control)
	if err != nil {
		return nil, nil, err
	}
	messages, err := unix.ParseSocketControlMessage(control[:controlSize])
	if err != nil {
		return nil, nil, err
	}
	for _, message := range messages {
		rights, parseErr := unix.ParseUnixRights(&message)
		if parseErr != nil {
			return nil, files, parseErr
		}
		for _, fd := range rights {
			unix.CloseOnExec(fd)
		}
		files = append(files, rights...)
	}
	if flags&(unix.MSG_CTRUNC|unix.MSG_TRUNC) != 0 {
		return nil, files, fmt.Errorf("truncated PageBroker process descriptor response")
	}
	if n == 0 {
		return nil, files, io.ErrUnexpectedEOF
	}
	message, err = readMessage(io.MultiReader(bytes.NewReader(header[:n]), connection))
	return message, files, err
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
