// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

const normalEventQueueCapacity = 128

type pendingPodEvent struct {
	pod       *corev1.Pod
	log       logr.Logger
	component string
	eventType string
	reason    string
	message   string
}

// podEventPublisher keeps informational Kubernetes Event writes out of the
// restore critical path. Warning events remain synchronous so an API failure is
// visible to the operation that reported it.
type podEventPublisher struct {
	clientset kubernetes.Interface
	log       logr.Logger
	pending   chan pendingPodEvent
	startOnce sync.Once
	started   atomic.Bool
}

func newPodEventPublisher(clientset kubernetes.Interface, log logr.Logger, capacity int) *podEventPublisher {
	return &podEventPublisher{
		clientset: clientset,
		log:       log,
		pending:   make(chan pendingPodEvent, capacity),
	}
}

func (p *podEventPublisher) Start(ctx context.Context) {
	p.startOnce.Do(func() {
		p.started.Store(true)
		go p.run(ctx)
	})
}

func (p *podEventPublisher) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case event := <-p.pending:
			emitPodEvent(ctx, p.clientset, event.log, event.pod, event.component, event.eventType, event.reason, event.message)
		}
	}
}

func (p *podEventPublisher) Publish(ctx context.Context, log logr.Logger, pod *corev1.Pod, component, eventType, reason, message string) {
	if eventType != corev1.EventTypeNormal || !p.started.Load() {
		emitPodEvent(ctx, p.clientset, log, pod, component, eventType, reason, message)
		return
	}

	event := pendingPodEvent{
		pod:       pod.DeepCopy(),
		log:       log,
		component: component,
		eventType: eventType,
		reason:    reason,
		message:   message,
	}
	select {
	case p.pending <- event:
	default:
		p.log.V(1).Info("Normal Kubernetes Event queue is full; dropping best-effort event",
			"pod", pod.Namespace+"/"+pod.Name,
			"reason", reason,
		)
	}
}

func (w *NodeController) emitPodEvent(ctx context.Context, log logr.Logger, pod *corev1.Pod, component, eventType, reason, message string) {
	if w.events == nil {
		emitPodEvent(ctx, w.clientset, log, pod, component, eventType, reason, message)
		return
	}
	w.events.Publish(ctx, log, pod, component, eventType, reason, message)
}
