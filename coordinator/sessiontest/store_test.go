// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sessiontest

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ai-dynamo/snapshot/coordinator"
	"github.com/ai-dynamo/snapshot/coordinator/server"
	"github.com/ai-dynamo/snapshot/coordinator/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInsert covers Insert: the first value under a key wins, inserting the same
// value again succeeds, and a different value is refused.
func TestInsert(t *testing.T) {
	h := newHarness(t)
	tests := []struct {
		name          string
		ns            session.Namespace
		key           string
		first, second []byte
		wantErr       error
	}{
		{name: "the same value again succeeds", ns: session.NamespaceSnapshot, key: "ip.0",
			first: []byte("10.0.0.1"), second: []byte("10.0.0.1")},
		{name: "a different value is refused", ns: session.NamespaceSnapshot, key: "ip.0",
			first: []byte("10.0.0.1"), second: []byte("10.0.0.2"), wantErr: session.ErrExists},
		{name: "an empty value is a value", ns: session.NamespaceCuInterpose, key: "round-1",
			first: []byte{}, second: []byte("x"), wantErr: session.ErrExists},
		{name: "a library key may hold any bytes", ns: session.NamespaceLibrary, key: "comm 0\x00/id",
			first: []byte("id"), second: []byte("id")},
		{name: "a library key's different value is refused", ns: session.NamespaceLibrary, key: "comm 0\x00/id",
			first: []byte("id"), second: []byte("other"), wantErr: session.ErrExists},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := within(t)
			sessions := h.session(t, 2).connectAll(t)
			require.NoError(t, in(sessions[0], tt.ns).Insert(ctx, tt.key, tt.first))
			err := in(sessions[1], tt.ns).Insert(ctx, tt.key, tt.second)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
			} else {
				require.NoError(t, err)
			}
			got, err := in(sessions[1], tt.ns).Get(ctx, tt.key, 0)
			require.NoError(t, err)
			assert.Equal(t, string(tt.first), string(got), "the first value must stay")
		})
	}
}

// TestInsertRaceHasOneWinner inserts different values under the same keys from
// two members at once: exactly one wins each key, and both read its value.
func TestInsertRaceHasOneWinner(t *testing.T) {
	h := newHarness(t)
	ctx := within(t)
	sessions := h.session(t, 2).connectAll(t)
	for i := range 20 {
		key := fmt.Sprintf("race.%d", i)
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for m := range 2 {
			wg.Go(func() {
				errs[m] = sessions[m].Snapshot().Insert(ctx, key, fmt.Appendf(nil, "%d", m))
			})
		}
		wg.Wait()
		winners := 0
		for _, err := range errs {
			if err == nil {
				winners++
			} else {
				require.ErrorIs(t, err, session.ErrExists)
			}
		}
		require.Equal(t, 1, winners, "key %s", key)
		a, err := sessions[0].Snapshot().Get(ctx, key, 0)
		require.NoError(t, err)
		b, err := sessions[1].Snapshot().Get(ctx, key, 0)
		require.NoError(t, err)
		require.Equal(t, a, b)
	}
}

// TestNamespacesAreSeparate stores the same key in every namespace.
func TestNamespacesAreSeparate(t *testing.T) {
	h := newHarness(t)
	ctx := within(t)
	s := h.session(t, 1).connect(t, 0)
	namespaces := []session.Namespace{session.NamespaceSnapshot, session.NamespaceCuInterpose, session.NamespaceLibrary}
	for _, ns := range namespaces {
		require.NoError(t, in(s, ns).Insert(ctx, "k", []byte(ns)))
	}
	for _, ns := range namespaces {
		got, err := in(s, ns).Get(ctx, "k", 0)
		require.NoError(t, err)
		assert.Equal(t, string(ns), string(got))
	}
}

// TestKeys covers the keys a Store refuses before sending anything.
func TestKeys(t *testing.T) {
	h := newHarness(t)
	s := h.session(t, 1).connect(t, 0)
	tests := []struct {
		name    string
		ns      session.Namespace
		key     string
		wantErr error
	}{
		{name: "an empty key", ns: session.NamespaceSnapshot, key: "", wantErr: session.ErrInvalid},
		{name: "a space", ns: session.NamespaceSnapshot, key: "a b", wantErr: session.ErrInvalid},
		{name: "a wildcard", ns: session.NamespaceCuInterpose, key: "a.*", wantErr: session.ErrInvalid},
		{name: "an empty token", ns: session.NamespaceSnapshot, key: "a..b", wantErr: session.ErrInvalid},
		{name: "a trailing dot", ns: session.NamespaceSnapshot, key: "a.", wantErr: session.ErrInvalid},
		{name: "a key over the limit", ns: session.NamespaceSnapshot, key: strings.Repeat("k", coordinator.MaxKeyLength+1),
			wantErr: session.ErrTooLarge},
		{name: "a library key over the limit", ns: session.NamespaceLibrary,
			key: strings.Repeat("k", coordinator.MaxKeyLength+1), wantErr: session.ErrTooLarge},
		{name: "the longest library key", ns: session.NamespaceLibrary,
			key: strings.Repeat("\xff", coordinator.MaxKeyLength)},
		{name: "the longest key", ns: session.NamespaceSnapshot, key: strings.Repeat("k", coordinator.MaxKeyLength)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := within(t)
			err := in(s, tt.ns).Insert(ctx, tt.key, []byte("v"))
			if tt.wantErr == nil {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, tt.wantErr)
			_, err = in(s, tt.ns).Get(ctx, tt.key, 0)
			require.ErrorIs(t, err, tt.wantErr)
			require.ErrorIs(t, in(s, tt.ns).Delete(ctx, tt.key), tt.wantErr)
		})
	}
}

