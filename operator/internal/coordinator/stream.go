// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordinator

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"

	wire "github.com/ai-dynamo/snapshot/agent/pkg/coordinator/v1alpha1"
	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/session"
	"github.com/go-logr/logr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// unansweredBackstop is the most requests a stream may have unanswered at
// once, whether still running or with a response waiting to be sent. The agent
// bounds what it forwards well below it (coordinator.proto, rule 10), so only a
// broken client reaches it. It caps the goroutines one stream can hold on the
// coordinator, and the responses they hold, which the session's byte budget
// does not: a blocked Get stores nothing, and a response holds a copy.
const unansweredBackstop = 4096

// exit is how a stream ended, which decides what the registry does next.
type exit int

const (
	// exitEnded: the session ended. The stream sent Ended, or its client gave
	// the stream up, or broke it, before Ended could go out.
	exitEnded exit = iota
	// exitSuperseded: a newer stream for the same member took over.
	exitSuperseded
	// exitLost: the stream broke, the client closed it, or it broke the
	// protocol, before the session ended.
	exitLost
)

// stream is one member's attached stream.
type stream struct {
	grpc    wire.Coordinator_AttachServer
	session *session.Session
	member  session.MemberID
	log     logr.Logger

	// seq numbers the stream by when the server read its Hello. Of a member's
	// streams, the one with the highest seq wins.
	seq uint64

	// ctx is the parent of every request's context. Cancelling it, when the
	// stream ends, cancels every request still running.
	ctx    context.Context
	cancel context.CancelFunc

	// superseded is closed by the registry when a newer stream for the same
	// member takes over.
	superseded chan struct{}

	// sendMu serializes sends, which gRPC forbids from running concurrently.
	sendMu sync.Mutex
	// silent, once set, stops every later send. It is atomic, so silencing the
	// stream never waits behind a send blocked on a client that stopped
	// reading.
	silent atomic.Bool

	// mu guards pending and unanswered.
	mu sync.Mutex
	// pending holds the cancel function of every request still running, by
	// request ID.
	pending map[string]context.CancelFunc
	// unanswered counts the requests admitted whose response has been neither
	// sent nor dropped, which the backstop bounds.
	unanswered int

	// running counts the request goroutines until their request has run, and
	// answering until its response is sent or dropped.
	running   sync.WaitGroup
	answering sync.WaitGroup
}

func newStream(grpcStream wire.Coordinator_AttachServer, sess *session.Session, member session.MemberID,
	seq uint64, log logr.Logger) *stream {
	ctx, cancel := context.WithCancel(grpcStream.Context())
	return &stream{
		grpc:       grpcStream,
		session:    sess,
		member:     member,
		log:        log,
		seq:        seq,
		ctx:        ctx,
		cancel:     cancel,
		superseded: make(chan struct{}),
		pending:    make(map[string]context.CancelFunc),
	}
}

// run sends the Welcome, then serves the stream until it ends, and reports how
// it ended. One goroutine receives, because Recv cannot be interrupted; this
// one dispatches what it receives and watches the session and the registry.
func (st *stream) run(welcome *wire.ServerMessage) (exit, error) {
	if err := st.send(welcome); err != nil {
		return st.stop(exitLost, err)
	}
	messages := make(chan *wire.ClientMessage)
	failed := make(chan error, 1)
	go st.receive(messages, failed)

	for {
		select {
		case msg := <-messages:
			if err := st.dispatch(msg); err != nil {
				return st.stop(exitLost, err)
			}
		case err := <-failed:
			if errors.Is(err, io.EOF) {
				err = nil
			}
			return st.stop(exitLost, err)
		case <-st.session.Done():
			return st.end(messages, failed)
		case <-st.superseded:
			st.log.Info("Stream superseded by a newer one for the same member")
			return st.stop(exitSuperseded, status.Error(codes.Aborted,
				"a newer stream for this member superseded this one"))
		}
	}
}

// receive hands every message the client sends to run, until Recv fails. It
// returns as soon as gRPC ends the stream, which happens when run's handler
// returns.
func (st *stream) receive(messages chan<- *wire.ClientMessage, failed chan<- error) {
	for {
		msg, err := st.grpc.Recv()
		if err != nil {
			failed <- err
			return
		}
		select {
		case messages <- msg:
		case <-st.ctx.Done():
			return
		}
	}
}

