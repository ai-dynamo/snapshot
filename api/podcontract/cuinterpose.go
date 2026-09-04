// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podcontract

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

// cuinterpose is the CUDA interposer: an LD_PRELOAD library that lets Snapshot
// checkpoint and restore CUDA memory shared between processes on one node.
// This file shapes an opted-in source Pod so the workload runs with the shim
// preloaded and launched through cuda-checkpoint. The shim and cuda-checkpoint
// are copied out of the configured Snapshot agent image by an init container
// into a per-Pod emptyDir; the agent bind-mounts the same two files at the same
// path when restoring, because CRIU restores file-backed mappings by path.
const (
	// CuinterposeVolumeName is the emptyDir that carries the shim and
	// cuda-checkpoint into the workload containers.
	CuinterposeVolumeName = "cuinterpose"
	// CuinterposeInitContainerName copies the two files out of the agent image.
	CuinterposeInitContainerName = "cuinterpose-install"
	// CuinterposeMountPath is the fixed path the shim and cuda-checkpoint are
	// visible at inside every checkpoint target, at capture and at restore. The
	// agent's ns-bind-mount helper hard-codes the same destination.
	CuinterposeMountPath = "/tmp/cuinterpose"
	// CuinterposeLibraryPath is the LD_PRELOAD entry.
	CuinterposeLibraryPath = CuinterposeMountPath + "/libcuinterpose.so"
	// CuinterposeCUDACheckpointPath is the cuda-checkpoint that wraps the
	// workload command with --launch-job.
	CuinterposeCUDACheckpointPath = CuinterposeMountPath + "/cuda-checkpoint"

	cuinterposeImageLibraryPath    = "/usr/local/lib/snapshot/libcuinterpose.so"
	cuinterposeImageCUDACheckpoint = "/usr/local/sbin/cuda-checkpoint"
	cuinterposeInstallMountPath    = "/cuinterpose"
	ldPreloadEnv                   = "LD_PRELOAD"
)

// persistCUDAJobFileScript copies cuda-checkpoint's transient launch-job file
// (handed to the workload as an inherited procfs fd path) into the per-Pod
// control volume, so the agent can stage it into the checkpoint artifact, then
// starts the original command.
const persistCUDAJobFileScript = `set -eu
job_file="$1"
shift
if [ -z "${CUDA_CHECKPOINT_JOB_FILE:-}" ]; then
    echo "CUDA_CHECKPOINT_JOB_FILE is missing; cuda-checkpoint --launch-job requires NVIDIA driver 610 or newer" >&2
    exit 1
fi
umask 077
cat "$CUDA_CHECKPOINT_JOB_FILE" > "$job_file"
export CUDA_CHECKPOINT_JOB_FILE="$job_file"
exec "$@"`

// imageReferencePattern is the Docker/OCI reference grammar: an optional
// registry (host[:port]), path components, an optional tag, and an optional
// sha256 digest. It rejects whitespace, uppercase path components, and other
// strings the kubelet would fail to pull.
var imageReferencePattern = regexp.MustCompile(
	`^(?:[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?(?:\.[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?)*(?::[0-9]+)?/)?` +
		`[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*` +
		`(?::[\w][\w.-]{0,127})?` +
		`(?:@sha256:[a-f0-9]{64})?$`,
)

// CuinterposeDelivery says where the shim and cuda-checkpoint come from. The
// Snapshot installation configures it once (Helm chart -> operator flags);
// workload users never choose an image.
type CuinterposeDelivery struct {
	// AgentImage is the Snapshot agent image reference.
	AgentImage string
	// PullPolicy for the install init container. Empty selects a safe default:
	// Always for tag references, IfNotPresent for digest references.
	PullPolicy corev1.PullPolicy
	// ImagePullSecrets are secret names, expected to exist in the workload's
	// namespace, added to the Pod so the init container can pull AgentImage
	// from a private registry.
	ImagePullSecrets []string
}

// CuinterposeEnabled reports whether a workload opted into the CUDA interposer
// through CuinterposeAnnotation. An absent annotation disables it; any value
// other than "enabled" is an error rather than a silent no.
func CuinterposeEnabled(annotations map[string]string) (bool, error) {
	raw, found := annotations[CuinterposeAnnotation]
	if !found {
		return false, nil
	}
	if raw != strings.TrimSpace(raw) {
		return false, fmt.Errorf("%s must not contain surrounding whitespace", CuinterposeAnnotation)
	}
	if raw != CuinterposeAnnotationEnabled {
		return false, fmt.Errorf("%s must be %q", CuinterposeAnnotation, CuinterposeAnnotationEnabled)
	}
	return true, nil
}

