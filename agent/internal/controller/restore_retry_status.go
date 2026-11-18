// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	corev1apply "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/util/workqueue"

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

const (
	// retryStatusWriteAttempts bounds the tries of one retry status write
	// before the agent reports it failed. The restore goes on either way.
	retryStatusWriteAttempts = 5
	// retryStatusRetryDelay is the wait before a failed write's first retry,
	// doubled for each one after it.
	retryStatusRetryDelay = time.Second
)

// retryStatusKey names a Pod whose retry status annotation lags its records.
type retryStatusKey struct {
	namespace string
	name      string
	uid       ktypes.UID
}

func newRetryStatusQueue(retryDelay time.Duration) workqueue.TypedRateLimitingInterface[retryStatusKey] {
	return workqueue.NewTypedRateLimitingQueueWithConfig(
		workqueue.NewTypedItemExponentialFailureRateLimiter[retryStatusKey](retryDelay, 30*retryDelay),
		workqueue.TypedRateLimitingQueueConfig[retryStatusKey]{Name: "restore-retry-status"},
	)
}

// queueRetryStatus queues a write of the Pod's retry status annotation. The
// queue holds a Pod once however many of its records change before the write,
// and one worker makes every write, so the destinations of a Pod, which
// restore in parallel, never overwrite each other's entries.
func (w *NodeController) queueRetryStatus(pod *corev1.Pod) {
	w.retryStatusQueue.Add(retryStatusKey{namespace: pod.Namespace, name: pod.Name, uid: pod.UID})
}

// runRetryStatusQueue makes the queued writes until the queue shuts down. A
// write lost to an agent restart leaves the annotation one update behind its
// records, which the restarted agent picks the restore up from.
func (w *NodeController) runRetryStatusQueue(ctx context.Context) {
	for w.processRetryStatusQueueItem(ctx) {
	}
}

func (w *NodeController) processRetryStatusQueueItem(ctx context.Context) bool {
	key, shutdown := w.retryStatusQueue.Get()
	if shutdown {
		return false
	}
	defer w.retryStatusQueue.Done(key)
	err := w.writeRetryStatus(ctx, key)
	switch {
	case err == nil || ctx.Err() != nil:
		w.retryStatusQueue.Forget(key)
	case w.retryStatusQueue.NumRequeues(key) < retryStatusWriteAttempts-1:
		w.retryStatusQueue.AddRateLimited(key)
	default:
		w.retryStatusQueue.Forget(key)
		log := w.log.WithValues("pod", key.namespace+"/"+key.name)
		log.Error(err, "Failed to record the restore retry status")
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: key.namespace, Name: key.name, UID: key.uid},
			Spec:       corev1.PodSpec{NodeName: w.config.NodeName},
		}
		emitPodEvent(ctx, w.clientset, log, pod, snapshotEventComponent, corev1.EventTypeWarning, restoreStatusUpdateFailedReason,
			fmt.Sprintf("Failed to record the restore retry status: %v", err))
	}
	return true
}

// writeRetryStatus applies the Pod's retry status annotation from its records
// as they are when the write runs, so one write carries every update queued
// before it. A Pod with no records left loses the annotation.
func (w *NodeController) writeRetryStatus(ctx context.Context, key retryStatusKey) error {
	status := podcontract.RestoreRetryStatus{}
	prefix := string(key.uid) + "/"
	w.restoreAttemptRecords.Range(func(k, v any) bool {
		if container, ok := strings.CutPrefix(k.(string), prefix); ok {
			status[container] = v.(podcontract.ContainerRetryStatus)
		}
		return true
	})
	value := ""
	if len(status) > 0 {
		var err error
		if value, err = podcontract.FormatRestoreRetryStatus(status); err != nil {
			return err
		}
	}
	written, wasWritten := w.retryStatusWritten.Load(key.uid)
	if (wasWritten && written.(string) == value) || (!wasWritten && value == "") {
		return nil
	}
	configuration := corev1apply.Pod(key.name, key.namespace).WithUID(key.uid)
	if value != "" {
		configuration.WithAnnotations(map[string]string{podcontract.RestoreRetryStatusAnnotation: value})
	}
	_, err := w.clientset.CoreV1().Pods(key.namespace).Apply(ctx, configuration, metav1.ApplyOptions{
		FieldManager: restoreStatusFieldManager,
		Force:        true,
	})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	w.retryStatusWritten.Store(key.uid, value)
	return nil
}
