// SPDX-FileCopyrightText: Copyright (c) 2025-2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ai-dynamo/snapshot/agent/internal/executor"
	"github.com/ai-dynamo/snapshot/agent/internal/nsmount"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// CheckpointParams carries everything the node driver needs to dump one container.
type CheckpointParams struct {
	// Pod is the live source pod (already provenance-verified by the reconciler).
	Pod *corev1.Pod
	// ContainerName is the single target container to checkpoint.
	ContainerName string
	// ContainerID is the agent-resolved running container ID (CRI scheme stripped).
	ContainerID string
	// ContainerPID is the agent-resolved host PID of the running container.
	ContainerPID int
	// ContentUID is the immutable PodSnapshotContent identity owning the artifact.
	ContentUID string
	// HostPath is the agent-resolved destination directory for the dump.
	HostPath string
	// StartedAt marks when the controller observed the work order, for timing.
	StartedAt time.Time
}

// singleTargetContainer returns the one capture-target container from the work order. The CRD
// enforces exactly one (PodReference.Containers MinItems=1/MaxItems=1) and the runtime dumps a
// single container this phase; the count is asserted here so a violated invariant becomes a
// terminal InvalidTargetContainer status rather than an out-of-bounds panic in the node agent.
func singleTargetContainer(content *snapshotv1alpha1.PodSnapshotContent) (string, error) {
	containers := content.Spec.Source.PodRef.Containers
	if len(containers) != 1 {
		return "", fmt.Errorf("source podRef must reference exactly one container, got %d", len(containers))
	}
	if strings.TrimSpace(containers[0]) == "" {
		return "", errors.New("source podRef container name must not be empty")
	}
	return containers[0], nil
}

