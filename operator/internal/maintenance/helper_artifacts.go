// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package maintenance

import (
	"context"
	"errors"
	"fmt"

	"github.com/ai-dynamo/snapshot/api/podcontract"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/go-logr/logr"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// processHelperSweep requires positive, authoritative evidence of failure.
// Absence of a SnapshotJob does not imply failure: successful helper artifacts
// must outlive their SnapshotJob along with its independently owned PodSnapshot.
// Unknown roots are retained for verified manual cleanup.
func (q *Queue) processHelperSweep(ctx context.Context, backend Backend, logger logr.Logger) error {
	candidates, err := backend.HelperCandidates(ctx, logger)
	if err != nil || len(candidates) == 0 {
		return err
	}

	// Enumerate before the API list. A new attempt creates its SnapshotJob
	// before its helper writes, so this ordering never guesses ownership from
	// an old list or from a reusable resource name.
	jobs := &snapshotv1alpha1.SnapshotJobList{}
	if err := q.apiReader.List(ctx, jobs); err != nil {
		return fmt.Errorf("list SnapshotJobs for helper artifact cleanup: %w", err)
	}
	var cleanupErrors []error
	processed := 0
	for i := range jobs.Items {
		sj := &jobs.Items[i]
		if sj.Spec.OnFailurePolicy != snapshotv1alpha1.SnapshotJobOnFailureCleanupHelpers {
			continue // storage ownership alone is not permission to delete bytes
		}
		uid := string(sj.UID)
		if _, found := candidates[uid]; !found || !snapshotv1alpha1.IsSnapshotJobFailed(sj) || snapshotv1alpha1.IsSnapshotJobCompleted(sj) {
			continue
		}
		if _, present := sj.Spec.PodTemplate.Annotations[podcontract.HelperArtifactContainersAnnotation]; !present {
			continue // legacy/user-owned paths are outside this contract
		}
		if processed == q.config.BatchSize {
			break
		}
		stopped, err := q.helperWritersStopped(ctx, sj)
		if err != nil {
			cleanupErrors = append(cleanupErrors, err)
			continue
		}
		if !stopped {
			continue
		}
		if err := backend.DeleteHelpers(ctx, uid); err != nil {
			cleanupErrors = append(cleanupErrors, err)
			logger.Error(err, "Unable to reclaim failed SnapshotJob helper artifacts", "snapshot_job_uid", uid)
			continue
		}
		processed++
		logger.Info("Reclaimed failed SnapshotJob helper artifacts", "snapshot_job_uid", uid, "namespace", sj.Namespace, "name", sj.Name)
		q.recorder.Event(sj, corev1.EventTypeNormal, "HelperArtifactsReclaimed",
			"Reclaimed this failed attempt's managed helper artifacts after all source containers stopped; source Job and capture failure evidence retained")
	}
	return errors.Join(cleanupErrors...)
}

// helperWritersStopped waits for both the one-shot Job and every matching Pod.
// Job failure alone is insufficient: helpers can still be publishing while the
// target is already dead. Authoritative reads keep stale caches from approving
// irreversible deletion, including native sidecars which can restart.
func (q *Queue) helperWritersStopped(ctx context.Context, sj *snapshotv1alpha1.SnapshotJob) (bool, error) {
	if sj.Status.SourceJobUID == "" {
		return false, nil // no durable source identity was accepted
	}
	job := &batchv1.Job{}
	if err := q.apiReader.Get(ctx, client.ObjectKeyFromObject(sj), job); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil // disappearance alone does not prove quiescence
		}
		return false, fmt.Errorf("read source Job for helper cleanup: %w", err)
	}
	if job.UID != sj.Status.SourceJobUID || !metav1.IsControlledBy(job, sj) ||
		job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 || job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
		return false, nil
	}
	helpers, err := podcontract.HelperArtifactContainers(job.Spec.Template.Annotations)
	if err != nil || len(helpers) == 0 ||
		job.Spec.Template.Annotations[podcontract.HelperArtifactContainersAnnotation] != sj.Spec.PodTemplate.Annotations[podcontract.HelperArtifactContainersAnnotation] ||
		job.Spec.Template.Labels[snapshotv1alpha1.SnapshotJobOwnerUIDLabel] != string(sj.UID) {
		return false, nil
	}
	subdir, err := podcontract.HelperArtifactSubdir(string(sj.UID))
	if err != nil {
		return false, err
	}
	for _, name := range helpers {
		bound := false
		for _, container := range job.Spec.Template.Spec.Containers {
			if container.Name != name {
				continue
			}
			var uidBound, pathBound bool
			for _, env := range container.Env {
				uidBound = uidBound || (env.Name == podcontract.SnapshotJobUIDEnv && env.Value == string(sj.UID) && env.ValueFrom == nil)
				pathBound = pathBound || (env.Name == podcontract.HelperArtifactSubdirEnv && env.Value == subdir && env.ValueFrom == nil)
			}
			bound = uidBound && pathBound
		}
		if !bound {
			return false, nil
		}
	}
	terminal := false
	for _, condition := range job.Status.Conditions {
		if condition.Status == corev1.ConditionTrue && (condition.Type == batchv1.JobFailed || condition.Type == batchv1.JobComplete) {
			terminal = true
		}
	}
	if !terminal {
		return false, nil
	}
	pods := &corev1.PodList{}
	if err := q.apiReader.List(ctx, pods, client.InNamespace(sj.Namespace),
		client.MatchingLabels{snapshotv1alpha1.SnapshotJobOwnerUIDLabel: string(sj.UID)}); err != nil {
		return false, fmt.Errorf("list source Pods for helper cleanup: %w", err)
	}
	if len(pods.Items) == 0 {
		return false, nil // require a retained, authoritative writer exit record
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !metav1.IsControlledBy(pod, job) || !allPodContainersStopped(pod) {
			return false, nil
		}
	}
	return true, nil
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
