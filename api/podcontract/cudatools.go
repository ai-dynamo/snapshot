// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podcontract

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

// Snapshot delivers two CUDA tools from its own agent image into source Pods:
// cuda-checkpoint, which launches the workload under --launch-job so a
// multi-GPU process tree can be checkpointed as one CUDA job, and the
// cuinterpose shim, which a Pod opts into separately (see cuinterpose.go). An
// init container copies both into a per-Pod emptyDir at a fixed path; the
// agent bind-mounts the same two files at the same path when restoring,
// because CRIU restores file-backed mappings by path.
const (
	// CUDAToolsVolumeName is the emptyDir that carries the tools into the
	// workload containers.
	CUDAToolsVolumeName = "snapshot-cuda"
	// CUDAToolsInitContainerName copies the tools out of the agent image.
	CUDAToolsInitContainerName = "snapshot-cuda-install"
	// CUDAToolsMountPath is the fixed path the tools are visible at inside
	// every checkpoint target, at capture and at restore. The agent's
	// ns-bind-mount helper hard-codes the same destination.
	CUDAToolsMountPath = "/tmp/snapshot-cuda"
	// CUDACheckpointPath is the cuda-checkpoint that wraps the workload
	// command with --launch-job.
	CUDACheckpointPath = CUDAToolsMountPath + "/cuda-checkpoint"
	// CuinterposeLibraryPath is the shim's path, used as the LD_PRELOAD entry
	// by Pods that opt into cuinterpose. It is copied for every delivery and
	// is inert until preloaded.
	CuinterposeLibraryPath = CUDAToolsMountPath + "/libcuinterpose.so"

	// GPUResourceName is the device-plugin GPU resource.
	GPUResourceName corev1.ResourceName = "nvidia.com/gpu"

	cudaToolsImageCUDACheckpoint = "/usr/local/sbin/cuda-checkpoint"
	cudaToolsImageLibrary        = "/usr/local/lib/snapshot/libcuinterpose.so"
	cudaToolsInstallMountPath    = "/snapshot-cuda"
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

// CUDAToolsDelivery says where the tools come from. The Snapshot installation
// configures it once (Helm chart -> operator flags); workload users never
// choose an image.
type CUDAToolsDelivery struct {
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

// ClaimDevices resolves one of the Pod's resourceClaims to the number of
// devices it asks for. known is false when the count cannot be determined
// (the claim or template is gone, or it allocates "All" devices); callers then
// assume more than one. A nil ClaimDevices treats every claim as unknown.
type ClaimDevices func(claim corev1.PodResourceClaim) (devices int, known bool, err error)

// ContainerGPUs counts the GPUs container may use: its nvidia.com/gpu limit
// (or request when no limit is set) plus every DRA claim it references.
// undetermined is true when some claim's size is unknown.
func ContainerGPUs(
	spec *corev1.PodSpec,
	container *corev1.Container,
	claims ClaimDevices,
) (gpus int, undetermined bool, err error) {
	quantity, found := container.Resources.Limits[GPUResourceName]
	if !found {
		quantity, found = container.Resources.Requests[GPUResourceName]
	}
	if found {
		gpus = int(quantity.Value())
	}
	for _, ref := range container.Resources.Claims {
		index := slices.IndexFunc(spec.ResourceClaims, func(c corev1.PodResourceClaim) bool { return c.Name == ref.Name })
		if index < 0 {
			return 0, false, fmt.Errorf(
				"container %q references resource claim %q, which the pod does not declare", container.Name, ref.Name)
		}
		if claims == nil {
			undetermined = true
			continue
		}
		devices, known, err := claims(spec.ResourceClaims[index])
		if err != nil {
			return 0, false, fmt.Errorf("resource claim %q of container %q: %w", ref.Name, container.Name, err)
		}
		if !known {
			undetermined = true
			continue
		}
		gpus += devices
	}
	return gpus, undetermined, nil
}

// NeedsCUDALaunchJob reports whether container must be launched under
// cuda-checkpoint --launch-job: it may use more than one GPU. The agent
// refuses to checkpoint a multi-GPU process tree without the job file, so the
// rule is applied where the Pod is shaped rather than discovered at capture.
func NeedsCUDALaunchJob(spec *corev1.PodSpec, container *corev1.Container, claims ClaimDevices) (bool, error) {
	gpus, undetermined, err := ContainerGPUs(spec, container, claims)
	if err != nil {
		return false, err
	}
	return gpus > 1 || undetermined, nil
}

// ShapeCUDALaunchJob installs the launch-job contract on every target
// container that may use more than one GPU: one emptyDir, one init container
// copying the tools from the agent image, a read-only mount in the target, and
// the target command wrapped with cuda-checkpoint --launch-job. Targets that
// need no wrapper are left alone, and a template with no such target is not
// touched at all. Reapplying to an already shaped template is a no-op. On
// error the template is left unchanged. Returns the names of the targets that
// carry the wrapper.
func ShapeCUDALaunchJob(
	podTemplate *corev1.PodTemplateSpec,
	targetContainers []string,
	delivery CUDAToolsDelivery,
	claims ClaimDevices,
) ([]string, error) {
	if podTemplate == nil {
		return nil, fmt.Errorf("cuda-checkpoint launch-job requires a pod template")
	}
	if len(targetContainers) == 0 {
		return nil, fmt.Errorf("cuda-checkpoint launch-job requires at least one target container")
	}
	shaped := podTemplate.DeepCopy()
	wrapped := make([]string, 0, len(targetContainers))
	for _, name := range uniqueNames(targetContainers) {
		container := findContainer(&shaped.Spec, name)
		if container == nil {
			return nil, fmt.Errorf("target container %q does not exist", name)
		}
		needed, err := NeedsCUDALaunchJob(&shaped.Spec, container, claims)
		if err != nil {
			return nil, err
		}
		if !needed {
			continue
		}
		wrapped = append(wrapped, name)
	}
	if len(wrapped) == 0 {
		return nil, nil
	}
	if err := shapeCUDALaunchJob(shaped, wrapped, delivery); err != nil {
		return nil, err
	}
	*podTemplate = *shaped
	return wrapped, nil
}

// shapeCUDALaunchJob applies the launch-job contract to the named containers
// unconditionally; cuinterpose uses it too, since the shim's checkpoint path
// needs the job file even on one GPU.
func shapeCUDALaunchJob(podTemplate *corev1.PodTemplateSpec, containers []string, delivery CUDAToolsDelivery) error {
	if err := ensureCUDATools(&podTemplate.Spec, delivery); err != nil {
		return err
	}
	for _, name := range containers {
		container := findContainer(&podTemplate.Spec, name)
		if container == nil {
			return fmt.Errorf("target container %q does not exist", name)
		}
		if err := ensureCUDAToolsMount(container); err != nil {
			return err
		}
		if err := EnsureCUDACheckpointLaunchJob(container, CUDACheckpointPath); err != nil {
			return err
		}
	}
	return nil
}

// VerifyCUDALaunchJob reports whether spec carries the complete launch-job
// contract for every named container: the volume, the install init container,
// and per container the mount and the cuda-checkpoint wrapper. The operator
// uses it to refuse adopting a Job that was created without the contract.
func VerifyCUDALaunchJob(spec *corev1.PodSpec, containers []string) error {
	if err := verifyCUDATools(spec); err != nil {
		return err
	}
	for _, name := range containers {
		container := findContainer(spec, name)
		if container == nil {
			return fmt.Errorf("target container %q does not exist", name)
		}
		if !CUDAToolsMounted(container) {
			return fmt.Errorf("container %q does not mount %s at %s", name, CUDAToolsVolumeName, CUDAToolsMountPath)
		}
		if len(container.Command) != 1 || container.Command[0] != CUDACheckpointPath ||
			!isCUDACheckpointLaunchJob(container.Args) {
			return fmt.Errorf("container %q is not launched through %s --launch-job", name, CUDACheckpointPath)
		}
	}
	return nil
}

// CUDAToolsDelivered reports whether the named container of a live Pod has
// the tools volume mounted at its fixed path, which is what the restore side
// must reproduce before CRIU runs.
func CUDAToolsDelivered(spec *corev1.PodSpec, containerName string) bool {
	if spec == nil || verifyCUDATools(spec) != nil {
		return false
	}
	container := findContainer(spec, containerName)
	return container != nil && CUDAToolsMounted(container)
}

// CUDAToolsMounted reports whether container mounts the tools volume at its
// fixed path.
func CUDAToolsMounted(container *corev1.Container) bool {
	return slices.ContainsFunc(container.VolumeMounts, func(m corev1.VolumeMount) bool {
		return m.Name == CUDAToolsVolumeName && m.MountPath == CUDAToolsMountPath
	})
}

func verifyCUDATools(spec *corev1.PodSpec) error {
	if spec == nil {
		return fmt.Errorf("cuda tools require a pod spec")
	}
	if !slices.ContainsFunc(spec.Volumes, func(v corev1.Volume) bool {
		return v.Name == CUDAToolsVolumeName && isEmptyDirVolumeSource(v.VolumeSource)
	}) {
		return fmt.Errorf("pod lacks the %s emptyDir volume", CUDAToolsVolumeName)
	}
	if !slices.ContainsFunc(spec.InitContainers, func(c corev1.Container) bool {
		return c.Name == CUDAToolsInitContainerName
	}) {
		return fmt.Errorf("pod lacks the %s init container", CUDAToolsInitContainerName)
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

func uniqueNames(names []string) []string {
	unique := make([]string, 0, len(names))
	for _, name := range names {
		if !slices.Contains(unique, name) {
			unique = append(unique, name)
		}
	}
	return unique
}

// ensureCUDATools adds the volume, the init container, and the pull secrets
// the delivery needs, checking that anything already present agrees.
func ensureCUDATools(spec *corev1.PodSpec, delivery CUDAToolsDelivery) error {
	if err := ValidateImageReference(delivery.AgentImage); err != nil {
		return fmt.Errorf("snapshot agent image: %w", err)
	}
	if err := ensureCUDAToolsVolume(spec); err != nil {
		return err
	}
	if err := ensureCUDAToolsInitContainer(spec, delivery); err != nil {
		return err
	}
	ensureImagePullSecrets(spec, delivery.ImagePullSecrets)
	return nil
}

func ensureCUDAToolsVolume(spec *corev1.PodSpec) error {
	found := false
	for i := range spec.Volumes {
		volume := &spec.Volumes[i]
		if volume.Name != CUDAToolsVolumeName {
			continue
		}
		if found {
			return fmt.Errorf("duplicate %s volume", CUDAToolsVolumeName)
		}
		found = true
		if !isEmptyDirVolumeSource(volume.VolumeSource) {
			return fmt.Errorf("volume %q must be an emptyDir", CUDAToolsVolumeName)
		}
	}
	if !found {
		spec.Volumes = append(spec.Volumes, corev1.Volume{
			Name:         CUDAToolsVolumeName,
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

func expectedCUDAToolsInitContainer(delivery CUDAToolsDelivery) corev1.Container {
	pullPolicy := delivery.PullPolicy
	if pullPolicy == "" {
		pullPolicy = corev1.PullAlways
		if strings.Contains(delivery.AgentImage, "@sha256:") {
			pullPolicy = corev1.PullIfNotPresent
		}
	}
	return corev1.Container{
		Name:            CUDAToolsInitContainerName,
		Image:           delivery.AgentImage,
		ImagePullPolicy: pullPolicy,
		Command:         []string{"/bin/cp"},
		Args: []string{
			"--",
			cudaToolsImageCUDACheckpoint,
			cudaToolsImageLibrary,
			cudaToolsInstallMountPath + "/",
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
			Name:      CUDAToolsVolumeName,
			MountPath: cudaToolsInstallMountPath,
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

func ensureCUDAToolsInitContainer(spec *corev1.PodSpec, delivery CUDAToolsDelivery) error {
	expected := expectedCUDAToolsInitContainer(delivery)
	found := false
	for i := range spec.InitContainers {
		container := &spec.InitContainers[i]
		if container.Name != CUDAToolsInitContainerName {
			continue
		}
		if found {
			return fmt.Errorf("duplicate %s init container", CUDAToolsInitContainerName)
		}
		found = true
		if !initContainerMatches(container, &expected) {
			return fmt.Errorf("init container %q conflicts with the snapshot-cuda contract", CUDAToolsInitContainerName)
		}
	}
	if !found {
		spec.InitContainers = append(spec.InitContainers, expected)
	}
	return nil
}

func ensureCUDAToolsMount(container *corev1.Container) error {
	found := false
	for i := range container.VolumeMounts {
		mount := &container.VolumeMounts[i]
		if mount.Name != CUDAToolsVolumeName && mount.MountPath != CUDAToolsMountPath {
			continue
		}
		if found {
			return fmt.Errorf("container %q has duplicate %s mounts", container.Name, CUDAToolsVolumeName)
		}
		found = true
		if mount.Name != CUDAToolsVolumeName || mount.MountPath != CUDAToolsMountPath ||
			!mount.ReadOnly || mount.SubPath != "" || mount.SubPathExpr != "" ||
			mount.RecursiveReadOnly != nil ||
			(mount.MountPropagation != nil && *mount.MountPropagation != corev1.MountPropagationNone) {
			return fmt.Errorf("container %q has conflicting options on the %s mount", container.Name, CUDAToolsVolumeName)
		}
	}
	if !found {
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name:      CUDAToolsVolumeName,
			MountPath: CUDAToolsMountPath,
			ReadOnly:  true,
		})
	}
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
		return fmt.Errorf("container %q command conflicts with the cuda-checkpoint launch-job contract", container.Name)
	}
	if len(container.Command) == 0 {
		return fmt.Errorf("container %q requires container.command for cuda-checkpoint launch-job", container.Name)
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
