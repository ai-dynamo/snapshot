//go:build integration

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package targetstatus

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

// The suite runs against the envtest release that KUBEBUILDER_ASSETS names;
// make test-integration runs it on the oldest supported Kubernetes and the
// newest.
func TestApplyAgainstAnAPIServer(t *testing.T) {
	env := &envtest.Environment{}
	restConfig, err := env.Start()
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, env.Stop()) })
	client, err := kubernetes.NewForConfig(restConfig)
	require.NoError(t, err)
	version, err := client.Discovery().ServerVersion()
	require.NoError(t, err)
	t.Logf("API server %s", version.GitVersion)

	t.Run("keeps the kubelet's conditions", func(t *testing.T) {
		pod := createPod(t, client)
		kubelet(t, client, pod, corev1.ConditionFalse)

		pod = mustApply(t, client, read(t, client, pod), pod.UID, StartedFieldManager, started())
		pod = mustApply(t, client, pod, pod.UID, RestoreFieldManager, restored(corev1.ConditionFalse, podcontract.RestoreReasonInProgress))
		kubelet(t, client, pod, corev1.ConditionTrue)
		mustApply(t, client, pod, pod.UID, RestoreFieldManager, restored(corev1.ConditionTrue, podcontract.RestoreReasonSucceeded))

		got := read(t, client, pod)
		for _, kubeletType := range []corev1.PodConditionType{corev1.PodScheduled, corev1.PodReady, corev1.ContainersReady} {
			c := condition(got, kubeletType)
			require.NotNil(t, c, "the kubelet's %s is gone", kubeletType)
			if kubeletType != corev1.PodScheduled {
				assert.Equal(t, corev1.ConditionTrue, c.Status, "the kubelet's last write of %s is kept", kubeletType)
			}
		}
		assert.Equal(t, corev1.ConditionTrue, condition(got, podcontract.RestoredCondition).Status)
		assert.NotNil(t, condition(got, podcontract.RestoreStartedCondition))
		assert.Equal(t, []string{"kubelet"}, owners(t, got, corev1.PodReady))
	})

	t.Run("retries a write that conflicts", func(t *testing.T) {
		pod := createPod(t, client)
		stale := read(t, client, pod)
		kubelet(t, client, pod, corev1.ConditionFalse)

		// The fence is the API server's: the stale resourceVersion alone is
		// refused.
		_, err := client.CoreV1().Pods(stale.Namespace).ApplyStatus(context.Background(),
			configuration(stale, RestoreFieldManager, []corev1.PodCondition{pending()}),
			metav1.ApplyOptions{FieldManager: RestoreFieldManager, Force: true})
		require.True(t, apierrors.IsConflict(err), "a stale resourceVersion must conflict, got %v", err)

		counting := &countingClient{Interface: client}
		written := mustApply(t, counting, stale, stale.UID, RestoreFieldManager, pending())
		assert.Equal(t, int32(1), counting.gets.Load(), "the conflict is followed by one read")
		assert.NotEqual(t, stale.ResourceVersion, written.ResourceVersion)
		assert.Equal(t, "GroupPending", condition(read(t, client, pod), podcontract.RestoredCondition).Reason)
	})

	t.Run("refuses a Pod replaced while a write is outstanding", func(t *testing.T) {
		original := read(t, client, createPod(t, client))
		replacement := replace(t, client, original)

		// The fence is the API server's too: the old UID at the new
		// resourceVersion is refused.
		fenced := replacement.DeepCopy()
		fenced.UID = original.UID
		_, err := client.CoreV1().Pods(fenced.Namespace).ApplyStatus(context.Background(),
			configuration(fenced, StartedFieldManager, []corev1.PodCondition{started()}),
			metav1.ApplyOptions{FieldManager: StartedFieldManager, Force: true})
		require.Error(t, err, "the old UID must not land on the replacement")
		t.Logf("an apply with the old UID is refused: %v (conflict %t, invalid %t)", err, apierrors.IsConflict(err), apierrors.IsInvalid(err))

		_, err = Apply(context.Background(), client, original, original.UID, StartedFieldManager, started())
		require.ErrorIs(t, err, ErrReplaced)
		_, err = Apply(context.Background(), client, replacement, original.UID, RestoreFieldManager, pending())
		require.ErrorIs(t, err, ErrReplaced)
		got := read(t, client, replacement)
		assert.Nil(t, condition(got, podcontract.RestoreStartedCondition), "the replacement got the old Pod's record")
		assert.Nil(t, condition(got, podcontract.RestoredCondition), "the replacement got the old Pod's report")

		require.NoError(t, client.CoreV1().Pods(got.Namespace).Delete(context.Background(), got.Name,
			metav1.DeleteOptions{GracePeriodSeconds: ptr.To[int64](0)}))
		_, err = Apply(context.Background(), client, original, original.UID, RestoreFieldManager, pending())
		require.True(t, apierrors.IsNotFound(err), "a deleted Pod reports NotFound, got %v", err)
	})

	t.Run("never removes RestoreStarted", func(t *testing.T) {
		pod := createPod(t, client)
		pod = mustApply(t, client, read(t, client, pod), pod.UID, StartedFieldManager, started())
		recorded := *condition(pod, podcontract.RestoreStartedCondition)
		time.Sleep(time.Second) // so a rewritten lastTransitionTime would differ

		pod = mustApply(t, client, pod, pod.UID, RestoreFieldManager, restored(corev1.ConditionFalse, podcontract.RestoreReasonInProgress))
		pod = mustApply(t, client, pod, pod.UID, RestoreFieldManager,
			restored(corev1.ConditionFalse, podcontract.RestoreReasonFailed), groupFailed("MemberFailed"))
		pod = mustApply(t, client, pod, pod.UID, StartedFieldManager, started())

		got := condition(read(t, client, pod), podcontract.RestoreStartedCondition)
		require.NotNil(t, got)
		assert.Equal(t, corev1.ConditionTrue, got.Status)
		assert.True(t, recorded.LastTransitionTime.Equal(&got.LastTransitionTime), "the record keeps its time")
		assert.Equal(t, []string{StartedFieldManager}, owners(t, read(t, client, pod), podcontract.RestoreStartedCondition))

		_, err := Apply(context.Background(), client, pod, pod.UID, StartedFieldManager,
			corev1.PodCondition{Type: podcontract.RestoreStartedCondition, Status: corev1.ConditionFalse})
		require.ErrorContains(t, err, "never set to False")
		_, err = Apply(context.Background(), client, pod, pod.UID, RestoreFieldManager, started())
		require.ErrorContains(t, err, "written only by "+StartedFieldManager)
	})

	t.Run("keeps the group's reason", func(t *testing.T) {
		pod := createPod(t, client)
		pod = mustApply(t, client, read(t, client, pod), pod.UID, RestoreFieldManager,
			restored(corev1.ConditionFalse, podcontract.RestoreReasonFailed), groupFailed("PodIPChanged"))
		pod = mustApply(t, client, pod, pod.UID, RestoreFieldManager, restored(corev1.ConditionFalse, podcontract.RestoreReasonFailed))
		got := condition(read(t, client, pod), podcontract.GroupRestoreFailedCondition)
		require.NotNil(t, got, "a later report dropped the group's reason")
		assert.Equal(t, "PodIPChanged", got.Reason)
	})
}

