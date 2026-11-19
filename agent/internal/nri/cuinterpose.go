// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nri

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"github.com/containerd/nri/pkg/api"

	snapshotruntime "github.com/ai-dynamo/snapshot/agent/internal/runtime"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/ai-dynamo/snapshot/api/podcontract"
)

const (
	// CuInterposeLauncherSrc is where the agent image ships the launcher.
	CuInterposeLauncherSrc = "/usr/local/bin/cuinterpose-launch"

	// cuInterposeDeliveryDir holds the delivered payload inside a container's
	// control directory. The plugin bind-mounts it at the delivery path.
	cuInterposeDeliveryDir = "snapshot-cuda"
)

// shapeCuInterposeCapture starts an opted-in checkpoint target under the
// cuinterpose launcher, with the agent's libraries at the delivery path. The
// runtime has already resolved the image's entrypoint into the process args, and
// the launcher receives the container's environment, so both stay unchanged.
// An error fails the container's creation: starting without the launcher would
// run a workload that cannot be checkpointed as requested.
func (p *Plugin) shapeCuInterposeCapture(pod *api.PodSandbox, ctr *api.Container) (*api.ContainerAdjustment, error) {
	// Check the container first, so an invalid annotation logs only for targets.
	if !needsCuInterposeDelivery(ctr) {
		return nil, nil
	}
	log := p.log.WithValues("pod", pod.GetNamespace()+"/"+pod.GetName(), "container", ctr.GetName())
	requested, err := podcontract.ParseCuInterposeAnnotation(pod.GetAnnotations())
	if err != nil {
		// Failing here would keep the Pod from starting. Capture reports the
		// invalid value on the PodSnapshot instead.
		log.Info("Ignoring invalid multi-GPU annotation", "error", err.Error())
		return nil, nil
	}
	if !requested {
		return nil, nil
	}
	args := ctr.GetArgs()
	if len(args) == 0 {
		return nil, fmt.Errorf("snapshot: container %q has no process to start with multi-GPU support", ctr.GetName())
	}

	source, err := p.installCuInterpose(pod.GetUid(), ctr.GetName())
	if err != nil {
		log.Error(err, "Cannot install multi-GPU support")
		return nil, fmt.Errorf("snapshot: install multi-GPU support: %w", err)
	}

	// CRIU reapplies these flags to the restore bind, so they match a
	// read-only volume mount rather than adding flags restore never saw.
	adjust := &api.ContainerAdjustment{}
	adjust.AddMount(&api.Mount{
		Destination: podcontract.CuInterposeMountPath,
		Type:        "bind",
		Source:      source,
		Options:     []string{"rbind", "rprivate", "ro"},
	})
	adjust.SetArgs(append(podcontract.CuInterposeLauncherPrefix(), args...))
	log.Info("Container starts with multi-GPU support", "source", source)
	return adjust, nil
}

// needsCuInterposeDelivery reports whether the plugin delivers cuinterpose to a
// container of an opted-in Pod. The snapshot-control mount marks a checkpoint
// target. A container that mounts the delivery path or already starts the
// launcher delivers cuinterpose itself, and wrapping it again would break it.
func needsCuInterposeDelivery(ctr *api.Container) bool {
	if podcontract.HasCuInterposeLauncher(ctr.GetArgs()) {
		return false
	}
	control := false
	for _, mount := range ctr.GetMounts() {
		if podcontract.IsCuInterposeDeliveryPath(mount.GetDestination()) {
			return false
		}
		if path.Clean(mount.GetDestination()) == podcontract.SnapshotControlMountPath {
			control = true
		}
	}
	return control
}

// installCuInterpose copies the libraries and launcher into the container's
// control directory and returns that copy's path on the node. Restore verifies
// a checkpoint's library hashes against the same agent bundle.
func (p *Plugin) installCuInterpose(podUID, containerName string) (string, error) {
	control, err := p.openControlDir(podUID, containerName)
	if err != nil {
		return "", err
	}
	defer control.Close()
	dir, err := snapshotruntime.OpenSubdir(control, cuInterposeDeliveryDir)
	if err != nil {
		return "", err
	}
	defer dir.Close()

	for _, name := range types.CuInterposeLibraries {
		if err := copyFile(filepath.Join(p.cuInterposeLibDir, name), dir, name, 0o644); err != nil {
			return "", fmt.Errorf("copy %s: %w", name, err)
		}
	}
	launcher := path.Base(podcontract.CuInterposeLauncherPath)
	if err := copyFile(p.cuInterposeLauncher, dir, launcher, 0o755); err != nil {
		return "", fmt.Errorf("copy %s: %w", launcher, err)
	}

	rel, ok := strings.CutPrefix(dir.Name(), strings.TrimSuffix(p.nodeRoot, "/")+"/")
	if !ok {
		return "", fmt.Errorf("delivery directory %s is outside the node root %s", dir.Name(), p.nodeRoot)
	}
	return "/" + rel, nil
}
