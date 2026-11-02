// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package session

// The limits a session's buckets have unless the coordinator is configured
// otherwise. Each bucket limits the size of its values, and the library bucket
// also its number of keys; no bucket reserves memory, and every session shares
// the coordinator's memory cap. MaxValue is SNEP-324's default, to revisit once
// the largest cuInterpose record has been measured on the largest supported
// topology. Meta carries the value limits a session actually has, and the
// client refuses a larger value itself.
const (
	// MaxValue is the largest value the Snapshot bucket holds: agent and relay
	// records, which can describe a whole topology.
	MaxValue = 32 << 20

	// MaxLibraryValue is the largest value the library bucket holds. NCCL's
	// communicator identifiers take 128 bytes.
	MaxLibraryValue = 4 << 10

	// MaxLibraryKeys is how many keys the library bucket holds, deleted ones
	// included, which keeps one session's library values under 16 MiB. With
	// keys and headers, a library bucket reaches about 22 MiB.
	MaxLibraryKeys = 4096
)

// MaxNameLength bounds an attempt UID, a member ID, and a barrier name.
const MaxNameLength = 253