// reconcileCapture drives one PodSnapshotContent work order end to end: it validates the source
// pod, promotes it with CaptureEligibleLabel, and runs the dump inline. Every trigger for this work
// order — content event, resync, or source-pod event — shares one capture queue key, which is what
// lets validation and the dump live in one function: they cannot observe each other half-done, so
// no terminal failure can be written out from under a running capture.
//
// Capture parameters come from the source pod, which is the single source of truth; this never
// mutates spec and writes status via Status().Patch only.
func (w *NodeController) reconcileCapture(ctx context.Context, name string) error {
	logger := logr.FromContextOrDiscard(ctx).WithValues("content", name)
	ctx = logr.NewContext(ctx, logger)

	content := &snapshotv1alpha1.PodSnapshotContent{}
	if err := w.client.Get(ctx, client.ObjectKey{Name: name}, content); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get PodSnapshotContent %q: %w", name, err)
	}

	if content.Spec.Source.NodeName != w.config.NodeName {
		return nil
	}
	if !content.DeletionTimestamp.IsZero() {
		return nil
	}
	if isContentTerminal(content) {
		return nil
	}

	// The immutable content UID and container own the artifact path. A recreated content object
	// receives a new UID and cannot adopt a stale path from the deleted object.
	contentUID := string(content.UID)
	if contentUID == "" {
		return w.setSnapshotContentFailed(ctx, content, "MissingContentUID",
			fmt.Errorf("PodSnapshotContent %q has no UID", content.Name))
	}
	containerName, err := singleTargetContainer(content)
	if err != nil {
		return w.setSnapshotContentFailed(ctx, content, "InvalidTargetContainer", err)
	}
	artifactPath, err := nsmount.ResolveArtifactPath(w.config.Storage.BasePath, contentUID, containerName)
	if err != nil {
		return w.setSnapshotContentFailed(ctx, content, "InvalidDestination", err)
	}

	pod := &corev1.Pod{}
	podKey := client.ObjectKey{Namespace: content.Spec.PodSnapshotRef.Namespace, Name: content.Spec.Source.PodRef.Name}
	if err := w.client.Get(ctx, podKey, pod); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get source pod %s: %w", podKey.String(), err)
		}
		// The operator creates the PodSnapshotContent only after the source pod exists, and this
		// is a linearizable (quorum) Get, so NotFound means the pod was deleted, not a creation
		// race. The dump kills the source, so deletion can also trail a successful capture
		// (Job cleanup, eviction): only a gone pod without a committed artifact fails the work
		// order terminally.
		if artifactPresent(artifactPath, contentUID, containerName) {
			return w.markCheckpointReady(ctx, content, artifactPath)
		}
		return w.setSnapshotContentFailed(ctx, content, "SourcePodNotFound", fmt.Errorf("source pod %q not found", podKey.String()))
	}

	if reason, msg := classifySourcePodIdentity(content, pod); reason != "" {
		err := w.setSnapshotContentFailed(ctx, content, reason, errors.New(msg))
		w.removeCaptureEligibleLabel(ctx, pod)
		return err
	}

	// Artifact recovery must precede the liveness checks below: the dump kills the source, so a
	// dead pod with a committed artifact is a success awaiting its Ready write — the write that
	// publishes the checkpoint for restore. The artifact dir exists only after the executor's
	// atomic rename.
	if artifactPresent(artifactPath, contentUID, containerName) {
		return w.markCheckpointReady(ctx, content, artifactPath)
	}

	// Everything below reads live pod state, and the unstick sweep below acts on it. Queue keys are
	// per work order, so several can name one pod; only the owner may draw conclusions from that
	// pod. A non-owner reading the source mid-dump sees a container the owner is busy killing, and
	// would SIGKILL the very container the owner is dumping. It waits instead: once the owner
	// reaches a terminal state, ownership passes and this work order settles on its own.
	chosen, err := w.captureOwnerForPod(pod)
	if err != nil {
		return err
	}
	if chosen != content.Name {
		logger.V(1).Info("Another work order owns this source pod", "pod", pod.Name, "owner", chosen)
		return nil
	}

	if handled, err := w.failCheckpointOnContainerExit(ctx, content, pod); handled {
		return err
	}
	if reason, msg := classifySourcePodLiveness(pod); reason != "" {
		err := w.setSnapshotContentFailed(ctx, content, reason, errors.New(msg))
		w.removeCaptureEligibleLabel(ctx, pod)
		return err
	}

	// The source-pod informer keys on CaptureEligibleLabel, so this patch is what makes pod status
	// changes (quiesce, a container crashing) reach the queue instead of waiting for the resync.
	// Failing it strands the work order, so it requeues rather than being logged and dropped.
	if err := w.labelCaptureEligible(ctx, pod); err != nil {
		return fmt.Errorf("mark source pod %s capture-eligible: %w", podKey.String(), err)
	}

	if !isContainerReady(pod, containerName) {
		logger.V(1).Info("Source container not ready, awaiting quiesce", "pod", pod.Name, "container", containerName)
		return nil
	}

	containerID := containerIDForName(pod, containerName)
	if containerID == "" {
		return w.setSnapshotContentFailed(ctx, content, "ContainerNotResolved",
			fmt.Errorf("could not resolve container %q ID", containerName))
	}
	containerPID, _, err := w.runtime.ResolveContainer(ctx, containerID)
	if err != nil {
		return w.setSnapshotContentFailed(ctx, content, "ContainerNotResolved", fmt.Errorf("resolve container %q: %w", containerName, err))
	}
	return w.runCheckpoint(ctx, content, pod, containerName, containerID, containerPID, contentUID, artifactPath)
}

