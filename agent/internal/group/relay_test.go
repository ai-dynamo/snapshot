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
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/ai-dynamo/snapshot/coordinator"
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

func TestRelayFailEndsTheAttemptAsTheMemberWould(t *testing.T) {
	for _, tc := range []struct {
		name        string
		committed   bool
		relayReason string
		want        string
	}{
		{"before the admitted barrier", false, session.ReasonMemberFailed, ReasonRefused},
		{"after the admitted barrier", true, ReasonRefused, session.ReasonMemberFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			members := joinAll(t, 2)
			if tc.committed {
				each(t, members, func(ctx context.Context, m *Member) error { return m.Barrier(ctx, Admitted) })
			}
			r := openRelay(t, members[0])
			write(t, r.conn, map[string]any{"op": "get", "id": 7, "key": "inspect.1.cmd", "wait_ms": 60_000})

			write(t, r.conn, map[string]any{"op": "fail", "id": 8, "reason": tc.relayReason})

			got := []answer{read(t, r.conn), read(t, r.conn)}
			assert.ElementsMatch(t, []answer{
				{"id": uint64(7), "ok": false, "code": "ABORTED", "reason": tc.want},
				{"id": uint64(8), "ok": true},
			}, got)
			var header [4]byte
			_, err := io.ReadFull(r.conn, header[:])
			assert.ErrorIs(t, err, io.EOF, "the agent closes its end after the last answer")
			require.NoError(t, r.close())
			<-members[1].session.Done()
			assert.ErrorIs(t, members[1].session.Err(), &session.AbortError{Reason: tc.want})
		})
	}
}

func TestRelayThatGoesAwayLeavesTheSession(t *testing.T) {
	members := joinAll(t, 2)
	r := openRelay(t, members[0])
	write(t, r.conn, map[string]any{"op": "get", "id": 1, "key": "inspect.1.cmd", "wait_ms": 60_000})

	require.NoError(t, r.conn.Close())

	require.NoError(t, r.close())
	assert.NoError(t, members[0].session.Err(), "the session outlives its relay")
}

func TestRelayWaitsForRoomInFlight(t *testing.T) {
	for _, tc := range []struct {
		name    string
		inserts int
		value   []byte
		want    int32
	}{
		// A get for the other member, and the requests besides.
		{"by requests", 10, []byte("x"), 1 + relayRequestsBesideGets},
		{"by bytes", 3, make([]byte, maxRelayBody-1024), 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meta := session.Meta{
				Deadline: time.Now().Add(time.Hour),
				Members:  []session.Member{{ID: "member-0"}, {ID: "member-1", Rank: 1}},
			}
			store := &holdingStore{release: make(chan struct{})}
			r := openRelay(t, &Member{session: holdingSession{metaSession{meta: meta}, store}, id: "member-0"})
			var frames []byte
			for id := range tc.inserts {
				frames = append(frames, frame(t, map[string]any{"op": "insert", "id": id, "key": strconv.Itoa(id), "value": tc.value})...)
			}
			written := make(chan error, 1)
			go func() {
				_, err := r.conn.Write(frames)
				written <- err
			}()

			require.Eventually(t, func() bool { return store.started.Load() == tc.want }, 10*time.Second, time.Millisecond)
			time.Sleep(50 * time.Millisecond)
			assert.Equal(t, tc.want, store.started.Load(), "the agent reads no request past the relay's room")

			close(store.release)
			for range tc.inserts {
				assert.Equal(t, true, read(t, r.conn)["ok"])
			}
			require.NoError(t, <-written)
		})
	}
}

