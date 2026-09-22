// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateEventMessage(t *testing.T) {
	short := strings.Repeat("a", eventMessageLengthLimit)
	if got := truncateEventMessage(short); got != short {
		t.Fatalf("message at the limit was altered: len %d", len(got))
	}

	long := strings.Repeat("a", eventMessageLengthLimit+50)
	got := truncateEventMessage(long)
	if len(got) != eventMessageLengthLimit || !strings.HasSuffix(got, "...") {
		t.Fatalf("got len %d, suffix %q", len(got), got[len(got)-3:])
	}

	// eventMessageLengthLimit-3 is odd, so a 2-byte rune straddles the cut.
	multibyte := strings.Repeat("é", eventMessageLengthLimit)
	got = truncateEventMessage(multibyte)
	if !utf8.ValidString(got) {
		t.Fatalf("truncation split a rune: %q", got[len(got)-6:])
	}
	if len(got) > eventMessageLengthLimit || !strings.HasSuffix(got, "...") {
		t.Fatalf("got len %d", len(got))
	}
}

func TestTruncateUTF8(t *testing.T) {
	if got := truncateUTF8("abc", 3); got != "abc" {
		t.Fatalf("got %q", got)
	}
	if got := truncateUTF8("abcd", 3); got != "abc" {
		t.Fatalf("got %q", got)
	}
	// "é" is 2 bytes; a 3-byte limit must not keep half of the second rune.
	if got := truncateUTF8("éé", 3); got != "é" {
		t.Fatalf("got %q", got)
	}
	if got := truncateUTF8("é", 1); got != "" {
		t.Fatalf("got %q", got)
	}

	node := strings.Repeat("n", eventReportingInstanceLengthLimit+100)
	if got := truncateUTF8(node, eventReportingInstanceLengthLimit); len(got) != eventReportingInstanceLengthLimit {
		t.Fatalf("reporting instance not capped: len %d", len(got))
	}
}
