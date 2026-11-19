// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cuda

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

// gb300Fabric is what nvidia-smi 580.173.02 prints for fabricQuery on a GB300
// node, whose four GPUs have registered with their NVLink fabric, with its UUIDs
// replaced.
const gb300Fabric = `GPU-00000000-0000-4000-8000-000000000000, Completed, Success, a1b2c3d4-0000-4000-8000-00000000c1a5, 1
GPU-00000000-0000-4000-8000-000000000001, Completed, Success, a1b2c3d4-0000-4000-8000-00000000c1a5, 1
GPU-00000000-0000-4000-8000-000000000002, Completed, Success, a1b2c3d4-0000-4000-8000-00000000c1a5, 1
GPU-00000000-0000-4000-8000-000000000003, Completed, Success, a1b2c3d4-0000-4000-8000-00000000c1a5, 1
`

func TestParseNvidiaSmiFabric(t *testing.T) {
	for _, tc := range []struct {
		name      string
		output    string
		uuid      string
		want      types.GPUFabric
		wantReady bool
		wantErr   string
	}{
		{
			name:   "a GPU registered with its fabric",
			output: gb300Fabric,
			uuid:   "GPU-00000000-0000-4000-8000-000000000002",
			want: types.GPUFabric{
				UUID: "GPU-00000000-0000-4000-8000-000000000002", ClusterUUID: "a1b2c3d4-0000-4000-8000-00000000c1a5",
				CliqueID: "1", State: "Completed", Status: "Success",
			},
			wantReady: true,
		},
		{
			name:   "a GPU still registering",
			output: "GPU-a, In Progress, N/A, N/A, N/A\n",
			uuid:   "GPU-a",
			want:   types.GPUFabric{UUID: "GPU-a", ClusterUUID: "N/A", CliqueID: "N/A", State: "In Progress", Status: "N/A"},
		},
		{
			name:   "a GPU whose registration failed",
			output: "GPU-a, Completed, Unknown Error, 00000000-0000-0000-0000-000000000000, 0\n",
			uuid:   "GPU-a",
			want: types.GPUFabric{
				UUID: "GPU-a", ClusterUUID: "00000000-0000-0000-0000-000000000000", CliqueID: "0",
				State: "Completed", Status: "Unknown Error",
			},
		},
		{
			name:   "a GPU without an NVLink fabric",
			output: "\nGPU-a, [N/A], [N/A], [N/A], [N/A]\n\n",
			uuid:   "GPU-a",
			want:   types.GPUFabric{UUID: "GPU-a", ClusterUUID: "[N/A]", CliqueID: "[N/A]", State: "[N/A]", Status: "[N/A]"},
		},
		{
			name:    "a GPU listed without its fabric",
			output:  "GPU-a\n",
			wantErr: `nvidia-smi reported "GPU-a", not a GPU's UUID and its NVLink fabric`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listed, err := parseNvidiaSmiFabric(tc.output)
			if tc.wantErr != "" {
				require.EqualError(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, listed[tc.uuid])
			assert.Equal(t, tc.wantReady, listed[tc.uuid].Ready())
		})
	}
}

func TestReadGPUFabric(t *testing.T) {
	asked := filepath.Join(t.TempDir(), "asked")
	installFakeNvidiaSMI(t, `
test "$#" = 4
test "$1" = "-i"
test "$3" = "`+fabricQuery+`"
test "$4" = "--format=csv,noheader"
echo "$2" > `+asked+`
cat <<'EOF'
`+gb300Fabric+`EOF
`)

	got, err := ReadGPUFabric(context.Background(),
		[]string{"GPU-00000000-0000-4000-8000-000000000003", "GPU-00000000-0000-4000-8000-000000000000"})
	require.NoError(t, err)
	recorded, err := os.ReadFile(asked)
	require.NoError(t, err)
	assert.Equal(t, "GPU-00000000-0000-4000-8000-000000000003,GPU-00000000-0000-4000-8000-000000000000\n", string(recorded),
		"nvidia-smi reads only the GPUs asked for")
	// In the order asked for, not in the order nvidia-smi lists them.
	assert.Equal(t, []string{"GPU-00000000-0000-4000-8000-000000000003", "GPU-00000000-0000-4000-8000-000000000000"},
		[]string{got[0].UUID, got[1].UUID})

	_, err = ReadGPUFabric(context.Background(), []string{"GPU-00000000-0000-4000-8000-000000000003", "GPU-b"})
	require.EqualError(t, err, "nvidia-smi did not report the NVLink fabric of GPU GPU-b")
}

func TestReadGPUFabricSaysWhyNvidiaSMIFailed(t *testing.T) {
	// An nvidia-smi older than the fabric fields.
	installFakeNvidiaSMI(t, `echo 'Field "fabric.state" is not a valid field to query.'; echo; exit 2`)

	_, err := ReadGPUFabric(context.Background(), []string{"GPU-a"})
	require.EqualError(t, err, "nvidia-smi "+fabricQuery+`: exit status 2: Field "fabric.state" is not a valid field to query.`)
}

func installFakeNvidiaSMI(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "nvidia-smi"), []byte("#!/bin/sh\nset -eu\n"+body+"\n"), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}
