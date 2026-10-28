// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package lifecyclehooks

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// maxResponseBody bounds how much of a response is read before the
// connection is closed. The body is never used or logged.
const maxResponseBody = 4 << 10

// client sends every hook request. It goes straight to the Pod IP: no proxy
// from the agent's environment, which would send Pod traffic elsewhere, and no
// redirects, which could send the agent somewhere the Pod chose. A redirect
// comes back as its own 3xx response and counts as a failure.
var client = &http.Client{
	Transport: &http.Transport{
		Proxy:             nil,
		DisableKeepAlives: true,
	},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// StatusError is a response with a status that doesn't count as success.
type StatusError struct {
	Method string
	URL    string
	Code   int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s %s returned %d", e.Method, e.URL, e.Code)
}

// HTTPTarget returns the address hook requests go to: the Pod IP. A Pod on
// the host network has the node's IP, so a request would reach the node, not
// the Pod; those Pods are refused.
func HTTPTarget(pod *corev1.Pod) (string, error) {
	if pod.Spec.HostNetwork {
		return "", fmt.Errorf("Pod %s/%s uses the host network; HTTP actions need the Pod's own IP", pod.Namespace, pod.Name)
	}
	if pod.Status.PodIP == "" {
		return "", fmt.Errorf("Pod %s/%s has no IP yet", pod.Namespace, pod.Name)
	}
	return pod.Status.PodIP, nil
}

// HTTPGet sends a GET for a probe. It succeeds on 200 only.
func HTTPGet(ctx context.Context, podIP string, action *snapshotv1alpha1.HTTPGetAction, timeout time.Duration) error {
	return send(ctx, http.MethodGet, podIP, action.Path, action.Port, timeout, http.StatusOK)
}

// HTTPPost sends a POST with an empty body for a notification. It succeeds on
// 200 or 204, which mean the application accepted it.
func HTTPPost(ctx context.Context, podIP string, action *snapshotv1alpha1.HTTPPostAction, timeout time.Duration) error {
	return send(ctx, http.MethodPost, podIP, action.Path, action.Port, timeout, http.StatusOK, http.StatusNoContent)
}

func send(ctx context.Context, method, podIP, path string, port int32,
	timeout time.Duration, success ...int,
) error {
	target, err := requestURL(podIP, path, port)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, target, http.NoBody)
	if err != nil {
		return fmt.Errorf("build %s %s: %w", method, target, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, target, err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBody))
	_ = resp.Body.Close()
	for _, code := range success {
		if resp.StatusCode == code {
			return nil
		}
	}
	return &StatusError{Method: method, URL: target, Code: resp.StatusCode}
}

// requestURL repeats the API's rules for the path and port, so a value that
// reached the agent some other way still can't name another host.
func requestURL(podIP, path string, port int32) (string, error) {
	if net.ParseIP(podIP) == nil {
		return "", fmt.Errorf("invalid Pod IP %q", podIP)
	}
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.ContainsAny(path, "?#") {
		return "", fmt.Errorf("invalid HTTP path %q", path)
	}
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("invalid port %d", port)
	}
	u := url.URL{Scheme: "http", Host: net.JoinHostPort(podIP, strconv.Itoa(int(port))), Path: path}
	return u.String(), nil
}
