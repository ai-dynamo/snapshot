// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordinator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	wire "github.com/ai-dynamo/snapshot/agent/pkg/coordinator/v1alpha1"
	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/kv"
	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/kv/memory"
	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"
	"k8s.io/apimachinery/pkg/types"
)

func TestEveryOperationRoundTrips(t *testing.T) {
	f := newFixture(t)
	f.ensure(t)
	c, _ := f.attach(t, memberA, 0)
	agent := asAgent()

	c.request(t, agent.insert("r-1", agentKey, []byte("handle")))
	c.ok(t, "r-1")
	c.request(t, agent.get("r-2", agentKey, 0))
	assert.Equal(t, []byte("handle"), c.ok(t, "r-2").GetValue())
	c.request(t, agent.unset("r-3", agentKey))
	c.ok(t, "r-3")
	c.request(t, agent.get("r-4", agentKey, 0))
	c.failure(t, "r-4", wire.Code_CODE_NOT_FOUND)
	c.request(t, agent.increment("r-5", agentOther))
	assert.Equal(t, int64(1), c.ok(t, "r-5").GetCounter())
	c.request(t, agent.decrement("r-6", agentOther))
	assert.Equal(t, int64(0), c.ok(t, "r-6").GetCounter())
	c.request(t, agent.insert("r-7", agentKey, []byte("old")))
	c.ok(t, "r-7")
	c.request(t, agent.compareAndSwap("r-8", agentKey, []byte("old"), []byte("new")))
	c.ok(t, "r-8")
	c.request(t, agent.get("r-9", agentKey, 0))
	assert.Equal(t, []byte("new"), c.ok(t, "r-9").GetValue())
	c.request(t, agent.createBarrier("r-10", barrier, 1))
	c.ok(t, "r-10")
	c.request(t, agent.postBarrier("r-11", barrier, string(memberA)))
	c.ok(t, "r-11")
	c.request(t, agent.awaitBarrier("r-12", barrier))
	c.ok(t, "r-12")
	c.request(t, asNCCL().insert("r-13", ncclKey, []byte("communicator")))
	c.ok(t, "r-13")
	c.request(t, asRelay().insert("r-14", "cuinterpose/command", []byte("checkpoint")))
	c.ok(t, "r-14")
	c.request(t, agent.leave("r-15"))
	c.ok(t, "r-15")
	c.request(t, agent.fail("r-16", "NodeLost"))
	c.ok(t, "r-16")
	c.ended(t, wire.Code_CODE_ABORTED, "NodeLost")
}

func TestSessionErrorsReachTheClientAsTheirCodes(t *testing.T) {
	agent := asAgent()
	tests := []struct {
		name    string
		setup   []*wire.Request
		request *wire.Request
		want    wire.Code
	}{
		{name: "an absent key", request: agent.get("r", agentKey, 0), want: wire.Code_CODE_NOT_FOUND},
		{
			name:    "another value",
			setup:   []*wire.Request{agent.insert("s", agentKey, []byte("first"))},
			request: agent.insert("r", agentKey, []byte("second")),
			want:    wire.Code_CODE_EXISTS,
		},
		{
			name:    "an unexpected value",
			setup:   []*wire.Request{agent.insert("s", agentKey, []byte("first"))},
			request: agent.compareAndSwap("r", agentKey, []byte("other"), []byte("new")),
			want:    wire.Code_CODE_MISMATCH,
		},
		{
			name:    "a value that is not a counter",
			setup:   []*wire.Request{agent.insert("s", agentKey, []byte("seven"))},
			request: agent.increment("r", agentKey),
			want:    wire.Code_CODE_NOT_COUNTER,
		},
		{name: "a barrier count of zero", request: agent.createBarrier("r", barrier, 0), want: wire.Code_CODE_BARRIER_COUNT},
		{name: "a key outside the origin's namespace", request: asNCCL().insert("r", agentKey, nil),
			want: wire.Code_CODE_FORBIDDEN},
		{
			name:    "a key over the length limit",
			request: agent.insert("r", "snapshot/"+strings.Repeat("k", session.MaxKeyLength), nil),
			want:    wire.Code_CODE_TOO_LARGE,
		},
		{name: "a failure without a reason", request: agent.fail("r", ""), want: wire.Code_CODE_INVALID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.ensure(t)
			c, _ := f.attach(t, memberA, 0)
			for _, req := range tt.setup {
				c.request(t, req)
				c.ok(t, req.GetId())
			}

			c.request(t, tt.request)

			e := c.failure(t, "r", tt.want)
			assert.NotEmpty(t, e.GetMessage())
			assert.Empty(t, e.GetReason(), "only ABORTED carries a reason")
		})
	}
}

