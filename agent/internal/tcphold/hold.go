// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package tcphold holds the TCP connections a group restore preserves.
//
// The members of a group finish their restores seconds apart, and each keeps its
// connections to the others exactly as dumped. Once CRIU takes such a socket
// out of repair mode, its kernel may send a window probe, a retransmission, or
// a keepalive to a peer whose socket does not exist yet, and the peer's kernel
// answers with a reset that kills the connection at both ends. So from before
// CRIU restores a member until every member has restored its sockets, the
// member's network namespace drops every packet of its preserved connections,
// in both directions. Nothing else is held back: new connections, such as a
// library's bootstrap traffic, pass.
//
// A hold is one nftables table in the Pod's network namespace, which the agent
// writes over netlink. The table has a set of the held connections and a base
// chain on the input hook and one on the output hook, each with one rule that
// drops a packet whose addresses and ports are in the set. The table is named
// for the attempt, so an agent that restarted finds and lifts a hold it did not
// install. Install and Lift each send one netlink batch, which the kernel
// applies whole or not at all, so a hold is never partly there.
package tcphold

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/netip"
	"os"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"

	"github.com/ai-dynamo/snapshot/agent/internal/criu"
)

const (
	// tablePrefix starts the name of every hold's table.
	tablePrefix = "snapshot-hold-"
	// setName names the set of held connections in a hold's table.
	setName = "connections"
	// setID refers to the set from the rules of the same batch. It only has
	// to be unique within one batch.
	setID = 1
)

// connectionType is the key of the set: source address, source port,
// destination address and destination port of a packet.
var connectionType = nftables.MustConcatSetType(
	nftables.TypeIPAddr, nftables.TypeInetService, nftables.TypeIPAddr, nftables.TypeInetService)

// Install drops every packet of connections, in both directions, in the network
// namespace of the process hostPID, under the table of attempt. It replaces the
// hold attempt already has there, if any, so a retried restore installs again.
// Only IPv4 connections can be held, as only IPv4 addresses are remapped.
func Install(hostPID int, attempt string, connections []criu.TCPConnection) error {
	elements := make([]nftables.SetElement, 0, 2*len(connections))
	for _, c := range connections {
		if !c.Local.Addr().Is4() || !c.Remote.Addr().Is4() {
			return fmt.Errorf("hold the connection from %s to %s: only IPv4 connections can be held", c.Local, c.Remote)
		}
		elements = append(elements, nftables.SetElement{Key: key(c.Local, c.Remote)},
			nftables.SetElement{Key: key(c.Remote, c.Local)})
	}
	err := inNetworkNamespace(hostPID, func(conn *nftables.Conn) error {
		table := removeTable(conn, attempt)
		conn.AddTable(table)
		set := &nftables.Set{
			Table:         table,
			ID:            setID,
			Name:          setName,
			KeyType:       connectionType,
			Concatenation: true,
			KeyByteOrder:  binaryutil.BigEndian,
		}
		if err := conn.AddSet(set, elements); err != nil {
			return err
		}
		for _, hook := range []struct {
			name string
			hook *nftables.ChainHook
		}{{"input", nftables.ChainHookInput}, {"output", nftables.ChainHookOutput}} {
			// The raw priority runs ahead of conntrack and of every usual
			// filter chain.
			chain := conn.AddChain(&nftables.Chain{
				Name:     hook.name,
				Table:    table,
				Type:     nftables.ChainTypeFilter,
				Hooknum:  hook.hook,
				Priority: nftables.ChainPriorityRaw,
			})
			conn.AddRule(&nftables.Rule{Table: table, Chain: chain, Exprs: dropHeld(set)})
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("install the restore hold %s: %w", tableName(attempt), err)
	}
	return nil
}

// Lift removes the hold of attempt from the network namespace of the process
// hostPID, which may be any process in that namespace. Lifting a hold that is
// not there succeeds.
func Lift(hostPID int, attempt string) error {
	err := inNetworkNamespace(hostPID, func(conn *nftables.Conn) error {
		removeTable(conn, attempt)
		return nil
	})
	if err != nil {
		return fmt.Errorf("lift the restore hold %s: %w", tableName(attempt), err)
	}
	return nil
}

// removeTable queues the removal of attempt's table, whether or not it is
// there: adding a table that exists changes nothing, and the kernel refuses to
// delete one that does not.
func removeTable(conn *nftables.Conn, attempt string) *nftables.Table {
	table := &nftables.Table{Name: tableName(attempt), Family: nftables.TableFamilyIPv4}
	conn.AddTable(table)
	conn.DelTable(table)
	return table
}

// tableName returns the name of the table of attempt's hold. The name carries
// a digest of the attempt, which fits the kernel's limit on a table name
// whatever the attempt is.
func tableName(attempt string) string {
	digest := sha256.Sum256([]byte(attempt))
	return tablePrefix + hex.EncodeToString(digest[:8])
}

// dropHeld returns a rule that drops a TCP packet whose source address and
// port and destination address and port are in set. A drop is final: no later
// chain on the same hook, in this table or another, sees the packet, so
// accepting it elsewhere, as a filter table that allows the Pod's traffic
// does, cannot let it through.
func dropHeld(set *nftables.Set) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.IPPROTO_TCP}},
		// The key fills registers 1 (4 bytes, also called 8), 9, 10 and 11,
		// one per field: a port is padded to a register.
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 12, Len: 4},
		&expr.Payload{DestRegister: 9, Base: expr.PayloadBaseTransportHeader, Offset: 0, Len: 2},
		&expr.Payload{DestRegister: 10, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
		&expr.Payload{DestRegister: 11, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
		&expr.Lookup{SourceRegister: 1, SetName: set.Name, SetID: set.ID},
		&expr.Verdict{Kind: expr.VerdictDrop},
	}
}

// key returns the set key of the packets that go from one address to another:
// each field in network byte order, padded to 4 bytes.
func key(from, to netip.AddrPort) []byte {
	k := make([]byte, 0, 16)
	k = append(k, from.Addr().AsSlice()...)
	k = binary.BigEndian.AppendUint16(k, from.Port())
	k = append(k, 0, 0)
	k = append(k, to.Addr().AsSlice()...)
	k = binary.BigEndian.AppendUint16(k, to.Port())
	return append(k, 0, 0)
}

// inNetworkNamespace sends what queue adds to a connection to nftables in the
// network namespace of hostPID, as one batch.
func inNetworkNamespace(hostPID int, queue func(*nftables.Conn) error) error {
	namespace, err := os.Open(fmt.Sprintf("/proc/%d/ns/net", hostPID))
	if err != nil {
		return fmt.Errorf("open the network namespace of %d: %w", hostPID, err)
	}
	defer namespace.Close()
	conn, err := nftables.New(nftables.WithNetNSFd(int(namespace.Fd())))
	if err != nil {
		return err
	}
	if err := queue(conn); err != nil {
		return err
	}
	return conn.Flush()
}
