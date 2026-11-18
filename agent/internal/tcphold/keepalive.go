// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tcphold

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/ai-dynamo/snapshot/agent/internal/criu"
)

// RestoreKeepalive turns SO_KEEPALIVE back on for the connections that had it
// at capture, which a group restore restores without it. It visits every process
// in the network namespace of hostPID, copies each of its sockets with
// pidfd_getfd, and matches the copy's addresses against connections. A copy
// refers to the same open socket, so setting the option on it changes the
// restored socket.
func RestoreKeepalive(hostPID int, connections []criu.TCPConnection) error {
	want := keepaliveConnections(connections)
	if len(want) == 0 {
		return nil
	}
	namespace, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/net", hostPID))
	if err != nil {
		return fmt.Errorf("read the network namespace of %d: %w", hostPID, err)
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return err
	}
	// The processes of one Pod share sockets, so each socket is set once.
	done := make(map[string]bool)
	var copyErrs []error
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		if ns, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/net", pid)); err != nil || ns != namespace {
			continue
		}
		copyErr, err := restoreProcessKeepalive(pid, want, done)
		if err != nil {
			return err
		}
		copyErrs = append(copyErrs, copyErr)
	}
	if len(done) < len(want) {
		return fmt.Errorf("found %d of the %d connections that had SO_KEEPALIVE: %w",
			len(done), len(want), errors.Join(copyErrs...))
	}
	return nil
}

// restoreProcessKeepalive sets SO_KEEPALIVE on the sockets of pid that want
// names and done does not, and records each socket it sets in done. It returns
// the first socket it could not copy separately, since another process may
// hold the same socket.
func restoreProcessKeepalive(pid int, want keepaliveSet, done map[string]bool) (copyErr, err error) {
	dir := fmt.Sprintf("/proc/%d/fd", pid)
	fds, err := os.ReadDir(dir)
	if err != nil {
		// The process exited.
		return nil, nil
	}
	pidfd := -1
	defer func() {
		if pidfd >= 0 {
			_ = unix.Close(pidfd)
		}
	}()
	for _, f := range fds {
		target, err := os.Readlink(dir + "/" + f.Name())
		if err != nil || !strings.HasPrefix(target, "socket:[") || done[target] {
			continue
		}
		fd, err := strconv.Atoi(f.Name())
		if err != nil {
			continue
		}
		if pidfd < 0 {
			if pidfd, err = unix.PidfdOpen(pid, 0); errors.Is(err, unix.ESRCH) {
				// The process exited.
				return nil, nil
			} else if err != nil {
				return nil, fmt.Errorf("open a pidfd for %d: %w", pid, err)
			}
		}
		copied, err := unix.PidfdGetfd(pidfd, fd, 0)
		if err != nil {
			if copyErr == nil {
				copyErr = fmt.Errorf("copy fd %d of %d: %w", fd, pid, err)
			}
			continue
		}
		if local, remote, ok := want.match(copied); ok {
			err = unix.SetsockoptInt(copied, unix.SOL_SOCKET, unix.SO_KEEPALIVE, 1)
			if err != nil {
				_ = unix.Close(copied)
				return nil, fmt.Errorf("set SO_KEEPALIVE on %s to %s: %w", local, remote, err)
			}
			done[target] = true
		}
		_ = unix.Close(copied)
	}
	return copyErr, nil
}

// keepaliveSet holds the connections that had SO_KEEPALIVE at capture, by
// their local and remote addresses.
type keepaliveSet map[[2]netip.AddrPort]bool

func keepaliveConnections(connections []criu.TCPConnection) keepaliveSet {
	set := make(keepaliveSet)
	for _, c := range connections {
		if c.KeepAlive {
			set[[2]netip.AddrPort{c.Local, c.Remote}] = true
		}
	}
	return set
}

// match returns the addresses of the socket fd, and whether it is one of the
// connections in s.
func (s keepaliveSet) match(fd int) (local, remote netip.AddrPort, ok bool) {
	l, err := unix.Getsockname(fd)
	if err != nil {
		return local, remote, false
	}
	r, err := unix.Getpeername(fd)
	if err != nil {
		return local, remote, false
	}
	return s.matchAddresses(l, r)
}

// matchAddresses is match for a socket whose addresses are local and remote.
// An IPv4 connection on an IPv6 socket has IPv4-mapped addresses.
func (s keepaliveSet) matchAddresses(l, r unix.Sockaddr) (local, remote netip.AddrPort, ok bool) {
	local, okLocal := addrPort(l)
	remote, okRemote := addrPort(r)
	return local, remote, okLocal && okRemote && s[[2]netip.AddrPort{local, remote}]
}

func addrPort(sa unix.Sockaddr) (netip.AddrPort, bool) {
	switch a := sa.(type) {
	case *unix.SockaddrInet4:
		return netip.AddrPortFrom(netip.AddrFrom4(a.Addr), uint16(a.Port)), true
	case *unix.SockaddrInet6:
		return netip.AddrPortFrom(netip.AddrFrom16(a.Addr).Unmap(), uint16(a.Port)), true
	}
	return netip.AddrPort{}, false
}
