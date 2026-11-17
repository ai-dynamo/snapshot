// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	corev1 "k8s.io/api/core/v1"
)

const (
	// restorePodIPWaitTimeout bounds how long, from the first time a restore
	// waits for its Pod's IP, its passes keep asking for it. Creating the
	// sandbox and its network normally takes well under a second. Past the
	// bound, the restore waits for kubelet's status update, which trails the
	// sandbox by about one relist, so waiting longer would gain little.
	restorePodIPWaitTimeout = 5 * time.Second

	// restorePodIPPollInterval is how often a waiting pass asks again.
	restorePodIPPollInterval = 100 * time.Millisecond
)

// preflightPodIP keeps a restore that restores established TCP connections
// pending until the restore Pod has an IP address. The API's status.podIP
// comes first. kubelet publishes it only at its next relist after the sandbox
// starts, up to a second later, so until then the IP comes from the Pod's
// sandbox in the node runtime. preflightPodIP returns the IP when this pass's
// copy of the Pod has none, and "" when the copy has one or the restore needs
// none.
func (w *NodeController) preflightPodIP(ctx context.Context, pod *corev1.Pod) (string, error) {
	if !w.config.CRIU.TcpEstablished || pod.Status.PodIP != "" {
		return "", nil
	}
	podKey := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
	ip, err := w.waitForPodIP(ctx, pod)
	if err != nil {
		w.log.V(1).Info("Restore Pod has no IP address in the API or its sandbox yet", "pod", podKey, "error", err.Error())
		return "", newRestorePendingError("PodIPPending", fmt.Sprintf("Waiting for restore Pod %s to receive an IP address", podKey))
	}
	w.log.V(1).Info("Using the restore Pod's IP address before the pass's copy of the Pod has it", "pod", podKey, "pod_ip", ip)
	return ip, nil
}

// waitForPodIP returns the IP of a Pod whose copy in this pass has none: its
// sandbox's, or the API's once the informer has it. Within podIPWaitTimeout of
// the Pod's first wait, it asks again every restorePodIPPollInterval, so a
// restore whose first pass ran before the sandbox had its network starts as
// soon as it does. Requeueing the Pod instead would repeat the whole preflight,
// a status write and an event each time. After the bound, it asks once.
func (w *NodeController) waitForPodIP(ctx context.Context, pod *corev1.Pod) (string, error) {
	uid := string(pod.UID)
	first, _ := w.podIPWaits.LoadOrStore(uid, time.Now())
	deadline := first.(time.Time).Add(w.podIPWaitTimeout)
	for {
		ip, err := w.sandboxPodIP(ctx, pod)
		if err == nil {
			w.podIPWaits.Delete(uid)
			return ip, nil
		}
		wait := time.Until(deadline)
		if wait <= 0 {
			return "", err
		}
		select {
		case <-ctx.Done():
			return "", err
		case <-time.After(min(wait, restorePodIPPollInterval)):
		}
		current, err := w.restorePodLister.Pods(pod.Namespace).Get(pod.Name)
		if err != nil || current.UID != pod.UID || current.DeletionTimestamp != nil {
			return "", errors.New("the restore Pod changed while its pass waited for its IP address")
		}
		if current.Status.PodIP != "" {
			w.podIPWaits.Delete(uid)
			return current.Status.PodIP, nil
		}
	}
}

// sandboxPodIP returns the IP that kubelet will publish as the Pod's
// status.podIP, from the IPs the node runtime reports for its newest sandbox,
// which kubelet reads only once that sandbox is ready. The runtime knows the
// Pod by its UID on the node; in a vCluster tenant that UID differs, no
// sandbox matches, and the restore waits for the API.
func (w *NodeController) sandboxPodIP(ctx context.Context, pod *corev1.Pod) (string, error) {
	if pod.Spec.HostNetwork {
		// kubelet publishes the node's IP, in the same status update as
		// status.hostIP; the sandbox has none of its own.
		return "", errors.New("the Pod uses the node's network")
	}
	ips, err := w.runtime.ResolvePodSandboxIPs(ctx, string(pod.UID))
	if err != nil {
		return "", err
	}
	return primaryPodIP(ips, pod.Status.HostIP)
}

// primaryPodIP picks status.podIP from a sandbox's IPs the way kubelet does:
// the first IP of the node's primary IP family, or else the first of the other
// family. kubelet sets the Pod's status.hostIP to the node's primary IP, so
// hostIP gives the family. Without it, only IPs of one family can be told
// apart, and IPs of both are an error. kubelet publishes no IP when one does
// not parse, so neither does this. Like kubelet, it returns the IP in its
// canonical form: an IPv4-mapped address as plain IPv4, which the socket remap
// requires.
func primaryPodIP(ips []string, hostIP string) (string, error) {
	var ipv4, ipv6 string
	for _, ip := range ips {
		addr, err := netip.ParseAddr(ip)
		if err != nil || addr.Zone() != "" {
			return "", fmt.Errorf("sandbox reports an invalid IP address %q", ip)
		}
		if addr = addr.Unmap(); addr.Is4() {
			ipv4 = cmp.Or(ipv4, addr.String())
		} else {
			ipv6 = cmp.Or(ipv6, addr.String())
		}
	}
	switch {
	case ipv4 == "" && ipv6 == "":
		return "", errors.New("sandbox reports no IP address")
	case ipv6 == "":
		return ipv4, nil
	case ipv4 == "":
		return ipv6, nil
	}
	host, err := netip.ParseAddr(hostIP)
	if err != nil {
		return "", errors.New("sandbox reports IPv4 and IPv6 addresses, and the Pod has no host IP to choose between them")
	}
	if host.Unmap().Is4() {
		return ipv4, nil
	}
	return ipv6, nil
}
