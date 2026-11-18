// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tcphold

import (
	"net/netip"
	"os"
	"testing"

	"github.com/google/nftables"
	"github.com/mdlayher/netlink"
	"golang.org/x/sys/unix"

	"github.com/ai-dynamo/snapshot/agent/internal/criu"
)

const attempt = "4f6c1a52-7b0e-4c8f-9d33-2a1e5b7c9d10"

func TestKeyFitsTheSet(t *testing.T) {
	key := key(netip.MustParseAddrPort("10.1.4.21:40000"), netip.MustParseAddrPort("10.1.4.22:6001"))
	if len(key) != int(connectionType.Bytes) {
		t.Fatalf("key length = %d, want the set's %d", len(key), connectionType.Bytes)
	}
}

// netlinkSendLimit is the most a netlink socket sends at once with the kernel's
// default send buffer, net.core.wmem_default, less the 32 bytes netlink keeps.
const netlinkSendLimit = 212992 - 32

func TestInstallSendsEveryElementInBatchesThatFit(t *testing.T) {
	// 3000 connections, both directions of each, are more elements than one
	// netlink attribute can list.
	local, remote := netip.MustParseAddr("10.1.4.21"), netip.MustParseAddrPort("10.1.4.22:6001")
	elements := make([]nftables.SetElement, 6000)
	for i := range elements {
		elements[i] = nftables.SetElement{Key: key(netip.AddrPortFrom(local, uint16(10000+i)), remote)}
	}
	sent := 0
	conn, err := nftables.New(nftables.WithTestDial(func(batch []netlink.Message) ([]netlink.Message, error) {
		size := 0
		for _, m := range batch {
			b, err := m.MarshalBinary()
			if err != nil {
				return nil, err
			}
			size += len(b)
			if m.Header.Type == netlink.HeaderType(unix.NFNL_SUBSYS_NFTABLES<<8|unix.NFT_MSG_NEWSETELEM) {
				listed, err := listedElements(m.Data)
				if err != nil {
					t.Errorf("decode the elements of a batch: %v", err)
				}
				sent += listed
			}
		}
		if size > netlinkSendLimit {
			t.Errorf("a batch has %d bytes, more than a netlink socket sends at once", size)
		}
		return batch, nil
	}))
	if err != nil {
		t.Fatal(err)
	}

	if err := install(conn, attempt, elements); err != nil {
		t.Fatal(err)
	}
	if sent != len(elements) {
		t.Fatalf("sent %d elements, want %d", sent, len(elements))
	}
}

// listedElements counts the elements a message that adds elements to a set
// lists, after its nfgenmsg header.
func listedElements(data []byte) (int, error) {
	decoder, err := netlink.NewAttributeDecoder(data[4:])
	if err != nil {
		return 0, err
	}
	listed := 0
	for decoder.Next() {
		if decoder.Type() == unix.NFTA_SET_ELEM_LIST_ELEMENTS {
			decoder.Nested(func(elements *netlink.AttributeDecoder) error {
				for elements.Next() {
					listed++
				}
				return elements.Err()
			})
		}
	}
	return listed, decoder.Err()
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