func (w *NodeController) captureOwnerForPod(pod *corev1.Pod) (string, error) {
	objs, err := w.contentIndexer.ByIndex(podRefIndex, pod.Namespace+"/"+pod.Name)
	if err != nil {
		return "", fmt.Errorf("look up PodSnapshotContent by source pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	return chooseActiveContent(objs), nil
}

// runCheckpoint executes the dump, then writes the terminal status. It runs on the capture queue
// worker rather than a detached goroutine, which is what keeps the work order's key claimed for
// the whole dump. The dump terminates the target process, so there is no post-Ready release step.
// The container ID, host PID, and resolved locations are pre-resolved by the reconciler so the
// dump does not re-resolve them.
//
// A returned error means the outcome was not recorded and the queue should retry; a recorded
// failure returns nil, because retrying a terminal work order achieves nothing.
func (w *NodeController) runCheckpoint(
	ctx context.Context,
	content *snapshotv1alpha1.PodSnapshotContent,
	pod *corev1.Pod,
	containerName, containerID string,
	containerPID int,
	contentUID string,
	artifactPath string,
) error {
	logger := logr.FromContextOrDiscard(ctx)

	params := CheckpointParams{
		Pod:           pod,
		ContainerName: containerName,
		ContainerID:   containerID,
		ContainerPID:  containerPID,
		ContentUID:    contentUID,
		HostPath:      artifactPath,
		StartedAt:     time.Now(),
	}
	if err := w.checkpointFn(ctx, params); err != nil {
		logger.Error(err, "Checkpoint failed")
		if patchErr := w.setSnapshotContentFailed(ctx, content, "CheckpointFailed", err); patchErr != nil {
			return fmt.Errorf("write PodSnapshotContent failed status %q: %w", content.Name, patchErr)
		}
		return nil
	}

	return w.markCheckpointReady(ctx, content, artifactPath)
}

// classifySourcePodIdentity reports whether the live pod is the work order's pinned source
// ("" reason means it is). Identity is checked separately from liveness because only a
// same-identity source may recover a committed artifact: a same-named replacement pod must
// never validate another incarnation's capture. Pod existence (NotFound) is handled by the
// caller, which holds the Get error.
func classifySourcePodIdentity(content *snapshotv1alpha1.PodSnapshotContent, pod *corev1.Pod) (string, string) {
	if content.Spec.Source.PodRef.UID != "" && pod.UID != content.Spec.Source.PodRef.UID {
		return "StalePodReference",
			fmt.Sprintf("source pod %q UID %q does not match work order UID %q", pod.Name, pod.UID, content.Spec.Source.PodRef.UID)
	}
	return "", ""
}

// classifySourcePodLiveness reports whether the source pod can still host a new dump
// ("" reason means it can). A terminal pod is not by itself a capture failure — the dump
// terminates the source process — so callers must check for an in-flight capture or a
// committed artifact before treating this as terminal.
func classifySourcePodLiveness(pod *corev1.Pod) (string, string) {
	if pod.DeletionTimestamp != nil || pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
		return "SourcePodGone",
			fmt.Sprintf("source pod %q is no longer running (phase %s)", pod.Name, pod.Status.Phase)
	}
	return "", ""
}

// failCheckpointOnContainerExit fails the work order and force-terminates the source pod's
// still-running containers when any checkpoint container has terminated non-zero. The bool
// reports that the caller must stop; the error is the status write's, so a write that did not
// land is retried by the queue rather than waiting out the resync. Callers must only reach this
// for a source pod they own — the sweep kills every running container in the pod. Init containers
// (pod.Status.InitContainerStatuses) are intentionally out of scope.
func (w *NodeController) failCheckpointOnContainerExit(ctx context.Context, content *snapshotv1alpha1.PodSnapshotContent, pod *corev1.Pod) (bool, error) {
	failed := failedCheckpointContainer(pod)
	if failed == nil {
		return false, nil
	}

	term := failed.State.Terminated
	message := fmt.Sprintf("checkpoint container %q terminated with exit code %d", failed.Name, term.ExitCode)
	if term.Reason != "" {
		message = fmt.Sprintf("%s: %s", message, term.Reason)
	}
	logger := logr.FromContextOrDiscard(ctx).WithValues("container", failed.Name)
	logger.Info("Checkpoint container failed", "exit_code", term.ExitCode, "reason", term.Reason)
	emitPodEvent(ctx, w.clientset, logger, pod, snapshotEventComponent, corev1.EventTypeWarning, "CheckpointFailed", message)
	w.killRunningContainers(ctx, logger, pod, fmt.Sprintf("checkpoint container %s failed", failed.Name))
	if err := w.setSnapshotContentFailed(ctx, content, "CheckpointContainerFailed", errors.New(message)); err != nil {
		return true, fmt.Errorf("write PodSnapshotContent failed status %q: %w", content.Name, err)
	}
	return true, nil
}

// failedCheckpointContainer returns the first checkpoint container that terminated non-zero, or
// nil. Init containers (pod.Status.InitContainerStatuses) are intentionally out of scope.
func failedCheckpointContainer(pod *corev1.Pod) *corev1.ContainerStatus {
	for i := range pod.Status.ContainerStatuses {
		cs := &pod.Status.ContainerStatuses[i]
		if cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0 {
			return cs
		}
	}
	return nil
}

// killRunningContainers SIGKILLs every still-running container in the pod, resolving each
// container's host PID through the node runtime. Best-effort: resolution and signal errors are
// logged and skipped so one stuck container does not block terminating the rest.
func (w *NodeController) killRunningContainers(ctx context.Context, logger logr.Logger, pod *corev1.Pod, reason string) {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Running == nil || cs.ContainerID == "" {
			continue
		}
		containerID := snapshotruntime.StripCRIScheme(cs.ContainerID)
		resolveCtx, cancel := context.WithTimeout(ctx, containerResolveAttemptTimeout)
		pid, _, err := w.runtime.ResolveContainer(resolveCtx, containerID)
		cancel()
		if err != nil {
			logger.Error(err, "Failed to resolve running checkpoint container", "container", cs.Name)
			continue
		}
		if err := snapshotruntime.SendSignalToPID(logger, pid, syscall.SIGKILL, reason); err != nil {
			logger.Error(err, "Failed to signal running checkpoint container", "container", cs.Name)
		}
	}
}

