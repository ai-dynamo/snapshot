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
	"github.com/ai-dynamo/snapshot/agent/internal/pagebroker"
	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/ai-dynamo/snapshot/api/podcontract"
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
	// CuInterposeRequested is the source Pod's explicit opt-in, already preflighted.
	CuInterposeRequested bool
	// DumpRetriesRemaining is what is left of the capture's retry budget; a repeat of
	// the dump spends it like any other retry.
	DumpRetriesRemaining int
	// DumpWorthRepeating reports whether repeating could reach a different outcome.
	DumpWorthRepeating func(err error) (bool, string)
	// OnDumpRepeat reports each repeat as it is taken. Nil reports nothing.
	OnDumpRepeat func(repeat int, err error)
	// StoreID binds this capture to a configured store; empty means legacy.
	StoreID string
}

// singleTargetContainer returns the one capture-target container a source podRef names, from a
// PodSnapshot or its PodSnapshotContent. The CRD enforces exactly one (PodReference.Containers
// MinItems=1/MaxItems=1) and the runtime dumps a single container this phase; the count is
// asserted here so a violated invariant becomes a terminal InvalidTargetContainer status rather
// than an out-of-bounds panic in the node agent.
func singleTargetContainer(containers []string) (string, error) {
	if len(containers) != 1 {
		return "", fmt.Errorf("source podRef must reference exactly one container, got %d", len(containers))
	}
	if strings.TrimSpace(containers[0]) == "" {
		return "", errors.New("source podRef container name must not be empty")
	}
	return containers[0], nil
}

