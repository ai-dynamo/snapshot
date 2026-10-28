// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// ContainerSnapshotConfig holds the settings for one checkpointed container,
// matched to it by name. Snapshot copies them into the PodSnapshotContent at
// checkpoint time, and every restore of that checkpoint uses that copy.
//
// Not enabled yet: the operator rejects a checkpointReadinessProbe or a
// lifecycle until support is complete.
// +kubebuilder:validation:XValidation:rule="!has(self.checkpointReadinessProbe) || !has(self.checkpointReadinessProbe.file) || !has(self.lifecycle) || !has(self.lifecycle.postRestore) || !has(self.lifecycle.postRestore.file) || self.checkpointReadinessProbe.file.path != self.lifecycle.postRestore.file.path",message="checkpointReadinessProbe and lifecycle.postRestore must use different file paths"
type ContainerSnapshotConfig struct {
	// Name of the checkpointed container these settings apply to.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`

	// CheckpointReadinessProbe tells when the application is safe to
	// checkpoint. When it is set, Snapshot waits for it instead of the
	// container's Ready condition. When it is omitted, Snapshot waits for the
	// container to be Ready, as before.
	// +optional
	CheckpointReadinessProbe *CheckpointReadinessProbe `json:"checkpointReadinessProbe,omitempty"`

	// Lifecycle holds actions Snapshot runs at points in the checkpoint and
	// restore lifecycle.
	// +optional
	Lifecycle *SnapshotLifecycle `json:"lifecycle,omitempty"`
}

// CheckpointReadinessProbe checks whether the application is safe to
// checkpoint. Snapshot checks it every periodSeconds until it succeeds
// successThreshold times in a row. A failed check resets the count and
// Snapshot keeps waiting; the container is never restarted.
// +kubebuilder:validation:XValidation:rule="has(self.file) != has(self.httpGet)",message="set exactly one of file or httpGet"
type CheckpointReadinessProbe struct {
	CheckpointReadinessProbeHandler `json:",inline"`

	// InitialDelaySeconds is how long to wait after the container starts
	// before the first check.
	// +optional
	// +kubebuilder:default=0
	// +kubebuilder:validation:Minimum=0
	InitialDelaySeconds int32 `json:"initialDelaySeconds,omitempty"`

	// PeriodSeconds is the time between checks.
	// +optional
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	PeriodSeconds int32 `json:"periodSeconds,omitempty"`

	// TimeoutSeconds limits each check.
	// +optional
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`

	// SuccessThreshold is how many checks in a row must succeed.
	// +optional
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	SuccessThreshold int32 `json:"successThreshold,omitempty"`
}

// CheckpointReadinessProbeHandler is how the probe checks the application.
type CheckpointReadinessProbeHandler struct {
	// File succeeds when a regular file exists at the path, inside the
	// container.
	// +optional
	File *ControlFileAction `json:"file,omitempty"`

	// HTTPGet succeeds when a GET to the Pod IP returns 200.
	// +optional
	HTTPGet *LifecycleHTTPAction `json:"httpGet,omitempty"`
}

// SnapshotLifecycle holds actions Snapshot runs at points in the checkpoint
// and restore lifecycle.
type SnapshotLifecycle struct {
	// PostRestore tells the application, after Snapshot restored its process
	// and GPU state, that it may resume.
	// +optional
	PostRestore *PostRestoreAction `json:"postRestore,omitempty"`
}

// PostRestoreAction tells a restored application that it may resume. Snapshot
// retries it every periodSeconds until it succeeds or the restore deadline
// passes; the restore succeeds only after it does. The application must
// handle the same notification more than once.
// +kubebuilder:validation:XValidation:rule="has(self.file) != has(self.httpPost)",message="set exactly one of file or httpPost"
type PostRestoreAction struct {
	PostRestoreHandler `json:",inline"`

	// PeriodSeconds is the time between attempts.
	// +optional
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	PeriodSeconds int32 `json:"periodSeconds,omitempty"`

	// TimeoutSeconds limits each attempt.
	// +optional
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
}

// PostRestoreHandler is how Snapshot notifies the restored application.
type PostRestoreHandler struct {
	// File creates a regular file at the path, inside the container. The
	// parent directory must exist. If the file already exists before the
	// restore, the restore fails.
	// +optional
	File *ControlFileAction `json:"file,omitempty"`

	// HTTPPost sends a POST with an empty body to the Pod IP. A 200 or 204
	// response means the application accepted the notification.
	// +optional
	HTTPPost *LifecycleHTTPAction `json:"httpPost,omitempty"`
}

// ControlFileAction names a file inside the container's filesystem.
type ControlFileAction struct {
	// Path is an absolute path inside the container, without "." or ".."
	// parts. It must be on the container's own filesystem or an emptyDir
	// volume; paths on hostPath and PersistentVolumeClaim volumes are refused.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=2
	// +kubebuilder:validation:MaxLength=4096
	// +kubebuilder:validation:XValidation:rule="self.startsWith('/') && self.split('/').all(p, p != '.' && p != '..')",message="path must be absolute, without . or .. parts"
	Path string `json:"path"`
}

// LifecycleHTTPAction is a plain HTTP request to a port on the Pod IP. Snapshot
// does not follow redirects, and does not support Pods on the host network.
type LifecycleHTTPAction struct {
	// Path of the request. It must start with "/" and has no host, query, or
	// fragment.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=4096
	// +kubebuilder:validation:XValidation:rule="self.startsWith('/') && !self.startsWith('//') && !self.contains('?') && !self.contains('#')",message="path must start with / and have no host, query, or fragment"
	Path string `json:"path"`

	// Port on the Pod IP.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
}
