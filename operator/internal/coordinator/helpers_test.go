// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordinator

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	wire "github.com/ai-dynamo/snapshot/agent/pkg/coordinator/v1alpha1"
	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/internal/clocktest"
	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/kv"
	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/kv/memory"
	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/session"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/durationpb"
	"k8s.io/apimachinery/pkg/types"
)

// The fixtures every test shares.
const (
	attempt     = types.UID("attempt-1")
	memberA     = session.MemberID("member-a")
	memberB     = session.MemberID("member-b")
	memberC     = session.MemberID("member-c")
	agentKey    = "snapshot/handles"
	agentOther  = "snapshot/ranks"
	ncclKey     = "nccl/bootstrap"
	barrier     = "snapshot/admitted"
	deadlineIn  = time.Minute
	gracePeriod = 3 * time.Second

	// awaitLimit bounds how long a test waits for the server before failing.
	// It is a safeguard against a hang, never a delay a test relies on.
	awaitLimit = 10 * time.Second
)

// baseTime is where every fake clock starts.
var baseTime = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

// fixture is a Server on in-memory gRPC, over a real session Manager on the
// memory store, with a fake clock and a fake Authorizer.
type fixture struct {
	clock   *clocktest.Clock
	manager *session.Manager
	auth    *authorizer
	logs    *logBuffer
	client  wire.CoordinatorClient
	// returned receives what each Attach handler returned, so a test can see
	// how the server ended a stream whose client already gave up on it.
	returned chan error
	// sends, once a test watches them, reports the server's sends on every
	// stream.
	sends atomic.Pointer[sendWatch]
	// listener serves the server, for a test that needs a client of its own.
	listener *bufconn.Listener
}

// newFixture starts a server over the memory store.
func newFixture(t *testing.T) *fixture {
	return newFixtureOn(t, memory.New())
}

// newFixtureOn starts a server over store. Every goroutine the test starts must
// have returned by the time it ends.
func newFixtureOn(t *testing.T, store kv.Store) *fixture {
	t.Helper()
	ignore := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, ignore) })

	f := &fixture{clock: clocktest.New(baseTime), auth: &authorizer{}, logs: &logBuffer{}, returned: make(chan error, 64)}
	manager, err := session.NewManager(session.Config{Store: store, Clock: f.clock})
	require.NoError(t, err)
	f.manager = manager
	server, err := NewServer(Config{
		Manager:    manager,
		Authorizer: f.auth,
		Clock:      f.clock,
		Grace:      gracePeriod,
		Log:        funcr.New(f.logs.write, funcr.Options{Verbosity: 1}),
	})
	require.NoError(t, err)

	listener := bufconn.Listen(1 << 20)
	f.listener = listener
	grpcServer := grpc.NewServer(grpc.MaxRecvMsgSize(wire.MaxMessageSize), grpc.WaitForHandlers(true),
		grpc.StreamInterceptor(f.report))
	wire.RegisterCoordinatorServer(grpcServer, server)
	served := make(chan error, 1)
	go func() { served <- grpcServer.Serve(listener) }()
	t.Cleanup(func() {
		grpcServer.Stop()
		require.NoError(t, <-served)
	})
	f.client = f.connect(t)
	return f
}

// connect returns a client on a connection of its own, which the test's
// cleanup closes before the server stops.
func (f *fixture) connect(t *testing.T, opts ...grpc.DialOption) wire.CoordinatorClient {
	t.Helper()
	opts = append([]grpc.DialOption{
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return f.listener.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(wire.MaxMessageSize),
			grpc.MaxCallSendMsgSize(wire.MaxMessageSize)),
	}, opts...)
	conn, err := grpc.NewClient("passthrough:///coordinator", opts...)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	return wire.NewCoordinatorClient(conn)
}

// attachUnread attaches member on a connection whose flow-control windows stay
// at gRPC's minimum of 64 KiB, reads Welcome, and then reads nothing more, as a
// client that stopped reading would. Once its windows fill, the server's sends
// on the stream block.
func (f *fixture) attachUnread(t *testing.T, member session.MemberID) wire.Coordinator_AttachClient {
	t.Helper()
	client := f.connect(t, grpc.WithInitialWindowSize(64<<10), grpc.WithInitialConnWindowSize(64<<10))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stream, err := client.Attach(ctx)
	require.NoError(t, err)
	require.NoError(t, stream.Send(hello(member, 0)))
	msg, err := stream.Recv()
	require.NoError(t, err)
	require.NotNil(t, msg.GetWelcome(), "expected Welcome, got %v", msg)
	return stream
}

