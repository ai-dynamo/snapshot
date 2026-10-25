// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package maintenance

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/ai-dynamo/snapshot/api/podcontract"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/go-logr/logr"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// processNonTargetArtifactSweep requires positive, authoritative evidence of failure.
// Absence of a SnapshotJob does not imply failure: successful non-target artifacts
// must outlive their SnapshotJob along with its independently owned PodSnapshot.
// Unknown roots are retained for verified manual cleanup.
func (q *Queue) processNonTargetArtifactSweep(ctx context.Context, backend Backend, logger logr.Logger) error {
	nonTargetBackend, supported := backend.(NonTargetArtifactBackend)
	if !supported {
		return nil // non-target artifacts are owned only by a supporting store
	}
	candidates, err := nonTargetBackend.NonTargetArtifactCandidates(ctx, logger)
	if err != nil {
		return err
	}
	if len(candidates) == 0 {
		q.nonTargetScan = nil
		clear(q.nonTargetRetentionWarned)
		return nil
	}
	for uid := range q.nonTargetRetentionWarned {
		if _, present := candidates[string(uid)]; !present {
			delete(q.nonTargetRetentionWarned, uid)
		}
	}

	// Enumerate before the API list. A new attempt creates its SnapshotJob
	// before its non-target writes, so this ordering never guesses ownership from
	// an old list or from a reusable resource name.
	jobs, complete, err := q.nonTargetArtifactJobs(ctx, candidates)
	if err != nil {
		// A failed page retains the last successful continuation. Retry with a
		// fresh work-item deadline instead of waiting for the scan interval,
		// which can outlive the API server's continuation token.
		if q.queue != nil {
			q.queue.AddAfter(newNonTargetSweepKey(), nonTargetScanRetryDelay)
		}
		return err
	}
	if !complete {
		if q.queue != nil {
			q.queue.AddAfter(newNonTargetSweepKey(), nonTargetScanContinuationDelay)
		}
		return nil
	}
	var cleanupErrors []error
	start := 0
	for start < len(jobs) && jobs[start].UID <= q.nonTargetCursor {
		start++
	}
	for i := 0; i < len(jobs) && i < q.config.BatchSize; i++ {
		candidate := jobs[(start+i)%len(jobs)]
		q.nonTargetCursor = candidate.UID
		sj := &snapshotv1alpha1.SnapshotJob{}
		if err := q.apiReader.Get(ctx, client.ObjectKey{Namespace: candidate.Namespace, Name: candidate.Name}, sj); err != nil {
			if !apierrors.IsNotFound(err) {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("read SnapshotJob for non-target artifact cleanup: %w", err))
			}
			continue
		}
		if sj.UID != candidate.UID {
			continue // same name, different attempt since the metadata snapshot
		}
		if sj.Spec.NonTargetArtifacts == nil || sj.Spec.NonTargetArtifacts.OnFailure != snapshotv1alpha1.NonTargetArtifactCleanup {
			continue // storage ownership alone is not permission to delete bytes
		}
		uid := string(sj.UID)
		if !snapshotv1alpha1.IsSnapshotJobFailed(sj) || snapshotv1alpha1.IsSnapshotJobCompleted(sj) {
			continue
		}
		if len(sj.Spec.NonTargetArtifacts.Containers) == 0 {
			logger.Info("Retaining failed SnapshotJob non-target artifacts: no declared writers", "snapshot_job_uid", uid)
			continue // legacy/user-owned paths are outside this contract
		}
		stopped, reason, err := q.nonTargetArtifactWritersStopped(ctx, sj)
		if err != nil {
			cleanupErrors = append(cleanupErrors, err)
			continue
		}
		if !stopped {
			logger.Info("Retaining failed SnapshotJob non-target artifacts until writer termination is proven", "snapshot_job_uid", uid, "reason", reason)
			if permanentNonTargetRetention(reason) {
				if q.nonTargetRetentionWarned == nil {
					q.nonTargetRetentionWarned = make(map[types.UID]struct{})
				}
				if _, warned := q.nonTargetRetentionWarned[sj.UID]; !warned {
					q.recorder.Eventf(sj, corev1.EventTypeWarning, "NonTargetArtifactsRetained",
						"Retaining attempt %s: %s; verify attempt identity and writer exit before manually removing its UID-scoped artifacts", uid, reason)
					q.nonTargetRetentionWarned[sj.UID] = struct{}{}
				}
			}
			continue
		}
		if err := nonTargetBackend.DeleteNonTargetArtifacts(ctx, uid); err != nil {
			cleanupErrors = append(cleanupErrors, err)
			logger.Error(err, "Unable to reclaim failed SnapshotJob non-target artifacts", "snapshot_job_uid", uid)
			continue
		}
		logger.Info("Reclaimed failed SnapshotJob non-target artifacts", "snapshot_job_uid", uid, "namespace", sj.Namespace, "name", sj.Name)
		delete(q.nonTargetRetentionWarned, sj.UID)
		q.recorder.Event(sj, corev1.EventTypeNormal, "NonTargetArtifactsReclaimed",
			"Reclaimed this failed attempt's managed non-target artifacts after all source containers stopped; source Job and capture failure evidence retained")
	}
	return errors.Join(cleanupErrors...)
}

