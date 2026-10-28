// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

var probeStart = time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)

// probeHarness drives reconcileCapture against a work order with a
// checkpoint readiness probe, with a settable clock and scripted results.
type probeHarness struct {
	t       *testing.T
	w       *NodeController
	fc      *fakeCheckpointer
	content *snapshotv1alpha1.PodSnapshotContent

	mu        sync.Mutex
	now       time.Time
	results   []error // one per check; the last one repeats
	checks    int
	requeues  []time.Duration
	filePID   int
	filePath  string
	httpIP    string
	httpPort  int32
	httpPath  string
	httpTO    time.Duration
	httpCalls int
}

func newProbeHarness(t *testing.T, probe snapshotv1alpha1.CheckpointReadinessProbe, mutatePod func(*corev1.Pod)) *probeHarness {
	t.Helper()
	content := makeWorkOrder("podsnapshotcontent-x", "node-a", "x")
	content.Spec.Containers = []snapshotv1alpha1.ContainerSnapshotConfig{{Name: "main", CheckpointReadinessProbe: &probe}}
	pod := makeSourcePod()
	pod.Spec.Containers = []corev1.Container{{Name: "main", Ports: []corev1.ContainerPort{{Name: "hooks", ContainerPort: 8000}}}}
	pod.Status.PodIP = "10.0.0.5"
	pod.Status.ContainerStatuses[0].Ready = false // the probe decides, not Ready
	pod.Status.ContainerStatuses[0].State.Running = &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(probeStart)}
	if mutatePod != nil {
		mutatePod(pod)
	}
	h := &probeHarness{t: t, fc: &fakeCheckpointer{}, content: content, now: probeStart}
	h.w = makeNodeController(t, h.fc, content, pod)
	h.w.runtime = &fakeRuntime{resolveContainerPID: 7}
	h.w.nowFn = func() time.Time {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.now
	}
	h.w.requeueCaptureFn = func(_ string, after time.Duration) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.requeues = append(h.requeues, after)
	}
	h.w.probeFileFn = func(pid int, path string) (bool, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.filePID, h.filePath = pid, path
		err := h.next()
		return err == nil, nil
	}
	h.w.probeHTTPFn = func(_ context.Context, ip string, port int32, path string, timeout time.Duration) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.httpIP, h.httpPort, h.httpPath, h.httpTO = ip, port, path, timeout
		h.httpCalls++
		return h.next()
	}
	return h
}

func (h *probeHarness) next() error {
	h.checks++
	if len(h.results) == 0 {
		return nil
	}
	err := h.results[0]
	if len(h.results) > 1 {
		h.results = h.results[1:]
	}
	return err
}

func (h *probeHarness) reconcile() {
	h.t.Helper()
	require.NoError(h.t, h.w.reconcileCapture(context.Background(), h.content.Name))
}

func (h *probeHarness) advance(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.now = h.now.Add(d)
}

func (h *probeHarness) lastRequeue() time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	require.NotEmpty(h.t, h.requeues, "no requeue was scheduled")
	return h.requeues[len(h.requeues)-1]
}

func (h *probeHarness) checkCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.checks
}

func (h *probeHarness) updatePod(mutate func(*corev1.Pod)) {
	h.t.Helper()
	pod := getPod(h.t, h.w, "inference", "worker-0")
	mutate(pod)
	require.NoError(h.t, h.w.client.Update(context.Background(), pod))
}

func fileProbe(path string, period, threshold, delay int32) snapshotv1alpha1.CheckpointReadinessProbe {
	return snapshotv1alpha1.CheckpointReadinessProbe{
		CheckpointReadinessProbeHandler: snapshotv1alpha1.CheckpointReadinessProbeHandler{
			File: &snapshotv1alpha1.ControlFileAction{Path: path},
		},
		InitialDelaySeconds: delay, PeriodSeconds: period, TimeoutSeconds: 1, SuccessThreshold: threshold,
	}
}

var errNotYet = errors.New("not yet")

