// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tcphold

import (
	"net/netip"
	"os"
	"testing"

	"github.com/ai-dynamo/snapshot/agent/internal/criu"
)

const attempt = "4f6c1a52-7b0e-4c8f-9d33-2a1e5b7c9d10"

func TestKeyFitsTheSet(t *testing.T) {
	key := key(netip.MustParseAddrPort("10.1.4.21:40000"), netip.MustParseAddrPort("10.1.4.22:6001"))
	if len(key) != int(connectionType.Bytes) {
		t.Fatalf("key length = %d, want the set's %d", len(key), connectionType.Bytes)
	}
}

func TestInstallRefusesIPv6(t *testing.T) {
	err := Install(os.Getpid(), attempt, []criu.TCPConnection{connection("[fd00::1]:40000", "[fd00::2]:6001")})
	if err == nil {
		t.Fatal("Install succeeded, want an error for an IPv6 connection")
	}
}

func connection(local, remote string) criu.TCPConnection {
	return criu.TCPConnection{Local: netip.MustParseAddrPort(local), Remote: netip.MustParseAddrPort(remote)}
}