// podLabelPatchBase returns a minimal Pod carrying only the identity + a clone of the source pod's
// labels, suitable as the MergeFrom base for a label-only patch — so the informer-cached pod is not
// mutated and the whole object is not deep-copied.
func podLabelPatchBase(pod *corev1.Pod) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: pod.Namespace,
		Name:      pod.Name,
		Labels:    maps.Clone(pod.Labels),
	}}
}

// labelCaptureEligible promotes a gate-validated source pod by adding CaptureEligibleLabel, which the
// source-pod informer keys on. Idempotent.
func (w *NodeController) labelCaptureEligible(ctx context.Context, pod *corev1.Pod) error {
	if pod.Labels[snapshotv1alpha1.CaptureEligibleLabel] == "true" {
		return nil
	}
	base := podLabelPatchBase(pod)
	updated := base.DeepCopy()
	if updated.Labels == nil {
		updated.Labels = map[string]string{}
	}
	updated.Labels[snapshotv1alpha1.CaptureEligibleLabel] = "true"
	return w.client.Patch(ctx, updated, client.MergeFrom(base))
}

// removeCaptureEligibleLabel drops CaptureEligibleLabel so the source-pod informer stops driving the
// pod after a terminal cancellation. Best-effort: a failure is logged, not surfaced.
func (w *NodeController) removeCaptureEligibleLabel(ctx context.Context, pod *corev1.Pod) {
	if _, ok := pod.Labels[snapshotv1alpha1.CaptureEligibleLabel]; !ok {
		return
	}
	base := podLabelPatchBase(pod)
	updated := base.DeepCopy()
	delete(updated.Labels, snapshotv1alpha1.CaptureEligibleLabel)
	if err := w.client.Patch(ctx, updated, client.MergeFrom(base)); err != nil {
		logr.FromContextOrDiscard(ctx).Error(err, "Failed to remove capture-eligible label", "pod", pod.Name)
	}
}