func TestCheckpointProbe_DecidesInsteadOfReady(t *testing.T) {
	h := newProbeHarness(t, fileProbe("/tmp/ready", 1, 1, 0), nil)

	h.reconcile()

	require.True(t, h.fc.wasCalled(), "a passing probe checkpoints a container that is not Ready")
	assert.Equal(t, 7, h.filePID, "the file is read in the source container")
	assert.Equal(t, "/tmp/ready", h.filePath)
}

func TestCheckpointProbe_WaitsForTheThreshold(t *testing.T) {
	h := newProbeHarness(t, fileProbe("/tmp/ready", 2, 3, 0), nil)

	for i := range 2 {
		h.reconcile()
		require.False(t, h.fc.wasCalled(), "checkpointed after %d successes, want 3", i+1)
		assert.Equal(t, 2*time.Second, h.lastRequeue())
		h.advance(2 * time.Second)
	}
	h.reconcile()
	assert.True(t, h.fc.wasCalled())
}

func TestCheckpointProbe_AFailureResetsTheCount(t *testing.T) {
	h := newProbeHarness(t, fileProbe("/tmp/ready", 1, 2, 0), nil)
	h.results = []error{nil, errNotYet, nil, nil}

	for range 3 {
		h.reconcile()
		require.False(t, h.fc.wasCalled(), "the failure must reset the count")
		h.advance(time.Second)
	}
	h.reconcile()
	assert.True(t, h.fc.wasCalled())
	assert.Equal(t, 4, h.checkCount())
}

func TestCheckpointProbe_IsPacedByThePeriod(t *testing.T) {
	h := newProbeHarness(t, fileProbe("/tmp/ready", 5, 2, 0), nil)

	h.reconcile()
	h.advance(time.Second)
	h.reconcile() // a Pod event between checks
	assert.Equal(t, 1, h.checkCount(), "a reconcile before the period must not check")
	assert.Equal(t, 4*time.Second, h.lastRequeue(), "the next check is still due at the period")
	assert.False(t, h.fc.wasCalled())
}

func TestCheckpointProbe_WaitsForTheInitialDelay(t *testing.T) {
	h := newProbeHarness(t, fileProbe("/tmp/ready", 1, 1, 10), nil)
	h.advance(3 * time.Second)

	h.reconcile()
	assert.Equal(t, 0, h.checkCount())
	assert.Equal(t, 7*time.Second, h.lastRequeue(), "the delay counts from the container's start")

	h.advance(7 * time.Second)
	h.reconcile()
	assert.True(t, h.fc.wasCalled())
}

func TestCheckpointProbe_AContainerRestartResetsTheCount(t *testing.T) {
	h := newProbeHarness(t, fileProbe("/tmp/ready", 1, 2, 5), nil)
	h.advance(5 * time.Second)
	h.reconcile()
	require.Equal(t, 1, h.checkCount())

	restartedAt := probeStart.Add(20 * time.Second)
	h.updatePod(func(pod *corev1.Pod) {
		pod.Status.ContainerStatuses[0].RestartCount = 1
		pod.Status.ContainerStatuses[0].ContainerID = "containerd://def456"
		pod.Status.ContainerStatuses[0].State.Running.StartedAt = metav1.NewTime(restartedAt)
	})
	h.advance(16 * time.Second) // 1s after the restart
	h.reconcile()
	assert.Equal(t, 1, h.checkCount(), "the restarted container gets its initial delay again")
	assert.Equal(t, 4*time.Second, h.lastRequeue())

	h.advance(4 * time.Second)
	h.reconcile()
	assert.False(t, h.fc.wasCalled(), "the success before the restart must not count")
	h.advance(time.Second)
	h.reconcile()
	assert.True(t, h.fc.wasCalled())
}

func TestCheckpointProbe_WaitsForARunningContainer(t *testing.T) {
	h := newProbeHarness(t, fileProbe("/tmp/ready", 1, 1, 0), func(pod *corev1.Pod) {
		pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}
	})

	h.reconcile()
	assert.Equal(t, 0, h.checkCount())
	assert.Empty(t, h.requeues, "a start is a Pod event; no timer is needed")
	assert.False(t, h.fc.wasCalled())
}

