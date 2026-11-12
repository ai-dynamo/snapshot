// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
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

// The agent's in-cluster client sets no request timeout, so an API server that
// stalls on an event would hold whatever its caller was doing — a capture with
// a suspended workload, among others — for as long as the stall lasted. The
// fake clientset ignores the request context, so this needs a real one.
func TestEmitPodEventDoesNotOutliveItsTimeout(t *testing.T) {
	original := eventWriteTimeout
	t.Cleanup(func() { eventWriteTimeout = original })
	eventWriteTimeout = 50 * time.Millisecond

	// Answers far later than the bound under test, and later than the window the
	// assertion allows, so an unbounded write would miss it. It gives up on its
	// own so the server can still close.
	const stall = 2 * time.Second
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(stall):
		}
	}))
	t.Cleanup(server.Close)

	clientset, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatalf("build client: %v", err)
	}

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		emitPodEvent(context.Background(), clientset, logr.Discard(), pod,
			snapshotEventComponent, corev1.EventTypeWarning, "StalledOnPurpose", "message")
	}()

	select {
	case <-done:
	case <-time.After(750 * time.Millisecond):
		t.Fatal("emitPodEvent did not return; a stalled API server blocks its caller")
	}
}
