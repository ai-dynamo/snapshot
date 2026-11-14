// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package group

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"time"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/ai-dynamo/snapshot/coordinator"
	"github.com/ai-dynamo/snapshot/coordinator/session"
)

// A relay's session socket carries frames, each a u32 little-endian body
// length, then a MessagePack map with named fields, as on cuInterpose's control
// socket.
//
// The relay asks for four operations, each with an id unique among its pending
// requests: insert {op, id, key, value}, get {op, id, key, wait_ms}, where a
// zero wait_ms does not wait, delete {op, id, key}, and fail {op, id, reason}.
// Values are bin. Keys are the relay's own: the agent places them in the
// session's cuinterpose namespace, so a relay reaches nothing else. A fail ends
// the attempt as the member's own failure would, whatever its reason says:
// Refused until the member calls the admitted barrier, and MemberFailed after.
// The agent answers each request once, in any order, with {id, ok}, a value on
// a successful get, and on a failure a code, such as NOT_YET, with the
// session's reason for ABORTED. When the session ends, the agent answers every
// pending request with ABORTED or CLOSED and closes its end.
//
// A get's wait starts once the coordinator has answered that the key is
// absent, so while the agent cannot reach the coordinator, the answer can come
// later than wait_ms, though never after the session's end.

// maxRelayBody is the largest frame body either end accepts: a value of the
// session's largest, with its key and the rest of the message.
const maxRelayBody = 33 << 20

// The operations a relay asks for.
const (
	opInsert = "insert"
	opGet    = "get"
	opDelete = "delete"
	opFail   = "fail"
)

// relayRequest is one message from the relay. Fields an operation does not
// use are absent, which decodes as their zero value, and a frame with any other
// field ends the service.
type relayRequest struct {
	Op     string `msgpack:"op"`
	ID     uint64 `msgpack:"id"`
	Key    string `msgpack:"key,omitempty"`
	Value  []byte `msgpack:"value,omitempty"`
	WaitMS uint64 `msgpack:"wait_ms,omitempty"`
	Reason string `msgpack:"reason,omitempty"`
}

// relayAnswer is one message to the relay, answering the request with its id.
type relayAnswer struct {
	ID     uint64
	OK     bool
	Value  []byte
	Code   string
	Reason string
	// hasValue marks a successful get, whose value is present even when it is
	// empty, unlike every other answer's.
	hasValue bool
}

// EncodeMsgpack writes only the fields the answer carries, so the relay can
// tell a successful get of an empty value from any other answer.
func (a *relayAnswer) EncodeMsgpack(enc *msgpack.Encoder) error {
	fields := map[string]any{"id": a.ID, "ok": a.OK}
	if a.hasValue {
		// A nil value would encode as nil, not as an empty bin.
		fields["value"] = append([]byte{}, a.Value...)
	}
	if a.Code != "" {
		fields["code"] = a.Code
	}
	if a.Reason != "" {
		fields["reason"] = a.Reason
	}
	return enc.Encode(fields)
}

// The codes a relay reads for a failed request.
const (
	codeAborted  = "ABORTED"
	codeInvalid  = "INVALID"
	codeInternal = "INTERNAL"
)

// codes maps the session's errors to the codes a relay reads.
var codes = []struct {
	err  error
	code string
}{
	{session.ErrNotYet, "NOT_YET"},
	{session.ErrNotFound, "NOT_FOUND"},
	{session.ErrExists, "EXISTS"},
	{session.ErrTooLarge, "TOO_LARGE"},
	{session.ErrTooMany, "TOO_MANY"},
	{session.ErrInvalid, codeInvalid},
	{session.ErrForbidden, "FORBIDDEN"},
	{session.ErrClosed, "CLOSED"},
	{context.Canceled, "CANCELLED"},
	{context.DeadlineExceeded, "CANCELLED"},
}

// serveRelay answers the relay on conn, the agent's end of the relay's socket,
// from the cuinterpose namespace of m's session. Requests run concurrently and
// are answered as they complete, so a waiting get never holds up a later
// request. A request whose id is already pending is answered INVALID.
//
// serveRelay returns when the relay closes its end, cancelling the requests it
// has pending, and leaving the session as it is; when the session ends, once
// it has answered every pending request with the session's end and closed
// conn; or when ctx ends.
func serveRelay(ctx context.Context, conn io.ReadWriteCloser, m *Member) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	p := &relayProxy{conn: conn, member: m, store: m.session.CuInterpose(), pending: make(map[uint64]bool)}
	requests := make(chan relayRequest)
	failed := make(chan error, 1)
	stopped := make(chan struct{})
	go p.read(requests, failed, stopped)
	// closeConn closes the agent's end once every answer is out, and waits for
	// the reader.
	closeConn := func() {
		p.running.Wait()
		close(stopped)
		_ = conn.Close()
		<-failed
	}

	for {
		select {
		case req := <-requests:
			p.start(ctx, req)
		case err := <-failed:
			// The relay is gone, or broke the protocol: nothing it asked for is
			// wanted any more.
			cancel()
			p.running.Wait()
			_ = conn.Close()
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		case <-m.session.Done():
			// Every pending request returns with the session's end now, and so
			// does any request the reader holds already.
			for drained := false; !drained; {
				select {
				case req := <-requests:
					p.start(ctx, req)
				default:
					drained = true
				}
			}
			closeConn()
			return nil
		case <-ctx.Done():
			closeConn()
			return ctx.Err()
		}
	}
}

