// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package lifecyclehooks

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// serve starts a server on 127.0.0.1 and returns its IP and port.
func serve(t *testing.T, handler http.HandlerFunc) (string, int32) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	host, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	return host, int32(p)
}

func statusCode(err error) int {
	var status *StatusError
	if errors.As(err, &status) {
		return status.Code
	}
	return 0
}

func TestHTTPGet(t *testing.T) {
	for _, tc := range []struct {
		code int
		ok   bool
	}{
		{http.StatusOK, true},
		{http.StatusNoContent, false},
		{http.StatusServiceUnavailable, false},
		{http.StatusNotFound, false},
	} {
		t.Run(strconv.Itoa(tc.code), func(t *testing.T) {
			ip, port := serve(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/checkpoint/status" {
					t.Errorf("got %s %s", r.Method, r.URL.Path)
				}
				w.WriteHeader(tc.code)
			})
			err := HTTPGet(t.Context(), ip, port, "/checkpoint/status", time.Second)
			if tc.ok && err != nil {
				t.Fatalf("HTTPGet = %v, want nil", err)
			}
			if !tc.ok && statusCode(err) != tc.code {
				t.Fatalf("HTTPGet = %v, want a StatusError with %d", err, tc.code)
			}
		})
	}
}

func TestHTTPPost(t *testing.T) {
	for _, tc := range []struct {
		code int
		ok   bool
	}{
		{http.StatusOK, true},
		{http.StatusNoContent, true},
		{http.StatusAccepted, false},
		{http.StatusNotFound, false},
		{http.StatusInternalServerError, false},
	} {
		t.Run(strconv.Itoa(tc.code), func(t *testing.T) {
			ip, port := serve(t, func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if r.Method != http.MethodPost || r.URL.Path != "/checkpoint/resume" || len(body) != 0 {
					t.Errorf("got %s %s with a %d-byte body", r.Method, r.URL.Path, len(body))
				}
				w.WriteHeader(tc.code)
			})
			err := HTTPPost(t.Context(), ip, port, "/checkpoint/resume", time.Second)
			if tc.ok && err != nil {
				t.Fatalf("HTTPPost = %v, want nil", err)
			}
			if !tc.ok && statusCode(err) != tc.code {
				t.Fatalf("HTTPPost = %v, want a StatusError with %d", err, tc.code)
			}
		})
	}
}

func TestHTTPDoesNotFollowRedirects(t *testing.T) {
	var followed atomic.Bool
	ip, port := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			followed.Store(true)
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	})
	err := HTTPGet(t.Context(), ip, port, "/status", time.Second)
	if statusCode(err) != http.StatusFound {
		t.Fatalf("HTTPGet = %v, want a StatusError with 302", err)
	}
	if followed.Load() {
		t.Fatal("the redirect was followed")
	}
}

func TestHTTPIgnoresTheProxyEnvironment(t *testing.T) {
	var proxied atomic.Bool
	proxy := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { proxied.Store(true) }))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	ip, port := serve(t, func(http.ResponseWriter, *http.Request) {})
	if err := HTTPGet(t.Context(), ip, port, "/status", time.Second); err != nil {
		t.Fatalf("HTTPGet = %v, want nil", err)
	}
	if proxied.Load() {
		t.Fatal("the request went through the proxy")
	}
}

func TestHTTPTimeout(t *testing.T) {
	release := make(chan struct{})
	ip, port := serve(t, func(http.ResponseWriter, *http.Request) { <-release })
	defer close(release)
	start := time.Now()
	err := HTTPGet(t.Context(), ip, port, "/status", 100*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("HTTPGet = %v, want a deadline error", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("HTTPGet took %s, want about the timeout", elapsed)
	}
}

func TestHTTPReadsOnlyPartOfALargeBody(t *testing.T) {
	ip, port := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		chunk := []byte(strings.Repeat("x", 64<<10))
		for range 1024 { // 64 MiB, far more than the limit
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	})
	if err := HTTPGet(t.Context(), ip, port, "/status", 5*time.Second); err != nil {
		t.Fatalf("HTTPGet = %v, want nil", err)
	}
}