type nonTargetJobKey struct {
	Namespace string
	Name      string
	UID       types.UID
}

type nonTargetJobScan struct {
	candidates      map[string]struct{}
	jobs            []nonTargetJobKey
	continueToken   string
	resourceVersion string
}

const nonTargetJobListPageLimit int64 = 500
const nonTargetJobPagesPerSweep = 4
const nonTargetScanContinuationDelay = 100 * time.Millisecond
const nonTargetScanRetryDelay = 5 * time.Second
const noPodWriterProofReason = "source Pod record is absent; writer exit cannot be proven across Kubernetes versions"
const sourcePodIdentityReason = "original source Pod identity or termination is not proven"
const missingSourceJobUIDReason = "no source Job UID was recorded"
const absentSourceJobReason = "source Job is absent; disappearance is not writer-exit proof"
const sourceJobIdentityMismatchReason = "source Job identity or one-shot policy does not match the attempt"
const writerDeclarationMismatchReason = "source Job writer declaration or attempt binding does not match"
const writerEnvironmentMismatchReason = "declared writer lacks the exact attempt UID and storage subdirectory"

func permanentNonTargetRetention(reason string) bool {
	switch reason {
	case noPodWriterProofReason, sourcePodIdentityReason, missingSourceJobUIDReason, absentSourceJobReason, sourceJobIdentityMismatchReason,
		writerDeclarationMismatchReason, writerEnvironmentMismatchReason:
		return true
	default:
		return false
	}
}

