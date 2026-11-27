// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ai-dynamo/snapshot/api/podcontract"
	snapshotprotocol "github.com/ai-dynamo/snapshot/operator/internal/protocol"
)

// A failed checkpoint's cleanup retries the Job delete every jobCleanupInterval, for at most
// jobCleanupTimeout. The limit is longer than one request can take (30 seconds), so a request
// that hangs is still followed by more attempts.
const (
	jobCleanupTimeout  = 2 * time.Minute
	jobCleanupInterval = 3 * time.Second
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

func runCheckpointFlow(ctx context.Context, opts checkpointOptions) (*result, error) {
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
	return checkpointPod(ctx, clientset, crClient, pod, namespace, snapshotName, containerName, opts)
}

// checkpointPod runs the manifest's Pod as a Job, asks for a PodSnapshot of that source Pod, and
// waits until the PodSnapshot is Ready or Failed.
func checkpointPod(ctx context.Context, clientset kubernetes.Interface, crClient client.Client, pod *corev1.Pod, namespace, snapshotName, containerName string, opts checkpointOptions) (_ *result, retErr error) {
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
		SeccompProfile:  podcontract.DefaultSeccompLocalhostProfile,
		Name:            checkpointJobName,
		WrapLaunchJob:   opts.CudaCheckpointWrap,
	})
	if err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("checkpoint interrupted: %w", context.Cause(ctx))
	}
	// An interrupt does not cancel the create: the command must learn whether the Job exists
	// before it can delete it. A canceled create can still reach the API server, and a delete
	// that follows at once can find no Job yet. The request ends by itself after 30 seconds.
	createdJob, err := clientset.BatchV1().Jobs(namespace).Create(context.WithoutCancel(ctx), job, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("checkpoint job %s/%s already exists", namespace, checkpointJobName)
	}
	if err != nil {
		return nil, err
	}

	// Clean up the Job and its source Pod on any error after this point, and when the command
	// is interrupted. The PodSnapshot is left in place to aid debugging when the flow fails.
	// The interrupt cancels ctx, so the cleanup runs on a context that outlives it.
	defer func() {
		if retErr == nil {
			return
		}
		if ctx.Err() != nil {
			retErr = fmt.Errorf("checkpoint interrupted: %w", context.Cause(ctx))
			snapshotctlLog.Info("Checkpoint interrupted, deleting its Job and source Pod; interrupt again to stop at once", "job", namespace+"/"+checkpointJobName)
		}
		if cleanupErr := cleanUpCheckpointJob(context.WithoutCancel(ctx), clientset, namespace, checkpointJobName, jobCleanupInterval, jobCleanupTimeout); cleanupErr != nil {
			retErr = fmt.Errorf("%w; %w", retErr, cleanupErr)
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

// cleanUpCheckpointJob removes what a checkpoint that failed leaves running: the Job and its
// source Pod. The API server that made the checkpoint fail can also fail the delete, so the delete
// is retried while a later attempt can succeed. A Job that is already gone counts as deleted.
// The error of a cleanup that gives up names the Job: its name ends in a random suffix that
// nothing else prints, and its source Pod holds a GPU until someone deletes the Job.
func cleanUpCheckpointJob(ctx context.Context, clientset kubernetes.Interface, namespace, name string, interval, timeout time.Duration) error {
	var deleteErr error
	_ = wait.PollUntilContextTimeout(ctx, interval, timeout, true, func(ctx context.Context) (bool, error) {
		err := deleteCheckpointJob(ctx, clientset, namespace, name)
		if err == nil || apierrors.IsNotFound(err) {
			deleteErr = nil
			return true, nil
		}
		if deleteErr != nil && ctx.Err() != nil {
			return false, nil // cut off by the cleanup timeout: keep the API server's last answer
		}
		if deleteErr == nil && canSucceedLater(err) {
			snapshotctlLog.Info("Deleting the checkpoint Job failed, retrying", "job", namespace+"/"+name, "timeout", timeout.String(), "error", err.Error())
		}
		deleteErr = err
		if !canSucceedLater(err) {
			return false, err
		}
		return false, nil
	})
	if deleteErr == nil {
		return nil
	}
	return fmt.Errorf("checkpoint Job %s/%s was not deleted, and its source Pod can still hold a GPU: %w; to delete both, run: kubectl delete job %s --namespace %s",
		namespace, name, deleteErr, name, namespace)
}

// canSucceedLater reports whether a request that failed is worth another attempt: it got no
// answer from the API server, or an answer that says to try again.
func canSucceedLater(err error) bool {
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return true
	}
	return apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) || apierrors.IsTooManyRequests(err) ||
		apierrors.IsInternalError(err) || apierrors.IsServiceUnavailable(err)
}

// deleteCheckpointJob removes the Job of a checkpoint that failed, and its source Pod with it.
// The API server keeps the Pods of a deleted batch/v1 Job unless the delete names a
// propagation policy, and the source Pod would go on holding its GPU.
func deleteCheckpointJob(ctx context.Context, clientset kubernetes.Interface, namespace, name string) error {
	return clientset.BatchV1().Jobs(namespace).Delete(ctx, name, metav1.DeleteOptions{
		PropagationPolicy: ptr.To(metav1.DeletePropagationBackground),
	})
}

func captureJobName(snapshotName string) string {
	const suffixLength = len("-capture-") + 5
	prefix := snapshotName
	if len(prefix) > 63-suffixLength {
		prefix = strings.TrimRight(prefix[:63-suffixLength], "-.")
	}
	return prefix + "-capture-" + rand.String(5)
}
