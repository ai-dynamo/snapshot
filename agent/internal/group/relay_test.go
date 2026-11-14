// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package group

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/ai-dynamo/snapshot/coordinator/session"
)

// These tests stand in for a cuInterpose relay: they write the frames it
// writes, as raw MessagePack maps, on its end of a socket the agent serves.

func TestRelaysRunARoundThroughTheSession(t *testing.T) {
	members := joinAll(t, 3)
	// Rank 1 has no CUDA processes, so it runs no relay.
	each(t, members, func(ctx context.Context, m *Member) error {
		return m.Admit(ctx, Admission{PodIP: netip.MustParseAddr("10.1.0.1"), RunsRelay: m.Rank() != 1})
	})
	relay, err := members[1].OpenRelay(within(t))
	require.NoError(t, err)
	require.Nil(t, relay)
	leader, follower := openRelay(t, members[0]), openRelay(t, members[2])
	deadline := strconv.FormatInt(members[2].session.Meta().Deadline.Unix(), 10)
	assert.Equal(t, []string{"--rank", "2", "--relay-ranks", "0,2", "--deadline", deadline}, follower.args)

	write(t, follower.conn, map[string]any{"op": "get", "id": 1, "key": "inspect.1.cmd", "wait_ms": 60_000})
	assert.Equal(t, answer{"id": uint64(1), "ok": true}, call(t, leader.conn,
		map[string]any{"op": "insert", "id": 1, "key": "inspect.1.cmd", "value": []byte("INSPECT")}))
	assert.Equal(t, answer{"id": uint64(1), "ok": true, "value": []byte("INSPECT")}, read(t, follower.conn))
	assert.Equal(t, answer{"id": uint64(2), "ok": true}, call(t, follower.conn,
		map[string]any{"op": "insert", "id": 2, "key": "inspect.1.reply.2", "value": []byte("records")}))
	assert.Equal(t, answer{"id": uint64(7), "ok": true, "value": []byte("records")}, call(t, leader.conn,
		map[string]any{"op": "get", "id": 7, "key": "inspect.1.reply.2", "wait_ms": 60_000}))

	// The relays' keys stay in the cuinterpose namespace.
	_, err = members[1].agent.Get(within(t), "inspect.1.cmd", 0)
	require.ErrorIs(t, err, session.ErrNotFound)
	got, err := members[1].session.CuInterpose().Get(within(t), "inspect.1.cmd", 0)
	require.NoError(t, err)
	assert.Equal(t, []byte("INSPECT"), got)
}

func TestRelayAnswersInAnyOrderAndRefusesAnIDAlreadyPending(t *testing.T) {
	members := joinAll(t, 2)
	r := openRelay(t, members[0])
	peer := members[1].session.CuInterpose()

	write(t, r.conn, map[string]any{"op": "get", "id": 1, "key": "inspect.1.cmd", "wait_ms": 60_000})
	assert.Equal(t, answer{"id": uint64(2), "ok": true},
		call(t, r.conn, map[string]any{"op": "insert", "id": 2, "key": "x", "value": []byte("x")}),
		"a waiting get does not hold up a later request")
	assert.Equal(t, answer{"id": uint64(1), "ok": false, "code": "INVALID"},
		call(t, r.conn, map[string]any{"op": "get", "id": 1, "key": "x"}))
	assert.Equal(t, answer{"id": uint64(3), "ok": false, "code": "INVALID"},
		call(t, r.conn, map[string]any{"op": "rename", "id": 3}))
	assert.Equal(t, answer{"id": uint64(4), "ok": false, "code": "EXISTS"},
		call(t, r.conn, map[string]any{"op": "insert", "id": 4, "key": "x", "value": []byte("y")}))
	assert.Equal(t, answer{"id": uint64(5), "ok": false, "code": "NOT_FOUND"},
		call(t, r.conn, map[string]any{"op": "get", "id": 5, "key": "y"}), "a zero wait does not wait")

	require.NoError(t, peer.Insert(within(t), "inspect.1.cmd", []byte{}))
	assert.Equal(t, answer{"id": uint64(1), "ok": true, "value": []byte{}}, read(t, r.conn),
		"the first get with id 1 is still the one answered, with its empty value")
}

