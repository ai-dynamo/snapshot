// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ai-dynamo/snapshot/api/podcontract"
	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

type restoreOptions struct {
	ManifestPath string
	Namespace    string
	KubeContext  string
	SnapshotName string
}

func runRestoreFlow(ctx context.Context, opts restoreOptions) (*result, error) {
	if strings.TrimSpace(opts.ManifestPath) == "" || strings.TrimSpace(opts.SnapshotName) == "" {
		return nil, fmt.Errorf("restore requires --manifest and --snapshot")
	}
	pod, clientset, crClient, namespace, err := loadRunContext(opts.ManifestPath, opts.Namespace, opts.KubeContext)
	if err != nil {
		return nil, err
	}
	snapshotName := strings.TrimSpace(opts.SnapshotName)
	snapshot := &snapshotv1alpha1.PodSnapshot{}
	if err := crClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: snapshotName}, snapshot); err != nil {
		return nil, fmt.Errorf("get PodSnapshot %s/%s: %w", namespace, snapshotName, err)
	}
	containers := snapshot.Spec.Source.PodRef.Containers
	if len(containers) != 1 || strings.TrimSpace(containers[0]) == "" {
		return nil, fmt.Errorf("PodSnapshot %s/%s must capture exactly one container", namespace, snapshotName)
	}
	mappings, err := podcontract.ContainerMappingsFromAnnotations(pod.Annotations, containers[0])
	if err != nil {
		return nil, err
	}

	candidate := &corev1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{
			Name:         pod.Name,
			GenerateName: pod.GenerateName,
			Namespace:    namespace,
			Labels:       pod.Labels,
			Annotations:  pod.Annotations,
		},
		Spec: *pod.Spec.DeepCopy(),
	}
	candidate.Spec.RestartPolicy = corev1.RestartPolicyNever
	restorePod, err := podcontract.Build(
		candidate,
		podcontract.Request{
			SnapshotName:    snapshotName,
			SourceContainer: containers[0],
			Mappings:        mappings,
		},
		podcontract.Options{
			SeccompProfile:    podcontract.DefaultSeccompLocalhostProfile,
			EnableStartupGate: true,
		},
	)
	if err != nil {
		return nil, err
	}
	restorePod, err = clientset.CoreV1().Pods(namespace).Create(ctx, restorePod, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("restore pod %s/%s already exists", namespace, pod.Name)
	}
	if err != nil {
		return nil, err
	}
	return &result{
		Name:        restorePod.Name,
		Namespace:   namespace,
		PodSnapshot: snapshotName,
		RestorePod:  restorePod.Name,
		Status:      "requested",
	}, nil
}
