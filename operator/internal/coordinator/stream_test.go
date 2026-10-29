// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordinator

import (
	"context"
	"fmt"
	"testing"

	wire "github.com/ai-dynamo/snapshot/agent/pkg/coordinator/v1alpha1"
	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/kv"
	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/kv/memory"
	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/types"
)

func TestABlockedGetDoesNotDelayALaterRequest(t *testing.T) {
	f := newFixture(t)
	f.ensure(t)
	c, _ := f.attach(t, memberA, 0)
	agent := asAgent()
	c.request(t, agent.get("blocked", agentKey, deadlineIn))
	f.clock.AwaitTimer(t)

	c.request(t, agent.insert("later", agentOther, []byte("ranks")))

	c.ok(t, "later")
	c.request(t, agent.insert("release", agentKey, []byte("handle")))
	answered := map[string]*wire.Response{}
	for range 2 {
		resp := c.next(t).GetResponse()
		require.NotNil(t, resp)
		answered[resp.GetId()] = resp
	}
	require.Contains(t, answered, "blocked", "responses are matched by id, in whatever order they come")
	require.Contains(t, answered, "release")
	assert.Equal(t, []byte("handle"), answered["blocked"].GetValue())
}

func TestARequestNoAnswerCouldIdentifyEndsTheStream(t *testing.T) {
	tests := []struct {
		name      string
		misbehave func(*testing.T, *client)
	}{
		{name: "no id", misbehave: func(t *testing.T, c *client) {
			c.request(t, asAgent().insert("", agentKey, nil))
		}},
		{name: "an id already pending", misbehave: func(t *testing.T, c *client) {
			c.request(t, asAgent().get("r-1", agentKey, deadlineIn))
			c.request(t, asAgent().get("r-1", agentOther, deadlineIn))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.ensure(t)
			c, _ := f.attach(t, memberA, 0)

			tt.misbehave(t, c)

			messages, err := c.drain(t)
			assert.Equal(t, codes.InvalidArgument, status.Code(err))
			for _, msg := range messages {
				// The request still pending may be answered as cancelled, but
				// nothing is answered with an error meant for the offender.
				assert.Equal(t, wire.Code_CODE_CANCELLED, msg.GetResponse().GetError().GetCode(), "%v", msg)
			}
		})
	}
}

func TestPastTheBackstopTheStreamEnds(t *testing.T) {
	f := newFixture(t)
	s := f.ensure(t)
	c, _ := f.attach(t, memberA, 0)
	for i := range unansweredBackstop {
		c.request(t, asAgent().get(fmt.Sprintf("r-%d", i), agentKey, deadlineIn))
	}

	c.request(t, asAgent().insert("one-more", agentOther, nil))

	messages, err := c.drain(t)
	assert.Equal(t, codes.ResourceExhausted, status.Code(err))
	for _, msg := range messages {
		// Pending requests may be answered as cancelled; the one past the
		// backstop never runs.
		assert.Equal(t, wire.Code_CODE_CANCELLED, msg.GetResponse().GetError().GetCode(), "%v", msg)
	}
	require.NoError(t, s.Err(), "the stream is lost, and the session stays open for a reattach")
}

func TestCancelAnswersAPendingWaitWithCancelled(t *testing.T) {
	tests := []struct {
		name    string
		request *wire.Request
	}{
		{name: "a Get", request: asAgent().get("r", agentKey, deadlineIn)},
		{name: "an AwaitBarrier", request: asAgent().awaitBarrier("r", barrier)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.ensure(t)
			c, _ := f.attach(t, memberA, 0)
			c.request(t, tt.request)

			c.cancelRequest(t, "r")

			c.failure(t, "r", wire.Code_CODE_CANCELLED)
		})
	}
}

func TestACancelForAnIDThatIsNotPendingIsIgnored(t *testing.T) {
	f := newFixture(t)
	f.ensure(t)
	c, _ := f.attach(t, memberA, 0)

	c.cancelRequest(t, "never-sent")
	c.request(t, asAgent().insert("r", agentKey, nil))

	c.ok(t, "r")
}

// gatedStore holds every Insert of gatedKey inside the space until release is
// closed, so a test can act while the write is in flight. Once released, the
// Insert honors its context, as a networked backend would.
type gatedStore struct {
	kv.Store
	entered chan struct{}
	release chan struct{}
}

const gatedKey = "snapshot/gated"

func (g gatedStore) Space(uid types.UID) kv.Space {
	return gatedSpace{Space: g.Store.Space(uid), gate: g}
}

type gatedSpace struct {
	kv.Space
	gate gatedStore
}