// ShapeCuinterposeCapture installs the capture-side contract on every target
// container in podTemplate when the template opts in: one emptyDir, one init
// container copying the shim and cuda-checkpoint from the agent image, a
// read-only mount of that volume in each target, LD_PRELOAD pointing at the
// shim, and the target command wrapped with cuda-checkpoint --launch-job.
// Reapplying to an already shaped template is a no-op. On error the template
// is left unchanged.
func ShapeCuinterposeCapture(
	podTemplate *corev1.PodTemplateSpec,
	targetContainers []string,
	delivery CuinterposeDelivery,
) error {
	if podTemplate == nil {
		return fmt.Errorf("cuinterpose requires a pod template")
	}
	shaped := podTemplate.DeepCopy()
	if err := shapeCuinterposeCapture(shaped, targetContainers, delivery); err != nil {
		return err
	}
	*podTemplate = *shaped
	return nil
}

func shapeCuinterposeCapture(
	podTemplate *corev1.PodTemplateSpec,
	targetContainers []string,
	delivery CuinterposeDelivery,
) error {
	enabled, err := CuinterposeEnabled(podTemplate.Annotations)
	if err != nil || !enabled {
		return err
	}
	if err := ValidateImageReference(delivery.AgentImage); err != nil {
		return fmt.Errorf("cuinterpose agent image: %w", err)
	}
	if len(targetContainers) == 0 {
		return fmt.Errorf("cuinterpose requires at least one target container")
	}
	if err := ensureCuinterposeVolume(&podTemplate.Spec); err != nil {
		return err
	}
	if err := ensureCuinterposeInitContainer(&podTemplate.Spec, delivery); err != nil {
		return err
	}
	ensureImagePullSecrets(&podTemplate.Spec, delivery.ImagePullSecrets)

	seen := make(map[string]struct{}, len(targetContainers))
	for _, name := range targetContainers {
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("duplicate cuinterpose target container %q", name)
		}
		seen[name] = struct{}{}
		container := findContainer(&podTemplate.Spec, name)
		if container == nil {
			return fmt.Errorf("cuinterpose target container %q does not exist", name)
		}
		if err := ensureCuinterposeMount(container); err != nil {
			return err
		}
		if err := setCuinterposePreload(container); err != nil {
			return err
		}
		if err := EnsureCUDACheckpointLaunchJob(container, CuinterposeCUDACheckpointPath); err != nil {
			return err
		}
	}
	return nil
}

// VerifyCuinterposeCapture reports whether spec already carries the complete
// capture contract for every target container: the volume, the install init
// container, and per target the mount, the LD_PRELOAD entry, and the
// cuda-checkpoint wrapper. The operator uses it to refuse adopting a Job that
// was created without the contract.
func VerifyCuinterposeCapture(spec *corev1.PodSpec, targetContainers []string) error {
	if spec == nil {
		return fmt.Errorf("cuinterpose requires a pod spec")
	}
	if !slices.ContainsFunc(spec.Volumes, func(v corev1.Volume) bool {
		return v.Name == CuinterposeVolumeName && isEmptyDirVolumeSource(v.VolumeSource)
	}) {
		return fmt.Errorf("pod lacks the %s emptyDir volume", CuinterposeVolumeName)
	}
	if !slices.ContainsFunc(spec.InitContainers, func(c corev1.Container) bool {
		return c.Name == CuinterposeInitContainerName
	}) {
		return fmt.Errorf("pod lacks the %s init container", CuinterposeInitContainerName)
	}
	for _, name := range targetContainers {
		container := findContainer(spec, name)
		if container == nil {
			return fmt.Errorf("cuinterpose target container %q does not exist", name)
		}
		if !slices.ContainsFunc(container.VolumeMounts, func(m corev1.VolumeMount) bool {
			return m.Name == CuinterposeVolumeName && m.MountPath == CuinterposeMountPath
		}) {
			return fmt.Errorf("container %q does not mount %s at %s", name, CuinterposeVolumeName, CuinterposeMountPath)
		}
		if !slices.Contains(preloadFields(envValue(container.Env, ldPreloadEnv)), CuinterposeLibraryPath) {
			return fmt.Errorf("container %q does not preload %s", name, CuinterposeLibraryPath)
		}
		if len(container.Command) != 1 || container.Command[0] != CuinterposeCUDACheckpointPath ||
			!isCUDACheckpointLaunchJob(container.Args) {
			return fmt.Errorf("container %q is not launched through %s --launch-job", name, CuinterposeCUDACheckpointPath)
		}
	}
	return nil
}