var podCount atomic.Int32

func createPod(t *testing.T, client kubernetes.Interface) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("target-%d", podCount.Add(1)), Namespace: metav1.NamespaceDefault},
		Spec: corev1.PodSpec{
			Containers:                   []corev1.Container{{Name: "main", Image: "registry.example/workload:1"}},
			AutomountServiceAccountToken: ptr.To(false),
		},
	}
	created, err := client.CoreV1().Pods(pod.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
	require.NoError(t, err)
	return created
}

// replace deletes pod and creates another Pod under its name.
func replace(t *testing.T, client kubernetes.Interface, pod *corev1.Pod) *corev1.Pod {
	t.Helper()
	require.NoError(t, client.CoreV1().Pods(pod.Namespace).Delete(context.Background(), pod.Name,
		metav1.DeleteOptions{GracePeriodSeconds: ptr.To[int64](0)}))
	require.Eventually(t, func() bool {
		_, err := client.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
		return apierrors.IsNotFound(err)
	}, 10*time.Second, 50*time.Millisecond)
	again := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace}, Spec: pod.Spec}
	created, err := client.CoreV1().Pods(pod.Namespace).Create(context.Background(), again, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NotEqual(t, pod.UID, created.UID)
	return created
}

// kubelet writes the conditions the kubelet owns, as the kubelet does: a
// strategic merge patch of the status, which keeps the conditions of others.
func kubelet(t *testing.T, client kubernetes.Interface, pod *corev1.Pod, ready corev1.ConditionStatus) {
	t.Helper()
	now := metav1.Now()
	patch, err := json.Marshal(map[string]any{"status": map[string]any{"conditions": []corev1.PodCondition{
		{Type: corev1.PodScheduled, Status: corev1.ConditionTrue, LastTransitionTime: now},
		{Type: corev1.PodReady, Status: ready, LastTransitionTime: now},
		{Type: corev1.ContainersReady, Status: ready, LastTransitionTime: now},
	}}})
	require.NoError(t, err)
	_, err = client.CoreV1().Pods(pod.Namespace).Patch(context.Background(), pod.Name, types.StrategicMergePatchType, patch,
		metav1.PatchOptions{FieldManager: "kubelet"}, "status")
	require.NoError(t, err)
}

