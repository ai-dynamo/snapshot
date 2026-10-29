// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package lifecyclehooks

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// serve starts a server on 127.0.0.1 and returns the URL of path on it, built
// the way the agent builds a hook's URL.
func serve(t *testing.T, path string, handler http.HandlerFunc) *url.URL {
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
	return RequestURL(host, p, path)
}

func TestHTTPStatus(t *testing.T) {
	for _, tc := range []struct {
		code int
		ok   bool
	}{
		{http.StatusOK, true},
		{http.StatusNoContent, true},
		{http.StatusNotModified, false},
		{http.StatusBadRequest, false},
		{http.StatusNotFound, false},
		{http.StatusServiceUnavailable, false},
	} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			t.Run(method+" "+strconv.Itoa(tc.code), func(t *testing.T) {
				u := serve(t, "/checkpoint/status", func(w http.ResponseWriter, r *http.Request) {
					if r.Method != method || r.URL.Path != "/checkpoint/status" {
						t.Errorf("got %s %s", r.Method, r.URL.Path)
					}
					w.WriteHeader(tc.code)
				})
				send := HTTPGet
				if method == http.MethodPost {
					send = HTTPPost
				}
				err := send(t.Context(), NewHTTPClient(), u, time.Second)
				if tc.ok && err != nil {
					t.Fatalf("%s = %v, want nil", method, err)
				}
				if !tc.ok && (err == nil || !strings.Contains(err.Error(), "failed with statuscode: "+strconv.Itoa(tc.code))) {
					t.Fatalf("%s = %v, want a failure with status %d", method, err, tc.code)
				}
			})
		}
	}
}

func TestHTTPSendsKubeletStyleHeaders(t *testing.T) {
	u := serve(t, "/", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); got != userAgent {
			t.Errorf("User-Agent = %q, want %q", got, userAgent)
		}
		if got := r.Header.Get("Accept"); got != "*/*" {
			t.Errorf("Accept = %q, want */*", got)
		}
		if got := r.Header.Get("Accept-Encoding"); got != "" {
			t.Errorf("Accept-Encoding = %q, want none", got)
		}
		if r.ContentLength > 0 {
			t.Errorf("body has %d bytes, want none", r.ContentLength)
		}
	})
	if err := HTTPPost(t.Context(), NewHTTPClient(), u, time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPFollowsRedirectsOnlyToTheSameHost(t *testing.T) {
	t.Run("same host", func(t *testing.T) {
		u := serve(t, "/old", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/old" {
				http.Redirect(w, r, "/new", http.StatusFound)
				return
			}
			w.WriteHeader(http.StatusServiceUnavailable)
		})
		// The redirect was followed: the result is the target's 503.
		if err := HTTPGet(t.Context(), NewHTTPClient(), u, time.Second); err == nil || !strings.Contains(err.Error(), "503") {
			t.Fatalf("HTTPGet = %v, want the redirect target's 503", err)
		}
	})
	t.Run("another host", func(t *testing.T) {
		u := serve(t, "/", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://other.invalid/", http.StatusFound)
		})
		// Not followed: the 302 itself is the result, and it fails.
		if err := HTTPGet(t.Context(), NewHTTPClient(), u, time.Second); err == nil || !strings.Contains(err.Error(), "302") {
			t.Fatalf("HTTPGet = %v, want the 302 to fail", err)
		}
	})
	t.Run("loop", func(t *testing.T) {
		u := serve(t, "/", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/", http.StatusFound)
		})
		if err := HTTPGet(t.Context(), NewHTTPClient(), u, time.Second); err == nil || !strings.Contains(err.Error(), "redirects") {
			t.Fatalf("HTTPGet = %v, want it to stop after %d redirects", err, maxRedirects)
		}
	})
}

func TestHTTPClientIgnoresTheProxyEnvironment(t *testing.T) {
	// Go caches the proxy environment on first use, so check the transport,
	// not a request.
	transport, ok := NewHTTPClient().Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatal("the transport must not use a proxy")
	}
	if !transport.DisableKeepAlives || !transport.DisableCompression {
		t.Fatal("the transport must disable keep-alives and compression")
	}
}

