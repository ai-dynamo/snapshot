// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// podSandboxIPsTimeout bounds both calls. The restore preflight waits on them.
const podSandboxIPsTimeout = 2 * time.Second

type podSandboxService interface {
	ListPodSandbox(context.Context, *runtimeapi.PodSandboxFilter) ([]*runtimeapi.PodSandbox, error)
	PodSandboxStatus(context.Context, string, bool) (*runtimeapi.PodSandboxStatusResponse, error)
}

// resolvePodSandboxIPs returns the IPs that the runtime reports for the
// current sandbox of the Pod with podUID, the one kubelet takes the Pod's IPs
// from. Like kubelet, it finds the Pod's sandboxes in any state by the label
// kubelet sets on them, takes the newest, and reads its IPs only if it is
// ready. It never falls back to an older sandbox: while kubelet replaces a
// Pod's sandbox, the old one can still be ready, and kubelet publishes no IP
// until the new one is. The primary IP comes first, then the additional ones,
// the order kubelet reads them in. A sandbox on the host network has none.
func resolvePodSandboxIPs(ctx context.Context, svc podSandboxService, podUID string) ([]string, error) {
	if podUID == "" {
		return nil, errors.New("pod UID is empty")
	}
	ctx, cancel := context.WithTimeout(ctx, podSandboxIPsTimeout)
	defer cancel()

	// No state filter: one on ready sandboxes would hide a newer sandbox that
	// is not ready, and leave an older one to be taken for it.
	sandboxes, err := svc.ListPodSandbox(ctx, &runtimeapi.PodSandboxFilter{
		LabelSelector: map[string]string{kubeletPodUIDLabel: podUID},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list sandboxes of pod %s: %w", podUID, err)
	}
	// The label is checked again: another Pod's IP must never be used.
	sandboxes = slices.DeleteFunc(sandboxes, func(sandbox *runtimeapi.PodSandbox) bool {
		return sandbox.GetLabels()[kubeletPodUIDLabel] != podUID
	})
	if len(sandboxes) == 0 {
		return nil, fmt.Errorf("pod %s has no sandbox", podUID)
	}
	newest := slices.MinFunc(sandboxes, newestSandboxFirst)
	response, err := svc.PodSandboxStatus(ctx, newest.GetId(), false)
	if err != nil {
		return nil, fmt.Errorf("failed to get status of sandbox %s: %w", newest.GetId(), err)
	}
	status := response.GetStatus()
	if status == nil {
		return nil, fmt.Errorf("sandbox %s has no status", newest.GetId())
	}
	// As in kubelet, the state comes from the sandbox's status, not the list.
	if status.GetState() != runtimeapi.PodSandboxState_SANDBOX_READY {
		return nil, fmt.Errorf("the newest sandbox %s of pod %s is not ready", newest.GetId(), podUID)
	}
	network := status.GetNetwork()
	var ips []string
	if ip := network.GetIp(); ip != "" {
		ips = append(ips, ip)
	}
	for _, additional := range network.GetAdditionalIps() {
		ips = append(ips, additional.GetIp())
	}
	return ips, nil
}

// newestSandboxFirst orders a Pod's sandboxes the way kubelet does
// (podSandboxByCreatedThenID in pkg/kubelet/kuberuntime): the higher attempt
// first, then the later creation time, then the lower ID. kubelet creates each
// new sandbox of a Pod with the next attempt number.
func newestSandboxFirst(a, b *runtimeapi.PodSandbox) int {
	if a.GetMetadata() != nil && b.GetMetadata() != nil {
		if order := cmp.Compare(b.GetMetadata().GetAttempt(), a.GetMetadata().GetAttempt()); order != 0 {
			return order
		}
	}
	return cmp.Or(
		cmp.Compare(b.GetCreatedAt(), a.GetCreatedAt()),
		strings.Compare(a.GetId(), b.GetId()),
	)
}