func TestCheckpointProbe_HTTPGoesToThePodIP(t *testing.T) {
	for _, tc := range []struct {
		name string
		port intstr.IntOrString
	}{
		{"numeric port", intstr.FromInt32(8000)},
		{"named port", intstr.FromString("hooks")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := snapshotv1alpha1.CheckpointReadinessProbe{
				CheckpointReadinessProbeHandler: snapshotv1alpha1.CheckpointReadinessProbeHandler{
					HTTPGet: &snapshotv1alpha1.HTTPGetAction{Path: "/checkpoint/status", Port: tc.port},
				},
				PeriodSeconds: 1, TimeoutSeconds: 3, SuccessThreshold: 1,
			}
			h := newProbeHarness(t, probe, nil)

			h.reconcile()
			assert.True(t, h.fc.wasCalled())
			assert.Equal(t, "10.0.0.5", h.httpIP)
			assert.Equal(t, int32(8000), h.httpPort)
			assert.Equal(t, "/checkpoint/status", h.httpPath)
			assert.Equal(t, 3*time.Second, h.httpTO)
		})
	}
}

func TestCheckpointProbe_AnnouncesTheWaitOnce(t *testing.T) {
	h := newProbeHarness(t, fileProbe("/tmp/ready", 1, 3, 0), nil)

	for range 2 {
		h.reconcile()
		h.advance(time.Second)
	}
	events, err := h.w.clientset.CoreV1().Events("inference").List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	var waiting int
	for _, e := range events.Items {
		if e.Reason == reasonWaitingForCheckpointReadiness {
			waiting++
		}
	}
	assert.Equal(t, 1, waiting)
}

func TestCheckpointProbe_RefusedTargetsFailTheWorkOrder(t *testing.T) {
	httpProbe := snapshotv1alpha1.CheckpointReadinessProbe{
		CheckpointReadinessProbeHandler: snapshotv1alpha1.CheckpointReadinessProbeHandler{
			HTTPGet: &snapshotv1alpha1.HTTPGetAction{Path: "/status", Port: intstr.FromInt32(8000)},
		},
		PeriodSeconds: 1, TimeoutSeconds: 1, SuccessThreshold: 1,
	}
	unknownPort := *httpProbe.DeepCopy()
	unknownPort.HTTPGet.Port = intstr.FromString("metrics")
	for _, tc := range []struct {
		name      string
		probe     snapshotv1alpha1.CheckpointReadinessProbe
		mutatePod func(*corev1.Pod)
	}{
		{"file on a PVC", fileProbe("/data/ready", 1, 1, 0), func(pod *corev1.Pod) {
			pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "data", MountPath: "/data"}}
			pod.Spec.Volumes = []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"},
			}}}
		}},
		{"HTTP on the host network", httpProbe, func(pod *corev1.Pod) { pod.Spec.HostNetwork = true }},
		{"HTTP on a port name the container doesn't have", unknownPort, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newProbeHarness(t, tc.probe, tc.mutatePod)

			h.reconcile()

			assert.False(t, h.fc.wasCalled())
			assert.Equal(t, 0, h.checkCount(), "a refused probe is never run")
			failed := meta.FindStatusCondition(getContent(t, h.w, h.content.Name).Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed)
			require.NotNil(t, failed)
			assert.Equal(t, reasonInvalidCheckpointReadinessProbe, failed.Reason)
		})
	}
}

func TestCheckpointProbe_StateIsDroppedWithTheWorkOrder(t *testing.T) {
	h := newProbeHarness(t, fileProbe("/tmp/ready", 1, 3, 0), nil)
	h.reconcile()
	_, tracked := h.w.checkpointProbes.Load(h.content.Name)
	require.True(t, tracked)

	require.NoError(t, h.w.contentIndexer.Delete(h.content))
	h.reconcile()
	_, tracked = h.w.checkpointProbes.Load(h.content.Name)
	assert.False(t, tracked)
}