func TestHTTPTimeout(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	u := serve(t, "/", func(http.ResponseWriter, *http.Request) { <-release })
	start := time.Now()
	if err := HTTPGet(t.Context(), NewHTTPClient(), u, 100*time.Millisecond); err == nil {
		t.Fatal("HTTPGet = nil, want a timeout")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("HTTPGet took %s, want about the timeout", elapsed)
	}
}

func TestHTTPReadsOnlyPartOfALargeBody(t *testing.T) {
	u := serve(t, "/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 4*maxResponseBody)))
	})
	if err := HTTPGet(t.Context(), NewHTTPClient(), u, time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPConnectionRefused(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	err = HTTPGet(t.Context(), NewHTTPClient(), RequestURL("127.0.0.1", port, "/"), time.Second)
	if err == nil || !strings.Contains(err.Error(), "HTTP GET failed") {
		t.Fatalf("HTTPGet = %v, want a connection error", err)
	}
}

func TestRequestURL(t *testing.T) {
	for _, tc := range []struct {
		ip   string
		port int
		path string
		want string
	}{
		{"10.0.0.5", 8000, "/checkpoint/status", "http://10.0.0.5:8000/checkpoint/status"},
		{"fd00::5", 80, "/resume", "http://[fd00::5]:80/resume"},
		{"10.0.0.5", 80, "", "http://10.0.0.5:80"},
		{"10.0.0.5", 80, "status", "http://10.0.0.5:80/status"},
		{"10.0.0.5", 80, "/x?y=1", "http://10.0.0.5:80/x?y=1"},
		{"10.0.0.5", 80, "/a b", "http://10.0.0.5:80/a%20b"},
		// Whatever host the path names, the request goes to the Pod.
		{"10.0.0.5", 80, "//other.example/x", "http://10.0.0.5:80/x"},
		{"10.0.0.5", 80, "https://other.example/x", "http://10.0.0.5:80/x"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			if got := RequestURL(tc.ip, tc.port, tc.path).String(); got != tc.want {
				t.Fatalf("RequestURL = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolvePort(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Name: "main",
		Ports: []corev1.ContainerPort{
			{Name: "http", ContainerPort: 8000},
			{Name: "zero", ContainerPort: 0},
		},
	}}}}
	for _, tc := range []struct {
		name      string
		container string
		port      intstr.IntOrString
		want      int
	}{
		{"number", "main", intstr.FromInt32(8080), 8080},
		{"name", "main", intstr.FromString("http"), 8000},
		{"unknown name", "main", intstr.FromString("metrics"), -1},
		{"unknown container", "other", intstr.FromString("http"), -1},
		{"zero", "main", intstr.FromInt32(0), -1},
		{"named zero", "main", intstr.FromString("zero"), -1},
		{"too large", "main", intstr.FromInt32(65536), -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolvePort(pod, tc.container, tc.port)
			if got != tc.want || (err == nil) != (tc.want > 0) {
				t.Fatalf("ResolvePort = %d, %v; want %d", got, err, tc.want)
			}
		})
	}
}

func TestHTTPTarget(t *testing.T) {
	pod := func(hostNetwork bool, ip string) *corev1.Pod {
		return &corev1.Pod{Spec: corev1.PodSpec{HostNetwork: hostNetwork}, Status: corev1.PodStatus{PodIP: ip}}
	}
	if ip, err := HTTPTarget(pod(false, "10.0.0.5")); err != nil || ip != "10.0.0.5" {
		t.Fatalf("HTTPTarget = %q, %v; want the Pod IP", ip, err)
	}
	if _, err := HTTPTarget(pod(true, "10.0.0.5")); err == nil {
		t.Fatal("HTTPTarget accepted a host-network Pod")
	}
	if _, err := HTTPTarget(pod(false, "")); err == nil {
		t.Fatal("HTTPTarget accepted a Pod with no IP")
	}
}
