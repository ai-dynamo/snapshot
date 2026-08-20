// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/ai-dynamo/snapshot/operator/internal/logging"
)

var snapshotctlLog = logging.ConfigureLogger("stderr").WithName("snapshotctl")

func main() {
	if err := run(os.Args[1:]); err != nil {
		snapshotctlLog.Error(err, "snapshotctl failed")
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		printUsage()
		return nil
	}

	switch args[0] {
	case "checkpoint":
		return runCheckpoint(args[1:])
	case "restore":
		return runRestore(args[1:])
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

func runCheckpoint(args []string) error {
	flags := flag.NewFlagSet("checkpoint", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)

	manifest := flags.String("manifest", "", "Path to a worker Pod manifest")
	namespace := flags.String("namespace", "", "Namespace override; defaults to the manifest namespace or current kube context namespace")
	kubeContext := flags.String("kube-context", "", "Kubernetes context override")
	snapshotName := flags.String("snapshot", "", "Required. Name of the PodSnapshot to create")
	container := flags.String("container", "", "Required. Name of the workload container inside the manifest to checkpoint")
	cudaCheckpointWrap := flags.Bool("cuda-checkpoint-wrap", false, "Wrap the container command with cuda-checkpoint --launch-job (required for multi-GPU checkpoints; the placeholder image must have cuda-checkpoint at the same path as the source container)")
	timeout := flags.Duration("timeout", 45*time.Minute, "Maximum time to wait for checkpoint completion")

	if err := flags.Parse(args); err != nil {
		return err
	}
	if len(flags.Args()) != 0 {
		return fmt.Errorf("unexpected arguments: %v", flags.Args())
	}
	if *manifest == "" {
		return fmt.Errorf("--manifest is required")
	}
	if *snapshotName == "" {
		return fmt.Errorf("--snapshot is required")
	}

	snapshotctlLog.Info("Running checkpoint", "manifest", *manifest, "namespace", *namespace)
	result, err := runCheckpointFlow(context.Background(), checkpointOptions{
		ManifestPath:       *manifest,
		Namespace:          *namespace,
		KubeContext:        *kubeContext,
		SnapshotName:       *snapshotName,
		Container:          *container,
		CudaCheckpointWrap: *cudaCheckpointWrap,
		Timeout:            *timeout,
	})
	if err != nil {
		return err
	}
	snapshotctlLog.Info("Checkpoint completed", "job", result.CheckpointJob, "pod_snapshot", result.PodSnapshot)

	fmt.Printf("status=%s\n", result.Status)
	fmt.Printf("namespace=%s\n", result.Namespace)
	fmt.Printf("name=%s\n", result.Name)
	fmt.Printf("checkpoint_job=%s\n", result.CheckpointJob)
	fmt.Printf("pod_snapshot=%s\n", result.PodSnapshot)
	if result.BoundContent != "" {
		fmt.Printf("bound_content=%s\n", result.BoundContent)
	}
	return nil
}

func runRestore(args []string) error {
	flags := flag.NewFlagSet("restore", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)

	manifest := flags.String("manifest", "", "Path to a worker Pod manifest used to create a new restore pod")
	namespace := flags.String("namespace", "", "Namespace override; defaults to the manifest namespace or current kube context namespace")
	kubeContext := flags.String("kube-context", "", "Kubernetes context override")
	snapshotName := flags.String("snapshot", "", "Required. PodSnapshot name in the restore pod namespace")

	if err := flags.Parse(args); err != nil {
		return err
	}
	if len(flags.Args()) != 0 {
		return fmt.Errorf("unexpected arguments: %v", flags.Args())
	}
	if *manifest == "" {
		return fmt.Errorf("--manifest is required")
	}
	if *snapshotName == "" {
		return fmt.Errorf("--snapshot is required")
	}

	snapshotctlLog.Info("Running restore", "manifest", *manifest, "namespace", *namespace, "pod_snapshot", *snapshotName)
	result, err := runRestoreFlow(context.Background(), restoreOptions{
		ManifestPath: *manifest,
		Namespace:    *namespace,
		KubeContext:  *kubeContext,
		SnapshotName: *snapshotName,
	})
	if err != nil {
		return err
	}
	snapshotctlLog.Info("Restore requested", "pod", result.RestorePod, "pod_snapshot", result.PodSnapshot)

	fmt.Printf("status=%s\n", result.Status)
	fmt.Printf("namespace=%s\n", result.Namespace)
	fmt.Printf("name=%s\n", result.Name)
	fmt.Printf("restore_pod=%s\n", result.RestorePod)
	fmt.Printf("pod_snapshot=%s\n", result.PodSnapshot)
	return nil
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `snapshotctl runs snapshot checkpoint and restore flows from a worker Pod manifest.

Subcommands:
  checkpoint
  restore

Examples:
  snapshotctl checkpoint --manifest /tmp/vllm-worker-pod.yaml --snapshot worker-snapshot --container main
  snapshotctl restore --manifest /tmp/sglang-worker-pod.yaml --snapshot worker-snapshot
`)
}
