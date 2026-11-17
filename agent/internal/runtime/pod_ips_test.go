// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

type fakePodSandboxService struct {
	sandboxes []*runtimeapi.PodSandbox
	listErr   error
	statuses  map[string]*runtimeapi.PodSandboxStatus
	statusErr error

	filter    *runtimeapi.PodSandboxFilter
	statusIDs []string
}

func (f *fakePodSandboxService) ListPodSandbox(_ context.Context, filter *runtimeapi.PodSandboxFilter) ([]*runtimeapi.PodSandbox, error) {
	f.filter = filter
	return f.sandboxes, f.listErr
}

func (f *fakePodSandboxService) PodSandboxStatus(_ context.Context, id string, _ bool) (*runtimeapi.PodSandboxStatusResponse, error) {
	f.statusIDs = append(f.statusIDs, id)
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	return &runtimeapi.PodSandboxStatusResponse{Status: f.statuses[id]}, nil
}

// readySandbox is a ready sandbox of the Pod with podUID as kubelet creates
// it, with the attempt number kubelet gives the Pod's sandboxes in turn.
func readySandbox(id, podUID string, attempt uint32, createdAt int64) *runtimeapi.PodSandbox {
	return &runtimeapi.PodSandbox{
		Id:        id,
		Metadata:  &runtimeapi.PodSandboxMetadata{Uid: podUID, Attempt: attempt},
		State:     runtimeapi.PodSandboxState_SANDBOX_READY,
		CreatedAt: createdAt,
		Labels:    map[string]string{kubeletPodUIDLabel: podUID},
	}
}

func notReadySandbox(id, podUID string, attempt uint32, createdAt int64) *runtimeapi.PodSandbox {
	sandbox := readySandbox(id, podUID, attempt, createdAt)
	sandbox.State = runtimeapi.PodSandboxState_SANDBOX_NOTREADY
	return sandbox
}

func readyStatus(ip string, additional ...string) *runtimeapi.PodSandboxStatus {
	network := &runtimeapi.PodSandboxNetworkStatus{Ip: ip}
	for _, extra := range additional {
		network.AdditionalIps = append(network.AdditionalIps, &runtimeapi.PodIP{Ip: extra})
	}
	return &runtimeapi.PodSandboxStatus{State: runtimeapi.PodSandboxState_SANDBOX_READY, Network: network}
}

// notReadyStatus still has an IP: a runtime can report the network of a
// sandbox that has stopped.
func notReadyStatus(ip string) *runtimeapi.PodSandboxStatus {
	status := readyStatus(ip)
	status.State = runtimeapi.PodSandboxState_SANDBOX_NOTREADY
	return status
}