// setSnapshotContentSucceeded patches status with the Ready condition and the source values the
// capture recorded. Uses optimistic locking so a concurrent terminal Failed write wins and this
// patch is rejected rather than overwriting it.
func (w *NodeController) setSnapshotContentSucceeded(
	ctx context.Context,
	content *snapshotv1alpha1.PodSnapshotContent,
	source *snapshotv1alpha1.CheckpointSource,
) error {
	patch := client.MergeFromWithOptions(content.DeepCopy(), client.MergeFromWithOptimisticLock{})
	meta.SetStatusCondition(&content.Status.Conditions, metav1.Condition{
		Type:    snapshotv1alpha1.PodSnapshotConditionReady,
		Status:  metav1.ConditionTrue,
		Reason:  "Captured",
		Message: "Checkpoint captured and verified",
	})
	if source != nil {
		content.Status.Source = source
	}
	return w.client.Status().Patch(ctx, content, patch)
}

// readyStatusConflictLimit caps Ready-patch retries after optimistic-lock conflicts so a
// livelock of non-terminal status writes cannot spin forever.
const readyStatusConflictLimit = 8

// markCheckpointReady makes the committed capture durable in the API. The source process is
// already dead — the dump terminates it — so no release step follows and no live PID is needed,
// which lets the artifact-recovery paths call this after the source pod turned terminal. On a
// Ready-patch conflict, re-read and retry until Ready lands or a terminal state is observed: an
// already-Failed work order is sticky and wins; Ready already set means another holder finished
// the write.
//
// The Ready write also publishes what the capture ran on, read from the artifact's manifest once
// rather than per retry. An unreadable manifest costs those values, never the capture: Ready is
// written regardless, because the artifact is already committed.
func (w *NodeController) markCheckpointReady(
	ctx context.Context,
	content *snapshotv1alpha1.PodSnapshotContent,
	artifactPath string,
) error {
	logger := logr.FromContextOrDiscard(ctx)
	source, err := checkpointSourceAtPath(artifactPath)
	if err != nil {
		logger.Error(err, "Failed to read the captured source; marking Ready without it",
			"content", content.Name,
			"artifactPath", artifactPath,
		)
	}
	ready := content
	for attempt := 0; attempt < readyStatusConflictLimit; attempt++ {
		if err := w.setSnapshotContentSucceeded(ctx, ready, source); err != nil {
			if !apierrors.IsConflict(err) {
				logger.Error(err, "Failed to write PodSnapshotContent ready status", "content", content.Name)
				return err
			}
			current := &snapshotv1alpha1.PodSnapshotContent{}
			if getErr := w.client.Get(ctx, client.ObjectKey{Name: content.Name}, current); getErr != nil {
				logger.Error(getErr, "Failed to re-read PodSnapshotContent after Ready conflict", "content", content.Name)
				return getErr
			}
			if isContentFailed(current) {
				logger.Info("Skipping Ready write; work order already failed", "content", content.Name)
				return nil
			}
			if isContentReady(current) {
				return nil
			}
			ready = current
			continue
		}
		return nil
	}
	return fmt.Errorf("write PodSnapshotContent ready status %q: exceeded conflict retries", content.Name)
}

// setSnapshotContentFailed patches status with the Failed condition. Uses optimistic locking so
// that a concurrent failure write wins and this patch is rejected rather than overwriting it.
func (w *NodeController) setSnapshotContentFailed(ctx context.Context, content *snapshotv1alpha1.PodSnapshotContent, reason string, cause error) error {
	patch := client.MergeFromWithOptions(content.DeepCopy(), client.MergeFromWithOptimisticLock{})
	meta.SetStatusCondition(&content.Status.Conditions, metav1.Condition{
		Type:    snapshotv1alpha1.PodSnapshotConditionFailed,
		Status:  metav1.ConditionTrue,
		Reason:  reason,
		Message: cause.Error(),
	})
	return w.client.Status().Patch(ctx, content, patch)
}