// ValidateImageReference rejects image references the kubelet cannot pull.
func ValidateImageReference(image string) error {
	if image == "" {
		return fmt.Errorf("image reference is required")
	}
	if image != strings.TrimSpace(image) {
		return fmt.Errorf("image reference %q must not contain surrounding whitespace", image)
	}
	if !imageReferencePattern.MatchString(image) {
		return fmt.Errorf("image reference %q is not a valid registry/repository[:tag][@sha256:digest]", image)
	}
	return nil
}

func ensureCuinterposeVolume(spec *corev1.PodSpec) error {
	found := false
	for i := range spec.Volumes {
		volume := &spec.Volumes[i]
		if volume.Name != CuinterposeVolumeName {
			continue
		}
		if found {
			return fmt.Errorf("duplicate %s volume", CuinterposeVolumeName)
		}
		found = true
		if !isEmptyDirVolumeSource(volume.VolumeSource) {
			return fmt.Errorf("volume %q must be an emptyDir", CuinterposeVolumeName)
		}
	}
	if !found {
		spec.Volumes = append(spec.Volumes, corev1.Volume{
			Name:         CuinterposeVolumeName,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		})
	}
	return nil
}

func ensureImagePullSecrets(spec *corev1.PodSpec, names []string) {
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if slices.ContainsFunc(spec.ImagePullSecrets, func(ref corev1.LocalObjectReference) bool {
			return ref.Name == name
		}) {
			continue
		}
		spec.ImagePullSecrets = append(spec.ImagePullSecrets, corev1.LocalObjectReference{Name: name})
	}
}

func expectedCuinterposeInitContainer(delivery CuinterposeDelivery) corev1.Container {
	pullPolicy := delivery.PullPolicy
	if pullPolicy == "" {
		pullPolicy = corev1.PullAlways
		if strings.Contains(delivery.AgentImage, "@sha256:") {
			pullPolicy = corev1.PullIfNotPresent
		}
	}
	return corev1.Container{
		Name:            CuinterposeInitContainerName,
		Image:           delivery.AgentImage,
		ImagePullPolicy: pullPolicy,
		Command:         []string{"/bin/cp"},
		Args: []string{
			"--",
			cuinterposeImageLibraryPath,
			cuinterposeImageCUDACheckpoint,
			cuinterposeInstallMountPath + "/",
		},
		// The agent image runs as root and only copies two files into a
		// world-writable emptyDir, so RunAsNonRoot is not set; everything else
		// that a restricted namespace expects is.
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		VolumeMounts: []corev1.VolumeMount{{
			Name:      CuinterposeVolumeName,
			MountPath: cuinterposeInstallMountPath,
		}},
	}
}

// initContainerMatches compares the fields the contract sets. Fields the API
// server defaults (termination message path, image pull policy on re-read) are
// deliberately not compared, so reshaping a live object does not report a
// conflict.
func initContainerMatches(actual, expected *corev1.Container) bool {
	if actual.Name != expected.Name || actual.Image != expected.Image {
		return false
	}
	if !slices.Equal(actual.Command, expected.Command) || !slices.Equal(actual.Args, expected.Args) {
		return false
	}
	if len(actual.VolumeMounts) != len(expected.VolumeMounts) {
		return false
	}
	for i := range expected.VolumeMounts {
		if actual.VolumeMounts[i].Name != expected.VolumeMounts[i].Name ||
			actual.VolumeMounts[i].MountPath != expected.VolumeMounts[i].MountPath {
			return false
		}
	}
	return true
}

func ensureCuinterposeInitContainer(spec *corev1.PodSpec, delivery CuinterposeDelivery) error {
	expected := expectedCuinterposeInitContainer(delivery)
	found := false
	for i := range spec.InitContainers {
		container := &spec.InitContainers[i]
		if container.Name != CuinterposeInitContainerName {
			continue
		}
		if found {
			return fmt.Errorf("duplicate %s init container", CuinterposeInitContainerName)
		}
		found = true
		if !initContainerMatches(container, &expected) {
			return fmt.Errorf(
				"init container %q conflicts with the cuinterpose contract",
				CuinterposeInitContainerName,
			)
		}
	}
	if !found {
		spec.InitContainers = append(spec.InitContainers, expected)
	}
	return nil
}