// sendWatch reports each time the server enters SendMsg, and each time
// SendMsg returns, on any stream.
type sendWatch struct {
	entered  chan struct{}
	returned chan struct{}
}

// watchSends makes the server report its sends on every stream, from now on.
func (f *fixture) watchSends() *sendWatch {
	w := &sendWatch{entered: make(chan struct{}, 1024), returned: make(chan struct{}, 1024)}
	f.sends.Store(w)
	return w
}

// await waits until the server has entered SendMsg entered times, and returned
// from it returned times, since the test began watching.
func (w *sendWatch) await(t *testing.T, entered, returned int) {
	t.Helper()
	for _, wait := range []struct {
		ch <-chan struct{}
		n  int
	}{{w.entered, entered}, {w.returned, returned}} {
		for range wait.n {
			select {
			case <-wait.ch:
			case <-time.After(awaitLimit):
				t.Fatal("the server did not send")
			}
		}
	}
}

// reportingStream reports each SendMsg to the fixture's watch, if a test is
// watching.
type reportingStream struct {
	grpc.ServerStream
	f *fixture
}

func (s reportingStream) SendMsg(m any) error {
	w := s.f.sends.Load()
	if w != nil {
		report(w.entered)
	}
	err := s.ServerStream.SendMsg(m)
	if w != nil {
		report(w.returned)
	}
	return err
}

