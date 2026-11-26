// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

const snapshotJobKind = "SnapshotJob"

// TrimJobForCache keeps the spec and status only of Jobs that a SnapshotJob
// controls. The operator watches every Job in the cluster, and of any other
// Job the controllers read only the metadata, to see that the Job is not
// theirs. The operator never updates a Job, so it never writes a trimmed one.
func TrimJobForCache(obj any) (any, error) {
	job, ok := obj.(*batchv1.Job)
	if !ok {
		return obj, nil
	}
	job.ManagedFields = nil
	if controlledBySnapshotJob(job) {
		return job, nil
	}
	job.Spec = batchv1.JobSpec{}
	job.Status = batchv1.JobStatus{}
	return job, nil
}

// controlledBySnapshotJob applies the test the Owns watch applies to route a
// Job to the SnapshotJob controller: the kind and group of the controller owner
// reference. The version is not compared, because a Job keeps the reference it
// was created with when the SnapshotJob API version changes.
func controlledBySnapshotJob(job *batchv1.Job) bool {
	owner := metav1.GetControllerOfNoCopy(job)
	if owner == nil || owner.Kind != snapshotJobKind {
		return false
	}
	gv, err := schema.ParseGroupVersion(owner.APIVersion)
	return err == nil && gv.Group == snapshotv1alpha1.GroupVersion.Group
}
