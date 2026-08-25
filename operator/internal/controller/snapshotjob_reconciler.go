// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

// +kubebuilder:rbac:groups=nvidia.com,resources=snapshotjobs,verbs=get;list;watch
// +kubebuilder:rbac:groups=nvidia.com,resources=snapshotjobs/status,verbs=patch
// +kubebuilder:rbac:groups=nvidia.com,resources=podsnapshots,verbs=create;get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=create;get;list;watch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=list
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

// SnapshotJobReconciler reconciles a SnapshotJob.
//
// Resource helpers create, find, and classify the Job, source Pod, and
// PodSnapshot without mutating SnapshotJob status. Reconcile then derives the
// complete status from those observations and persists it once. Completion is
// a two-stage gate: a durably Ready PodSnapshot sets Captured=True, and the
// SnapshotJob then waits for the source Job to finish before Completed=True
// and Job deletion — the checkpoint terminates the target container (its exit
// is expected, so the Job reports Failed on every successful capture), but
// helper containers must run to completion with exit 0 first, or cleanup
// would kill them mid-work. Failures preserve the Job for debugging.
type SnapshotJobReconciler struct {
	client.Client
	NonCacheReadClient client.Reader
	Recorder           record.EventRecorder
}

type snapshotJobFailure struct {
	reason string
	cause  error
}

type snapshotJobObservation struct {
	job              *batchv1.Job
	podSnapshot      *snapshotv1alpha1.PodSnapshot
	sourcePodMissing bool
	// sourceJobFinished is true once the capture is Ready AND the source Job
	// reached a terminal state with every non-target container exited 0 — the
	// second stage of the completion gate.
	sourceJobFinished bool
	failure           *snapshotJobFailure
}

// PodSnapshot failure reasons consumed by SnapshotJob. These values are part
// of the downstream condition contract; keep their classification in one place
// until PodSnapshot exposes shared reason constants.
const (
	podSnapshotReasonContentConflict   = "ContentConflict"
	podSnapshotReasonSourcePodNotFound = "SourcePodNotFound"
	podSnapshotReasonStalePodReference = "StalePodReference"
)