func TestAWaitThatExpiresIsNotYet(t *testing.T) {
	f := newFixture(t)
	f.ensure(t)
	c, _ := f.attach(t, memberA, 0)
	c.request(t, asAgent().get("r", agentKey, 5*time.Second))
	f.clock.AwaitTimer(t)

	f.clock.Step(5 * time.Second)

	c.failure(t, "r", wire.Code_CODE_NOT_YET)
}

func TestNamespacesAndRolesHoldThroughTheWire(t *testing.T) {
	tests := []struct {
		name    string
		request *wire.Request
	}{
		{name: "a library reaching snapshot/", request: asNCCL().get("r", agentKey, 0)},
		{name: "a library failing the session", request: asNCCL().fail("r", "Sabotage")},
		{name: "a library leaving", request: asNCCL().leave("r")},
		{name: "a relay leaving", request: asRelay().leave("r")},
		{name: "a relay posting as another member", request: asRelay().postBarrier("r", "cuinterpose/round", "member-b")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			s := f.ensure(t)
			c, _ := f.attach(t, memberA, 0)

			c.request(t, tt.request)

			c.failure(t, "r", wire.Code_CODE_FORBIDDEN)
			require.NoError(t, s.Err(), "the session is still open")
		})
	}
}

func TestARequestWithoutAUsableOriginOperationOrWaitIsInvalid(t *testing.T) {
	tests := []struct {
		name    string
		request *wire.Request
	}{
		{name: "no origin", request: &wire.Request{Id: "r", Operation: asAgent().unset("r", agentKey).GetOperation()}},
		{name: "an unspecified role", request: origin{&wire.Origin{Role: wire.Role_ROLE_UNSPECIFIED}}.unset("r", agentKey)},
		{name: "no operation", request: asAgent().req("r")},
		{name: "a negative wait", request: asAgent().get("r", agentKey, -time.Second)},
		{name: "a malformed wait", request: func() *wire.Request {
			r := asAgent().get("r", agentKey, 0)
			r.GetGet().Wait = &durationpb.Duration{Seconds: 1, Nanos: -1}
			return r
		}()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.ensure(t)
			c, _ := f.attach(t, memberA, 0)

			c.request(t, tt.request)

			c.failure(t, "r", wire.Code_CODE_INVALID)
		})
	}
}

// errBackend is what failingStore reports, as a backend outage would.
var errBackend = errors.New("the backend is unreachable")

// failingStore is a store whose every Insert fails as a backend outage would,
// wrapping the cause with the key, as a backend may.
type failingStore struct{ kv.Store }

func (s failingStore) Space(uid types.UID) kv.Space { return failingSpace{s.Store.Space(uid)} }

type failingSpace struct{ kv.Space }

func (failingSpace) Insert(_ context.Context, key string, _ []byte) error {
	return fmt.Errorf("key %q: %w", key, errBackend)
}

func TestAnInternalErrorIsLoggedWithoutWhatTheRequestNamed(t *testing.T) {
	const secretKey, secretValue = "snapshot/fabric-handle-7", "handle-bytes-7"
	f := newFixtureOn(t, failingStore{memory.New()})
	f.ensure(t)
	c, _ := f.attach(t, memberA, 0)

	c.request(t, asAgent().insert("r", secretKey, []byte(secretValue)))

	e := c.failure(t, "r", wire.Code_CODE_INTERNAL)
	assert.Contains(t, e.GetMessage(), errBackend.Error())
	logs := f.logs.String()
	assert.Contains(t, logs, errBackend.Error(), "the cause is logged")
	assert.Contains(t, logs, `"operation"="insert"`)
	assert.Contains(t, logs, `"role"="ROLE_AGENT"`)
	assert.NotContains(t, logs, secretKey)
	assert.NotContains(t, logs, secretValue)
}
