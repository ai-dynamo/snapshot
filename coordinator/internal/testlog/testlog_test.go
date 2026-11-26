// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package testlog

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSessionEvents(t *testing.T) {
	lines := []string{
		` "level"=0 "msg"="Session opened" "attempt"="a" "members"=2`,
		` "level"=0 "msg"="Session opened" "attempt"="b" "members"=1`,
		` "level"=0 "msg"="Member disconnected" "attempt"="a" "member"="member-0"`,
		` "level"=0 "msg"="Session failed" "attempt"="a" "reason"="Refused"`,
		` "level"=0 "msg"="Session closed" "attempt"="b"`,
	}
	assert.Equal(t, []string{"opened", "failed Refused"}, SessionEvents(lines, "a"))
	assert.Equal(t, []string{"opened", "closed"}, SessionEvents(lines, "b"))
	assert.Empty(t, SessionEvents(lines, "c"))
}