// Reconcile first drives child resources toward the desired state, then derives
// and patches SnapshotJob status once from the resulting observation. Failed is
// terminal, while Completed keeps retrying successful Job cleanup until it is
// confirmed gone.
func (r *SnapshotJobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	sj := &snapshotv1alpha1.SnapshotJob{}
	if err := r.Get(ctx, req.NamespacedName, sj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !sj.GetDeletionTimestamp().IsZero() {
		return ctrl.Result{}, nil
	}
	if snapshotv1alpha1.IsSnapshotJobFailed(sj) {
		return ctrl.Result{}, nil
	}
	if snapshotv1alpha1.IsSnapshotJobCompleted(sj) {
		return r.ensureJobDeleted(ctx, sj)
	}

	observed, result, err := r.reconcileResources(ctx, sj)
	if err != nil {
		return result, err
	}
	if observed.failure != nil {
		r.Recorder.Event(sj, corev1.EventTypeWarning, observed.failure.reason, observed.failure.cause.Error())
	}

	reconciliationTime := metav1.Now()
	desiredStatus := deriveSnapshotJobStatus(sj, observed, reconciliationTime)
	if err := r.patchSnapshotJobStatus(ctx, sj, desiredStatus); err != nil {
		return ctrl.Result{}, err
	}
	if meta.IsStatusConditionTrue(desiredStatus.Conditions, snapshotv1alpha1.SnapshotJobConditionCompleted) {
		// Cleanup in this reconcile must use the UID that was just persisted,
		// even though sj is the pre-patch object returned by the original Get.
		completed := sj.DeepCopy()
		completed.Status = desiredStatus
		return r.ensureJobDeleted(ctx, completed)
	}
	return result, nil
}

// reconcileResources observes an existing source Job or builds and creates it
// when absent, then reconciles its PodSnapshot. Retryable API failures are
// returned as errors; invalid input on the create path and deterministic-name
// conflicts are typed observations.
func (r *SnapshotJobReconciler) reconcileResources(ctx context.Context, sj *snapshotv1alpha1.SnapshotJob) (snapshotJobObservation, ctrl.Result, error) {
	job := &batchv1.Job{}
	err := r.Get(ctx, client.ObjectKey{Namespace: sj.Namespace, Name: sj.Name}, job)
	switch {
	case apierrors.IsNotFound(err):
		if sj.Status.SourceJobUID != "" || sj.Status.PodSnapshotName != "" {
			authoritativeJob, failure, readErr := r.readAuthoritativeSourceJob(ctx, sj)
			if readErr != nil {
				return snapshotJobObservation{}, ctrl.Result{}, fmt.Errorf(
					"confirm source Job %q after cached NotFound: %w", sj.Name, readErr)
			}
			if failure != nil {
				return snapshotJobObservation{failure: failure}, ctrl.Result{}, nil
			}
			return r.reconcileAcceptedSourceJob(ctx, sj, authoritativeJob)
		}
		desiredJob, buildErr := buildSourceJob(sj)
		if buildErr != nil {
			return terminalObservation(snapshotv1alpha1.ReasonInvalidSpec, buildErr), ctrl.Result{}, nil
		}
		return r.createSourceJob(ctx, sj, desiredJob)
	case err != nil:
		return snapshotJobObservation{}, ctrl.Result{}, fmt.Errorf("get source Job %q: %w", sj.Name, err)
	default:
		return r.reconcileExistingSourceJob(ctx, sj, job)
	}
}

// reconcilePodSnapshotResources drives the capture and arbitrates it against
// the source Job. Once a PodSnapshot exists, its terminal result decides the
// SnapshotJob: Ready completes it (regardless of the Job's state), Failed fails
// it with the capture's own reason, and a pending capture is waited on with a
// bounded requeue even when the Job is already terminal — the checkpoint
// terminates the source process, so a Job failure racing a committed artifact
// is the expected success sequence, not an error. Only before a capture exists
// does a terminal source Job fail the SnapshotJob directly.
func (r *SnapshotJobReconciler) reconcilePodSnapshotResources(ctx context.Context, sj *snapshotv1alpha1.SnapshotJob, job *batchv1.Job) (snapshotJobObservation, ctrl.Result, error) {
	observed := snapshotJobObservation{job: job}
	snap, err := r.findOwnedPodSnapshot(ctx, sj)
	if apierrors.IsNotFound(err) && (sj.Status.PodSnapshotName != "" || sj.Status.PodSnapshotUID != "") {
		// The recorded capture arbitrates even against a terminal source Job, so
		// the cache miss must be authoritatively confirmed before any decision.
		snap, err = r.readAuthoritativeOwnedPodSnapshot(ctx, sj)
		if apierrors.IsNotFound(err) {
			observed.failure = &snapshotJobFailure{
				reason: snapshotv1alpha1.ReasonPodSnapshotDeleted,
				cause: fmt.Errorf("PodSnapshot %q (uid %q) no longer exists",
					sj.Status.PodSnapshotName, sj.Status.PodSnapshotUID),
			}
			return observed, ctrl.Result{}, nil
		}
	}
	switch {
	case errors.Is(err, errPodSnapshotNameConflict):
		observed.failure = &snapshotJobFailure{reason: snapshotv1alpha1.ReasonPodSnapshotNameConflict, cause: err}
		return observed, ctrl.Result{}, nil
	case apierrors.IsNotFound(err):
		terminal := classifySourceJobTerminal(job)
		if terminal.failure != nil {
			observed.failure = terminal.failure
			return observed, ctrl.Result{}, nil
		}
		if terminal.state != sourceJobComplete {
			observed, result, err := r.createPodSnapshotForSourceJob(ctx, sj, job)
			if err != nil {
				return observed, result, err
			}
			if observed.failure == nil {
				observed.failure = snapshotJobTerminalFailure(observed.job, observed.podSnapshot)
			}
			return observed, result, nil
		}
		// The source Job is complete, so a capture created now can never succeed.
		// Confirm the capture's absence authoritatively before the sticky failure.
		snap, err = r.readAuthoritativeOwnedPodSnapshot(ctx, sj)
		switch {
		case apierrors.IsNotFound(err):
			observed.failure = &snapshotJobFailure{
				reason: snapshotv1alpha1.ReasonSourceCompletedWithoutCapture,
				cause:  fmt.Errorf("source Job completed before a PodSnapshot capture was created"),
			}
			return observed, ctrl.Result{}, nil
		case errors.Is(err, errPodSnapshotNameConflict):
			observed.failure = &snapshotJobFailure{reason: snapshotv1alpha1.ReasonPodSnapshotNameConflict, cause: err}
			return observed, ctrl.Result{}, nil
		case err != nil:
			return snapshotJobObservation{}, ctrl.Result{}, err
		}
		// The capture exists after all; fall through and treat it as authoritative.
	case err != nil:
		return snapshotJobObservation{}, ctrl.Result{}, fmt.Errorf("find owned PodSnapshot: %w", err)
	}
	if sj.Status.PodSnapshotUID == "" {
		failure, err := r.validatePodSnapshotForAdoption(ctx, sj, job, snap)
		if err != nil {
			return snapshotJobObservation{}, ctrl.Result{}, err
		}
		if failure != nil {
			observed.failure = failure
			return observed, ctrl.Result{}, nil
		}
	}

	observed.podSnapshot = snap
	observed.failure = snapshotJobTerminalFailure(observed.job, snap)
	if observed.failure != nil {
		return observed, ctrl.Result{}, nil
	}
	if snapshotv1alpha1.IsPodSnapshotSucceeded(snap) {
		return r.reconcileCapturedSourceJob(ctx, sj, observed)
	}
	if classifySourceJobTerminal(observed.job).state != sourceJobActive {
		// The source Job is terminal but the capture is still resolving. The
		// PodSnapshot watch is the normal wake-up; the requeue is a backstop for
		// a missed event. The wait itself is bounded by the PodSnapshot
		// reconciler, which fails an abandoned pending capture once the source
		// pod has been terminal past its grace window.
		return observed, ctrl.Result{RequeueAfter: captureResolutionBackstop}, nil
	}
	return observed, ctrl.Result{}, nil
}

// captureResolutionBackstop re-checks a capture whose resolution depends on a
// watch event (a pending capture with a terminal Job, or a Ready capture with
// a still-running Job), in case that event is missed.
const captureResolutionBackstop = 30 * time.Second

// reconcileCapturedSourceJob applies the second stage of the completion gate.
// The artifact is durable, but cleanup must wait for the source Job to finish:
// the checkpoint killed the target container (expected), while helper
// containers may still be doing real work (e.g. a GMS saver writing weight
// artifacts) — deleting the Job now would kill them mid-work. Success requires
// every non-target container to exit 0; an explicit deadline still fails the
// run, because it bounds the whole source lifecycle, helpers included.
func (r *SnapshotJobReconciler) reconcileCapturedSourceJob(ctx context.Context, sj *snapshotv1alpha1.SnapshotJob, observed snapshotJobObservation) (snapshotJobObservation, ctrl.Result, error) {
	terminal := classifySourceJobTerminal(observed.job)
	switch terminal.state {
	case sourceJobComplete:
		// Every container exited 0; nothing to inspect.
		observed.sourceJobFinished = true
		return observed, ctrl.Result{}, nil
	case sourceJobDeadlineExceeded:
		observed.failure = terminal.failure
		return observed, ctrl.Result{}, nil
	case sourceJobFailed:
		failure, helpersStillRunning, err := r.classifyHelperContainers(ctx, sj, observed.job)
		if err != nil {
			return snapshotJobObservation{}, ctrl.Result{}, err
		}
		if helpersStillRunning {
			// The Job can report Failed before the kubelet publishes the
			// helpers' final states, so wait for pod status to converge.
			return observed, ctrl.Result{RequeueAfter: captureResolutionBackstop}, nil
		}
		observed.failure = failure
		observed.sourceJobFinished = failure == nil
		return observed, ctrl.Result{}, nil
	default: // sourceJobActive: helpers still running.
		// The owned-Job watch is the normal wake-up; the requeue is a backstop.
		return observed, ctrl.Result{RequeueAfter: captureResolutionBackstop}, nil
	}
}

// classifyHelperContainers verifies that every expected non-target container of
// the failed source Job finished cleanly. The target container's exit is
// ignored — the checkpoint terminates it by design, which is also why the
// source Job reports Failed on every successful capture. Expected helpers are
// enumerated from the pod template, not from the observed statuses, so a
// helper the kubelet has not reported yet counts as still running rather than
// silently done. Regular helpers must exit 0; native sidecars only need a
// terminated state — the kubelet SIGTERMs them after the regular containers
// finish, so their exit code reflects shutdown, not helper work.
func (r *SnapshotJobReconciler) classifyHelperContainers(ctx context.Context, sj *snapshotv1alpha1.SnapshotJob, job *batchv1.Job) (failure *snapshotJobFailure, helpersStillRunning bool, err error) {
	targetContainer, err := snapshotJobTargetContainer(sj)
	if err != nil {
		return &snapshotJobFailure{reason: snapshotv1alpha1.ReasonInvalidSpec, cause: err}, false, nil
	}
	helpers, sidecars := expectedHelperContainers(&sj.Spec.PodTemplate.Spec, targetContainer)
	if len(helpers) == 0 && len(sidecars) == 0 {
		return nil, false, nil // the target is the only container
	}
	pod, found, err := findSourcePod(ctx, r.NonCacheReadClient, job)
	if err != nil {
		return nil, false, fmt.Errorf("find source pod to verify helper containers: %w", err)
	}
	if !found {
		return &snapshotJobFailure{
			reason: snapshotv1alpha1.ReasonJobFailed,
			cause:  errors.New("source pod is gone before its helper containers could be verified"),
		}, false, nil
	}
	for _, name := range helpers {
		terminated := containerTerminatedState(pod.Status.ContainerStatuses, name)
		if terminated == nil {
			return nil, true, nil
		}
		if terminated.ExitCode != 0 {
			return &snapshotJobFailure{
				reason: snapshotv1alpha1.ReasonJobFailed,
				cause: fmt.Errorf("helper container %q exited with code %d after the capture succeeded",
					name, terminated.ExitCode),
			}, false, nil
		}
	}
	for _, name := range sidecars {
		if containerTerminatedState(pod.Status.InitContainerStatuses, name) == nil {
			return nil, true, nil
		}
	}
	return nil, false, nil
}

// expectedHelperContainers splits the pod template's non-target containers into
// regular helpers and native sidecars (init containers with restartPolicy
// Always, which report under InitContainerStatuses).
func expectedHelperContainers(spec *corev1.PodSpec, targetContainer string) (helpers, sidecars []string) {
	for i := range spec.Containers {
		if spec.Containers[i].Name != targetContainer {
			helpers = append(helpers, spec.Containers[i].Name)
		}
	}
	for i := range spec.InitContainers {
		c := &spec.InitContainers[i]
		if c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			sidecars = append(sidecars, c.Name)
		}
	}
	return helpers, sidecars
}

