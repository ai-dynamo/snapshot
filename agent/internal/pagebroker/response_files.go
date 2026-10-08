// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package pagebroker

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"

	"golang.org/x/sys/unix"
)

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
	if _, err := io.ReadFull(connection, header[n:]); err != nil {
		return nil, files, err
	}
	size := binary.BigEndian.Uint32(header)
	if size > maxMessageSize {
		return nil, files, errMessageTooLarge
	}
	message = make([]byte, size)
	_, err = io.ReadFull(connection, message)
	return message, files, err
}
