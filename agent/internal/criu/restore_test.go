// SPDX-License-Identifier: Apache-2.0

package criu

import (
	"os"
	"path/filepath"
	"testing"

	criurpc "github.com/checkpoint-restore/go-criu/v8/rpc"
	"github.com/go-logr/logr"
	"google.golang.org/protobuf/proto"

	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

func TestRestoreFDLayoutUsesSWRKAndInheritedFDs(t *testing.T) {
	if swrkTransportFD != 3 || firstInheritedFD != 4 {
		t.Fatalf("swrk fd layout starts at transport=%d inherited=%d, want 3/4", swrkTransportFD, firstInheritedFD)
	}
	image, work, provider, netns := os.NewFile(10, "image"), os.NewFile(11, "work"), os.NewFile(12, "provider"), os.NewFile(13, "netns")
	opts := new(criurpc.CriuOpts)
	layout := restoreFDLayout{files: []*os.File{image}, opts: opts}
	opts.ImagesDirFd = proto.Int32(int32(image.Fd()))
	layout.appendFile("work-dir", work)
	opts.WorkDirFd = proto.Int32(int32(work.Fd()))
	layout.add("0-extmem-provider", provider)
	layout.add("extNetNs", netns)
	if err := layout.validate(); err != nil {
		t.Fatal(err)
	}
	if got := opts.GetImagesDirFd(); got != int32(image.Fd()) {
		t.Fatalf("RPC image fd = %d, want parent fd %d", got, image.Fd())
	}
	if got := opts.GetWorkDirFd(); got != int32(work.Fd()) {
		t.Fatalf("RPC work fd = %d, want parent fd %d", got, work.Fd())
	}
	if got := layout.fd("work-dir"); got != 5 {
		t.Fatalf("child work fd = %d, want 5", got)
	}
	if got := opts.GetInheritFd()[0].GetFd(); got != 6 {
		t.Fatalf("provider fd = %d, want 6", got)
	}
	if got := opts.GetInheritFd()[1].GetFd(); got != 7 {
		t.Fatalf("netns fd = %d, want 7", got)
	}
}

func TestBuildRestoreOptsIgnoresCheckpointCgroups(t *testing.T) {
	manifest := types.NewCheckpointManifest(
		"checkpoint",
		types.CRIUDumpManifest{CRIU: types.CRIUSettings{ManageCgroupsMode: "soft"}, ExtMnt: map[string]string{"/": "."}},
		types.SourcePodManifest{}, types.OverlayManifest{},
	)
	opts, err := BuildRestoreOpts(manifest, t.TempDir(), "/target-cgroup", logr.Discard())
	if err != nil {
		t.Fatal(err)
	}
	if opts.GetManageCgroups() || opts.GetManageCgroupsMode() != criurpc.CriuCgMode_IGNORE {
		t.Fatalf("restore cgroup mode = manage=%t mode=%v, want false/IGNORE", opts.GetManageCgroups(), opts.GetManageCgroupsMode())
	}
	if len(opts.GetCgRoot()) != 0 {
		t.Fatalf("restore cgroup root = %#v, want none", opts.GetCgRoot())
	}
}

func TestCopyRestoreLogFromInheritedWorkFD(t *testing.T) {
	workDir := t.TempDir()
	checkpointDir := t.TempDir()
	want := "(00.000001) Error (criu/cr-restore.c:123): concrete failure\n"
	if err := os.WriteFile(filepath.Join(workDir, RestoreLogFilename), []byte(want), 0600); err != nil {
		t.Fatal(err)
	}
	work, err := os.Open(workDir)
	if err != nil {
		t.Fatal(err)
	}
	defer work.Close()

	gotPath, err := copyRestoreLog(checkpointDir, int(work.Fd()), "")
	if err != nil {
		t.Fatal(err)
	}
	if wantPath := filepath.Join(checkpointDir, RestoreLogFilename+".failed"); gotPath != wantPath {
		t.Fatalf("copied log path = %q, want %q", gotPath, wantPath)
	}
	got, err := os.ReadFile(gotPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("copied log = %q, want %q", got, want)
	}
}