func mustApply(t *testing.T, client kubernetes.Interface, pod *corev1.Pod, uid types.UID, manager string, conditions ...corev1.PodCondition) *corev1.Pod {
	t.Helper()
	written, err := Apply(context.Background(), client, pod, uid, manager, conditions...)
	require.NoError(t, err)
	return written
}

func read(t *testing.T, client kubernetes.Interface, pod *corev1.Pod) *corev1.Pod {
	t.Helper()
	got, err := client.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	return got
}

func condition(pod *corev1.Pod, conditionType corev1.PodConditionType) *corev1.PodCondition {
	return find(pod, conditionType)
}

// owners returns the field managers that own the condition of a type.
func owners(t *testing.T, pod *corev1.Pod, conditionType corev1.PodConditionType) []string {
	t.Helper()
	key := fmt.Sprintf(`k:{"type":%q}`, conditionType)
	var managers []string
	for _, entry := range pod.ManagedFields {
		if entry.Subresource != "status" || entry.FieldsV1 == nil {
			continue
		}
		var fields struct {
			Status struct {
				Conditions map[string]json.RawMessage `json:"f:conditions"`
			} `json:"f:status"`
		}
		require.NoError(t, json.Unmarshal(entry.FieldsV1.Raw, &fields))
		if _, ok := fields.Status.Conditions[key]; ok {
			managers = append(managers, entry.Manager)
		}
	}
	return managers
}

func started() corev1.PodCondition {
	return corev1.PodCondition{Type: podcontract.RestoreStartedCondition, Status: corev1.ConditionTrue, Reason: "RestoreStarted"}
}

func pending() corev1.PodCondition {
	return restored(corev1.ConditionFalse, "GroupPending")
}

func restored(status corev1.ConditionStatus, reason string) corev1.PodCondition {
	return corev1.PodCondition{Type: podcontract.RestoredCondition, Status: status, Reason: reason, Message: reason}
}

func groupFailed(reason string) corev1.PodCondition {
	return corev1.PodCondition{Type: podcontract.GroupRestoreFailedCondition, Status: corev1.ConditionTrue, Reason: reason, Message: reason}
}

// countingClient counts the Pod reads of an Apply.
type countingClient struct {
	kubernetes.Interface
	gets atomic.Int32
}

func (c *countingClient) CoreV1() typedcorev1.CoreV1Interface {
	return countingCoreV1{CoreV1Interface: c.Interface.CoreV1(), gets: &c.gets}
}

type countingCoreV1 struct {
	typedcorev1.CoreV1Interface
	gets *atomic.Int32
}

func (c countingCoreV1) Pods(namespace string) typedcorev1.PodInterface {
	return countingPods{PodInterface: c.CoreV1Interface.Pods(namespace), gets: c.gets}
}

type countingPods struct {
	typedcorev1.PodInterface
	gets *atomic.Int32
}

func (p countingPods) Get(ctx context.Context, name string, options metav1.GetOptions) (*corev1.Pod, error) {
	p.gets.Add(1)
	return p.PodInterface.Get(ctx, name, options)
}
