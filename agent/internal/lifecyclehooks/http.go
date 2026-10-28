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
	"k8s.io/apimachinery/pkg/util/intstr"
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

// ResolvePort returns the port number an action's port names: the number
// itself, or the containerPort of the container's port with that name, as
// Kubernetes resolves a probe's named port.
func ResolvePort(pod *corev1.Pod, container string, port intstr.IntOrString) (int32, error) {
	if port.Type == intstr.Int {
		if port.IntVal < 1 || port.IntVal > 65535 {
			return 0, fmt.Errorf("invalid port %d", port.IntVal)
		}
		return port.IntVal, nil
	}
	c := findContainer(pod, container)
	if c == nil {
		return 0, fmt.Errorf("container %q is not in Pod %s/%s", container, pod.Namespace, pod.Name)
	}
	for _, p := range c.Ports {
		if p.Name == port.StrVal {
			return p.ContainerPort, nil
		}
	}
	return 0, fmt.Errorf("container %q has no port named %q", container, port.StrVal)
}

// HTTPGet sends a GET for a probe. It succeeds on 200 only.
func HTTPGet(ctx context.Context, podIP string, port int32, path string, timeout time.Duration) error {
	return send(ctx, http.MethodGet, podIP, port, path, timeout, http.StatusOK)
}

// HTTPPost sends a POST with an empty body for a notification. It succeeds on
// 200 or 204, which mean the application accepted it.
func HTTPPost(ctx context.Context, podIP string, port int32, path string, timeout time.Duration) error {
	return send(ctx, http.MethodPost, podIP, port, path, timeout, http.StatusOK, http.StatusNoContent)
}

func send(ctx context.Context, method, podIP string, port int32, path string,
	timeout time.Duration, success ...int,
) error {
	target, err := requestURL(podIP, port, path)
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

// requestURL builds the request URL the way kubelet does for an httpGet probe:
// the path, with a query if it has one, on the Pod IP and port. Whatever the
// path says about a scheme, user or host is replaced, so it can never send the
// request anywhere but the Pod. An empty path is "/".
func requestURL(podIP string, port int32, path string) (string, error) {
	if net.ParseIP(podIP) == nil {
		return "", fmt.Errorf("invalid Pod IP %q", podIP)
	}
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("invalid port %d", port)
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	u, err := url.Parse(path)
	if err != nil {
		u = &url.URL{Path: path}
	}
	u.Scheme = "http"
	u.User = nil
	u.Host = net.JoinHostPort(podIP, strconv.Itoa(int(port)))
	u.Fragment = ""
	return u.String(), nil
}
