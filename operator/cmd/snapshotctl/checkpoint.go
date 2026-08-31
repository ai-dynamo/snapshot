// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/validation"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
	snapshotprotocol "github.com/ai-dynamo/snapshot/operator/internal/protocol"
)

type checkpointOptions struct {
	ManifestPath       string
	Namespace          string
	KubeContext        string
	SnapshotName       string
	Container          string
	CudaCheckpointWrap bool
	Timeout            time.Duration
}

type result struct {
	Name          string
	Namespace     string
	CheckpointJob string
	PodSnapshot   string
	BoundContent  string
	RestorePod    string
	Status        string
}

func runCheckpointFlow(ctx context.Context, opts checkpointOptions) (_ *result, retErr error) {
	if strings.TrimSpace(opts.ManifestPath) == "" {
		return nil, fmt.Errorf("missing required flags: --manifest")
	}
	if opts.Timeout <= 0 {
		return nil, fmt.Errorf("--timeout must be greater than zero")
	}
	snapshotName := strings.TrimSpace(opts.SnapshotName)
	if snapshotName == "" {
		return nil, fmt.Errorf("missing required flags: --snapshot")
	}
	if errs := validation.IsDNS1123Subdomain(snapshotName); len(errs) != 0 {
		return nil, fmt.Errorf("--snapshot %q is invalid: %s", snapshotName, strings.Join(errs, "; "))
	}
	containerName := strings.TrimSpace(opts.Container)
	if containerName == "" {
		return nil, fmt.Errorf("missing required flags: --container")
	}

	pod, clientset, crClient, namespace, err := loadRunContext(opts.ManifestPath, opts.Namespace, opts.KubeContext)
	if err != nil {
		return nil, err
	}

	checkpointJobName := captureJobName(snapshotName)
	job, err := snapshotprotocol.NewSourceJob(&corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      pod.Labels,
			Annotations: pod.Annotations,
		},
		Spec: *pod.Spec.DeepCopy(),
	}, snapshotprotocol.SourceJobOptions{
		Namespace:       namespace,
		TargetContainer: containerName,
		SeccompProfile:  snapshotv1alpha1.DefaultSeccompLocalhostProfile,
		Name:            checkpointJobName,
		WrapLaunchJob:   opts.CudaCheckpointWrap,
	})
	if err != nil {
		return nil, err
	}
	createdJob, err := clientset.BatchV1().Jobs(namespace).Create(ctx, job, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("checkpoint job %s/%s already exists", namespace, checkpointJobName)
	}
	if err != nil {
		return nil, err
	}

	// Clean up the Job on any error after this point. The PodSnapshot is left in place
	// to aid debugging when the flow fails.
	defer func() {
		if retErr != nil {
			_ = clientset.BatchV1().Jobs(namespace).Delete(ctx, checkpointJobName, metav1.DeleteOptions{})
		}
	}()

	waitCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	sourcePod, err := waitForSourcePod(waitCtx, clientset, namespace, checkpointJobName, createdJob.UID)
	if err != nil {
		return nil, err
	}

	snap, err := createPodSnapshot(waitCtx, crClient, namespace, snapshotName, sourcePod.Name, sourcePod.UID, []string{containerName})
	if err != nil {
		return nil, err
	}

	snap, err = waitForPodSnapshot(waitCtx, crClient, namespace, snap.Name)
	if err != nil {
		return nil, err
	}

	res := &result{
		Name:          pod.Name,
		Namespace:     namespace,
		CheckpointJob: checkpointJobName,
		PodSnapshot:   snap.Name,
		Status:        "completed",
	}
	if snap.Status.BoundPodSnapshotContentName != nil {
		res.BoundContent = strings.TrimSpace(*snap.Status.BoundPodSnapshotContentName)
	}
	return res, nil
}

func captureJobName(snapshotName string) string {
	const suffixLength = len("-capture-") + 5
	prefix := snapshotName
	if len(prefix) > 63-suffixLength {
		prefix = strings.TrimRight(prefix[:63-suffixLength], "-.")
	}
	return prefix + "-capture-" + rand.String(5)
}
