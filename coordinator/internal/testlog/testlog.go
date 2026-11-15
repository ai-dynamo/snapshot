// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package testlog reads what the coordinator logs of its sessions, for tests.
package testlog

import (
	"strings"

	"k8s.io/apimachinery/pkg/types"
)

// SessionEvents returns what the coordinator logged of the life of uid's
// session in lines, in order: "opened", "closed" once every member has left,
// or "failed" and the reason. lines are funcr's, with its default options.
func SessionEvents(lines []string, uid types.UID) []string {
	var events []string
	for _, line := range lines {
		if !strings.Contains(line, `"attempt"="`+string(uid)+`"`) {
			continue
		}
		switch {
		case strings.Contains(line, `"msg"="Session opened"`):
			events = append(events, "opened")
		case strings.Contains(line, `"msg"="Session closed"`):
			events = append(events, "closed")
		case strings.Contains(line, `"msg"="Session failed"`):
			_, rest, _ := strings.Cut(line, `"reason"="`)
			reason, _, _ := strings.Cut(rest, `"`)
			events = append(events, "failed "+reason)
		}
	}
	return events
}
