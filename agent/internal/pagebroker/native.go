// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package pagebroker

import (
	"context"
	"fmt"
	"net"
	"os"

	"google.golang.org/protobuf/proto"
)

// BindNative retains a transaction-scoped connection, not access to the general
// broker. Admission pins the container PID namespace before CRIU replaces it.
func (c Client) BindNative(ctx context.Context, transaction string, binding *BindNativeSession) (*os.File, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", c.ControlSocketPath)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	reply, err := exchange(conn, transaction, &Request_BindNative{BindNative: binding})
	if err != nil {
		return nil, err
	}
	if reply.GetNativeSession() == nil {
		return nil, fmt.Errorf("PageBroker did not bind native session")
	}
	return conn.(*net.UnixConn).File()
}

// NativeOperation performs one phase on an already-bound capability. The
// caller serializes native preparation and joins all transfers before COMPLETE.
func NativeOperation(ctx context.Context, file *os.File, operation NativeSessionRequest_Operation, targetPID int) (string, error) {
	conn, err := net.FileConn(file)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	data, err := proto.Marshal(&NativeSessionRequest{Operation: operation, TargetPid: uint32(targetPID)})
	if err != nil {
		return "", err
	}
	if err := writeMessage(conn, data); err != nil {
		return "", err
	}
	data, err = readMessage(conn)
	if err != nil {
		return "", err
	}
	var reply NativeSessionReply
	if err := proto.Unmarshal(data, &reply); err != nil {
		return "", err
	}
	if failure := reply.GetFailure(); failure != nil {
		return "", fmt.Errorf("native PageBroker operation: %s", failure.GetMessage())
	}
	return reply.GetReport(), nil
}