// containerTerminatedState returns the named container's terminated state, or
// nil when the container has no status entry yet or has not terminated.
func containerTerminatedState(statuses []corev1.ContainerStatus, name string) *corev1.ContainerStateTerminated {
	for i := range statuses {
		if statuses[i].Name == name {
			return statuses[i].State.Terminated
		}
	}
	return nil
}

func terminalObservation(reason string, cause error) snapshotJobObservation {
	return snapshotJobObservation{failure: &snapshotJobFailure{reason: reason, cause: cause}}
}

// deriveSnapshotJobStatus is a pure derivation over current status and observed
// resources. Existing timestamps are monotonic; conditions and references are
// reconstructed whenever their source resource is observed.
func deriveSnapshotJobStatus(sj *snapshotv1alpha1.SnapshotJob, observed snapshotJobObservation, reconciliationTime metav1.Time) snapshotv1alpha1.SnapshotJobStatus {
	next := sj.DeepCopy()
	if next.Status.SourceJobUID == "" && observed.job != nil {
		next.Status.SourceJobUID = observed.job.UID
	}
	initializeConditions(next, reconciliationTime)
	deriveRunningStatus(next, observed, reconciliationTime)
	failure := deriveCapturedStatus(next, observed, reconciliationTime)
	if failure != nil {
		deriveFailureStatus(next, failure, reconciliationTime)
		return next.Status
	}
	deriveCompletionStatus(next, observed, reconciliationTime)
	return next.Status
}

