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
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
)

// The HTTP actions behave like a Kubernetes httpGet probe. Each choice below
// follows kubelet's prober (pkg/probe/http), unless it says otherwise.
const (
	// maxResponseBody bounds how much of a response is read. The body is
	// never used or logged.
	maxResponseBody = 10 << 10
	// userAgent names the agent the way kubelet's kube-probe names kubelet.
	userAgent = "snapshot-agent"
)

// NewHTTPClient returns the client hook requests go through. Like kubelet's
// prober, it ignores the agent's proxy environment, which would send Pod
// traffic elsewhere, and it opens a new connection for every request and asks
// for no compression.
//
// Unlike kubelet, which follows redirects to the same host, it follows none:
// the 3xx response is the result, and it fails. A redirect could otherwise
// send the agent somewhere the Pod chose, and Go would turn a redirected POST
// into a GET, so a notification could pass without the application getting
// it. The path in the spec must be the final one.
func NewHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:              nil,
			DisableKeepAlives:  true,
			DisableCompression: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
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
// itself, or the containerPort of the container's port with that name. Like
// kubelet's ResolveContainerPort, it returns -1 with the error when the port
// is not a valid port number.
func ResolvePort(pod *corev1.Pod, container string, port intstr.IntOrString) (int, error) {
	number := -1
	switch port.Type {
	case intstr.Int:
		number = port.IntValue()
	case intstr.String:
		c := findContainer(pod, container)
		if c == nil {
			return -1, fmt.Errorf("container %q is not in Pod %s/%s", container, pod.Namespace, pod.Name)
		}
		found := false
		for _, p := range c.Ports {
			if p.Name == port.StrVal {
				number, found = int(p.ContainerPort), true
				break
			}
		}
		if !found {
			return -1, fmt.Errorf("container %q has no port named %q", container, port.StrVal)
		}
	default:
		return -1, fmt.Errorf("port %v has no type", port)
	}
	if len(validation.IsValidPortNum(number)) > 0 {
		return -1, fmt.Errorf("invalid port number: %d", number)
	}
	return number, nil
}

// RequestURL builds a hook request's URL the way kubelet builds an httpGet
// probe's: the path as parsed, with the scheme and host set to http and the
// Pod IP and port. Setting the host replaces any host the path names, so the
// request goes to the Pod.
func RequestURL(podIP string, port int, path string) *url.URL {
	u, err := url.Parse(path)
	if err != nil {
		// Like kubelet, pass a path that doesn't parse along as it is.
		u = &url.URL{Path: path}
	}
	u.Scheme = "http"
	u.Host = net.JoinHostPort(podIP, strconv.Itoa(port))
	return u
}

// HTTPGet sends a probe's GET. It succeeds on a 2xx status.
func HTTPGet(ctx context.Context, client *http.Client, u *url.URL, timeout time.Duration) error {
	return send(ctx, client, http.MethodGet, u, timeout)
}

// HTTPPost sends a notification's POST, with an empty body. It succeeds on a
// 2xx status, the probe's rule. Kubelet's own lifecycle hooks accept any
// response; this one must confirm that the application accepted the
// notification.
func HTTPPost(ctx context.Context, client *http.Client, u *url.URL, timeout time.Duration) error {
	return send(ctx, client, http.MethodPost, u, timeout)
}

func send(ctx context.Context, client *http.Client, method string, u *url.URL, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), http.NoBody)
	if err != nil {
		return fmt.Errorf("failed to build the %s request for %s: %w", method, u.Redacted(), err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "*/*")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send HTTP %s: %w", method, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBody))
	// Kubelet passes 200 to 399. Here a 3xx is a redirect that wasn't
	// followed, so only a 2xx passes.
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		return nil
	}
	// Like kubelet's message, it leaves the body out.
	return fmt.Errorf("HTTP %s %s failed with statuscode: %d", method, u.Redacted(), resp.StatusCode)
}