// TestGet covers Get's outcomes: the value, ErrNotFound without a wait, the
// value once inserted or ErrNotYet with one, and how the session ended or the
// context's error when either comes first.
func TestGet(t *testing.T) {
	h := newHarness(t)
	tests := []struct {
		name string
		ns   session.Namespace
		wait time.Duration
		// before runs before the Get, and during while it waits. The Get is
		// the first member's.
		before, during func(t *testing.T, sessions []coordinator.Session)
		// cancel cancels the Get's context while it waits.
		cancel  bool
		want    string
		wantErr error
	}{
		{name: "a present key, without a wait", ns: session.NamespaceSnapshot,
			before: insert(session.NamespaceSnapshot, "v"), want: "v"},
		{name: "a present key, with a wait", ns: session.NamespaceSnapshot, wait: time.Minute,
			before: insert(session.NamespaceSnapshot, "v"), want: "v"},
		{name: "a present key, with a wait shorter than a round trip", ns: session.NamespaceSnapshot,
			wait: time.Microsecond, before: insert(session.NamespaceSnapshot, "v"), want: "v"},
		{name: "a present key, with a wait of a millisecond", ns: session.NamespaceLibrary,
			wait: time.Millisecond, before: insert(session.NamespaceLibrary, "v"), want: "v"},
		{name: "an absent key, without a wait", ns: session.NamespaceSnapshot, wantErr: session.ErrNotFound},
		{name: "an absent key whose wait expires", ns: session.NamespaceSnapshot, wait: 200 * time.Millisecond,
			wantErr: session.ErrNotYet},
		{name: "a key inserted during the wait", ns: session.NamespaceSnapshot, wait: time.Minute,
			during: insert(session.NamespaceSnapshot, "late"), want: "late"},
		// The one case that delivers megabytes through a wait's watch. The
		// server asks the watch for flow control, but whether the reply goes
		// through changes nothing here: package server's TestFlowControlReplies
		// tests that.
		{name: "a large value inserted during the wait", ns: session.NamespaceSnapshot,
			wait: time.Minute, during: insert(session.NamespaceSnapshot, strings.Repeat("v", 4<<20)),
			want: strings.Repeat("v", 4<<20)},
		{name: "a library key inserted during the wait", ns: session.NamespaceLibrary, wait: time.Minute,
			during: insert(session.NamespaceLibrary, "late"), want: "late"},
		{name: "a key deleted, then inserted again, during the wait", ns: session.NamespaceCuInterpose, wait: time.Minute,
			before: insert(session.NamespaceCuInterpose, "old"),
			during: func(t *testing.T, sessions []coordinator.Session) {
				require.NoError(t, sessions[1].CuInterpose().Delete(within(t), "k"))
				insert(session.NamespaceCuInterpose, "new")(t, sessions)
			}, want: "old"},
		{name: "the session fails during the wait", ns: session.NamespaceSnapshot, wait: time.Minute,
			during: func(t *testing.T, sessions []coordinator.Session) {
				require.NoError(t, sessions[1].Fail(within(t), "PeerFailed"))
			}, wantErr: &session.AbortError{Reason: "PeerFailed"}},
		{name: "the session fails during a library wait", ns: session.NamespaceLibrary, wait: time.Minute,
			during: func(t *testing.T, sessions []coordinator.Session) {
				require.NoError(t, sessions[1].Fail(within(t), "PeerFailed"))
			}, wantErr: &session.AbortError{Reason: "PeerFailed"}},
		{name: "the session closes during the wait", ns: session.NamespaceSnapshot, wait: time.Minute,
			during: func(t *testing.T, sessions []coordinator.Session) {
				for _, s := range sessions {
					require.NoError(t, s.Leave(within(t)))
				}
			}, wantErr: session.ErrClosed},
		{name: "the context is cancelled during the wait", ns: session.NamespaceSnapshot, wait: time.Minute,
			cancel: true, wantErr: context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sessions := h.session(t, 2).connectAll(t)
			if tt.before != nil {
				tt.before(t, sessions)
			}
			ctx, cancel := context.WithCancel(within(t))
			defer cancel()
			type result struct {
				value []byte
				err   error
			}
			got := make(chan result, 1)
			go func() {
				value, err := in(sessions[0], tt.ns).Get(ctx, "k", tt.wait)
				got <- result{value, err}
			}()
			if tt.during != nil || tt.cancel {
				// Let the Get start waiting first.
				time.Sleep(100 * time.Millisecond)
			}
			if tt.during != nil {
				tt.during(t, sessions)
			}
			if tt.cancel {
				cancel()
			}
			r := <-got
			if tt.wantErr != nil {
				require.ErrorIs(t, r.err, tt.wantErr)
				return
			}
			require.NoError(t, r.err)
			assert.Equal(t, tt.want, string(r.value))
		})
	}
}