// dispatch acts on one message after the handshake. A message that breaks the
// protocol ends the stream.
func (st *stream) dispatch(msg *wire.ClientMessage) error {
	switch m := msg.GetMessage().(type) {
	case *wire.ClientMessage_Request:
		return st.start(m.Request)
	case *wire.ClientMessage_Cancel:
		st.cancelRequest(m.Cancel.GetId())
		return nil
	case *wire.ClientMessage_Hello:
		return status.Error(codes.InvalidArgument, "a second Hello")
	default:
		return status.Error(codes.InvalidArgument, "a message with nothing in it")
	}
}

// start runs one request in a goroutine of its own. A request with no id, or
// with an id already pending, breaks the protocol and ends the stream, since no
// answer to it could be told apart from another's; so does a request past the
// backstop.
func (st *stream) start(req *wire.Request) error {
	ctx, err := st.admit(req.GetId())
	if err != nil {
		return err
	}
	st.running.Add(1)
	st.answering.Add(1)
	go func() {
		defer st.answering.Done()
		resp := st.execute(ctx, req)
		// The request leaves the pending set before its response goes out, so
		// its id is free again by the time the client sees the response. It
		// stays unanswered, against the backstop, until the response is sent.
		st.release(req.GetId())
		st.running.Done()
		_ = st.send(responseMessage(resp))
		st.answered()
	}()
	return nil
}

// admit reserves a pending slot for a request and returns its context, or the
// gRPC status that ends the stream.
func (st *stream) admit(id string) (context.Context, error) {
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "a request without an id")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if _, ok := st.pending[id]; ok {
		return nil, status.Errorf(codes.InvalidArgument, "request %q is already pending", id)
	}
	if st.unanswered >= unansweredBackstop {
		return nil, status.Errorf(codes.ResourceExhausted, "more than %d requests unanswered", unansweredBackstop)
	}
	ctx, cancel := context.WithCancel(st.ctx)
	st.pending[id] = cancel
	st.unanswered++
	return ctx, nil
}

// release frees the id and the context of a request that has run.
func (st *stream) release(id string) {
	st.mu.Lock()
	cancel := st.pending[id]
	delete(st.pending, id)
	st.mu.Unlock()
	cancel()
}

// answered counts a request as answered once its response is sent or dropped.
func (st *stream) answered() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.unanswered--
}

// cancelRequest cancels a pending request. A Cancel for an id that is not
// pending is ignored.
func (st *stream) cancelRequest(id string) {
	st.mu.Lock()
	cancel, ok := st.pending[id]
	st.mu.Unlock()
	if ok {
		cancel()
	}
}

// end closes a stream whose session has ended. Every request still running
// returns promptly, since the session has ended, and is answered before Ended,
// which is the stream's last message. Any of those sends may wait on a client
// that is not reading, so a goroutine makes them, reporting how Ended went
// through a buffered channel it never blocks on, while end keeps receiving: a
// request read now gets no Response, which Ended settles, and a client that
// gives the stream up, or breaks it, ends the stream at once, without Ended.
func (st *stream) end(messages <-chan *wire.ClientMessage, failed <-chan error) (exit, error) {
	delivered := make(chan error, 1)
	go func() {
		st.answering.Wait()
		delivered <- st.send(endedMessage(st.session.Err()))
	}()
	for {
		select {
		case err := <-delivered:
			st.cancel()
			return exitEnded, err
		case <-messages:
			// Read after the session ended: Ended will settle it.
		case err := <-failed:
			if errors.Is(err, io.EOF) {
				err = nil
			}
			return st.stop(exitEnded, err)
		}
	}
}

// stop closes a stream that is not ending with its session. It silences the
// stream, cancels every request still running, and waits for each to return,
// but not for any response to go out: a client that stopped reading must not
// hold the stream, or its member's grace period, up. A send already under way
// is left to gRPC, which interrupts it once the handler returns.
func (st *stream) stop(how exit, err error) (exit, error) {
	st.silence()
	st.cancel()
	st.running.Wait()
	return how, err
}

// send sends one message, unless the stream has been silenced.
func (st *stream) send(msg *wire.ServerMessage) error {
	st.sendMu.Lock()
	defer st.sendMu.Unlock()
	if st.silent.Load() {
		return nil
	}
	return st.grpc.Send(msg)
}

// silence stops every later send, for a stream that is stopping.
func (st *stream) silence() {
	st.silent.Store(true)
}

// key identifies the member the stream acts for.
func (st *stream) key() memberKey {
	return memberKey{uid: st.session.UID(), member: st.member}
}

// supersede tells the stream a newer one took over. The registry calls it once,
// when it replaces the stream.
func (st *stream) supersede() {
	close(st.superseded)
}
