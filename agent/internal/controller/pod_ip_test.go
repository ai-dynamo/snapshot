// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"
	corev1listers "k8s.io/client-go/listers/core/v1"
	clientgotesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	"github.com/ai-dynamo/snapshot/agent/internal/executor"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/ai-dynamo/snapshot/api/podcontract"
)

func TestPrimaryPodIP(t *testing.T) {
	for name, tc := range map[string]struct {
		ips     []string
		hostIP  string
		want    string
		wantErr bool
	}{
		"one IPv4":                             {ips: []string{"10.0.0.7"}, want: "10.0.0.7"},
		"one IPv6":                             {ips: []string{"fd00::7"}, want: "fd00::7"},
		"the first of one family":              {ips: []string{"10.0.0.7", "10.0.0.8"}, want: "10.0.0.7"},
		"dual-stack on an IPv4 node":           {ips: []string{"10.0.0.7", "fd00::7"}, hostIP: "192.168.0.1", want: "10.0.0.7"},
		"dual-stack on an IPv6 node":           {ips: []string{"10.0.0.7", "fd00::7"}, hostIP: "fd00::1", want: "fd00::7"},
		"the node's family before the primary": {ips: []string{"fd00::7", "10.0.0.7"}, hostIP: "192.168.0.1", want: "10.0.0.7"},
		"an IPv4-mapped address is IPv4":       {ips: []string{"::ffff:10.0.0.7", "fd00::7"}, hostIP: "192.168.0.1", want: "10.0.0.7"},
		"an IPv6 address in canonical form":    {ips: []string{"FD00:0:0::7"}, want: "fd00::7"},
		"an IPv6 address with a zone":          {ips: []string{"fe80::7%eth0"}, wantErr: true},
		"dual-stack without a host IP":         {ips: []string{"10.0.0.7", "fd00::7"}, wantErr: true},
		"no IP":                                {wantErr: true},
		"an IP that does not parse":            {ips: []string{"10.0.0.7", "not-an-ip"}, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := primaryPodIP(tc.ips, tc.hostIP)

			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// tcpRestore is a restore-from Pod with a Ready snapshot and its artifact,
// on an agent that restores established TCP connections.
func tcpRestore(t *testing.T, runtime *fakeRuntime) (*NodeController, *corev1.Pod) {
	t.Helper()
	pod := restorePod(map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"})
	snapshot, content := readySnapshotObjects()
	w := makeTestController(t, pod, snapshot, content)
	w.config.CRIU.TcpEstablished = true
	w.runtime = runtime
	writeTestArtifact(t, w.config.Storage.BasePath, string(content.UID), &types.CheckpointManifest{
		Artifact: types.ArtifactManifest{ContentUID: string(content.UID), ContainerName: "main"},
	})
	return w, pod
}

// targetPodIPs records the TargetPodIP of each restore request.
func targetPodIPs(w *NodeController) *[]string {
	var mu sync.Mutex
	var ips []string
	w.restoreFn = func(_ context.Context, _ snapshotruntime.Runtime, _ logr.Logger, req executor.RestoreRequest, _ executor.RestoreMounter) (executor.RestoreResult, error) {
		mu.Lock()
		defer mu.Unlock()
		ips = append(ips, req.TargetPodIP)
		return executor.RestoreResult{PlaceholderHostPID: 4242, RestoredPID: 1}, nil
	}
	return &ips
}

// noRestoreExpected fails the test if any destination restores.
func noRestoreExpected(t *testing.T) func(context.Context, snapshotruntime.Runtime, logr.Logger, executor.RestoreRequest, executor.RestoreMounter) (executor.RestoreResult, error) {
	return func(context.Context, snapshotruntime.Runtime, logr.Logger, executor.RestoreRequest, executor.RestoreMounter) (executor.RestoreResult, error) {
		t.Error("no destination may restore")
		return executor.RestoreResult{}, nil
	}
}

// kubelet publishes the Pod IP only at its next relist; the sandbox already
// has it.
func TestPreflightRestoreUsesTheSandboxIPBeforeTheAPIHasIt(t *testing.T) {
	runtime := &fakeRuntime{sandboxIPs: []string{"10.0.0.7"}}
	w, pod := tcpRestore(t, runtime)
	targets := targetPodIPs(w)

	plan, err := w.preflightRestore(context.Background(), pod)

	require.NoError(t, err)
	assert.Equal(t, "10.0.0.7", plan.podIP)
	assert.Equal(t, []string{string(pod.UID)}, runtime.sandboxPodUIDs)

	requeue := w.reconcileRestorePod(context.Background(), pod)

	assert.False(t, requeue)
	assert.Equal(t, []string{"10.0.0.7"}, *targets)
	assert.Contains(t, string(lastPodStatusApply(t, w).GetPatch()), `"reason":"RestoreSucceeded"`)
}

func TestPreflightRestorePrefersTheAPIPodIP(t *testing.T) {
	runtime := &fakeRuntime{sandboxIPs: []string{"10.0.0.7"}}
	w, pod := tcpRestore(t, runtime)
	pod.Status.PodIP = "10.0.0.9"
	targets := targetPodIPs(w)

	plan, err := w.preflightRestore(context.Background(), pod)

	require.NoError(t, err)
	assert.Empty(t, plan.podIP)
	assert.Empty(t, runtime.sandboxPodUIDs, "the runtime is not asked when the API has the IP")

	w.reconcileRestorePod(context.Background(), pod)

	assert.Equal(t, []string{"10.0.0.9"}, *targets)
}

func TestPreflightRestoreWaitsWhileTheSandboxHasNoUsableIP(t *testing.T) {
	for name, tc := range map[string]struct {
		sandboxIPs []string
		hostIP     string
	}{
		"the runtime call fails":       {},
		"dual-stack without a host IP": {sandboxIPs: []string{"10.0.0.7", "fd00::7"}},
		"an IP that does not parse":    {sandboxIPs: []string{"not-an-ip"}},
	} {
		t.Run(name, func(t *testing.T) {
			w, pod := tcpRestore(t, &fakeRuntime{sandboxIPs: tc.sandboxIPs})
			pod.Status.HostIP = tc.hostIP
			w.restoreFn = noRestoreExpected(t)

			plan, err := w.preflightRestore(context.Background(), pod)

			assert.Nil(t, plan)
			assert.Equal(t, "PodIPPending", pendingRestoreReason(t, err))
			assert.EqualError(t, err, "Waiting for restore Pod inference/restore-worker to receive an IP address")
		})
	}
}

func TestPreflightRestoreUsesTheSandboxIPOfTheNodesFamily(t *testing.T) {
	w, pod := tcpRestore(t, &fakeRuntime{sandboxIPs: []string{"fd00::7", "10.0.0.7"}})
	pod.Status.HostIP = "192.168.0.1"

	plan, err := w.preflightRestore(context.Background(), pod)

	require.NoError(t, err)
	assert.Equal(t, "10.0.0.7", plan.podIP)
}

func TestPreflightRestoreWithoutTCPRestoreNeedsNoPodIP(t *testing.T) {
	runtime := &fakeRuntime{sandboxIPs: []string{"10.0.0.7"}}
	w, pod := tcpRestore(t, runtime)
	w.config.CRIU.TcpEstablished = false
	targets := targetPodIPs(w)

	plan, err := w.preflightRestore(context.Background(), pod)

	require.NoError(t, err)
	assert.Empty(t, plan.podIP)
	assert.Empty(t, runtime.sandboxPodUIDs)

	w.reconcileRestorePod(context.Background(), pod)

	assert.Equal(t, []string{""}, *targets)
}

// sandboxReadyAfter is a runtime whose Pod sandbox has its IP only after
// failures lookups, as when a pass runs while the sandbox's network is being
// set up.
type sandboxReadyAfter struct {
	*fakeRuntime
	failures int

	mu    sync.Mutex
	calls int
}

func (r *sandboxReadyAfter) ResolvePodSandboxIPs(context.Context, string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.calls <= r.failures {
		return nil, errors.New("pod has no ready sandbox")
	}
	return []string{"10.0.0.7"}, nil
}

func (r *sandboxReadyAfter) lookups() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// waitingTCPRestore is tcpRestore on an agent that waits for the Pod's IP, with
// the Pod in the restore informer's cache, as Run serves it.
func waitingTCPRestore(t *testing.T, runtime *sandboxReadyAfter, timeout time.Duration) (*NodeController, *corev1.Pod, cache.Indexer) {
	t.Helper()
	w, pod := tcpRestore(t, &fakeRuntime{})
	runtime.fakeRuntime = &fakeRuntime{}
	w.runtime = runtime
	w.podIPWaitTimeout = timeout
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	require.NoError(t, indexer.Add(pod))
	w.restorePodLister = corev1listers.NewPodLister(indexer)
	return w, pod, indexer
}

func podIPPendingApplied(w *NodeController) bool {
	for _, action := range w.clientset.(*fake.Clientset).Actions() {
		patch, ok := action.(clientgotesting.PatchAction)
		if ok && patch.GetSubresource() == "status" && strings.Contains(string(patch.GetPatch()), "PodIPPending") {
			return true
		}
	}
	return false
}

func TestNewDefaultControllerWaitsForThePodIP(t *testing.T) {
	w := newDefaultController(&types.AgentConfig{}, fake.NewClientset(), nil, nil, &fakeRuntime{}, noopInjector{}, testr.New(t))
	t.Cleanup(w.restoreQueue.ShutDown)

	assert.Equal(t, restorePodIPWaitTimeout, w.podIPWaitTimeout)
}

// Without NRI, a Pod's first pass can run before its sandbox has a network.
// The pass waits for it instead of leaving the Pod pending until kubelet's
// next status update or the safety requeue.
func TestRestoreWaitsWithinItsPassForTheSandboxIP(t *testing.T) {
	runtime := &sandboxReadyAfter{failures: 3}
	w, pod, _ := waitingTCPRestore(t, runtime, restorePodIPWaitTimeout)
	targets := targetPodIPs(w)
	start := time.Now()

	requeue := w.reconcileRestorePod(context.Background(), pod)

	assert.False(t, requeue)
	assert.Equal(t, []string{"10.0.0.7"}, *targets)
	assert.Equal(t, 4, runtime.lookups())
	assert.Less(t, time.Since(start), 10*restorePodIPPollInterval)
	assert.False(t, podIPPendingApplied(w), "the wait writes no pending condition")
	assert.Empty(t, eventsForReason(w.clientset.(*fake.Clientset), "PodIPPending"))
	_, waiting := w.podIPWaits.Load(string(pod.UID))
	assert.False(t, waiting)
}

// A sandbox that never gets an IP costs one wait per Pod, not one per pass.
func TestRestoreStopsWaitingForThePodIPAfterTheBound(t *testing.T) {
	runtime := &sandboxReadyAfter{failures: math.MaxInt}
	w, pod, _ := waitingTCPRestore(t, runtime, 3*restorePodIPPollInterval)
	w.restoreFn = noRestoreExpected(t)
	start := time.Now()

	requeue := w.reconcileRestorePod(context.Background(), pod)

	assert.True(t, requeue)
	assert.GreaterOrEqual(t, time.Since(start), 3*restorePodIPPollInterval)
	assert.True(t, podIPPendingApplied(w))
	waited := runtime.lookups()
	assert.Greater(t, waited, 1)

	start = time.Now()
	requeue = w.reconcileRestorePod(context.Background(), pod)

	assert.True(t, requeue)
	assert.Equal(t, waited+1, runtime.lookups(), "after the bound a pass asks once")
	assert.Less(t, time.Since(start), restorePodIPPollInterval)

	w.forgetRestorePod(pod)
	_, waiting := w.podIPWaits.Load(string(pod.UID))
	assert.False(t, waiting, "a deleted Pod's wait is forgotten")
}

// In a vCluster tenant no sandbox matches the Pod; the API's IP ends the wait.
func TestRestoreTakesTheAPIPodIPThatArrivesWhileItWaits(t *testing.T) {
	runtime := &sandboxReadyAfter{failures: math.MaxInt}
	w, pod, indexer := waitingTCPRestore(t, runtime, restorePodIPWaitTimeout)
	targets := targetPodIPs(w)
	go func() {
		time.Sleep(2 * restorePodIPPollInterval)
		published := pod.DeepCopy()
		published.Status.PodIP = "10.0.0.9"
		assert.NoError(t, indexer.Update(published))
	}()
	start := time.Now()

	requeue := w.reconcileRestorePod(context.Background(), pod)

	assert.False(t, requeue)
	assert.Equal(t, []string{"10.0.0.9"}, *targets)
	assert.Less(t, time.Since(start), restorePodIPWaitTimeout/2)
}

func TestRestoreStopsWaitingForThePodIP(t *testing.T) {
	for name, stop := range map[string]func(cancel context.CancelFunc, indexer cache.Indexer, pod *corev1.Pod){
		"the agent stops":     func(cancel context.CancelFunc, _ cache.Indexer, _ *corev1.Pod) { cancel() },
		"the Pod is deleted":  func(_ context.CancelFunc, indexer cache.Indexer, pod *corev1.Pod) { _ = indexer.Delete(pod) },
		"the Pod is replaced": replaceCachedPod,
	} {
		t.Run(name, func(t *testing.T) {
			runtime := &sandboxReadyAfter{failures: math.MaxInt}
			w, pod, indexer := waitingTCPRestore(t, runtime, restorePodIPWaitTimeout)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				time.Sleep(2 * restorePodIPPollInterval)
				stop(cancel, indexer, pod)
			}()
			start := time.Now()

			_, err := w.preflightRestore(ctx, pod)

			assert.Equal(t, "PodIPPending", pendingRestoreReason(t, err))
			assert.Less(t, time.Since(start), restorePodIPWaitTimeout/2)
		})
	}
}

func replaceCachedPod(_ context.CancelFunc, indexer cache.Indexer, pod *corev1.Pod) {
	replacement := pod.DeepCopy()
	replacement.UID = "replacement-uid"
	_ = indexer.Update(replacement)
}

func TestHostNetworkRestoreDoesNotAskTheSandbox(t *testing.T) {
	runtime := &fakeRuntime{sandboxIPs: []string{"10.0.0.7"}}
	w, pod := tcpRestore(t, runtime)
	pod.Spec.HostNetwork = true

	_, err := w.preflightRestore(context.Background(), pod)

	assert.Equal(t, "PodIPPending", pendingRestoreReason(t, err))
	assert.Empty(t, runtime.sandboxPodUIDs)
}
