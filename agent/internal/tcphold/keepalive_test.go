// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tcphold

import (
	"errors"
	"net"
	"net/netip"
	"os"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/ai-dynamo/snapshot/agent/internal/criu"
)

func TestKeepaliveSetMatchesTheConnectionsThatHadKeepalive(t *testing.T) {
	had := connection("10.1.4.21:40000", "10.1.4.22:6001")
	had.KeepAlive = true
	set := keepaliveConnections([]criu.TCPConnection{had, connection("10.1.4.21:40001", "10.1.4.22:6001")})

	v4 := func(a [4]byte, port int) unix.Sockaddr { return &unix.SockaddrInet4{Addr: a, Port: port} }
	mapped := func(a [4]byte, port int) unix.Sockaddr {
		return &unix.SockaddrInet6{Addr: [16]byte{10: 0xff, 11: 0xff, 12: a[0], 13: a[1], 14: a[2], 15: a[3]}, Port: port}
	}
	local, remote := [4]byte{10, 1, 4, 21}, [4]byte{10, 1, 4, 22}
	for _, tc := range []struct {
		name          string
		local, remote unix.Sockaddr
		want          bool
	}{
		{"the connection", v4(local, 40000), v4(remote, 6001), true},
		{"the connection on an IPv6 socket", mapped(local, 40000), mapped(remote, 6001), true},
		{"its other end", v4(remote, 6001), v4(local, 40000), false},
		{"a connection without keepalive", v4(local, 40001), v4(remote, 6001), false},
		{"a Unix socket", &unix.SockaddrUnix{Name: "/run/sock"}, &unix.SockaddrUnix{}, false},
	} {
		if _, _, got := set.matchAddresses(tc.local, tc.remote); got != tc.want {
			t.Errorf("%s: match = %t, want %t", tc.name, got, tc.want)
		}
	}
}

func TestRestoreKeepaliveSetsTheOptionOnTheMatchingSocket(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	for _, c := range []net.Conn{client, server} {
		if err := c.(*net.TCPConn).SetKeepAlive(false); err != nil {
			t.Fatal(err)
		}
	}
	local := netip.MustParseAddrPort(client.LocalAddr().String())
	remote := netip.MustParseAddrPort(client.RemoteAddr().String())

	err = RestoreKeepalive(os.Getpid(), []criu.TCPConnection{
		{Local: local, Remote: remote, KeepAlive: true},
		{Local: remote, Remote: local, KeepAlive: false},
	})
	if errors.Is(err, unix.EPERM) {
		t.Skipf("pidfd_getfd is not permitted here: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if got := keepalive(t, client); got != 1 {
		t.Fatalf("client SO_KEEPALIVE = %d, want 1", got)
	}
	if got := keepalive(t, server); got != 0 {
		t.Fatalf("server SO_KEEPALIVE = %d, want 0: it did not have keepalive at capture", got)
	}
}

func keepalive(t *testing.T, c net.Conn) int {
	t.Helper()
	raw, err := c.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var value int
	var getErr error
	if err := raw.Control(func(fd uintptr) {
		value, getErr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_KEEPALIVE)
	}); err != nil {
		t.Fatal(err)
	}
	if getErr != nil {
		t.Fatal(getErr)
	}
	return value
}

func TestRestoreKeepaliveReportsAConnectionItCannotFind(t *testing.T) {
	missing := criu.TCPConnection{
		Local:     netip.MustParseAddrPort("10.1.4.21:40000"),
		Remote:    netip.MustParseAddrPort("10.1.4.22:6001"),
		KeepAlive: true,
	}
	if err := RestoreKeepalive(os.Getpid(), []criu.TCPConnection{missing}); err == nil {
		t.Fatal("RestoreKeepalive succeeded, want an error for a connection no process holds")
	}
}
