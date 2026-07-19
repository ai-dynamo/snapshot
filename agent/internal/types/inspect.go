// Copyright 2026 NVIDIA Corporation
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package types

import (
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// MountInfo holds parsed mount information from /proc/pid/mountinfo.
type MountInfo struct {
	MountPoint string
	FSType     string
	VFSOptions string // superblock options (e.g. "upperdir=...")

	// IsOCIManaged is true when the mount destination matches an OCI spec entry
	// (including /run/ ↔ /var/run/ aliasing). Set by ClassifyMounts.
	IsOCIManaged bool
}

// CheckpointContainerSnapshot holds runtime container state collected during checkpoint inspection.
type CheckpointContainerSnapshot struct {
	PID            int
	RootFS         string
	UpperDir       string
	OCISpec        *specs.Spec
	Mounts         []MountInfo
	NetNSInode     uint64
	StdioFDs       []string // readlink targets for FDs 0, 1, 2 (e.g. "pipe:[12345]")
	HostCgroupPath string   // host filesystem path for CRIU's --freeze-cgroup
	CUDAHostPIDs   []int    // host-visible PIDs used for checkpoint-side CUDA actions
	CUDANSPIDs     []int    // namespace-relative PIDs stored in the checkpoint manifest
	GPUUUIDs       []string // source GPU UUIDs from kubelet PodResources API
}

// RestoreContainerSnapshot holds inspected state for the restore target.
type RestoreContainerSnapshot struct {
	CheckpointPath string
	PlaceholderPID int
	TargetRoot     string
	CgroupRoot     string
	CUDADeviceMap  string
}
