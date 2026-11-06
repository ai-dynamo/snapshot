// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package lifecyclehooks

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// The probes below tell whether a source container is ready for a
// checkpoint. Each Probe call is one check, and any error is a failed check;
// the caller paces the checks and counts the results. Check refuses a probe
// that can never run. It reads the Pod spec only, so waiting would not change
// its answer.

// ReadyCondition is the probe for a container without a
// checkpointReadinessProbe: the container's Ready condition.
type ReadyCondition struct{}

func (ReadyCondition) Check(*corev1.Pod, string) error { return nil }

func (ReadyCondition) Probe(_ context.Context, pod *corev1.Pod, status *corev1.ContainerStatus, _ time.Duration) error {
	if pod.Status.Phase != corev1.PodRunning || !status.Ready {
		return errors.New("the container is not Ready")
	}
	return nil
}

// FileProbe succeeds when a regular file exists at Path in the container. It
// reads as the container's user, so any path is allowed.
type FileProbe struct {
	Exec Execer
	Path string
}

func (p *FileProbe) Check(*corev1.Pod, string) error { return nil }

func (p *FileProbe) Probe(ctx context.Context, _ *corev1.Pod, status *corev1.ContainerStatus, timeout time.Duration) error {
	exists, err := FileExists(ctx, p.Exec, status.ContainerID, p.Path, timeout)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("%s does not exist", p.Path)
	}
	return nil
}

// HTTPProbe succeeds when a GET to the Pod's IP returns a 2xx status.
type HTTPProbe struct {
	Client *http.Client
	Action *snapshotv1alpha1.HTTPGetAction
}

// Check refuses a path that doesn't parse and a named port the container
// doesn't have.
func (p *HTTPProbe) Check(pod *corev1.Pod, containerName string) error {
	if _, err := RequestURL("", 0, p.Action.Path); err != nil {
		return err
	}
	_, err := ResolvePort(pod, containerName, p.Action.Port)
	return err
}

func (p *HTTPProbe) Probe(ctx context.Context, pod *corev1.Pod, status *corev1.ContainerStatus, timeout time.Duration) error {
	podIP, err := HTTPTarget(pod)
	if err != nil {
		return err
	}
	// Check already resolved it, so this only fails if the Pod changed.
	port, err := ResolvePort(pod, status.Name, p.Action.Port)
	if err != nil {
		return err
	}
	u, err := RequestURL(podIP, port, p.Action.Path)
	if err != nil {
		return err
	}
	return HTTPGet(ctx, p.Client, u, timeout)
}