// report signals ch without ever blocking the server.
func report(ch chan<- struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// ensure creates the session for attempt with the given members, defaulting to
// two, and consumes the signal of the deadline it armed.
func (f *fixture) ensure(t *testing.T, members ...session.MemberID) *session.Session {
	t.Helper()
	if len(members) == 0 {
		members = []session.MemberID{memberA, memberB}
	}
	spec := session.Spec{UID: attempt, Deadline: baseTime.Add(deadlineIn)}
	for rank, id := range members {
		spec.Members = append(spec.Members, session.Member{ID: id, Rank: rank})
	}
	s, err := f.manager.Ensure(spec)
	require.NoError(t, err)
	f.clock.AwaitTimer(t)
	return s
}

// attach opens a stream for member with epoch, and returns it once Welcome
// arrives.
func (f *fixture) attach(t *testing.T, member session.MemberID, epoch uint64) (*client, *wire.Welcome) {
	t.Helper()
	c := f.open(t)
	c.send(t, hello(member, epoch))
	return c, c.welcome(t)
}

// open opens a stream without sending anything.
func (f *fixture) open(t *testing.T) *client {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := f.client.Attach(ctx)
	require.NoError(t, err)
	c := &client{stream: stream, cancel: cancel, received: make(chan *wire.ServerMessage, 1024)}
	go c.read()
	t.Cleanup(cancel)
	return c
}

// report hands what an Attach handler returned to returned, without ever
// blocking the handler, and has the stream report its sends.
func (f *fixture) report(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	err := handler(srv, reportingStream{ServerStream: ss, f: f})
	select {
	case f.returned <- err:
	default:
	}
	return err
}

// awaitReturn waits for the next Attach handler to return, and returns its
// status error, or nil for OK.
func (f *fixture) awaitReturn(t *testing.T) error {
	t.Helper()
	select {
	case err := <-f.returned:
		return err
	case <-time.After(awaitLimit):
		t.Fatal("no stream's handler returned")
		return nil
	}
}

// epoch returns the epoch of the Manager the server serves.
func (f *fixture) epoch() uint64 { return uint64(f.manager.Epoch()) }

// client is one stream, seen from the agent's side.
type client struct {
	stream   wire.Coordinator_AttachClient
	cancel   context.CancelFunc
	received chan *wire.ServerMessage
	// err is what ended the stream; it is set before received closes.
	err error
}

// read receives every message until the stream ends.
func (c *client) read() {
	for {
		msg, err := c.stream.Recv()
		if err != nil {
			c.err = err
			close(c.received)
			return
		}
		c.received <- msg
	}
}

func (c *client) send(t *testing.T, msg *wire.ClientMessage) {
	t.Helper()
	require.NoError(t, c.stream.Send(msg))
}

func (c *client) request(t *testing.T, req *wire.Request) {
	t.Helper()
	c.send(t, &wire.ClientMessage{Message: &wire.ClientMessage_Request{Request: req}})
}

func (c *client) cancelRequest(t *testing.T, id string) {
	t.Helper()
	c.send(t, &wire.ClientMessage{Message: &wire.ClientMessage_Cancel{Cancel: &wire.Cancel{Id: id}}})
}

// lose breaks the stream, as a network failure or an agent crash would.
func (c *client) lose() { c.cancel() }

// next returns the next message the server sent.
func (c *client) next(t *testing.T) *wire.ServerMessage {
	t.Helper()
	select {
	case msg, ok := <-c.received:
		if !ok {
			t.Fatalf("the stream ended: %v", c.err)
		}
		return msg
	case <-time.After(awaitLimit):
		t.Fatal("the server sent nothing")
		return nil
	}
}

func (c *client) welcome(t *testing.T) *wire.Welcome {
	t.Helper()
	msg := c.next(t)
	require.NotNil(t, msg.GetWelcome(), "expected Welcome, got %v", msg)
	return msg.GetWelcome()
}

// response returns the next message, which must answer id.
func (c *client) response(t *testing.T, id string) *wire.Response {
	t.Helper()
	msg := c.next(t)
	resp := msg.GetResponse()
	require.NotNil(t, resp, "expected a Response to %q, got %v", id, msg)
	require.Equal(t, id, resp.GetId())
	return resp
}

// ok expects the next message to answer id with success.
func (c *client) ok(t *testing.T, id string) *wire.Response {
	t.Helper()
	resp := c.response(t, id)
	require.Nil(t, resp.GetError(), "request %q failed: %v", id, resp.GetError())
	return resp
}

// failure expects the next message to answer id with code.
func (c *client) failure(t *testing.T, id string, code wire.Code) *wire.Error {
	t.Helper()
	e := c.response(t, id).GetError()
	require.NotNil(t, e, "request %q succeeded", id)
	require.Equal(t, code, e.GetCode(), "message: %s", e.GetMessage())
	return e
}

// ended expects Ended with code and reason, and then the end of the stream
// with OK.
func (c *client) ended(t *testing.T, code wire.Code, reason session.Reason) {
	t.Helper()
	msg := c.next(t)
	ended := msg.GetEnded()
	require.NotNil(t, ended, "expected Ended, got %v", msg)
	require.Equal(t, code, ended.GetError().GetCode())
	require.Equal(t, string(reason), ended.GetError().GetReason())
	require.NoError(t, c.end(t), "a stream ends with OK after Ended")
}

// end waits for the stream to end with nothing more sent, and returns its
// status error, or nil for OK.
func (c *client) end(t *testing.T) error {
	t.Helper()
	select {
	case msg, ok := <-c.received:
		require.False(t, ok, "expected the stream to end, got %v", msg)
	case <-time.After(awaitLimit):
		t.Fatal("the stream did not end")
	}
	if errors.Is(c.err, io.EOF) {
		return nil
	}
	return c.err
}

// drain collects every message until the stream ends, and returns them with
// its status error, or nil for OK.
func (c *client) drain(t *testing.T) ([]*wire.ServerMessage, error) {
	t.Helper()
	var messages []*wire.ServerMessage
	for {
		select {
		case msg, ok := <-c.received:
			if !ok {
				if errors.Is(c.err, io.EOF) {
					return messages, nil
				}
				return messages, c.err
			}
			messages = append(messages, msg)
		case <-time.After(awaitLimit):
			t.Fatal("the stream did not end")
			return nil, nil
		}
	}
}

// hello is the first message of a stream.
func hello(member session.MemberID, epoch uint64) *wire.ClientMessage {
	return &wire.ClientMessage{Message: &wire.ClientMessage_Hello{Hello: &wire.Hello{
		Session: string(attempt), Member: string(member), Epoch: epoch,
	}}}
}

// origin builds requests from one origin.
type origin struct{ *wire.Origin }

func asAgent() origin { return origin{&wire.Origin{Role: wire.Role_ROLE_AGENT}} }
func asRelay() origin { return origin{&wire.Origin{Role: wire.Role_ROLE_RELAY}} }
func asNCCL() origin {
	return origin{&wire.Origin{Role: wire.Role_ROLE_LIBRARY, Library: session.LibraryNCCL}}
}

func (o origin) req(id string) *wire.Request { return &wire.Request{Id: id, Origin: o.Origin} }

func (o origin) insert(id, key string, value []byte) *wire.Request {
	r := o.req(id)
	r.Operation = &wire.Request_Insert{Insert: &wire.Insert{Key: key, Value: value}}
	return r
}

func (o origin) get(id, key string, wait time.Duration) *wire.Request {
	r := o.req(id)
	r.Operation = &wire.Request_Get{Get: &wire.Get{Key: key, Wait: durationpb.New(wait)}}
	return r
}

func (o origin) unset(id, key string) *wire.Request {
	r := o.req(id)
	r.Operation = &wire.Request_Unset{Unset: &wire.Unset{Key: key}}
	return r
}

func (o origin) increment(id, key string) *wire.Request {
	r := o.req(id)
	r.Operation = &wire.Request_Increment{Increment: &wire.Increment{Key: key}}
	return r
}

func (o origin) decrement(id, key string) *wire.Request {
	r := o.req(id)
	r.Operation = &wire.Request_Decrement{Decrement: &wire.Decrement{Key: key}}
	return r
}

func (o origin) compareAndSwap(id, key string, expected, value []byte) *wire.Request {
	r := o.req(id)
	r.Operation = &wire.Request_CompareAndSwap{CompareAndSwap: &wire.CompareAndSwap{
		Key: key, Expected: expected, Value: value,
	}}
	return r
}

func (o origin) createBarrier(id, name string, count int32) *wire.Request {
	r := o.req(id)
	r.Operation = &wire.Request_CreateBarrier{CreateBarrier: &wire.CreateBarrier{Name: name, Count: count}}
	return r
}

func (o origin) postBarrier(id, name, participant string) *wire.Request {
	r := o.req(id)
	r.Operation = &wire.Request_PostBarrier{PostBarrier: &wire.PostBarrier{Name: name, Participant: participant}}
	return r
}

func (o origin) awaitBarrier(id, name string) *wire.Request {
	r := o.req(id)
	r.Operation = &wire.Request_AwaitBarrier{AwaitBarrier: &wire.AwaitBarrier{Name: name}}
	return r
}

func (o origin) leave(id string) *wire.Request {
	r := o.req(id)
	r.Operation = &wire.Request_Leave{Leave: &wire.Leave{}}
	return r
}

func (o origin) fail(id, reason string) *wire.Request {
	r := o.req(id)
	r.Operation = &wire.Request_Fail{Fail: &wire.Fail{Reason: reason}}
	return r
}

// authorizer is a fake Authorizer that refuses with refusal, when set, holds
// the next call at gate, when set, and records whom it was asked about.
type authorizer struct {
	mu      sync.Mutex
	refusal error
	gate    *authGate
	asked   []memberKey
}

// authGate holds one Authorize call: the call hands its context to entered,
// then waits for release to close.
type authGate struct {
	entered chan context.Context
	release chan struct{}
}

func (a *authorizer) Authorize(ctx context.Context, uid types.UID, member session.MemberID) error {
	a.mu.Lock()
	a.asked = append(a.asked, memberKey{uid: uid, member: member})
	refusal, gate := a.refusal, a.gate
	a.gate = nil
	a.mu.Unlock()
	if gate != nil {
		gate.entered <- ctx
		<-gate.release
	}
	return refusal
}

// holdNext makes the next Authorize call wait until the returned function is
// called, and returns that call's context once it arrives. The test's cleanup
// releases the call if the test did not.
func (a *authorizer) holdNext(t *testing.T) (entered <-chan context.Context, release func()) {
	t.Helper()
	gate := &authGate{entered: make(chan context.Context, 1), release: make(chan struct{})}
	a.mu.Lock()
	a.gate = gate
	a.mu.Unlock()
	release = sync.OnceFunc(func() { close(gate.release) })
	t.Cleanup(release)
	return gate.entered, release
}

func (a *authorizer) refuse(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.refusal = err
}

// logBuffer collects what the server logs.
type logBuffer struct {
	mu    sync.Mutex
	lines []string
}

func (b *logBuffer) write(prefix, args string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lines = append(b.lines, prefix+" "+args)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Join(b.lines, "\n")
}