// executorCheckpoint is the production checkpointFn. The reconciler has already resolved the
// container ID and host PID. It runs executor.Checkpoint to the destination and verifies the
// artifact directory. On dump or verification failure it SIGKILLs the CUDA-locked process before
// returning the error; on success the dump itself has already terminated the source process.
func (w *NodeController) executorCheckpoint(ctx context.Context, params CheckpointParams) error {
	log := logr.FromContextOrDiscard(ctx)

	req := executor.CheckpointRequest{
		ContainerID:         params.ContainerID,
		ContainerName:       params.ContainerName,
		ContentUID:          params.ContentUID,
		StartedAt:           params.StartedAt,
		NodeName:            w.config.NodeName,
		PodName:             params.Pod.Name,
		PodNamespace:        params.Pod.Namespace,
		PodIP:               params.Pod.Status.PodIP,
		Pod:                 podEnvironment(params.Pod, params.ContainerName),
		Clientset:           w.clientset,
		PageBrokerRequested: params.Pod.Annotations[snapshotv1alpha1.PageBrokerAnnotation] == snapshotv1alpha1.PageBrokerAnnotationEnabled,
	}
	if err := executor.Checkpoint(ctx, w.runtime, log, req, w.config); err != nil {
		if executor.CheckpointNeedsSourceKill(err) {
			if killErr := w.killCheckpointProcess(log, params.ContainerPID, "checkpoint failed"); killErr != nil {
				log.Error(killErr, "Failed to kill target after checkpoint failure")
			}
		}
		return fmt.Errorf("checkpoint: %w", err)
	}

	info, statErr := os.Stat(params.HostPath)
	if statErr != nil || !info.IsDir() {
		var verifyErr error
		if statErr != nil {
			verifyErr = fmt.Errorf("verify checkpoint path %s: %w", params.HostPath, statErr)
		} else {
			verifyErr = fmt.Errorf("verify checkpoint path %s: not a directory", params.HostPath)
		}
		if killErr := w.killCheckpointProcess(log, params.ContainerPID, "checkpoint verification failed"); killErr != nil {
			log.Error(killErr, "Failed to kill target after checkpoint verification failure")
		}
		return verifyErr
	}

	return nil
}

// killCheckpointProcess SIGKILLs the CUDA-locked process so it does not hang after a failed dump.
// ESRCH (already exited) is success. Any other signal error is returned so callers can fail closed.
func (w *NodeController) killCheckpointProcess(log logr.Logger, pid int, reason string) error {
	if err := snapshotruntime.SendSignalToPID(log, pid, syscall.SIGKILL, reason); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		log.Error(err, "Failed to signal checkpoint process", "reason", reason)
		return err
	}
	return nil
}

// containerIDForName returns the running container's CRI-stripped ID, or "" if absent.
func containerIDForName(pod *corev1.Pod, containerName string) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == containerName {
			return snapshotruntime.StripCRIScheme(cs.ContainerID)
		}
	}
	return ""
}

// isContentTerminal reports whether the work order already has a terminal condition.
func isContentTerminal(content *snapshotv1alpha1.PodSnapshotContent) bool {
	return isContentReady(content) || isContentFailed(content)
}

func isContentReady(content *snapshotv1alpha1.PodSnapshotContent) bool {
	return meta.IsStatusConditionTrue(content.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionReady)
}

func isContentFailed(content *snapshotv1alpha1.PodSnapshotContent) bool {
	return meta.IsStatusConditionTrue(content.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed)
}

// artifactPresent reports whether a completed checkpoint directory already exists on disk.
func artifactPresent(destination, contentUID, containerName string) bool {
	info, err := os.Stat(destination)
	if err != nil || !info.IsDir() {
		return false
	}
	manifest, err := types.ReadManifest(destination)
	return err == nil &&
		manifest.Artifact.ContentUID == contentUID &&
		manifest.Artifact.ContainerName == containerName
}

// contentNameFromInformerObj extracts the object name from a dynamic informer object,
// handling the DeletedFinalStateUnknown tombstone.
func contentNameFromInformerObj(obj interface{}) (string, bool) {
	if accessor, err := meta.Accessor(obj); err == nil {
		return accessor.GetName(), true
	}
	tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
	if !ok {
		return "", false
	}
	accessor, err := meta.Accessor(tombstone.Obj)
	if err != nil {
		return "", false
	}
	return accessor.GetName(), true
}