// reconcileCapture drives one PodSnapshotContent work order end to end: validate the source pod,
// promote it with CaptureEligibleLabel, run the dump inline. Every trigger for this work order
// shares one queue key, which is what lets validation and the dump live in one function: neither
// can observe the other half-done, so no terminal failure lands under a running capture.
//
// It never mutates spec and writes status via Status().Patch only.
func (w *NodeController) reconcileCapture(ctx context.Context, name string) error {
	logger := logr.FromContextOrDiscard(ctx).WithValues("content", name)
	ctx = logr.NewContext(ctx, logger)

	content, err := w.cachedContent(name)
	if err != nil {
		return err
	}
	if content == nil || content.Spec.Source.NodeName != w.config.NodeName ||
		!content.DeletionTimestamp.IsZero() || isContentTerminal(content) {
		w.checkpointGates.Delete(name)
		return nil
	}

	// The immutable content UID and container own the artifact path. A recreated content object
	// receives a new UID and cannot adopt a stale path from the deleted object.
	contentUID := string(content.UID)
	if contentUID == "" {
		return w.setSnapshotContentFailed(ctx, content, "MissingContentUID",
			fmt.Errorf("PodSnapshotContent %q has no UID", content.Name))
	}
	containerName, err := singleTargetContainer(content.Spec.Source.PodRef.Containers)
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
		if recovered, err := w.recoverCommittedArtifact(ctx, content, contentUID, containerName, artifactPath); recovered || err != nil {
			return err
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
	if recovered, err := w.recoverCommittedArtifact(ctx, content, contentUID, containerName, artifactPath); recovered || err != nil {
		return err
	}

	// Several work orders can name one pod, and everything below draws conclusions from live pod
	// state — including a sweep that SIGKILLs its containers. Only the owner may. A non-owner
	// reading the source mid-dump would kill the container the owner is dumping, so it waits:
	// ownership passes when the owner goes terminal.
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

	// Check the opt-in before labeling the source and waiting for readiness, so a misconfigured
	// Pod fails without waiting for its workload to become ready.
	cuInterposeRequested, err := preflightCuInterpose(pod, containerName)
	if err != nil {
		return w.setSnapshotContentFailed(ctx, content, "CuInterposeMisconfigured", err)
	}

	// The source-pod informer keys on this label, so the patch is what makes pod status changes
	// reach the queue instead of waiting for the resync. Failing it strands the work order.
	if err := w.labelCaptureEligible(ctx, pod); err != nil {
		return fmt.Errorf("mark source pod %s capture-eligible: %w", podKey.String(), err)
	}

	gate, err := w.getCheckpointGate(ctx, content, pod, containerName)
	if gate == nil {
		return err
	}
	now := w.nowFn()
	ready, next := gate.runProbeIfDue(ctx, pod, containerStatus(pod, containerName), now)
	if !ready {
		// Pod events wake the work order more often than the period, so the
		// gate paces the checks and says when the next one is due.
		if !next.IsZero() {
			w.requeueCaptureFn(name, next.Sub(now))
		}
		return nil
	}
	w.checkpointGates.Delete(name)
	logger.Info("Source container is ready for checkpoint", "pod", pod.Name, "container", containerName)

	containerID := containerIDForName(pod, containerName)
	if containerID == "" {
		return w.setSnapshotContentFailed(ctx, content, "ContainerNotResolved",
			fmt.Errorf("could not resolve container %q ID", containerName))
	}
	containerPID, _, err := w.runtime.ResolveContainer(ctx, containerID)
	if err != nil {
		return w.setSnapshotContentFailed(ctx, content, "ContainerNotResolved", fmt.Errorf("resolve container %q: %w", containerName, err))
	}
	// Refused before the suspend, so the workload is never disturbed for a
	// capture the node cannot stage.
	if err := w.checkStagingCapacity(ctx, containerPID); err != nil {
		return w.setSnapshotContentFailed(ctx, content, reasonImageTooLarge, err)
	}
	return w.runCheckpoint(ctx, content, pod, containerName, containerID, containerPID, cuInterposeRequested, contentUID, artifactPath)
}

// recoverCommittedArtifact promotes a legacy capture that committed before this reconcile — the
// agent died, or the Ready write was lost — to Ready, which is the case when its directory
// exists on the shared filesystem. Bound content has no equivalent: its publication lives in
// PageBroker's store, and the capture is Ready only through the Commit reply. The bool says
// Ready was written (or is being written).
func (w *NodeController) recoverCommittedArtifact(
	ctx context.Context,
	content *snapshotv1alpha1.PodSnapshotContent,
	contentUID, containerName, artifactPath string,
) (bool, error) {
	if content.Spec.Storage != nil || !artifactPresent(artifactPath, contentUID, containerName) {
		return false, nil
	}
	return true, w.markCheckpointReady(ctx, content, artifactPath, "", nil)
}

// cachedContent returns the PodSnapshotContent as last observed by the content informer, or nil
// when the cache does not hold it.
func (w *NodeController) cachedContent(name string) (*snapshotv1alpha1.PodSnapshotContent, error) {
	obj, exists, err := w.contentIndexer.GetByKey(name)
	if err != nil {
		return nil, fmt.Errorf("look up PodSnapshotContent %q in the informer cache: %w", name, err)
	}
	if !exists {
		return nil, nil
	}
	content, ok := contentFromInformerObj(obj)
	if !ok {
		return nil, fmt.Errorf("PodSnapshotContent %q in the informer cache has an unexpected type %T", name, obj)
	}
	return content, nil
}

func (w *NodeController) captureOwnerForPod(pod *corev1.Pod) (string, error) {
	objs, err := w.contentIndexer.ByIndex(podRefIndex, pod.Namespace+"/"+pod.Name)
	if err != nil {
		return "", fmt.Errorf("look up PodSnapshotContent by source pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	return chooseActiveContent(objs), nil
}

const attemptRetryDelayCeiling = 60 * time.Second

// Overridden in tests so a retry does not wait out a real backoff.
var attemptRetryInitialDelay = 5 * time.Second

// runCheckpoint executes the dump, then writes the terminal status. It runs inline on the queue
// item rather than a goroutine of its own, which is what keeps the key claimed for the whole dump.
// The dump terminates the target, so there is no release step.
//
// A returned error means the outcome was not recorded and the queue should retry; a recorded
// failure returns nil, because retrying a terminal work order achieves nothing.
func (w *NodeController) runCheckpoint(
	ctx context.Context,
	content *snapshotv1alpha1.PodSnapshotContent,
	pod *corev1.Pod,
	containerName, containerID string,
	containerPID int,
	cuInterposeRequested bool,
	contentUID string,
	artifactPath string,
) error {
	captureCtx := ctx
	if timeout := w.config.Checkpoint.CheckpointTimeout(); timeout > 0 {
		var cancel context.CancelFunc
		captureCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	logger := logr.FromContextOrDiscard(ctx)
	budget := w.config.Checkpoint.ResolveRetryLimit(content.Spec.CheckpointRetryLimit)
	spent := 0
	// A workload an earlier attempt put back for a retry. Recovery only hears
	// about the attempt that suspended one, so without this a revived source
	// would be abandoned the moment a later attempt failed before suspending
	// anything — left running and holding its GPUs with nothing left to capture it.
	sourceLive := false
	logger.V(1).Info("Resolved the capture's retry budget", "retries", budget)
	for {
		onDumpRepeat := func(repeat int, dumpErr error) {
			// Refined like the outer loop's, so one cause does not read as two
			// problems under two reasons.
			refined, _ := w.refineFullStaging(ctx, classifyCaptureFailure(dumpErr), dumpErr, containerPID)
			w.reportAttemptFailed(ctx, logger, pod, refined.reason, spent+repeat, budget, dumpErr)
		}
		artifact, err := w.attemptCheckpoint(captureCtx, content, pod, containerName, containerID, containerPID,
			cuInterposeRequested, contentUID, artifactPath, budget-spent, w.dumpWorthRepeating(ctx, containerPID), onDumpRepeat)
		if err == nil {
			return w.markCheckpointReady(ctx, content, artifactPath, containerName, artifact)
		}

		attempt := classifyAttempt(err)
		// Measured while the source is still suspended, so the tree the dump was
		// sizing is the one still there to size.
		attempt.failure, err = w.refineFullStaging(ctx, attempt.failure, err, containerPID)
		// Repeats of the dump come out of the same budget as fresh attempts, so a
		// caller asking for two retries gets two, however they were spent.
		spent += attempt.suspended.DumpRetriesUsed
		wantRetry := attempt.failure.retryable && spent < budget && captureCtx.Err() == nil &&
			w.workOrderStillCapturable(ctx, content)

		// Recovery has the last word on an attempt that suspended something: a
		// source it could not put back has nothing left for another attempt.
		retryWillFollow := wantRetry
		if attempt.suspendedSource {
			retryWillFollow = w.recoverSourceAfterFailedDump(ctx, logger, pod, containerPID, attempt.suspended, wantRetry)
			sourceLive = retryWillFollow
		}
		if !retryWillFollow {
			w.endSource(ctx, logger, pod, containerPID, sourceLive)
			return w.failCheckpoint(ctx, logger, content, pod,
				terminalReason(attempt.failure, spent, budget), recordedCause(err, spent, budget))
		}

		spent++
		w.reportAttemptFailed(ctx, logger, pod, attempt.failure.reason, spent, budget, err)
		select {
		case <-captureCtx.Done():
			w.endSource(ctx, logger, pod, containerPID, sourceLive)
			return w.failCheckpoint(ctx, logger, content, pod, attempt.failure.reason, err)
		case <-time.After(attemptRetryDelay(spent)):
		}
	}
}

// failedAttempt is what one failed attempt left behind.
type failedAttempt struct {
	failure captureFailure
	// suspended describes the workload the attempt left suspended, and
	// suspendedSource reports whether it left one at all.
	suspended       executor.SuspendedSource
	suspendedSource bool
}

// classifyAttempt reads a failed attempt: why it failed, whether another attempt
// could end differently, and what it left behind.
func classifyAttempt(err error) failedAttempt {
	failure := classifyCaptureFailure(err)
	// classifyCaptureFailure is given only the error, never the step it came
	// from. The dump terminates the source, so a commit failing after it leaves
	// no workload for another attempt to capture.
	if executor.StepOf(err) == executor.StepCommitCheckpoint {
		failure.retryable = false
	}
	// An attempt whose abort failed may still be holding its transaction, and the
	// next one would prepare the same destination against it. SNEP-394 requires a
	// re-stage to confirm release first; a failed abort confirms nothing.
	if executor.IsCleanupFailure(err) {
		failure.retryable = false
	}
	suspended, suspendedSource := executor.SuspendedSourceOf(err)
	return failedAttempt{failure: failure, suspended: suspended, suspendedSource: suspendedSource}
}

// endSource terminates a workload an earlier attempt revived, once no further
// attempt is coming. It is the call recovery itself makes when it decides not to
// revive, so an owner sees the same SourcePodTerminated whichever path got here.
func (w *NodeController) endSource(
	ctx context.Context, logger logr.Logger, pod *corev1.Pod, containerPID int, live bool,
) {
	if !live {
		return
	}
	w.recoverSourceAfterFailedDump(ctx, logger, pod, containerPID, executor.SuspendedSource{}, false)
}

// recordedCause names the retries a failure outlived, so the recorded message
// says how much was spent reaching it.
func recordedCause(err error, spent, budget int) error {
	if spent == 0 {
		return err
	}
	return fmt.Errorf("after %d of %d retries: %w", spent, budget, err)
}

// failCheckpoint ends the capture: the work order records the outcome, and the
// source Pod's owner gets the failure that ended it as an event. Only a failure
// another attempt follows reports as an attempt, so without this the last cause
// — the one to act on — would be the only one the events leave out.
func (w *NodeController) failCheckpoint(
	ctx context.Context, logger logr.Logger, content *snapshotv1alpha1.PodSnapshotContent,
	pod *corev1.Pod, reason string, cause error,
) error {
	emitPodEvent(ctx, w.clientset, logger, pod, snapshotEventComponent, corev1.EventTypeWarning,
		reason, fmt.Sprintf("Checkpoint failed: %v", cause))
	return w.markCheckpointFailed(ctx, content, reason, cause)
}

// reportAttemptFailed tells the source Pod's owner that an attempt failed and
// another is coming. Both a repeated dump and a fresh attempt report through
// here, so the retries read the same whichever kind was spent.
func (w *NodeController) reportAttemptFailed(
	ctx context.Context, logger logr.Logger, pod *corev1.Pod, reason string, spent, budget int, err error,
) {
	emitPodEvent(ctx, w.clientset, logger, pod, snapshotEventComponent, corev1.EventTypeWarning,
		reasonCheckpointAttemptFailed,
		fmt.Sprintf("Checkpoint attempt failed (%s); retrying, %d of %d retries used: %v",
			reason, spent, budget, err))
}

// terminalReason separates a checkpoint that used up its retries from one that
// failed on its first try, which read the same before. Retries spent repeating
// the dump count: the caller asked for that many tries and got them.
//
// Only a failure another attempt could have cleared reads as exhaustion; a
// settled one ended the checkpoint on its own terms.
func terminalReason(failure captureFailure, spent, budget int) string {
	if failure.retryable && spent > 0 && spent >= budget {
		return reasonRetriesExhausted
	}
	return failure.reason
}

// attemptRetryDelay spaces fresh attempts, each of which suspends the workload
// again, so a node that is briefly unable to take one is not hammered.
func attemptRetryDelay(attempt int) time.Duration {
	// Far enough out the shift overflows int64, and a delay that lands on zero or
	// negative makes time.After fire at once — attempts with no backoff at all.
	delay := attemptRetryInitialDelay << (attempt - 1)
	if delay <= 0 || delay > attemptRetryDelayCeiling {
		return attemptRetryDelayCeiling
	}
	return delay
}

// workOrderStillCapturable reports whether the work order is still ours to act
// on. The exclusion that holds it is in-memory, so nothing can take it
// mid-capture, but it can be deleted or made terminal from elsewhere.
//
// It reads the informer cache rather than the API server. A live read would
// turn an API hiccup into "not capturable", which abandons the retry and, on an
// attempt that suspended the source, has recovery kill a workload it could have
// revived. The cache is at most seconds stale, and the next attempt is 5-60s
// away, so staleness costs nothing here. A cache lookup that errors leaves the
// answer unknown, and acting on the work order already in hand is the choice
// that cannot destroy a workload.
func (w *NodeController) workOrderStillCapturable(ctx context.Context, content *snapshotv1alpha1.PodSnapshotContent) bool {
	current, err := w.cachedContent(content.Name)
	if err != nil {
		logr.FromContextOrDiscard(ctx).V(1).Info("Cannot read the work order; assuming it is still ours",
			"content", content.Name, "error", err.Error())
		return true
	}
	if current == nil {
		return false
	}
	return current.UID == content.UID && current.DeletionTimestamp.IsZero() && !isContentTerminal(current)
}

// attemptCheckpoint runs one dump and reports what happened, writing no status.
// It imposes no deadline of its own: the caller owns the capture's budget, so
// retrying does not hand each attempt a fresh one.
func (w *NodeController) attemptCheckpoint(
	ctx context.Context,
	content *snapshotv1alpha1.PodSnapshotContent,
	pod *corev1.Pod,
	containerName, containerID string,
	containerPID int,
	cuInterposeRequested bool,
	contentUID string,
	artifactPath string,
	dumpRetriesRemaining int,
	dumpWorthRepeating func(error) (bool, string),
	onDumpRepeat func(repeat int, err error),
) (*pagebroker.PublishedArtifact, error) {
	storeID := ""
	if content.Spec.Storage != nil {
		storeID = content.Spec.Storage.StoreID
	}
	params := CheckpointParams{
		Pod:                  pod,
		ContainerName:        containerName,
		ContainerID:          containerID,
		ContainerPID:         containerPID,
		ContentUID:           contentUID,
		HostPath:             artifactPath,
		StartedAt:            time.Now(),
		CuInterposeRequested: cuInterposeRequested,
		StoreID:              storeID,
		DumpRetriesRemaining: dumpRetriesRemaining,
		DumpWorthRepeating:   dumpWorthRepeating,
		OnDumpRepeat:         onDumpRepeat,
	}
	// Success is success even past the deadline: executorCheckpoint returns nil only after it has
	// stat'd the committed artifact, and the dump has already killed the source. Failing it here
	// would discard a usable checkpoint and, being terminal, block recovery from ever promoting it.
	return w.checkpointFn(ctx, params)
}

// markCheckpointFailed makes a failed attempt terminal.
func (w *NodeController) markCheckpointFailed(
	ctx context.Context,
	content *snapshotv1alpha1.PodSnapshotContent,
	reason string,
	cause error,
) error {
	logWithStep(logr.FromContextOrDiscard(ctx), cause).Error(cause, "Checkpoint failed")
	if patchErr := w.setSnapshotContentFailed(ctx, content, reason, cause); patchErr != nil {
		return fmt.Errorf("write PodSnapshotContent failed status %q: %w", content.Name, patchErr)
	}
	return nil
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

// failCheckpointOnContainerExit fails the work order and force-terminates the pod's still-running
// containers when any checkpoint container has terminated non-zero. The bool says the caller must
// stop; the error is the status write's, so one that did not land is retried. Only call this for a
// pod this work order owns — the sweep kills every running container in it. Init containers
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

// preflightCuInterpose reads the explicit opt-in. An opted-in target must also start
// under the launcher, which SnapshotJob adds and ordinary Pods must add themselves.
func preflightCuInterpose(pod *corev1.Pod, target string) (bool, error) {
	requested, err := podcontract.ParseCuInterposeAnnotation(pod.Annotations)
	if err != nil || !requested {
		return false, err
	}
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == target {
			return true, podcontract.ValidateCuInterposeLauncher(&pod.Spec.Containers[i])
		}
	}
	return false, fmt.Errorf("cuinterpose target container %q not found", target)
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

// setSnapshotContentSucceeded patches status with Ready, the recorded source,
// and the published artifact descriptor for bound content.
func (w *NodeController) setSnapshotContentSucceeded(
	ctx context.Context,
	content *snapshotv1alpha1.PodSnapshotContent,
	source *snapshotv1alpha1.CheckpointSource,
	containerName string,
	publishedArtifact *pagebroker.PublishedArtifact,
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
	if publishedArtifact != nil {
		if content.Status.Storage == nil {
			content.Status.Storage = &snapshotv1alpha1.CheckpointStorageStatus{}
		}
		recorded := snapshotv1alpha1.PublishedContainerArtifact{
			ContainerName:  containerName,
			ArtifactHandle: publishedArtifact.GetArtifactHandle(),
		}
		replaced := false
		for i, existing := range content.Status.Storage.Artifacts {
			if existing.ContainerName == containerName {
				content.Status.Storage.Artifacts[i] = recorded
				replaced = true
				break
			}
		}
		if !replaced {
			content.Status.Storage.Artifacts = append(content.Status.Storage.Artifacts, recorded)
		}
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
// containerName and publishedArtifact are set only for a just-committed bound checkpoint.
func (w *NodeController) markCheckpointReady(
	ctx context.Context,
	content *snapshotv1alpha1.PodSnapshotContent,
	artifactPath string,
	containerName string,
	publishedArtifact *pagebroker.PublishedArtifact,
) error {
	logger := logr.FromContextOrDiscard(ctx)
	var source *snapshotv1alpha1.CheckpointSource
	if publishedArtifact == nil {
		var err error
		source, err = checkpointSourceAtPath(artifactPath)
		if err != nil {
			logger.Error(err, "Failed to read the captured source; marking Ready without it",
				"content", content.Name,
				"artifactPath", artifactPath,
			)
		}
	} else if w.fetchManifestFn != nil {
		manifest, err := w.fetchManifestFn(ctx, publishedArtifact)
		if err != nil {
			logger.Error(err, "Failed to read the published manifest; marking Ready without the source",
				"content", content.Name,
				"artifactHandle", publishedArtifact.GetArtifactHandle(),
			)
		} else {
			source = checkpointSourceFromManifest(manifest)
		}
	}
	return w.markCheckpointReadyWithSource(ctx, content, source, containerName, publishedArtifact)
}

func (w *NodeController) markCheckpointReadyWithSource(
	ctx context.Context,
	content *snapshotv1alpha1.PodSnapshotContent,
	source *snapshotv1alpha1.CheckpointSource,
	containerName string,
	publishedArtifact *pagebroker.PublishedArtifact,
) error {
	logger := logr.FromContextOrDiscard(ctx)
	ready := content
	for attempt := 0; attempt < readyStatusConflictLimit; attempt++ {
		if err := w.setSnapshotContentSucceeded(ctx, ready, source, containerName, publishedArtifact); err != nil {
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
//
// When params.StoreID is set, PageBroker's descriptor is the proof of
// success, so the filesystem verification below does not run.
func (w *NodeController) executorCheckpoint(ctx context.Context, params CheckpointParams) (*pagebroker.PublishedArtifact, error) {
	log := logr.FromContextOrDiscard(ctx)

	req := executor.CheckpointRequest{
		ContainerID:          params.ContainerID,
		ContainerName:        params.ContainerName,
		ContentUID:           params.ContentUID,
		StartedAt:            params.StartedAt,
		NodeName:             w.config.NodeName,
		PodName:              params.Pod.Name,
		PodNamespace:         params.Pod.Namespace,
		PodIP:                params.Pod.Status.PodIP,
		Pod:                  podEnvironment(params.Pod, params.ContainerName),
		Clientset:            w.clientset,
		CuInterposeRequested: params.CuInterposeRequested,
		StoreID:              params.StoreID,
		DumpRetriesRemaining: params.DumpRetriesRemaining,
		DumpWorthRepeating:   params.DumpWorthRepeating,
		OnDumpRepeat:         params.OnDumpRepeat,
	}
	// Recovering the source belongs to runCheckpoint, which is the only place that
	// knows whether another attempt follows.
	artifact, err := executor.Checkpoint(ctx, w.runtime, log, req, w.config)
	if err != nil {
		return nil, fmt.Errorf("checkpoint: %w", err)
	}

	if params.StoreID != "" {
		return artifact, nil
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
		return nil, verifyErr
	}

	return nil, nil
}

// killCheckpointProcess SIGKILLs the CUDA-locked process so it does not hang after a failed dump.
// ESRCH (already exited) is success. Any other signal error is returned so callers can fail closed.
func (w *NodeController) killCheckpointProcess(log logr.Logger, pid int, reason string) error {
	if err := w.sendSignalFn(log, pid, syscall.SIGKILL, reason); err != nil {
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
