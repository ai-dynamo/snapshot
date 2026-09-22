// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCommandInNamespacesPassesOpenFiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("TEST_CONTAINER_ROOT", dir)
	script := `#!/bin/sh
set -eu
while [ "$1" != -- ]; do
    case "$1" in
        --mount=*) test "$(readlink "${1#*=}")" = "$(readlink /proc/self/ns/mnt)" ;;
        --root=*|--wd=*) test "$(readlink "${1#*=}")" = "$TEST_CONTAINER_ROOT" ;;
        -t) shift ;;
        -u|-i|-n|-p) ;;
        *) exit 1 ;;
    esac
    shift
done
shift
exec "$@"
`
	binary := filepath.Join(dir, "worker")
	for path, contents := range map[string]string{
		filepath.Join(dir, "nsenter"):  script,
		binary:                         "#!/bin/sh\ncat \"$1\"\n",
		filepath.Join(dir, "artifact"): "checkpoint contents",
	} {
		if err := os.WriteFile(path, []byte(contents), 0700); err != nil {
			t.Fatal(err)
		}
	}
	mountNS, err := os.Open("/proc/self/ns/mnt")
	if err != nil {
		t.Fatal(err)
	}
	defer mountNS.Close()
	cmd, closeFiles, err := CommandInNamespaces(context.Background(), os.Getpid(), mountNS, dir, binary)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFiles()
	// The child must execute the already-open binary, not resolve its old path.
	if err := os.Rename(binary, binary+".moved"); err != nil {
		t.Fatal(err)
	}
	artifact, err := os.Open(filepath.Join(dir, "artifact"))
	if err != nil {
		t.Fatal(err)
	}
	defer artifact.Close()
	cmd.Args = append(cmd.Args, InheritFile(cmd, artifact))
	output, err := cmd.CombinedOutput()
	if err != nil || string(output) != "checkpoint contents" {
		t.Fatalf("namespace command: %v, output %q", err, output)
	}
}