// relayProxy is the agent's end of one relay's socket.
type relayProxy struct {
	conn   io.ReadWriteCloser
	member *Member
	store  coordinator.Store

	// writeMu serializes answers, so frames never interleave.
	writeMu sync.Mutex

	// mu guards pending, the ids of the relay's requests not answered yet.
	mu      sync.Mutex
	pending map[uint64]bool

	running sync.WaitGroup
}

// read hands every request the relay sends to serveRelay, until reading fails.
func (p *relayProxy) read(requests chan<- relayRequest, failed chan<- error, stopped <-chan struct{}) {
	for {
		var req relayRequest
		if err := readRelayFrame(p.conn, &req); err != nil {
			failed <- err
			return
		}
		select {
		case requests <- req:
		case <-stopped:
			failed <- nil
			return
		}
	}
}

// start forwards one request in a goroutine of its own.
func (p *relayProxy) start(ctx context.Context, req relayRequest) {
	p.mu.Lock()
	duplicate := p.pending[req.ID]
	p.pending[req.ID] = true
	p.mu.Unlock()
	p.running.Go(func() {
		if duplicate {
			p.answer(relayAnswer{ID: req.ID, Code: codeInvalid})
			return
		}
		answer := p.forward(ctx, req)
		// The id is free again by the time the relay reads the answer.
		p.mu.Lock()
		delete(p.pending, req.ID)
		p.mu.Unlock()
		p.answer(answer)
	})
}

// forward runs one request in the relay's namespace.
func (p *relayProxy) forward(ctx context.Context, req relayRequest) relayAnswer {
	var (
		value []byte
		err   error
	)
	switch req.Op {
	case opInsert:
		err = p.store.Insert(ctx, req.Key, req.Value)
	case opGet:
		value, err = p.store.Get(ctx, req.Key, waitOf(req.WaitMS))
	case opDelete:
		err = p.store.Delete(ctx, req.Key)
	case opFail:
		err = p.member.Fail(ctx)
	default:
		return relayAnswer{ID: req.ID, Code: codeInvalid}
	}
	if err != nil {
		return failure(req.ID, err)
	}
	return relayAnswer{ID: req.ID, OK: true, Value: value, hasValue: req.Op == opGet}
}

// answer writes one answer. The relay may be gone, which serveRelay learns
// from reading, so a failed write is dropped.
func (p *relayProxy) answer(a relayAnswer) {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	_ = writeRelayFrame(p.conn, &a)
}

// failure translates a failed request into its answer: the session's end with
// its reason, the code of a session error, or INTERNAL for anything else, such
// as a connection that closed.
func failure(id uint64, err error) relayAnswer {
	var abort *session.AbortError
	if errors.As(err, &abort) {
		return relayAnswer{ID: id, Code: codeAborted, Reason: abort.Reason}
	}
	for _, c := range codes {
		if errors.Is(err, c.err) {
			return relayAnswer{ID: id, Code: c.code}
		}
	}
	return relayAnswer{ID: id, Code: codeInternal}
}

// waitOf converts a relay's wait, saturating one too long for a Duration.
func waitOf(ms uint64) time.Duration {
	if ms > uint64(math.MaxInt64/int64(time.Millisecond)) {
		return math.MaxInt64
	}
	return time.Duration(ms) * time.Millisecond
}

// readRelayFrame reads one frame into v, and refuses a field v does not have.
// A clean end of the stream before a frame is io.EOF.
func readRelayFrame(r io.Reader, v any) error {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return err
	}
	size := binary.LittleEndian.Uint32(header[:])
	if size > maxRelayBody {
		return fmt.Errorf("a frame of %d bytes is over the limit of %d", size, maxRelayBody)
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(r, body); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return err
	}
	// Skipping a field recurses once for each level its value nests, without a
	// limit, so a deep one would overflow the stack, which ends the agent.
	dec := msgpack.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields(true)
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("decode a frame: %w", err)
	}
	return nil
}

// writeRelayFrame writes v as one frame, in a single write.
func writeRelayFrame(w io.Writer, v any) error {
	body, err := msgpack.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode a frame: %w", err)
	}
	if len(body) > maxRelayBody {
		return fmt.Errorf("a frame of %d bytes is over the limit of %d", len(body), maxRelayBody)
	}
	frame := binary.LittleEndian.AppendUint32(make([]byte, 0, 4+len(body)), uint32(len(body)))
	_, err = w.Write(append(frame, body...))
	return err
}
