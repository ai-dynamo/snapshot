// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/ai-dynamo/snapshot/operator/internal/coordinator/kv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests take the wake-up paths that the conformance suite cannot tell
// apart from finding a key or a release already there. Each runs in a synctest
// bubble and writes only once synctest.Wait reports every waiter durably
// blocked, and checks that the waiters are registered in the space first.

func TestWaitIsWokenByTheWriteThatCreatesItsKey(t *testing.T) {
	const waiters = 4
	tests := []struct {
		name  string
		write func(context.Context, kv.Space) error
		want  []byte
	}{
		{
			name:  "an insert",
			write: func(ctx context.Context, sp kv.Space) error { return sp.Insert(ctx, "key", []byte("handle")) },
			want:  []byte("handle"),
		},
		{
			name: "an add",
			write: func(ctx context.Context, sp kv.Space) error {
				_, err := sp.Add(ctx, "key", 1)
				return err
			},
			want: kv.EncodeCounter(1),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				sp, ctx := New().Space("attempt-1"), context.Background()
				got := make(chan []byte, waiters)
				for range waiters {
					go func() {
						value, err := sp.Wait(ctx, "key")
						assert.NoError(t, err)
						got <- value
					}()
				}
				synctest.Wait()
				require.Equal(t, waiters, registered(sp, "key"), "every waiter waits before the write")

				require.NoError(t, tt.write(ctx, sp))

				for range waiters {
					assert.Equal(t, tt.want, <-got)
				}
				assert.Zero(t, registered(sp, "key"), "a released key leaves no waiter behind")
			})
		})
	}
}

func TestAwaitBarrierIsWokenByWhatReleasesIt(t *testing.T) {
	tests := []struct {
		name    string
		before  func(context.Context, kv.Space) error
		release func(context.Context, kv.Space) error
	}{
		{
			name: "the last participant's post",
			before: func(ctx context.Context, sp kv.Space) error {
				if err := sp.CreateBarrier(ctx, "barrier", 2); err != nil {
					return err
				}
				return sp.PostBarrier(ctx, "barrier", "a")
			},
			release: func(ctx context.Context, sp kv.Space) error { return sp.PostBarrier(ctx, "barrier", "b") },
		},
		{
			name: "creating it once every participant has posted",
			before: func(ctx context.Context, sp kv.Space) error {
				if err := sp.PostBarrier(ctx, "barrier", "a"); err != nil {
					return err
				}
				return sp.PostBarrier(ctx, "barrier", "b")
			},
			release: func(ctx context.Context, sp kv.Space) error { return sp.CreateBarrier(ctx, "barrier", 2) },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				sp, ctx := New().Space("attempt-1"), context.Background()
				require.NoError(t, tt.before(ctx, sp))
				released := make(chan error, 1)
				go func() { released <- sp.AwaitBarrier(ctx, "barrier") }()
				synctest.Wait()
				select {
				case err := <-released:
					t.Fatalf("the barrier released before anything released it: %v", err)
				default:
				}

				require.NoError(t, tt.release(ctx, sp))

				require.NoError(t, <-released)
			})
		})
	}
}

// registered returns how many waiters the space holds for key.
func registered(sp kv.Space, key string) int {
	s := sp.(*space)
	s.mu.Lock()
	defer s.mu.Unlock()
	if w, ok := s.waiters[key]; ok {
		return w.refs
	}
	return 0
}