func TestHTTPConnectionRefused(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	err = HTTPPost(t.Context(), "127.0.0.1", int32(port), "/status", time.Second)
	if err == nil || statusCode(err) != 0 {
		t.Fatalf("HTTPPost = %v, want a connection error", err)
	}
}

func TestRequestURL(t *testing.T) {
	for _, tc := range []struct {
		ip   string
		path string
		port int32
		want string
	}{
		{"10.0.0.5", "/checkpoint/status", 8000, "http://10.0.0.5:8000/checkpoint/status"},
		{"fd00::5", "/resume", 80, "http://[fd00::5]:80/resume"},
		{"10.0.0.5", "/a b", 80, "http://10.0.0.5:80/a%20b"},
		{"10.0.0.5", "", 80, "http://10.0.0.5:80/"},
		{"10.0.0.5", "status", 80, "http://10.0.0.5:80/status"},
		{"10.0.0.5", "/x?y=1", 80, "http://10.0.0.5:80/x?y=1"},
		{"10.0.0.5", "/x#y", 80, "http://10.0.0.5:80/x"},
		// Whatever the path says about another host, the request goes to the Pod.
		{"10.0.0.5", "//evil.example/x", 80, "http://10.0.0.5:80/x"},
		{"10.0.0.5", "http://evil.example/x", 80, "http://10.0.0.5:80/http://evil.example/x"},
		{"10.0.0.5", "//user:pass@evil.example/x", 80, "http://10.0.0.5:80/x"},
		{"10.0.0.5", "/x", 0, ""},
		{"10.0.0.5", "/x", 65536, ""},
		{"pod.example", "/x", 80, ""},
		{"", "/x", 80, ""},
	} {
		got, err := requestURL(tc.ip, tc.port, tc.path)
		if tc.want == "" {
			if err == nil {
				t.Errorf("requestURL(%q, %q, %d) = %q, want an error", tc.ip, tc.path, tc.port, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("requestURL(%q, %q, %d) = %q, %v, want %q", tc.ip, tc.path, tc.port, got, err, tc.want)
		}
	}
}

func TestResolvePort(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{
		{Name: "main", Ports: []corev1.ContainerPort{{Name: "metrics", ContainerPort: 9090}, {Name: "hooks", ContainerPort: 8000}}},
		{Name: "sidecar", Ports: []corev1.ContainerPort{{Name: "admin", ContainerPort: 7000}}},
	}}}
	for _, tc := range []struct {
		port intstr.IntOrString
		want int32
	}{
		{intstr.FromInt32(8080), 8080},
		{intstr.FromString("hooks"), 8000},
		{intstr.FromInt32(0), 0},
		{intstr.FromInt32(65536), 0},
		{intstr.FromString("admin"), 0}, // another container's port
		{intstr.FromString("missing"), 0},
	} {
		got, err := ResolvePort(pod, "main", tc.port)
		if tc.want == 0 {
			if err == nil {
				t.Errorf("ResolvePort(%s) = %d, want an error", tc.port.String(), got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("ResolvePort(%s) = %d, %v, want %d", tc.port.String(), got, err, tc.want)
		}
	}
	if _, err := ResolvePort(pod, "other", intstr.FromString("hooks")); err == nil {
		t.Error("ResolvePort accepted a container that is not in the Pod")
	}
}

func TestHTTPTarget(t *testing.T) {
	pod := &corev1.Pod{Status: corev1.PodStatus{PodIP: "10.0.0.5"}}
	if ip, err := HTTPTarget(pod); err != nil || ip != "10.0.0.5" {
		t.Fatalf("HTTPTarget = %q, %v, want 10.0.0.5", ip, err)
	}
	hostNetwork := pod.DeepCopy()
	hostNetwork.Spec.HostNetwork = true
	if _, err := HTTPTarget(hostNetwork); err == nil {
		t.Error("HTTPTarget accepted a host-network Pod")
	}
	if _, err := HTTPTarget(&corev1.Pod{}); err == nil {
		t.Error("HTTPTarget accepted a Pod with no IP")
	}
}