// insert returns a step that inserts value under k in ns from the second
// member.
func insert(ns session.Namespace, value string) func(*testing.T, []coordinator.Session) {
	return func(t *testing.T, sessions []coordinator.Session) {
		require.NoError(t, in(sessions[1], ns).Insert(within(t), "k", []byte(value)))
	}
}

// TestDelete covers Delete: an absent key is no error, and an Insert after a
// Delete starts afresh.
func TestDelete(t *testing.T) {
	h := newHarness(t)
	ctx := within(t)
	s := h.session(t, 1).connect(t, 0)
	namespaces := []session.Namespace{session.NamespaceSnapshot, session.NamespaceCuInterpose, session.NamespaceLibrary}
	for _, ns := range namespaces {
		store := in(s, ns)
		require.NoError(t, store.Delete(ctx, "never-inserted"), "deleting an absent key")
		require.NoError(t, store.Insert(ctx, "k", []byte("first")))
		require.NoError(t, store.Delete(ctx, "k"))
		require.NoError(t, store.Delete(ctx, "k"), "deleting twice")
		_, err := store.Get(ctx, "k", 0)
		require.ErrorIs(t, err, session.ErrNotFound)
		require.NoError(t, store.Insert(ctx, "k", []byte("second")), "an Insert after a Delete starts afresh")
		got, err := store.Get(ctx, "k", 0)
		require.NoError(t, err)
		require.Equal(t, "second", string(got))
	}
}

// TestStoreLimits covers the errors a write maps the coordinator's limits onto.
func TestStoreLimits(t *testing.T) {
	small := func(cfg *server.Config) { cfg.MaxValue, cfg.MaxLibraryValue, cfg.MaxLibraryKeys = 4<<10, 1<<10, 8 }
	defaults := func(*server.Config) {}
	tests := []struct {
		name    string
		limits  func(*server.Config)
		ns      session.Namespace
		value   int
		repeat  bool
		wantErr error
	}{
		{name: "a value of the Snapshot bucket's largest", limits: defaults, ns: session.NamespaceSnapshot,
			value: session.MaxValue},
		{name: "a value of the library bucket's largest", limits: defaults, ns: session.NamespaceLibrary,
			value: session.MaxLibraryValue},
		{name: "a value over Meta's limit", limits: small, ns: session.NamespaceSnapshot, value: 4<<10 + 1,
			wantErr: session.ErrTooLarge},
		{name: "a library value over Meta's limit", limits: defaults, ns: session.NamespaceLibrary,
			value: session.MaxLibraryValue + 1, wantErr: session.ErrTooLarge},
		{name: "a full library bucket", limits: small, ns: session.NamespaceLibrary, value: 1 << 10, repeat: true,
			wantErr: session.ErrTooMany},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := within(t)
			s := newHarness(t, tt.limits).session(t, 1).connect(t, 0)
			value := make([]byte, tt.value)
			var err error
			for i := 0; err == nil; i++ {
				err = in(s, tt.ns).Insert(ctx, fmt.Sprintf("k%d", i), value)
				if !tt.repeat {
					break
				}
			}
			require.ErrorIs(t, err, tt.wantErr)
		})
	}

	t.Run("deleting from a full library bucket", func(t *testing.T) {
		ctx := within(t)
		store := newHarness(t, small).session(t, 1).connect(t, 0).Library()
		for i := range 8 {
			require.NoError(t, store.Insert(ctx, fmt.Sprintf("k%d", i), nil))
		}
		require.ErrorIs(t, store.Insert(ctx, "k8", nil), session.ErrTooMany)
		require.NoError(t, store.Delete(ctx, "never-inserted"), "a key that was never inserted")
		require.NoError(t, store.Delete(ctx, "k0"), "a key that exists")
		require.NoError(t, store.Insert(ctx, "k0", []byte("again")), "an Insert after a Delete")
	})
}
