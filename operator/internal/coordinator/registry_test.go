// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordinator

import (
	"context"
	"testing"
	"time"

	wire "github.com/ai-dynamo/snapshot/agent/pkg/coordinator/v1alpha1"
	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/kv"
	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestAReattachReplaysAReissuedIncrement(t *testing.T) {
	f := newFixture(t)
	f.ensure(t)
	c, welcome := f.attach(t, memberA, 0)
	c.request(t, asAgent().increment("boot-1/1", agentKey))
	require.Equal(t, int64(1), c.ok(t, "boot-1/1").GetCounter())

	c.lose()
	again, rewelcome := f.attach(t, memberA, welcome.GetEpoch())
	again.request(t, asAgent().increment("boot-1/1", agentKey))

	assert.Equal(t, welcome, rewelcome, "a reattach is welcomed as the first attach was")
	assert.Equal(t, int64(1), again.ok(t, "boot-1/1").GetCounter(), "the re-issued id replays")
	again.request(t, asAgent().increment("boot-2/1", agentKey))
	assert.Equal(t, int64(2), again.ok(t, "boot-2/1").GetCounter(), "a restarted agent's new ids apply")
	again.request(t, asAgent().get("check", agentKey, 0))
	assert.Equal(t, kv.EncodeCounter(2), again.ok(t, "check").GetValue())
}

func TestAStaleEpochFailsTheSessionForEveryStream(t *testing.T) {
	f := newFixture(t)
	s := f.ensure(t)
	other, _ := f.attach(t, memberB, 0)
	stale := f.open(t)

	stale.send(t, hello(memberA, f.epoch()+1))

	stale.ended(t, wire.Code_CODE_ABORTED, session.ReasonCoordinatorLost)
	other.ended(t, wire.Code_CODE_ABORTED, session.ReasonCoordinatorLost)
	var aborted *session.AbortError
	require.ErrorAs(t, s.Err(), &aborted)
	assert.Equal(t, session.ReasonCoordinatorLost, aborted.Reason)
}

func TestAStaleEpochForAnUnknownSessionIsCoordinatorLost(t *testing.T) {
	f := newFixture(t)
	c := f.open(t)

	c.send(t, hello(memberA, f.epoch()+1))

	c.ended(t, wire.Code_CODE_ABORTED, session.ReasonCoordinatorLost)
}

func TestTheCurrentEpochOnARemovedSessionIsCoordinatorLost(t *testing.T) {
	f := newFixture(t)
	f.ensure(t)
	first, welcome := f.attach(t, memberA, 0)
	require.NoError(t, f.manager.Remove(context.Background(), attempt))
	first.ended(t, wire.Code_CODE_ABORTED, session.ReasonCancelled)
	c := f.open(t)

	c.send(t, hello(memberA, welcome.GetEpoch()))

	c.ended(t, wire.Code_CODE_ABORTED, session.ReasonCoordinatorLost)
}

func TestALostStreamFailsTheSessionAfterGrace(t *testing.T) {
	f := newFixture(t)
	s := f.ensure(t)
	lost, _ := f.attach(t, memberA, 0)
	other, _ := f.attach(t, memberB, 0)

	lost.lose()
	require.Equal(t, gracePeriod, f.clock.AwaitTimer(t), "the grace period starts")
	f.clock.Step(gracePeriod)

	other.ended(t, wire.Code_CODE_ABORTED, session.ReasonMemberFailed)
	var aborted *session.AbortError
	require.ErrorAs(t, s.Err(), &aborted)
	assert.Equal(t, session.ReasonMemberFailed, aborted.Reason)
	assert.Contains(t, f.logs.String(), "no new one attached within the grace period")
}

func TestAReattachWithinGraceFailsNothing(t *testing.T) {
	f := newFixture(t)
	s := f.ensure(t)
	lost, welcome := f.attach(t, memberA, 0)
	lost.lose()
	f.clock.AwaitTimer(t)

	f.attach(t, memberA, welcome.GetEpoch())
	f.clock.Step(gracePeriod)

	require.NoError(t, s.Err())
	assert.Equal(t, 1, f.clock.Waiters(), "only the deadline is still armed")
}

func TestGraceStillAppliesAfterLeave(t *testing.T) {
	f := newFixture(t)
	s := f.ensure(t)
	left, _ := f.attach(t, memberA, 0)
	f.attach(t, memberB, 0)
	left.request(t, asAgent().leave("leave"))
	left.ok(t, "leave")

	left.lose()
	f.clock.AwaitTimer(t)
	f.clock.Step(gracePeriod)

	awaitDone(t, s)
	var aborted *session.AbortError
	require.ErrorAs(t, s.Err(), &aborted)
	assert.Equal(t, session.ReasonMemberFailed, aborted.Reason)
}

func TestClosingAfterEndedStartsNoTimer(t *testing.T) {
	f := newFixture(t)
	f.ensure(t)
	c, _ := f.attach(t, memberA, 0)

	c.request(t, asAgent().fail("f", "NodeLost"))
	c.ok(t, "f")
	c.ended(t, wire.Code_CODE_ABORTED, "NodeLost")

	assert.Zero(t, f.clock.Waiters(), "neither the deadline nor a grace period is armed")
}

func TestAGracePeriodEndsWithItsSession(t *testing.T) {
	f := newFixture(t)
	f.ensure(t)
	lost, _ := f.attach(t, memberA, 0)
	other, _ := f.attach(t, memberB, 0)
	lost.lose()
	f.clock.AwaitTimer(t)

	other.request(t, asAgent().fail("f", "NodeLost"))
	other.ok(t, "f")
	other.ended(t, wire.Code_CODE_ABORTED, "NodeLost")

	assert.Zero(t, f.clock.Waiters(), "an ended session leaves no grace period behind")
}

// awaitDone waits for a session to end, which a grace period's expiry brings
// about from a goroutine of its own.
func awaitDone(t *testing.T, s *session.Session) {
	t.Helper()
	select {
	case <-s.Done():
	case <-time.After(awaitLimit):
		t.Fatal("the session did not end")
	}
}

func TestAStaleEpochForAnEndedSessionGetsHowItEnded(t *testing.T) {
	f := newFixture(t)
	s := f.ensure(t, memberA)
	require.NoError(t, s.Leave(context.Background(), session.Caller{Member: memberA, Role: session.RoleAgent}))
	c := f.open(t)

	c.send(t, hello(memberA, f.epoch()+1))

	c.ended(t, wire.Code_CODE_CLOSED, "")
}

func TestAStaleEpochFromANonMemberFailsNothing(t *testing.T) {
	f := newFixture(t)
	s := f.ensure(t)
	c := f.open(t)

	c.send(t, hello(memberC, f.epoch()+1))

	assert.Equal(t, codes.PermissionDenied, status.Code(c.end(t)))
	require.NoError(t, s.Err())
}

func TestAStreamWhoseHandshakeFinishesLateNeverDisplacesANewerOne(t *testing.T) {
	tests := []struct {
		name string
		// first reports whether the late stream's Hello arrives before the
		// newer stream's.
		first bool
		// abandon reports whether the late stream's client gives up on it while
		// it is being authorized.
		abandon bool
		want    codes.Code
	}{
		{name: "an abandoned Hello that arrived first", first: true, abandon: true, want: codes.Canceled},
		{name: "a Hello that arrived first", first: true, want: codes.Aborted},
		{name: "an abandoned Hello that arrived last", abandon: true, want: codes.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			s := f.ensure(t)
			var newer *client
			if !tt.first {
				newer, _ = f.attach(t, memberA, 0)
			}
			entered, release := f.auth.holdNext(t)
			late := f.open(t)
			late.send(t, hello(memberA, 0))
			ctx := <-entered
			if tt.abandon {
				late.lose()
				<-ctx.Done()
			}
			if tt.first {
				newer, _ = f.attach(t, memberA, 0)
			}

			release()

			assert.Equal(t, tt.want, status.Code(f.awaitReturn(t)))
			newer.request(t, asAgent().insert("r", agentKey, []byte("handle")))
			newer.ok(t, "r")
			assert.NotContains(t, f.logs.String(), "Stream superseded")
			assert.Equal(t, 1, f.clock.Waiters(), "only the deadline is armed, and no grace period")
			require.NoError(t, s.Err())
		})
	}
}

func TestAnOlderStreamNeverEndsANewerOnesGracePeriod(t *testing.T) {
	f := newFixture(t)
	s := f.ensure(t)
	entered, release := f.auth.holdNext(t)
	older := f.open(t)
	older.send(t, hello(memberA, 0))
	<-entered
	newer, _ := f.attach(t, memberA, 0)
	newer.lose()
	f.clock.AwaitTimer(t)

	release()

	assert.Equal(t, codes.Aborted, status.Code(older.end(t)))
	f.clock.Step(gracePeriod)
	awaitDone(t, s)
	var aborted *session.AbortError
	require.ErrorAs(t, s.Err(), &aborted)
	assert.Equal(t, session.ReasonMemberFailed, aborted.Reason)
}
