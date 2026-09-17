// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clientgotesting "k8s.io/client-go/testing"
)

func TestNormalPodEventDoesNotWaitForAPIServer(t *testing.T) {
	clientset := fake.NewClientset()
	apiEntered := make(chan struct{})
	releaseAPI := make(chan struct{})
	clientset.PrependReactor("create", "events", func(clientgotesting.Action) (bool, runtime.Object, error) {
		close(apiEntered)
		<-releaseAPI
		return false, nil, nil
	})
	publisher := newPodEventPublisher(clientset, testr.New(t), 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	publisher.Start(ctx)

	returned := make(chan struct{})
	go func() {
		publisher.Publish(ctx, testr.New(t), restorePod(nil), snapshotEventComponent, corev1.EventTypeNormal, "RestoreRequested", "restore requested")
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("publishing a Normal event blocked on the API server")
	}
	select {
	case <-apiEntered:
	case <-time.After(time.Second):
		t.Fatal("event worker did not attempt the API write")
	}
	close(releaseAPI)
	require.Eventually(t, func() bool {
		return len(clientset.Actions()) != 0
	}, time.Second, 10*time.Millisecond)
}

func TestWarningPodEventWaitsForAPIServer(t *testing.T) {
	clientset := fake.NewClientset()
	apiEntered := make(chan struct{})
	releaseAPI := make(chan struct{})
	clientset.PrependReactor("create", "events", func(clientgotesting.Action) (bool, runtime.Object, error) {
		close(apiEntered)
		<-releaseAPI
		return false, nil, nil
	})
	publisher := newPodEventPublisher(clientset, testr.New(t), 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	publisher.Start(ctx)

	returned := make(chan struct{})
	go func() {
		publisher.Publish(ctx, testr.New(t), restorePod(nil), snapshotEventComponent, corev1.EventTypeWarning, "RestoreFailed", "restore failed")
		close(returned)
	}()

	select {
	case <-apiEntered:
	case <-time.After(time.Second):
		t.Fatal("warning event did not attempt the API write")
	}
	select {
	case <-returned:
		t.Fatal("publishing a Warning event returned before the API write completed")
	default:
	}
	close(releaseAPI)
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("publishing a Warning event did not return after the API write completed")
	}
}
