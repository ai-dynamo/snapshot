// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package lifecyclehooks

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// The post-restore actions below tell a restored application that it may
// resume. Check runs before any CRIU work and reads the Pod spec only.
// Recheck runs once the restored files are in place, before restore-complete
// releases the restored process. Notify makes one attempt; the caller retries
// it every period until it succeeds or the restore deadline passes.

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

// Recheck tests the file only once the restored files are in place, since
// they can bring the file or its directory. The file must not exist yet: an
// existing file would be taken for the notification, so the application
// would never get one. And it must be possible to create: its directory
// exists and the container's user may write it, so a read-only mount fails
// the restore here rather than after the application was released.
func (h *FilePostRestore) Recheck(ctx context.Context, target PostRestoreTarget) error {
	err := CheckMarkerTarget(ctx, h.Exec, target.ContainerID, h.Path, h.Timeout)
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("%s already exists in container %s once the restored files are in place; "+
			"the post-restore file must not exist before Snapshot creates it", h.Path, target.Container)
	}
	return err
}

// Notify counts an existing file as its own. Recheck found no file before
// restore-complete, and only Notify creates it, so finding it here means an
// earlier attempt created it: one whose reply was lost, or one before the
// agent restarted.
func (h *FilePostRestore) Notify(ctx context.Context, target PostRestoreTarget) error {
	err := CreateMarker(ctx, h.Exec, target.ContainerID, h.Path, h.Timeout)
	if errors.Is(err, fs.ErrExist) {
		logr.FromContextOrDiscard(ctx).Info("Post-restore file already exists; an earlier attempt created it", "path", h.Path)
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

// Recheck has nothing to check once the restored files are in place.
func (h *HTTPPostRestore) Recheck(context.Context, PostRestoreTarget) error { return nil }

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
