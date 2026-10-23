// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"fmt"
	"strings"
)

// The namespace each role reaches. A key or a barrier name must begin with its
// caller's prefix and name something below it, so the bare prefix is not a key.
const (
	agentNamespace   = "snapshot/"
	relayNamespace   = "cuinterpose/"
	ncclNamespace    = "nccl/"
	libraryNamespace = "cuda/"
)

// The largest value each role may store.
const (
	// MaxValue is the limit for an agent and for a relay, which exchange
	// records that can hold a whole topology.
	MaxValue = 32 << 20

	// MaxLibraryValue is the limit for a library, matching the 16-bit length
	// of CUDA Checkpoint's rendezvous interface rather than a round 64 KiB.
	MaxLibraryValue = 65535
)

// MaxKeyLength bounds a key, a barrier name, and a barrier participant, for
// every role.
const MaxKeyLength = 1024

// MaxSessionBytes bounds everything one session holds, for every role: each key
// and its value, each barrier, each participant, and each request recorded for
// replay, at what cost charges for it. The coordinator holds every attempt in
// the cluster in one address space, so this is what one attempt can cost it. A
// flood from a workload process can exhaust its own session's budget, which at
// worst fails that attempt, never another one.
const MaxSessionBytes = 256 << 20

// LibraryNCCL names the NCCL Checkpoint compatibility endpoint.
const LibraryNCCL = "nccl"

// namespace returns the one prefix the caller reaches.
func (c Caller) namespace() (string, error) {
	switch c.Role {
	case RoleAgent:
		return agentNamespace, nil
	case RoleRelay:
		return relayNamespace, nil
	case RoleLibrary:
		return c.libraryNamespace()
	default:
		return "", fmt.Errorf("role %q reaches no namespace: %w", c.Role, ErrForbidden)
	}
}

// libraryNamespace returns the namespace of one library's clients, which CUDA's
// interface promises to isolate from every other library's.
func (c Caller) libraryNamespace() (string, error) {
	// The compatibility endpoint is the versionless one: its clients speak
	// Redis rather than a versioned CUDA interface, so they share one flat
	// namespace. Having no version select it, rather than the library's name,
	// is what lets NCCL reach cuda/nccl/<version>/ like any other library once
	// it moves onto CUDA Checkpoint.
	if c.Library == LibraryNCCL && c.Version == "" {
		return ncclNamespace, nil
	}
	if c.Library == "" || c.Version == "" {
		return "", fmt.Errorf("library caller without a library and a version: %w", ErrForbidden)
	}
	if strings.Contains(c.Library, "/") || strings.Contains(c.Version, "/") {
		return "", fmt.Errorf("library %q version %q crosses a namespace: %w", c.Library, c.Version, ErrForbidden)
	}
	return libraryNamespace + c.Library + "/" + c.Version + "/", nil
}

// limit returns the largest value the caller may store.
func (c Caller) limit() int {
	if c.Role == RoleLibrary {
		return MaxLibraryValue
	}
	return MaxValue
}

// authorize reports whether the caller reaches name and may store every value
// it carries. It covers keys and barrier names alike, which share one set of
// namespaces even though they are separate spaces.
func (c Caller) authorize(name string, values ...[]byte) error {
	ns, err := c.namespace()
	if err != nil {
		return err
	}
	if !strings.HasPrefix(name, ns) || len(name) == len(ns) {
		return fmt.Errorf("%q is not below %q: %w", name, ns, ErrForbidden)
	}
	if len(name) > MaxKeyLength {
		return fmt.Errorf("a name of %d bytes is over the %d-byte limit: %w", len(name), MaxKeyLength, ErrTooLarge)
	}
	limit := c.limit()
	for _, value := range values {
		if len(value) > limit {
			return fmt.Errorf("%d bytes over the %d-byte limit of role %q: %w", len(value), limit, c.Role, ErrTooLarge)
		}
	}
	return nil
}
