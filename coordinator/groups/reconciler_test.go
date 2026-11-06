// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package groups

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// TestSnapshotRequests covers what a PodGroupSnapshot's events reconcile: the
// snapshot, and each PodGroupRestore of that snapshot, by UID.
func TestSnapshotRequests(t *testing.T) {
	ctx := context.Background()
	restoreOf := func(snapshot *v1alpha1.PodGroupSnapshot, name string) *v1alpha1.PodGroupRestore {
		r := newRestore(snapshot, "a")
		r.Name, r.UID = name, types.UID(name+"-uid")
		return r
	}
	twice := newSnapshot("twice-uid", "a")
	twice.Name = "twice"
	other := newSnapshot("other-uid", "a")
	other.Name = "other"
	none := newSnapshot("none-uid", "a")
	none.Name = "none"
	first, second, ofOther := restoreOf(twice, "first"), restoreOf(twice, "second"), restoreOf(other, "of-other")
	// recreated took the name of twice, which the cache no longer holds.
	recreated := newSnapshot("recreated-uid", "a")
	recreated.Name = twice.Name
	reader := fakeReader(t, twice, other, none, first, second, ofOther).Build()
	requests := snapshotRequests(reader, logr.Discard())

	for _, tt := range []struct {
		name     string
		snapshot *v1alpha1.PodGroupSnapshot
		want     []request
	}{
		{"a snapshot with two restores", twice, []request{requestFor(snapshotKind, twice),
			requestFor(restoreKind, first), requestFor(restoreKind, second)}},
		{"a snapshot with one restore", other, []request{requestFor(snapshotKind, other),
			requestFor(restoreKind, ofOther)}},
		{"a snapshot with none", none, []request{requestFor(snapshotKind, none)}},
		{"a recreated snapshot wakes none of the earlier one's", recreated,
			[]request{requestFor(snapshotKind, recreated)}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.ElementsMatch(t, tt.want, requests(ctx, tt.snapshot))
		})
	}

	t.Run("a failed List reconciles the snapshot alone", func(t *testing.T) {
		failing := fakeReader(t, twice, first).WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return errors.New("the cache is gone")
			},
		}).Build()
		var (
			mu    sync.Mutex
			lines []string
		)
		log := funcr.New(func(prefix, args string) {
			mu.Lock()
			defer mu.Unlock()
			lines = append(lines, args)
		}, funcr.Options{})
		assert.Equal(t, []request{requestFor(snapshotKind, twice)}, snapshotRequests(failing, log)(ctx, twice))
		require.Len(t, lines, 1)
		assert.Contains(t, lines[0], `"error"="the cache is gone"`)
	})
}