// nonTargetArtifactJobs pages only SnapshotJob metadata and carries its
// continuation across work items. No deletion is considered until the whole
// consistent list has completed; full objects are fetched for a bounded batch.
func (q *Queue) nonTargetArtifactJobs(ctx context.Context, candidates map[string]struct{}) ([]nonTargetJobKey, bool, error) {
	if q.nonTargetScan == nil {
		q.nonTargetScan = &nonTargetJobScan{candidates: candidates}
	}
	scan := q.nonTargetScan
	for page := 0; page < nonTargetJobPagesPerSweep; page++ {
		var list *metav1.PartialObjectMetadataList
		var err error
		for attempt := 0; attempt < q.config.ListAttempts; attempt++ {
			list = &metav1.PartialObjectMetadataList{}
			list.SetGroupVersionKind(snapshotv1alpha1.GroupVersion.WithKind("SnapshotJobList"))
			err = q.apiReader.List(ctx, list, &client.ListOptions{Limit: nonTargetJobListPageLimit, Continue: scan.continueToken})
			if err == nil || ctx.Err() != nil {
				break
			}
		}
		if err != nil {
			if apierrors.IsResourceExpired(err) || apierrors.IsGone(err) {
				q.nonTargetScan = nil // the consistent-list snapshot has expired
			}
			return nil, false, fmt.Errorf("list SnapshotJob metadata for non-target cleanup: %w", err)
		}
		if scan.continueToken != "" && list.ResourceVersion != scan.resourceVersion {
			q.nonTargetScan = nil
			return nil, false, fmt.Errorf("SnapshotJob metadata resource version changed across pages")
		}
		if scan.resourceVersion == "" {
			scan.resourceVersion = list.ResourceVersion
		}
		for i := range list.Items {
			job := &list.Items[i]
			if job.UID == "" {
				q.nonTargetScan = nil
				return nil, false, fmt.Errorf("SnapshotJob %s/%s has no UID", job.Namespace, job.Name)
			}
			if _, found := scan.candidates[string(job.UID)]; found {
				scan.jobs = append(scan.jobs, nonTargetJobKey{Namespace: job.Namespace, Name: job.Name, UID: job.UID})
			}
		}
		if list.Continue == "" {
			slices.SortFunc(scan.jobs, func(a, b nonTargetJobKey) int { return cmp.Compare(a.UID, b.UID) })
			q.nonTargetScan = nil
			return scan.jobs, true, nil
		}
		if list.Continue == scan.continueToken || scan.resourceVersion == "" {
			q.nonTargetScan = nil
			return nil, false, fmt.Errorf("SnapshotJob metadata list has invalid continuation")
		}
		scan.continueToken = list.Continue
	}
	return nil, false, nil
}