func TestRelayLeaderWaitingForEveryOtherRelayStillWrites(t *testing.T) {
	members := joinAll(t, 4)
	leader := openRelay(t, members[0])
	// A request that waits for room fails the test rather than hang it.
	require.NoError(t, leader.conn.SetDeadline(time.Now().Add(10*time.Second)))

	// The leader waits for every other relay's reply at once.
	for rank := 1; rank < 4; rank++ {
		write(t, leader.conn, map[string]any{
			"op": "get", "id": rank, "key": "round.reply." + strconv.Itoa(rank), "wait_ms": 60_000,
		})
	}
	// Its writes still have room, the largest value among them, and so has an
	// insert that ends one of its waits.
	largest := make([]byte, members[0].session.Meta().MaxValue)
	assert.Equal(t, answer{"id": uint64(4), "ok": true}, call(t, leader.conn,
		map[string]any{"op": "insert", "id": 4, "key": "round.cmd", "value": largest}))
	write(t, leader.conn, map[string]any{"op": "insert", "id": 5, "key": "round.reply.1", "value": []byte("1")})
	assert.ElementsMatch(t, []answer{
		{"id": uint64(1), "ok": true, "value": []byte("1")},
		{"id": uint64(5), "ok": true},
	}, []answer{read(t, leader.conn), read(t, leader.conn)})

	// The other relays' replies end the other waits.
	for rank := 2; rank < 4; rank++ {
		require.NoError(t, members[rank].session.CuInterpose().Insert(within(t), "round.reply."+strconv.Itoa(rank), []byte("r")))
	}
	assert.ElementsMatch(t, []answer{
		{"id": uint64(2), "ok": true, "value": []byte("r")},
		{"id": uint64(3), "ok": true, "value": []byte("r")},
	}, []answer{read(t, leader.conn), read(t, leader.conn)})
}

func TestRelayThatStopsReadingDoesNotHoldTheAgent(t *testing.T) {
	original := relayAnswerTimeout
	t.Cleanup(func() { relayAnswerTimeout = original })
	relayAnswerTimeout = 100 * time.Millisecond

	for _, tc := range []struct {
		name string
		end  func(t *testing.T, peer *Member, conn net.Conn)
	}{
		{"the session ends", func(t *testing.T, peer *Member, _ net.Conn) {
			require.NoError(t, peer.Fail(within(t)))
		}},
		{"the relay breaks the protocol", func(t *testing.T, _ *Member, conn net.Conn) {
			_, err := conn.Write(binary.LittleEndian.AppendUint32(nil, maxRelayBody+1))
			require.NoError(t, err)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			members := joinAll(t, 2)
			r := openRelay(t, members[0])
			// The relay starts to read an answer far larger than its socket's
			// buffer, and then reads no more.
			require.NoError(t, members[1].session.CuInterpose().Insert(within(t), "records", make([]byte, 8<<20)))
			write(t, r.conn, map[string]any{"op": "get", "id": 1, "key": "records"})
			var header [4]byte
			_, err := io.ReadFull(r.conn, header[:])
			require.NoError(t, err)

			tc.end(t, members[1], r.conn)

			served := make(chan struct{})
			go func() {
				_ = r.close()
				close(served)
			}()
			select {
			case <-served:
			case <-time.After(10 * time.Second):
				t.Fatal("the agent still writes to a relay that stopped reading")
			}
		})
	}
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
	_, err := conn.Write(frame(t, request))
	require.NoError(t, err)
}

// frame encodes request as one frame.
func frame(t *testing.T, request map[string]any) []byte {
	t.Helper()
	body, err := msgpack.Marshal(request)
	require.NoError(t, err)
	return append(binary.LittleEndian.AppendUint32(nil, uint32(len(body))), body...)
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

// holdingSession is a member's session of rank 0, which never ends, whose
// relay store is store.
type holdingSession struct {
	metaSession
	store *holdingStore
}

func (s holdingSession) CuInterpose() coordinator.Store { return s.store }

func (holdingSession) Done() <-chan struct{} { return nil }

// holdingStore holds every insert until release is closed, and counts the
// inserts it has started.
type holdingStore struct {
	coordinator.Store
	release chan struct{}
	started atomic.Int32
}

func (s *holdingStore) Insert(ctx context.Context, _ string, _ []byte) error {
	s.started.Add(1)
	select {
	case <-s.release:
	case <-ctx.Done():
	}
	return nil
}
