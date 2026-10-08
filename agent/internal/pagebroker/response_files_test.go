// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

package pagebroker

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func countProcessDescriptors(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		if target, _ := os.Readlink("/proc/self/fd/" + entry.Name()); target == "anon_inode:[pidfd]" {
			count++
		}
	}
	return count
}

func TestTruncatedPreparationClosesReceivedProcessDescriptor(t *testing.T) {
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(t.TempDir(), "broker.sock"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	process := openTestPidfd(t)
	before := countProcessDescriptors(t)
	server := make(chan error, 1)
	go func() {
		connection, err := listener.AcceptUnix()
		if err != nil {
			server <- err
			return
		}
		defer connection.Close()
		// Announce a body, transfer the pidfd, then disconnect without the body.
		_, _, err = connection.WriteMsgUnix([]byte{0, 0, 0, 100}, unix.UnixRights(int(process.Fd())), nil)
		server <- err
	}()
	connection, err := net.DialUnix("unix", nil, listener.Addr().(*net.UnixAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_, files, err := readMessageWithFiles(connection)
	if !errors.Is(err, io.EOF) || len(files) != 0 {
		t.Fatalf("truncated preparation: files=%v, error=%v", files, err)
	}
	if err := <-server; err != nil {
		t.Fatal(err)
	}
	if after := countProcessDescriptors(t); after != before {
		t.Fatalf("process descriptor leak: before %d, after %d", before, after)
	}
}