// initializeConditions makes every condition type present from the first
// status write. Conditions are append-only: once present they are only
// updated (status/reason/message), never removed, so consumers can always
// distinguish "known False" from "not yet evaluated". Reconcile passes after
// the first are no-ops here because SetStatusCondition never deletes.
func initializeConditions(next *snapshotv1alpha1.SnapshotJob, reconciliationTime metav1.Time) {
	defaults := []struct {
		conditionType string
		reason        string
		message       string
	}{
		{snapshotv1alpha1.SnapshotJobConditionRunning, snapshotv1alpha1.ReasonPodPending, "waiting for the source pod to become ready"},
		{snapshotv1alpha1.SnapshotJobConditionCaptured, snapshotv1alpha1.ReasonCaptureInProgress, "waiting for the checkpoint capture to start"},
		{snapshotv1alpha1.SnapshotJobConditionCompleted, snapshotv1alpha1.ReasonCaptureInProgress, "waiting for the checkpoint capture to complete"},
		{snapshotv1alpha1.SnapshotJobConditionFailed, snapshotv1alpha1.ReasonNoFailure, "no failure observed"},
	}
	for _, def := range defaults {
		if meta.FindStatusCondition(next.Status.Conditions, def.conditionType) == nil {
			setCondition(next, def.conditionType, metav1.ConditionFalse, reconciliationTime, def.reason, def.message)
		}
	}
}