func TestRelayFailEndsTheAttempt(t *testing.T) {
	members := joinAll(t, 2)
	r := openRelay(t, members[0])
	write(t, r.conn, map[string]any{"op": "get", "id": 7, "key": "inspect.1.cmd", "wait_ms": 60_000})

	write(t, r.conn, map[string]any{"op": "fail", "id": 8, "reason": "Refused"})

	got := []answer{read(t, r.conn), read(t, r.conn)}
	assert.ElementsMatch(t, []answer{
		{"id": uint64(7), "ok": false, "code": "ABORTED", "reason": "Refused"},
		{"id": uint64(8), "ok": true},
	}, got)
	var header [4]byte
	_, err := io.ReadFull(r.conn, header[:])
	assert.ErrorIs(t, err, io.EOF, "the agent closes its end after the last answer")
	require.NoError(t, r.close())
	<-members[1].session.Done()
	assert.ErrorIs(t, members[1].session.Err(), &session.AbortError{Reason: "Refused"})
}

func TestRelayThatGoesAwayLeavesTheSession(t *testing.T) {
	members := joinAll(t, 2)
	r := openRelay(t, members[0])
	write(t, r.conn, map[string]any{"op": "get", "id": 1, "key": "inspect.1.cmd", "wait_ms": 60_000})

	require.NoError(t, r.conn.Close())

	require.NoError(t, r.close())
	assert.NoError(t, members[0].session.Err(), "the session outlives its relay")
}

func TestRelayFrameWithAnUnknownFieldEndsTheService(t *testing.T) {
	members := joinAll(t, 2)
	r := openRelay(t, members[0])
	// A map whose one field is unknown, and nests arrays as deep as a frame
	// allows: skipping it would recurse once for each level, past the stack's
	// limit, which would end the agent.
	body := append([]byte{0x81, 0xa1, 'x'}, bytes.Repeat([]byte{0x91}, maxRelayBody-4)...)
	body = append(body, 0xc0)
	_, err := r.conn.Write(append(binary.LittleEndian.AppendUint32(nil, uint32(len(body))), body...))
	require.NoError(t, err)

	var header [4]byte
	_, err = io.ReadFull(r.conn, header[:])
	assert.ErrorIs(t, err, io.EOF, "the agent closes its end")
	require.ErrorContains(t, r.close(), `unknown field "x"`)
	assert.NoError(t, members[0].session.Err(), "the session outlives its relay")
}

// testRelay is the relay's end of a relay socket, as a connection.
type testRelay struct {
	conn  net.Conn
	args  []string
	close func() error
}

// openRelay admits m alone as a relay, unless it was admitted already, and
// opens its relay socket.
func openRelay(t *testing.T, m *Member) testRelay {
	t.Helper()
	if m.Admissions() == nil {
		m.admissions = make([]Admission, len(m.session.Meta().Members))
		m.admissions[m.Rank()].RunsRelay = true
	}
	r, err := m.OpenRelay(within(t))
	require.NoError(t, err)
	conn, err := net.FileConn(r.File)
	require.NoError(t, err)
	// Only the test's connection keeps the relay's end open.
	require.NoError(t, r.File.Close())
	closeRelay := sync.OnceValue(r.Close)
	t.Cleanup(func() {
		_ = conn.Close()
		_ = closeRelay()
	})
	return testRelay{conn: conn, args: r.Args, close: closeRelay}
}

// answer is an answer as the relay decodes it: only the fields the agent sent.
type answer map[string]any

func write(t *testing.T, conn net.Conn, request map[string]any) {
	t.Helper()
	body, err := msgpack.Marshal(request)
	require.NoError(t, err)
	_, err = conn.Write(append(binary.LittleEndian.AppendUint32(nil, uint32(len(body))), body...))
	require.NoError(t, err)
}

func read(t *testing.T, conn net.Conn) answer {
	t.Helper()
	var header [4]byte
	_, err := io.ReadFull(conn, header[:])
	require.NoError(t, err)
	body := make([]byte, binary.LittleEndian.Uint32(header[:]))
	_, err = io.ReadFull(conn, body)
	require.NoError(t, err)
	var a answer
	require.NoError(t, msgpack.Unmarshal(body, &a))
	return a
}

// call writes request and reads the next answer, for a request answered
// before any other pending one.
func call(t *testing.T, conn net.Conn, request map[string]any) answer {
	t.Helper()
	write(t, conn, request)
	return read(t, conn)
}
