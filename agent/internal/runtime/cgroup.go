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

package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const HostCgroupPath = "/sys/fs/cgroup"

// ResolveCgroupRootFromHostPID reads the unified cgroup v2 path for a PID via /host/proc.
func ResolveCgroupRootFromHostPID(pid int) (string, error) {
	cgroupFile := filepath.Join(HostProcPath, strconv.Itoa(pid), "cgroup")
	data, err := os.ReadFile(cgroupFile)
	if err != nil {
		return "", fmt.Errorf("failed reading %s: %w", cgroupFile, err)
	}

	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "0::") {
			continue
		}
		path := strings.TrimPrefix(line, "0::")
		if path == "" {
			return "/", nil
		}
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		return filepath.Clean(path), nil
	}

	return "", fmt.Errorf("unified cgroup entry not found in %s", cgroupFile)
}
