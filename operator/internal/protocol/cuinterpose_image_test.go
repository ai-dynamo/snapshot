// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package protocol

import (
	"context"
	"crypto/rand"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

// Run with SNAPSHOT_CUINTERPOSE_TEST_IMAGE=<built-agent-image> go test
// ./internal/protocol -run TestCuInterposeInstallerImage -count=1 -v.
// The image must already be available to Docker. No GPU or cluster is required.
func TestCuInterposeInstallerImage(t *testing.T) {
	image := os.Getenv("SNAPSHOT_CUINTERPOSE_TEST_IMAGE")
	if image == "" {
		t.Skip("set SNAPSHOT_CUINTERPOSE_TEST_IMAGE to check the built agent image")
	}
	template := cuInterposeTemplate()
	template.Spec.Containers[0].Command = []string{"/bin/sh", "-eu", "-c", `
grep -F -- "$SNAPSHOT_TEST_LIBRARY" /proc/$$/maps >/dev/null
test "$LD_PRELOAD" = "$SNAPSHOT_TEST_LIBRARY"
test "$UNCHANGED" = "original value"
test "$1" = "two words"
printf 'frontend-loaded\n'
`}
	template.Spec.Containers[0].Args = []string{"workload", "two words"}
	installerConfig := testCuInterposeInstaller()
	installerConfig.Image = image
	require.NoError(t, ShapeCuInterposeCapture(template, "worker", installerConfig))
	installer := template.Spec.InitContainers[0]
	worker := template.Spec.Containers[0]
	require.Len(t, installer.VolumeMounts, 1)
	require.False(t, installer.VolumeMounts[0].ReadOnly)
	security := installer.SecurityContext
	require.True(t, *security.RunAsNonRoot)
	require.True(t, *security.ReadOnlyRootFilesystem)
	require.False(t, *security.AllowPrivilegeEscalation)
	require.Equal(t, []corev1.Capability{"ALL"}, security.Capabilities.Drop)

	// Bash splits the two argv lists by count, without evaluating their contents.
	// Copy and launch share a disposable tmpfs so the test needs no host bind paths.
	const installAndLaunch = `installer_argc=$1
shift
installer=( "${@:1:installer_argc}" )
shift "$installer_argc"
"${installer[@]}"
exec "$@"`
	installerArgv := append(append([]string{}, installer.Command...), installer.Args...)
	containerName := "snapshot-cuinterpose-test-" + rand.Text()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// A deadline can stop the Docker client before its container exits.
		// --rm handles normal exit. Forced removal handles that timeout path.
		_ = exec.CommandContext(ctx, "docker", "rm", "-f", containerName).Run()
	})
	args := []string{
		"run", "--rm", "--pull=never", "--network=none", "--read-only",
		"--name", containerName,
		"--user", strconv.FormatInt(*security.RunAsUser, 10),
		"--cap-drop=ALL", "--security-opt=no-new-privileges",
		"--memory", strconv.FormatInt(installer.Resources.Limits.Memory().Value(), 10),
		"--cpus", strconv.FormatFloat(installer.Resources.Limits.Cpu().AsApproximateFloat64(), 'f', -1, 64),
		"--tmpfs", installer.VolumeMounts[0].MountPath + ":rw,exec,mode=1777",
		"--env", "SNAPSHOT_TEST_LIBRARY=" + podcontract.CuInterposeLibraryPath,
		"--env", "UNCHANGED=original value",
		"--entrypoint", "/bin/bash", installer.Image,
		"-euc", installAndLaunch, "installer-test", strconv.Itoa(len(installerArgv)),
	}
	args = append(args, installerArgv...)
	args = append(args, worker.Command...)
	args = append(args, worker.Args...)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	output, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	require.NoError(t, err, "installer and workload in %s: %s", image, output)
	require.Contains(t, string(output), "frontend-loaded\n")
}
