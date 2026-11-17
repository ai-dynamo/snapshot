// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordinator

import (
	"context"
	"errors"

	"github.com/ai-dynamo/snapshot/coordinator/session"
	"github.com/nats-io/nats.go/jetstream"
)

// bucket is one of the attempt's buckets, bound twice: kv writes and watches
// its keys, and stream reads a key's latest record, which tells a value from a
// delete marker and carries the revision that a write over the marker, or the
// deletion of the value, expects.
type bucket struct {
	kv     jetstream.KeyValue
	stream jetstream.Stream
}

// The header that marks a record as a key's deletion, and its values, which
// nats.go writes and reads but does not export.
const (
	kvOperation = "KV-Operation"
	kvDelete    = "DEL"
	kvPurge     = "PURGE"
)

// record is the latest record of a key.
type record struct {
	value    []byte
	revision uint64
	// deleted is set for a delete marker, which leaves the key absent.
	deleted bool
}

// latest returns an operation that reads the latest record of key in b into
// r, or reports session.ErrNotFound if key has none.
func latest(b bucket, key string, r *record) func(context.Context) error {
	subject := session.Subject(b.kv.Bucket(), key)
	return func(ctx context.Context) error {
		msg, err := b.stream.GetLastMsgForSubject(ctx, subject)
		if errors.Is(err, jetstream.ErrMsgNotFound) {
			return session.ErrNotFound
		}
		if err != nil {
			return err
		}
		op := msg.Header.Get(kvOperation)
		*r = record{value: msg.Data, revision: msg.Sequence, deleted: op == kvDelete || op == kvPurge}
		return nil
	}
}

// create writes value under key in b unless key holds a value, and reports
// whether it did, or returns the value key holds. It reads a value only
// within the connection's budget, taking what cost says of it: a write that finds
// key taken fails without reading it, where nats.go's Create would read it
// outside the budget, and create then reads key's latest record once. A
// delete marker, which leaves key absent, it writes over.
func (s *client) create(ctx context.Context, b bucket, key string, value []byte,
	cost readCost) (bool, []byte, error) {
	var revision uint64
	for {
		err := s.write(ctx, func(ctx context.Context) error {
			_, err := b.kv.Update(ctx, key, value, revision)
			if errors.Is(err, jetstream.ErrKeyExists) {
				return session.ErrExists
			}
			return err
		})
		if !errors.Is(err, session.ErrExists) {
			return err == nil, nil, err
		}
		var r record
		err = s.read(ctx, cost, latest(b, key, &r))
		switch {
		case errors.Is(err, session.ErrNotFound):
			// Purged since: create it again.
			revision = 0
		case err != nil:
			return false, nil, err
		case r.deleted:
			revision = r.revision
		default:
			return false, r.value, nil
		}
	}
}