func (s gatedSpace) Insert(ctx context.Context, key string, value []byte) error {
	if key == gatedKey {
		close(s.gate.entered)
		<-s.gate.release
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return s.Space.Insert(ctx, key, value)
}

func TestACancelledWriteStillAppliesAndReplays(t *testing.T) {
	gate := gatedStore{Store: memory.New(), entered: make(chan struct{}), release: make(chan struct{})}
	f := newFixtureOn(t, gate)
	f.ensure(t)
	c, _ := f.attach(t, memberA, 0)
	c.request(t, asAgent().insert("w", gatedKey, []byte("applied")))
	<-gate.entered

	c.cancelRequest(t, "w")
	// The stream acts on messages in order, so once this read answers, the
	// request behind the write was cancelled before the write went on. A write
	// could not answer here: it would wait for the gated one.
	c.request(t, asAgent().get("after", agentOther, 0))
	c.failure(t, "after", wire.Code_CODE_NOT_FOUND)
	close(gate.release)

	c.ok(t, "w")
	c.request(t, asAgent().insert("w", gatedKey, []byte("something else")))
	c.ok(t, "w")
	c.request(t, asAgent().get("check", gatedKey, 0))
	assert.Equal(t, []byte("applied"), c.ok(t, "check").GetValue(), "the re-issued id replayed rather than applied")
}

func TestAFailReachesEveryStreamAfterItsPendingResponses(t *testing.T) {
	f := newFixture(t)
	f.ensure(t, memberA, memberB, memberC)
	failing, _ := f.attach(t, memberA, 0)
	idle, _ := f.attach(t, memberB, 0)
	waiting, _ := f.attach(t, memberC, 0)
	waiting.request(t, asAgent().get("pending", agentKey, deadlineIn))
	f.clock.AwaitTimer(t)

	failing.request(t, asRelay().fail("f", "NodeLost"))

	failing.ok(t, "f")
	failing.ended(t, wire.Code_CODE_ABORTED, "NodeLost")
	idle.ended(t, wire.Code_CODE_ABORTED, "NodeLost")
	e := waiting.failure(t, "pending", wire.Code_CODE_ABORTED)
	assert.Equal(t, "NodeLost", e.GetReason())
	waiting.ended(t, wire.Code_CODE_ABORTED, "NodeLost")
}

func TestTheDeadlineEndsEveryStream(t *testing.T) {
	f := newFixture(t)
	f.ensure(t)
	a, _ := f.attach(t, memberA, 0)
	b, _ := f.attach(t, memberB, 0)
	// The wait outlasts the attempt, so the deadline is what ends it.
	b.request(t, asAgent().get("pending", agentKey, 2*deadlineIn))
	f.clock.AwaitTimer(t)

	f.clock.Step(deadlineIn)

	a.ended(t, wire.Code_CODE_ABORTED, session.ReasonDeadlineExceeded)
	b.failure(t, "pending", wire.Code_CODE_ABORTED)
	b.ended(t, wire.Code_CODE_ABORTED, session.ReasonDeadlineExceeded)
}

func TestShutdownEndsEveryStream(t *testing.T) {
	f := newFixture(t)
	f.ensure(t)
	a, _ := f.attach(t, memberA, 0)
	b, _ := f.attach(t, memberB, 0)

	require.NoError(t, f.manager.Shutdown(context.Background(), session.ReasonCoordinatorLost))

	a.ended(t, wire.Code_CODE_ABORTED, session.ReasonCoordinatorLost)
	b.ended(t, wire.Code_CODE_ABORTED, session.ReasonCoordinatorLost)
}

func TestTheLastLeaveEndsEveryStreamWithClosed(t *testing.T) {
	f := newFixture(t)
	f.ensure(t)
	a, _ := f.attach(t, memberA, 0)
	b, _ := f.attach(t, memberB, 0)
	a.request(t, asNCCL().get("library-wait", ncclKey, deadlineIn))
	f.clock.AwaitTimer(t)

	a.request(t, asAgent().leave("leave-a"))
	a.ok(t, "leave-a")
	b.request(t, asAgent().leave("leave-b"))

	b.ok(t, "leave-b")
	b.ended(t, wire.Code_CODE_CLOSED, "")
	a.failure(t, "library-wait", wire.Code_CODE_CLOSED)
	a.ended(t, wire.Code_CODE_CLOSED, "")
}

func TestASecondStreamSupersedesTheFirst(t *testing.T) {
	f := newFixture(t)
	s := f.ensure(t)
	first, _ := f.attach(t, memberA, 0)
	first.request(t, asAgent().get("pending", agentKey, deadlineIn))
	f.clock.AwaitTimer(t)

	second, _ := f.attach(t, memberA, f.epoch())

	err := first.end(t)
	assert.Equal(t, codes.Aborted, status.Code(err), "the older stream ends, its pending request unanswered")
	second.request(t, asAgent().insert("r", agentKey, []byte("handle")))
	second.ok(t, "r")
	require.NoError(t, s.Err(), "superseding a stream fails nothing")
	assert.Contains(t, f.logs.String(), "Stream superseded")
}

// requestMessage wraps a request for a stream a test sends on directly.
func requestMessage(req *wire.Request) *wire.ClientMessage {
	return &wire.ClientMessage{Message: &wire.ClientMessage_Request{Request: req}}
}

func TestARequestCountsAgainstTheBackstopUntilItsResponseIsSent(t *testing.T) {
	f := newFixture(t)
	s := f.ensure(t)
	writer, _ := f.attach(t, memberA, 0)
	writer.request(t, asAgent().insert("value", agentKey, make([]byte, 4<<10)))
	writer.ok(t, "value")
	unread := f.attachUnread(t, memberB)

	// Every Get finds its value at once, but only the first few responses fit
	// the client's windows; the rest wait to be sent, and still count. Past the
	// backstop, the client's sends may fail, which is the point.
	for i := range unansweredBackstop + 256 {
		_ = unread.Send(requestMessage(asAgent().get(fmt.Sprintf("r-%d", i), agentKey, 0)))
	}

	assert.Equal(t, codes.ResourceExhausted, status.Code(f.awaitReturn(t)))
	require.NoError(t, s.Err(), "the stream is lost, and the session stays open for a reattach")
}

// blockSends has an unread client ask for n responses of 1 MiB each, and
// returns once the server has entered every send and finished the first. The
// first response fills the client's windows, so the server's next send on the
// stream blocks: the second response's, or whatever follows a lone one.
func blockSends(t *testing.T, f *fixture, writer *client, unread wire.Coordinator_AttachClient, n int) {
	t.Helper()
	writer.request(t, asAgent().insert("large", agentOther, make([]byte, 1<<20)))
	writer.ok(t, "large")
	sends := f.watchSends()
	for i := range n {
		require.NoError(t, unread.Send(requestMessage(asAgent().get(fmt.Sprintf("r-%d", i), agentOther, 0))))
	}
	sends.await(t, n, 1)
}

func TestAStreamWhoseClientStoppedReadingIsLostWhenItClosesSending(t *testing.T) {
	f := newFixture(t)
	s := f.ensure(t)
	writer, _ := f.attach(t, memberA, 0)
	unread := f.attachUnread(t, memberB)
	blockSends(t, f, writer, unread, 2)

	require.NoError(t, unread.CloseSend())

	require.NoError(t, f.awaitReturn(t), "the stream ends with OK, without waiting to answer")
	require.Equal(t, gracePeriod, f.clock.AwaitTimer(t), "the member's grace period starts")
	f.clock.Step(gracePeriod)
	awaitDone(t, s)
	var aborted *session.AbortError
	require.ErrorAs(t, s.Err(), &aborted)
	assert.Equal(t, session.ReasonMemberFailed, aborted.Reason)
}

func TestAStreamWhoseClientStoppedReadingIsStillSuperseded(t *testing.T) {
	f := newFixture(t)
	s := f.ensure(t)
	writer, _ := f.attach(t, memberA, 0)
	unread := f.attachUnread(t, memberB)
	blockSends(t, f, writer, unread, 2)

	newer, _ := f.attach(t, memberB, f.epoch())

	assert.Equal(t, codes.Aborted, status.Code(f.awaitReturn(t)))
	newer.request(t, asAgent().get("r", agentKey, 0))
	newer.failure(t, "r", wire.Code_CODE_NOT_FOUND)
	require.NoError(t, s.Err(), "superseding a stream fails nothing")
}

func TestAStreamDrainingAfterItsSessionEndedStillSeesItsClientGiveUp(t *testing.T) {
	closeSend := func(t *testing.T, unread wire.Coordinator_AttachClient) {
		require.NoError(t, unread.CloseSend())
	}
	tests := []struct {
		name string
		// responses is how many large responses the client leaves unread: with
		// one, Ended is the send that blocks; with two, a response is.
		responses int
		giveUp    func(*testing.T, wire.Coordinator_AttachClient)
	}{
		{name: "Ended blocked, then closing its sending side", responses: 1, giveUp: closeSend},
		{name: "a response blocked, then closing its sending side", responses: 2, giveUp: closeSend},
		{
			name:      "a response blocked, a request, then closing its sending side",
			responses: 2,
			giveUp: func(t *testing.T, unread wire.Coordinator_AttachClient) {
				require.NoError(t, unread.Send(requestMessage(asAgent().get("late", agentKey, 0))))
				closeSend(t, unread)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.ensure(t)
			writer, _ := f.attach(t, memberA, 0)
			unread := f.attachUnread(t, memberB)
			blockSends(t, f, writer, unread, tt.responses)
			// The unread stream sees the session end long before its client
			// gives up: the writer reads Ended first.
			writer.request(t, asRelay().fail("f", "NodeLost"))
			writer.ok(t, "f")
			writer.ended(t, wire.Code_CODE_ABORTED, "NodeLost")
			require.NoError(t, f.awaitReturn(t), "the writer's stream ends once it has read Ended")

			tt.giveUp(t, unread)

			require.NoError(t, f.awaitReturn(t), "the stream ends with OK, without waiting to deliver the rest")
		})
	}
}