// nonTargetArtifactWritersStopped waits for both the one-shot Job and every matching Pod.
// Job failure alone is insufficient: non-target containers can still be publishing while the
// target is already dead. Authoritative reads keep stale caches from approving
// irreversible deletion, including native sidecars which can restart.
func (q *Queue) nonTargetArtifactWritersStopped(ctx context.Context, sj *snapshotv1alpha1.SnapshotJob) (bool, string, error) {
	if sj.Status.SourceJobUID == "" {
		return false, missingSourceJobUIDReason, nil
	}
	job := &batchv1.Job{}
	if err := q.apiReader.Get(ctx, client.ObjectKeyFromObject(sj), job); err != nil {
		if apierrors.IsNotFound(err) {
			return false, absentSourceJobReason, nil
		}
		return false, "", fmt.Errorf("read source Job for non-target cleanup: %w", err)
	}
	if job.UID != sj.Status.SourceJobUID || !metav1.IsControlledBy(job, sj) ||
		job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 || job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
		return false, sourceJobIdentityMismatchReason, nil
	}
	nonTargets, err := podcontract.NonTargetArtifactContainers(job.Spec.Template.Annotations)
	if err != nil || len(nonTargets) == 0 || sj.Spec.NonTargetArtifacts == nil ||
		job.Spec.Template.Labels[snapshotv1alpha1.SnapshotJobOwnerUIDLabel] != string(sj.UID) {
		return false, writerDeclarationMismatchReason, nil
	}
	jobWriters := slices.Clone(nonTargets)
	declared := slices.Clone(sj.Spec.NonTargetArtifacts.Containers)
	slices.Sort(jobWriters)
	slices.Sort(declared)
	if !slices.Equal(jobWriters, declared) {
		return false, writerDeclarationMismatchReason, nil
	}
	subdir, err := podcontract.NonTargetArtifactSubdir(string(sj.UID))
	if err != nil {
		return false, "", err
	}
	for _, name := range nonTargets {
		bound := false
		for _, container := range job.Spec.Template.Spec.Containers {
			if container.Name != name {
				continue
			}
			var uidBound, pathBound bool
			for _, env := range container.Env {
				uidBound = uidBound || (env.Name == podcontract.SnapshotJobUIDEnv && env.Value == string(sj.UID) && env.ValueFrom == nil)
				pathBound = pathBound || (env.Name == podcontract.NonTargetArtifactSubdirEnv && env.Value == subdir && env.ValueFrom == nil)
			}
			bound = uidBound && pathBound
		}
		if !bound {
			return false, writerEnvironmentMismatchReason, nil
		}
	}
	terminal := false
	for _, condition := range job.Status.Conditions {
		if condition.Status == corev1.ConditionTrue && (condition.Type == batchv1.JobFailed || condition.Type == batchv1.JobComplete) {
			terminal = true
		}
	}
	if !terminal {
		return false, "source Job is not terminal", nil
	}
	pods := &corev1.PodList{}
	if err := q.apiReader.List(ctx, pods, client.InNamespace(sj.Namespace),
		client.MatchingLabels{snapshotv1alpha1.SnapshotJobOwnerUIDLabel: string(sj.UID)}); err != nil {
		return false, "", fmt.Errorf("list source Pods for non-target cleanup: %w", err)
	}
	if len(pods.Items) == 0 {
		// A terminal Job condition proves writer termination on Kubernetes
		// 1.31+, but earlier supported clusters can publish it while Pods are
		// still terminating. With no Pod record, this operator cannot prove
		// quiescence across versions. Retain the root for verified manual cleanup.
		return false, noPodWriterProofReason, nil
	}
	if job.Status.Active != 0 || (job.Status.Terminating != nil && *job.Status.Terminating != 0) ||
		(job.Status.UncountedTerminatedPods != nil &&
			(len(job.Status.UncountedTerminatedPods.Failed) != 0 || len(job.Status.UncountedTerminatedPods.Succeeded) != 0)) {
		return false, "source Job still has active or uncounted Pod attempts", nil
	}
	// A counted Job attempt can still have a terminating Pod on supported
	// clusters. Do not let a surviving replacement stand in for the original.
	counted := job.Status.Failed + job.Status.Succeeded
	if counted > int32(len(pods.Items)) {
		return false, noPodWriterProofReason, nil
	}
	if counted < int32(len(pods.Items)) {
		return false, "source Job has not counted all Pod attempts", nil
	}
	if len(pods.Items) != 1 || counted != 1 || sj.Status.PodSnapshotName == "" || sj.Status.PodSnapshotUID == "" {
		return false, sourcePodIdentityReason, nil
	}
	snap := &snapshotv1alpha1.PodSnapshot{}
	if err := q.apiReader.Get(ctx, client.ObjectKey{Namespace: sj.Namespace, Name: sj.Status.PodSnapshotName}, snap); err != nil {
		if apierrors.IsNotFound(err) {
			return false, sourcePodIdentityReason, nil
		}
		return false, "", fmt.Errorf("read PodSnapshot for source Pod identity: %w", err)
	}
	if snap.UID != sj.Status.PodSnapshotUID ||
		snap.Labels[snapshotv1alpha1.SnapshotJobOwnerLabel] != sj.Name ||
		snap.Labels[snapshotv1alpha1.SnapshotJobOwnerUIDLabel] != string(sj.UID) ||
		snap.Spec.Source.PodRef.Name != pods.Items[0].Name || snap.Spec.Source.PodRef.UID != pods.Items[0].UID {
		return false, sourcePodIdentityReason, nil
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !metav1.IsControlledBy(pod, job) || !allPodContainersStopped(pod) {
			return false, "source Pod identity or container termination is not proven", nil
		}
	}
	return true, "", nil
}

func allPodContainersStopped(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodFailed && pod.Status.Phase != corev1.PodSucceeded {
		return false
	}
	terminated := func(name string, statuses []corev1.ContainerStatus) bool {
		for _, status := range statuses {
			if status.Name == name {
				return status.State.Terminated != nil
			}
		}
		return false
	}
	for _, container := range pod.Spec.Containers {
		if !terminated(container.Name, pod.Status.ContainerStatuses) {
			return false
		}
	}
	for _, container := range pod.Spec.InitContainers {
		if !terminated(container.Name, pod.Status.InitContainerStatuses) {
			return false
		}
	}
	for _, container := range pod.Spec.EphemeralContainers {
		if !terminated(container.Name, pod.Status.EphemeralContainerStatuses) {
			return false
		}
	}
	return true
}
