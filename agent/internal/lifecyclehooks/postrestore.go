// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package lifecyclehooks

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// The post-restore actions below tell a restored application that it may
// resume. Check runs before any CRIU work and reads the Pod spec only. Notify
// runs after restore-complete released the restored process, and makes one
// attempt; the caller retries it every period until it succeeds or the
// restore deadline passes.

// PostRestoreTarget is the restored container a post-restore action notifies.
type PostRestoreTarget struct {
	Pod         *corev1.Pod
	Container   string
	ContainerID string
	// PodIP returns the Pod's IP, reading the Pod again when the copy above
	// doesn't have one yet.
	PodIP func(ctx context.Context) (string, error)
}

// FilePostRestore creates a file in the restored container, as the
// container's user. Timeout bounds each attempt.
type FilePostRestore struct {
	Exec    Execer
	Path    string
	Timeout time.Duration
}

// Check refuses one of Snapshot's own control files.
func (h *FilePostRestore) Check(pod *corev1.Pod, container string) error {
	return CheckWritePath(pod, container, h.Path)
}

// Notify counts an existing file as the notification. Usually an earlier
// attempt created it: one whose reply was lost, or one before the agent
// restarted. Nothing checks the file before the release, to keep the restore
// fast, so a file that was there before the restore, such as one the restored
// files brought back, counts too: the workload contract forbids such a file,
// and the application then gets no notification. A file that can't be created
// fails each attempt with the reason, until the restore deadline.
func (h *FilePostRestore) Notify(ctx context.Context, target PostRestoreTarget) error {
	err := CreateMarker(ctx, h.Exec, target.ContainerID, h.Path, h.Timeout)
	if errors.Is(err, fs.ErrExist) {
		logr.FromContextOrDiscard(ctx).Info("Post-restore file already exists; counting it as the notification", "path", h.Path)
		return nil
	}
	return err
}

// HTTPPostRestore sends a POST to the restored Pod's IP. Timeout bounds each
// attempt.
type HTTPPostRestore struct {
	Client  *http.Client
	Action  *snapshotv1alpha1.HTTPPostAction
	Timeout time.Duration
}

// Check refuses a path that doesn't parse and a port name the container
// doesn't have. A named port resolves against the restored container, which
// runs the restored application.
func (h *HTTPPostRestore) Check(pod *corev1.Pod, container string) error {
	if _, err := RequestURL("", 0, h.Action.Path); err != nil {
		return err
	}
	_, err := ResolvePort(pod, container, h.Action.Port)
	return err
}

func (h *HTTPPostRestore) Notify(ctx context.Context, target PostRestoreTarget) error {
	podIP, err := target.PodIP(ctx)
	if err != nil {
		return err
	}
	port, err := ResolvePort(target.Pod, target.Container, h.Action.Port)
	if err != nil {
		return err
	}
	u, err := RequestURL(podIP, port, h.Action.Path)
	if err != nil {
		return err
	}
	return HTTPPost(ctx, h.Client, u, h.Timeout)
}