func deriveRunningStatus(next *snapshotv1alpha1.SnapshotJob, observed snapshotJobObservation, reconciliationTime metav1.Time) {
	if observed.job == nil {
		if observed.failure != nil {
			setCondition(next, snapshotv1alpha1.SnapshotJobConditionRunning, metav1.ConditionFalse, reconciliationTime,
				observed.failure.reason, "source pod is unavailable: "+observed.failure.cause.Error())
		}
		return
	}

	ready := observed.job.Status.Ready != nil && *observed.job.Status.Ready > 0
	if ready && next.Status.StartedAt == nil {
		next.Status.StartedAt = &reconciliationTime
	}

	terminal := classifySourceJobTerminal(observed.job)
	switch terminal.state {
	case sourceJobActive:
		// Continue below and derive current readiness.
	case sourceJobComplete:
		setCondition(next, snapshotv1alpha1.SnapshotJobConditionRunning, metav1.ConditionFalse, reconciliationTime,
			snapshotv1alpha1.ReasonJobCompleted, "source Job completed")
		return
	case sourceJobFailed, sourceJobDeadlineExceeded:
		setCondition(next, snapshotv1alpha1.SnapshotJobConditionRunning, metav1.ConditionFalse, reconciliationTime,
			terminal.failure.reason, terminal.failure.cause.Error())
		return
	default:
		setCondition(next, snapshotv1alpha1.SnapshotJobConditionRunning, metav1.ConditionFalse, reconciliationTime,
			snapshotv1alpha1.ReasonPodPending, fmt.Sprintf("source Job has unknown state %d", terminal.state))
		return
	}

	if ready {
		setCondition(next, snapshotv1alpha1.SnapshotJobConditionRunning, metav1.ConditionTrue, reconciliationTime,
			snapshotv1alpha1.ReasonPodReady, "source pod is ready")
		return
	}

	message := "waiting for the source pod to become ready"
	if observed.sourcePodMissing {
		message = "waiting for the source Job to create a pod"
	}
	setCondition(next, snapshotv1alpha1.SnapshotJobConditionRunning, metav1.ConditionFalse, reconciliationTime,
		snapshotv1alpha1.ReasonPodPending, message)
}

