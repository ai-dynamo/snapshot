// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package maintenance

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

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
	if err != nil || len(candidates) == 0 {
		return err
	}

	// Enumerate before the API list. A new attempt creates its SnapshotJob
	// before its non-target writes, so this ordering never guesses ownership from
	// an old list or from a reusable resource name.
	jobs, err := q.nonTargetArtifactJobs(ctx, candidates)
	if err != nil {
		return err
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
		if sj.Spec.OnFailurePolicy != snapshotv1alpha1.SnapshotJobOnFailureCleanupNonTargetArtifacts {
			continue // storage ownership alone is not permission to delete bytes
		}
		uid := string(sj.UID)
		if !snapshotv1alpha1.IsSnapshotJobFailed(sj) || snapshotv1alpha1.IsSnapshotJobCompleted(sj) {
			continue
		}
		if _, present := sj.Spec.PodTemplate.Annotations[podcontract.NonTargetArtifactContainersAnnotation]; !present {
			logger.Info("Retaining failed SnapshotJob non-target artifacts: no declared writer annotation", "snapshot_job_uid", uid)
			continue // legacy/user-owned paths are outside this contract
		}
		stopped, reason, err := q.nonTargetArtifactWritersStopped(ctx, sj)
		if err != nil {
			cleanupErrors = append(cleanupErrors, err)
			continue
		}
		if !stopped {
			logger.Info("Retaining failed SnapshotJob non-target artifacts until writer termination is proven", "snapshot_job_uid", uid, "reason", reason)
			continue
		}
		if err := nonTargetBackend.DeleteNonTargetArtifacts(ctx, uid); err != nil {
			cleanupErrors = append(cleanupErrors, err)
			logger.Error(err, "Unable to reclaim failed SnapshotJob non-target artifacts", "snapshot_job_uid", uid)
			continue
		}
		logger.Info("Reclaimed failed SnapshotJob non-target artifacts", "snapshot_job_uid", uid, "namespace", sj.Namespace, "name", sj.Name)
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

const nonTargetJobListPageLimit int64 = 500

// nonTargetArtifactJobs pages only SnapshotJob metadata, then retains exact
// candidate UIDs. Full objects are fetched only for the bounded work batch.
func (q *Queue) nonTargetArtifactJobs(ctx context.Context, candidates map[string]struct{}) ([]nonTargetJobKey, error) {
	var lastErr error
	for attempt := 0; attempt < q.config.ListAttempts; attempt++ {
		jobs, err := q.nonTargetArtifactJobsOnce(ctx, candidates)
		if err == nil {
			return jobs, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("list SnapshotJob metadata for non-target cleanup: %w", lastErr)
}

func (q *Queue) nonTargetArtifactJobsOnce(ctx context.Context, candidates map[string]struct{}) ([]nonTargetJobKey, error) {
	var jobs []nonTargetJobKey
	continueToken := ""
	resourceVersion := ""
	for {
		list := &metav1.PartialObjectMetadataList{}
		list.SetGroupVersionKind(snapshotv1alpha1.GroupVersion.WithKind("SnapshotJobList"))
		if err := q.apiReader.List(ctx, list, &client.ListOptions{Limit: nonTargetJobListPageLimit, Continue: continueToken}); err != nil {
			return nil, err
		}
		if continueToken != "" && list.ResourceVersion != resourceVersion {
			return nil, fmt.Errorf("SnapshotJob metadata resource version changed across pages")
		}
		if resourceVersion == "" {
			resourceVersion = list.ResourceVersion
		}
		for i := range list.Items {
			job := &list.Items[i]
			if job.UID == "" {
				return nil, fmt.Errorf("SnapshotJob %s/%s has no UID", job.Namespace, job.Name)
			}
			if _, found := candidates[string(job.UID)]; found {
				jobs = append(jobs, nonTargetJobKey{Namespace: job.Namespace, Name: job.Name, UID: job.UID})
			}
		}
		if list.Continue == "" {
			slices.SortFunc(jobs, func(a, b nonTargetJobKey) int { return cmp.Compare(a.UID, b.UID) })
			return jobs, nil
		}
		if list.Continue == continueToken || resourceVersion == "" {
			return nil, fmt.Errorf("SnapshotJob metadata list has invalid continuation")
		}
		continueToken = list.Continue
	}
}

// nonTargetArtifactWritersStopped waits for both the one-shot Job and every matching Pod.
// Job failure alone is insufficient: non-target containers can still be publishing while the
// target is already dead. Authoritative reads keep stale caches from approving
// irreversible deletion, including native sidecars which can restart.
func (q *Queue) nonTargetArtifactWritersStopped(ctx context.Context, sj *snapshotv1alpha1.SnapshotJob) (bool, string, error) {
	if sj.Status.SourceJobUID == "" {
		return false, "no source Job UID was recorded", nil
	}
	job := &batchv1.Job{}
	if err := q.apiReader.Get(ctx, client.ObjectKeyFromObject(sj), job); err != nil {
		if apierrors.IsNotFound(err) {
			return false, "source Job is absent; disappearance is not writer-exit proof", nil
		}
		return false, "", fmt.Errorf("read source Job for non-target cleanup: %w", err)
	}
	if job.UID != sj.Status.SourceJobUID || !metav1.IsControlledBy(job, sj) ||
		job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 || job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
		return false, "source Job identity or one-shot policy does not match the attempt", nil
	}
	nonTargets, err := podcontract.NonTargetArtifactContainers(job.Spec.Template.Annotations)
	if err != nil || len(nonTargets) == 0 ||
		job.Spec.Template.Annotations[podcontract.NonTargetArtifactContainersAnnotation] != sj.Spec.PodTemplate.Annotations[podcontract.NonTargetArtifactContainersAnnotation] ||
		job.Spec.Template.Labels[snapshotv1alpha1.SnapshotJobOwnerUIDLabel] != string(sj.UID) {
		return false, "source Job writer declaration or attempt binding does not match", nil
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
			return false, "declared writer lacks the exact attempt UID and storage subdirectory", nil
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
		return false, "source Pod record is absent; writer exit cannot be proven across Kubernetes versions", nil
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