func ensureCuinterposeMount(container *corev1.Container) error {
	found := false
	for i := range container.VolumeMounts {
		mount := &container.VolumeMounts[i]
		if mount.Name != CuinterposeVolumeName && mount.MountPath != CuinterposeMountPath {
			continue
		}
		if found {
			return fmt.Errorf("container %q has duplicate cuinterpose mounts", container.Name)
		}
		found = true
		if mount.Name != CuinterposeVolumeName || mount.MountPath != CuinterposeMountPath ||
			!mount.ReadOnly || mount.SubPath != "" || mount.SubPathExpr != "" ||
			mount.RecursiveReadOnly != nil ||
			(mount.MountPropagation != nil && *mount.MountPropagation != corev1.MountPropagationNone) {
			return fmt.Errorf("container %q has conflicting options on the cuinterpose mount", container.Name)
		}
	}
	if !found {
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name:      CuinterposeVolumeName,
			MountPath: CuinterposeMountPath,
			ReadOnly:  true,
		})
	}
	return nil
}

// setCuinterposePreload puts the shim first in LD_PRELOAD, keeping any entries
// the workload already had. First position matters: the dynamic loader
// resolves symbols in LD_PRELOAD order, and the shim must see CUDA calls
// before any other interposer.
func setCuinterposePreload(container *corev1.Container) error {
	index := -1
	for i := range container.Env {
		if container.Env[i].Name != ldPreloadEnv {
			continue
		}
		if index != -1 {
			return fmt.Errorf("container %q has duplicate %s environment variables", container.Name, ldPreloadEnv)
		}
		index = i
	}
	if index == -1 {
		container.Env = append(container.Env, corev1.EnvVar{
			Name:  ldPreloadEnv,
			Value: CuinterposeLibraryPath,
		})
		return nil
	}
	env := &container.Env[index]
	if env.ValueFrom != nil {
		return fmt.Errorf("container %q uses valueFrom for %s", container.Name, ldPreloadEnv)
	}
	fields := preloadFields(env.Value)
	filtered := make([]string, 0, len(fields)+1)
	filtered = append(filtered, CuinterposeLibraryPath)
	for _, field := range fields {
		if field != CuinterposeLibraryPath {
			filtered = append(filtered, field)
		}
	}
	env.Value = strings.Join(filtered, " ")
	return nil
}

// EnsureCUDACheckpointLaunchJob launches container under
// `binaryPath --launch-job` and persists the driver's transient job file in
// the snapshot control volume. Reapplying the same wrapper is a no-op.
func EnsureCUDACheckpointLaunchJob(container *corev1.Container, binaryPath string) error {
	if container == nil {
		return fmt.Errorf("cuda-checkpoint launch-job requires a container")
	}
	if binaryPath = strings.TrimSpace(binaryPath); binaryPath == "" {
		return fmt.Errorf("cuda-checkpoint launch-job requires a binary path")
	}
	if len(container.Command) == 1 && container.Command[0] == binaryPath {
		if isCUDACheckpointLaunchJob(container.Args) {
			return nil
		}
		return fmt.Errorf(
			"container %q command conflicts with the cuda-checkpoint launch-job contract",
			container.Name,
		)
	}
	if len(container.Command) == 0 {
		return fmt.Errorf(
			"container %q requires container.command for cuda-checkpoint launch-job",
			container.Name,
		)
	}

	original := make([]string, 0, len(container.Command)+len(container.Args))
	original = append(original, container.Command...)
	original = append(original, container.Args...)
	container.Command = []string{binaryPath}
	container.Args = []string{
		"--launch-job",
		"/bin/sh",
		"-c",
		persistCUDAJobFileScript,
		"dynamo-cuda-checkpoint",
		CUDAJobFilePath,
	}
	container.Args = append(container.Args, original...)
	return nil
}

func isCUDACheckpointLaunchJob(args []string) bool {
	return len(args) > 6 &&
		args[0] == "--launch-job" &&
		args[1] == "/bin/sh" &&
		args[2] == "-c" &&
		args[3] == persistCUDAJobFileScript &&
		args[4] == "dynamo-cuda-checkpoint" &&
		args[5] == CUDAJobFilePath
}

func preloadFields(value string) []string {
	return strings.FieldsFunc(value, func(r rune) bool {
		return r == ':' || unicode.IsSpace(r)
	})
}

func envValue(env []corev1.EnvVar, name string) string {
	for i := range env {
		if env[i].Name == name {
			return env[i].Value
		}
	}
	return ""
}