func deriveCapturedStatus(next *snapshotv1alpha1.SnapshotJob, observed snapshotJobObservation, reconciliationTime metav1.Time) *snapshotJobFailure {
	failure := observed.failure
	if observed.podSnapshot != nil {
		next.Status.PodSnapshotName = observed.podSnapshot.Name
		if next.Status.PodSnapshotUID == "" {
			next.Status.PodSnapshotUID = observed.podSnapshot.UID
		}
		switch {
		case snapshotv1alpha1.IsPodSnapshotFailed(observed.podSnapshot):
			if failure == nil {
				reason, message := captureFailureReason(observed.podSnapshot)
				failure = &snapshotJobFailure{reason: reason, cause: errors.New(message)}
			}
		case snapshotv1alpha1.IsPodSnapshotSucceeded(observed.podSnapshot):
			setCondition(next, snapshotv1alpha1.SnapshotJobConditionCaptured, metav1.ConditionTrue, reconciliationTime,
				snapshotv1alpha1.ReasonCaptureCompleted, "CRIU dump of the target container is complete")
		default:
			setCondition(next, snapshotv1alpha1.SnapshotJobConditionCaptured, metav1.ConditionFalse, reconciliationTime,
				snapshotv1alpha1.ReasonCaptureInProgress, "waiting for the node agent to capture the checkpoint")
		}
	}
	return failure
}

func deriveFailureStatus(next *snapshotv1alpha1.SnapshotJob, failure *snapshotJobFailure, reconciliationTime metav1.Time) {
	// A terminal object never reconciles again, so a Running=True left over
	// from the last observation would advertise a live source forever. Close
	// it out; an already-False Running keeps its more specific last reason.
	if meta.IsStatusConditionTrue(next.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionRunning) {
		setCondition(next, snapshotv1alpha1.SnapshotJobConditionRunning, metav1.ConditionFalse, reconciliationTime,
			failure.reason, "the SnapshotJob failed terminally; the source Job is preserved for debugging")
	}
	if !meta.IsStatusConditionTrue(next.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionCaptured) {
		setCondition(next, snapshotv1alpha1.SnapshotJobConditionCaptured, metav1.ConditionFalse, reconciliationTime,
			failure.reason, "capture did not complete: "+failure.cause.Error())
	}
	if !meta.IsStatusConditionTrue(next.Status.Conditions, snapshotv1alpha1.SnapshotJobConditionCompleted) {
		setCondition(next, snapshotv1alpha1.SnapshotJobConditionCompleted, metav1.ConditionFalse, reconciliationTime,
			failure.reason, "the SnapshotJob failed before completing: "+failure.cause.Error())
	}
	setCondition(next, snapshotv1alpha1.SnapshotJobConditionFailed, metav1.ConditionTrue, reconciliationTime,
		failure.reason, failure.cause.Error())
	if next.Status.CompletedAt == nil {
		next.Status.CompletedAt = &reconciliationTime
	}
}

// deriveCompletionStatus applies the two-stage completion gate to status. A
// durable capture alone yields Completed=False/WaitingForPodCompletion: the
// artifact is ready, but the source pod's other containers may still be doing
// real work and cleanup must not kill them. Only once the source Job finished
// (sourceJobFinished, verified by reconcileCapturedSourceJob) does Completed
// flip True — and Running is closed out with the same reason so a terminal
// SnapshotJob does not advertise the expected target kill as JobFailed.
func deriveCompletionStatus(next *snapshotv1alpha1.SnapshotJob, observed snapshotJobObservation, reconciliationTime metav1.Time) {
	if observed.podSnapshot == nil || !snapshotv1alpha1.IsPodSnapshotSucceeded(observed.podSnapshot) {
		return
	}
	if !observed.sourceJobFinished {
		// The target is dead by design, so deriveRunningStatus's job-derived
		// view ("waiting for the source pod to become ready" / "source Job
		// failed") misdescribes this phase; say what is actually happening.
		setCondition(next, snapshotv1alpha1.SnapshotJobConditionRunning, metav1.ConditionFalse, reconciliationTime,
			snapshotv1alpha1.ReasonWaitingForPodCompletion,
			"checkpoint captured; the source pod's remaining containers are finishing")
		setCondition(next, snapshotv1alpha1.SnapshotJobConditionCompleted, metav1.ConditionFalse, reconciliationTime,
			snapshotv1alpha1.ReasonWaitingForPodCompletion,
			"checkpoint captured; waiting for the source pod's remaining containers to finish")
		return
	}
	setCondition(next, snapshotv1alpha1.SnapshotJobConditionRunning, metav1.ConditionFalse, reconciliationTime,
		snapshotv1alpha1.ReasonJobCompleted, "source Job finished")
	setCondition(next, snapshotv1alpha1.SnapshotJobConditionCompleted, metav1.ConditionTrue, reconciliationTime,
		snapshotv1alpha1.ReasonJobCompleted, "checkpoint captured and source workload finished; source Job cleanup initiated")
	if next.Status.CompletedAt == nil {
		next.Status.CompletedAt = &reconciliationTime
	}
}