func TestResolvePodSandboxIPs(t *testing.T) {
	t.Run("primary IP first, then the additional ones", func(t *testing.T) {
		svc := &fakePodSandboxService{
			sandboxes: []*runtimeapi.PodSandbox{readySandbox("sandbox", "pod-uid", 0, 1)},
			statuses:  map[string]*runtimeapi.PodSandboxStatus{"sandbox": readyStatus("10.0.0.7", "fd00::7")},
		}

		ips, err := resolvePodSandboxIPs(context.Background(), svc, "pod-uid")

		require.NoError(t, err)
		assert.Equal(t, []string{"10.0.0.7", "fd00::7"}, ips)
		assert.Nil(t, svc.filter.GetState(), "sandboxes in every state are listed")
		assert.Equal(t, map[string]string{kubeletPodUIDLabel: "pod-uid"}, svc.filter.GetLabelSelector())
	})

	t.Run("the newest sandbox when it is ready", func(t *testing.T) {
		svc := &fakePodSandboxService{
			sandboxes: []*runtimeapi.PodSandbox{
				readySandbox("old", "pod-uid", 0, 1),
				readySandbox("new", "pod-uid", 1, 2),
			},
			statuses: map[string]*runtimeapi.PodSandboxStatus{
				"old": readyStatus("10.0.0.6"),
				"new": readyStatus("10.0.0.7"),
			},
		}

		ips, err := resolvePodSandboxIPs(context.Background(), svc, "pod-uid")

		require.NoError(t, err)
		assert.Equal(t, []string{"10.0.0.7"}, ips)
		assert.Equal(t, []string{"new"}, svc.statusIDs)
	})

	// kubelet publishes no IP until the newest sandbox is ready, so the
	// restore waits rather than take the older sandbox's IP.
	t.Run("an older ready sandbox and a newer one that is not ready", func(t *testing.T) {
		svc := &fakePodSandboxService{
			sandboxes: []*runtimeapi.PodSandbox{
				readySandbox("old", "pod-uid", 0, 1),
				notReadySandbox("new", "pod-uid", 1, 2),
			},
			statuses: map[string]*runtimeapi.PodSandboxStatus{
				"old": readyStatus("10.0.0.6"),
				"new": notReadyStatus("10.0.0.7"),
			},
		}

		ips, err := resolvePodSandboxIPs(context.Background(), svc, "pod-uid")

		require.Error(t, err)
		assert.Empty(t, ips)
		assert.Equal(t, []string{"new"}, svc.statusIDs, "the older sandbox is never read")
	})

	t.Run("only a sandbox that is not ready", func(t *testing.T) {
		svc := &fakePodSandboxService{
			sandboxes: []*runtimeapi.PodSandbox{notReadySandbox("sandbox", "pod-uid", 0, 1)},
			statuses:  map[string]*runtimeapi.PodSandboxStatus{"sandbox": notReadyStatus("10.0.0.7")},
		}

		ips, err := resolvePodSandboxIPs(context.Background(), svc, "pod-uid")

		require.Error(t, err)
		assert.Empty(t, ips)
	})

	// The sandbox stopped between the two calls. kubelet goes by the status.
	t.Run("a sandbox listed as ready whose status is not ready", func(t *testing.T) {
		svc := &fakePodSandboxService{
			sandboxes: []*runtimeapi.PodSandbox{readySandbox("sandbox", "pod-uid", 0, 1)},
			statuses:  map[string]*runtimeapi.PodSandboxStatus{"sandbox": notReadyStatus("10.0.0.7")},
		}

		ips, err := resolvePodSandboxIPs(context.Background(), svc, "pod-uid")

		require.Error(t, err)
		assert.Empty(t, ips)
	})

	// A runtime that ignored the filter would otherwise hand over another
	// Pod's IP.
	t.Run("another Pod's newer sandbox", func(t *testing.T) {
		svc := &fakePodSandboxService{
			sandboxes: []*runtimeapi.PodSandbox{
				readySandbox("other-pod", "other-uid", 3, 5),
				readySandbox("sandbox", "pod-uid", 0, 1),
			},
			statuses: map[string]*runtimeapi.PodSandboxStatus{
				"other-pod": readyStatus("10.0.0.8"),
				"sandbox":   readyStatus("10.0.0.7"),
			},
		}

		ips, err := resolvePodSandboxIPs(context.Background(), svc, "pod-uid")

		require.NoError(t, err)
		assert.Equal(t, []string{"10.0.0.7"}, ips)
		assert.Equal(t, []string{"sandbox"}, svc.statusIDs)
	})

	t.Run("only another Pod's sandbox", func(t *testing.T) {
		svc := &fakePodSandboxService{
			sandboxes: []*runtimeapi.PodSandbox{readySandbox("other-pod", "other-uid", 0, 1)},
			statuses:  map[string]*runtimeapi.PodSandboxStatus{"other-pod": readyStatus("10.0.0.8")},
		}

		_, err := resolvePodSandboxIPs(context.Background(), svc, "pod-uid")

		require.Error(t, err)
		assert.Empty(t, svc.statusIDs)
	})

	t.Run("host network", func(t *testing.T) {
		svc := &fakePodSandboxService{
			sandboxes: []*runtimeapi.PodSandbox{readySandbox("sandbox", "pod-uid", 0, 1)},
			statuses: map[string]*runtimeapi.PodSandboxStatus{
				"sandbox": {State: runtimeapi.PodSandboxState_SANDBOX_READY},
			},
		}

		ips, err := resolvePodSandboxIPs(context.Background(), svc, "pod-uid")

		require.NoError(t, err)
		assert.Empty(t, ips)
	})

	for name, svc := range map[string]*fakePodSandboxService{
		"no sandbox":   {},
		"list fails":   {listErr: errors.New("unavailable")},
		"status fails": {sandboxes: []*runtimeapi.PodSandbox{readySandbox("sandbox", "pod-uid", 0, 1)}, statusErr: errors.New("not found")},
		"no status":    {sandboxes: []*runtimeapi.PodSandbox{readySandbox("sandbox", "pod-uid", 0, 1)}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := resolvePodSandboxIPs(context.Background(), svc, "pod-uid")

			require.Error(t, err)
		})
	}

	t.Run("no Pod UID", func(t *testing.T) {
		svc := &fakePodSandboxService{}

		_, err := resolvePodSandboxIPs(context.Background(), svc, "")

		require.Error(t, err)
		assert.Nil(t, svc.filter, "an empty UID would select every sandbox")
	})
}

func TestNewestSandboxFirst(t *testing.T) {
	withoutMetadata := func(id string, createdAt int64) *runtimeapi.PodSandbox {
		return &runtimeapi.PodSandbox{Id: id, CreatedAt: createdAt}
	}
	for name, tc := range map[string]struct {
		a, b *runtimeapi.PodSandbox
		want string
	}{
		"the higher attempt, though created earlier": {
			a: readySandbox("retry", "pod-uid", 1, 1), b: readySandbox("first", "pod-uid", 0, 2), want: "retry",
		},
		"the same attempt: the later creation time": {
			a: readySandbox("earlier", "pod-uid", 1, 1), b: readySandbox("later", "pod-uid", 1, 2), want: "later",
		},
		"the same attempt and creation time: the lower ID": {
			a: readySandbox("b", "pod-uid", 1, 1), b: readySandbox("a", "pod-uid", 1, 1), want: "a",
		},
		"without metadata: the later creation time": {
			a: withoutMetadata("earlier", 1), b: withoutMetadata("later", 2), want: "later",
		},
	} {
		t.Run(name, func(t *testing.T) {
			for _, sandboxes := range [][]*runtimeapi.PodSandbox{{tc.a, tc.b}, {tc.b, tc.a}} {
				assert.Equal(t, tc.want, slices.MinFunc(sandboxes, newestSandboxFirst).GetId())
			}
		})
	}
}