// captureFailureReason separates bind-stage PodSnapshot failures from failures
// reported by the node agent while capturing the checkpoint.
func captureFailureReason(snap *snapshotv1alpha1.PodSnapshot) (reason, message string) {
	condition := meta.FindStatusCondition(snap.Status.Conditions, snapshotv1alpha1.PodSnapshotConditionFailed)
	if condition == nil {
		return snapshotv1alpha1.ReasonCaptureFailed, "PodSnapshot Failed=True with no condition detail"
	}
	switch condition.Reason {
	case podSnapshotReasonContentConflict,
		podSnapshotReasonSourcePodNotFound,
		podSnapshotReasonStalePodReference:
		return snapshotv1alpha1.ReasonPodSnapshotFailed, condition.Message
	case snapshotv1alpha1.ReasonSourceCompletedWithoutCapture:
		return snapshotv1alpha1.ReasonSourceCompletedWithoutCapture, condition.Message
	default:
		return snapshotv1alpha1.ReasonCaptureFailed, condition.Message
	}
}

func (r *SnapshotJobReconciler) patchSnapshotJobStatus(ctx context.Context, sj *snapshotv1alpha1.SnapshotJob, desired snapshotv1alpha1.SnapshotJobStatus) error {
	if apiequality.Semantic.DeepEqual(sj.Status, desired) {
		return nil
	}
	updated := sj.DeepCopy()
	updated.Status = desired
	if err := r.Status().Patch(ctx, updated, client.MergeFrom(sj)); err != nil {
		return fmt.Errorf("patch SnapshotJob status: %w", err)
	}
	return nil
}

// setCondition sets a status condition on the SnapshotJob.
func setCondition(sj *snapshotv1alpha1.SnapshotJob, condType string, status metav1.ConditionStatus, reconciliationTime metav1.Time, reason, message string) {
	meta.SetStatusCondition(&sj.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		ObservedGeneration: sj.Generation,
		LastTransitionTime: reconciliationTime,
		Reason:             reason,
		Message:            message,
	})
}

// SetupWithManager wires the controller: it owns the batch/v1 Job it creates
// and watches PodSnapshot via a label map function because capture artifacts
// deliberately carry no ownerReference and must outlive the SnapshotJob.
func (r *SnapshotJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.NonCacheReadClient == nil {
		return errors.New("snapshot job reconciler requires a non-cache read client")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&snapshotv1alpha1.SnapshotJob{}).
		Owns(&batchv1.Job{}, builder.WithPredicates(predicate.Funcs{
			CreateFunc:  func(event.CreateEvent) bool { return false },
			UpdateFunc:  func(event.UpdateEvent) bool { return true },
			DeleteFunc:  func(event.DeleteEvent) bool { return true },
			GenericFunc: func(event.GenericEvent) bool { return true },
		})).
		Watches(&snapshotv1alpha1.PodSnapshot{},
			handler.EnqueueRequestsFromMapFunc(mapPodSnapshotToSnapshotJob),
			builder.WithPredicates(predicate.Funcs{
				CreateFunc:  func(event.CreateEvent) bool { return false },
				UpdateFunc:  func(event.UpdateEvent) bool { return true },
				DeleteFunc:  func(event.DeleteEvent) bool { return true },
				GenericFunc: func(event.GenericEvent) bool { return false },
			})).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}
