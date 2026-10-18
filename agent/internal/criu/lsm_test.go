// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package criu

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/checkpoint-restore/go-criu/v8/crit"
	"github.com/checkpoint-restore/go-criu/v8/crit/images/creds"
	criu_core "github.com/checkpoint-restore/go-criu/v8/crit/images/criu-core"
	"google.golang.org/protobuf/proto"
)

func coreEntry(withCreds bool, label string) *criu_core.CoreEntry {
	entry := &criu_core.CoreEntry{Mtype: criu_core.CoreEntry_X86_64.Enum()}
	if !withCreds {
		return entry
	}
	c := &creds.CredsEntry{
		Uid: proto.Uint32(0), Gid: proto.Uint32(0), Euid: proto.Uint32(0), Egid: proto.Uint32(0),
		Suid: proto.Uint32(0), Sgid: proto.Uint32(0), Fsuid: proto.Uint32(0), Fsgid: proto.Uint32(0),
		Secbits: proto.Uint32(0),
	}
	if label != "" {
		c.LsmProfile = proto.String(label)
	}
	entry.ThreadCore = &criu_core.ThreadCoreEntry{
		FutexRla: proto.Uint64(0), FutexRlaLen: proto.Uint32(0), Creds: c,
	}
	return entry
}

func writeCoreImage(t *testing.T, dir, name string, entries ...*criu_core.CoreEntry) {
	t.Helper()
	image := &crit.CriuImage{Magic: coreImageMagic}
	for _, e := range entries {
		image.Entries = append(image.Entries, &crit.CriuEntry{Message: e})
	}
	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := crit.New(nil, f, "", false, false).Encode(image); err != nil {
		t.Fatal(err)
	}
}

func TestCheckRecordedLSMLabels(t *testing.T) {
	const profile = "apparmor:cri-containerd.apparmor.d"
	for name, tc := range map[string]struct {
		images  map[string][]*criu_core.CoreEntry
		profile string
		wantErr string
	}{
		"every process labeled": {
			images: map[string][]*criu_core.CoreEntry{
				"core-1.img": {coreEntry(true, "cri-containerd.apparmor.d")},
				"core-2.img": {coreEntry(true, "cri-containerd.apparmor.d")},
			},
			profile: profile,
		},
		// The target's label replaces it, as it replaces any recorded label.
		"a process in a child profile": {
			images: map[string][]*criu_core.CoreEntry{
				"core-1.img": {coreEntry(true, "cri-containerd.apparmor.d//restricted")},
			},
			profile: profile,
		},
		"a zombie has no credentials": {
			images: map[string][]*criu_core.CoreEntry{
				"core-1.img": {coreEntry(true, "cri-containerd.apparmor.d")},
				"core-2.img": {coreEntry(false, "")},
			},
			profile: profile,
		},
		"one process without a label": {
			images: map[string][]*criu_core.CoreEntry{
				"core-1.img": {coreEntry(true, "cri-containerd.apparmor.d")},
				"core-2.img": {coreEntry(true, "")},
			},
			profile: profile,
			wantErr: "core-2.img",
		},
		"no credentials at all": {
			images:  map[string][]*criu_core.CoreEntry{"core-1.img": {coreEntry(false, "")}},
			profile: profile,
			wantErr: "no CRIU process credentials",
		},
		"no core images": {
			profile: profile,
			wantErr: "no CRIU process credentials",
		},
		"target without a label": {
			images:  map[string][]*criu_core.CoreEntry{"core-1.img": {coreEntry(true, "")}},
			profile: LSMProfileNone,
		},
		"no profile passed": {
			images: map[string][]*criu_core.CoreEntry{"core-1.img": {coreEntry(true, "")}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			for imageName, entries := range tc.images {
				writeCoreImage(t, dir, imageName, entries...)
			}
			err := CheckRecordedLSMLabels(dir, tc.profile)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("CheckRecordedLSMLabels: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("CheckRecordedLSMLabels = %v, want an error with %q", err, tc.wantErr)
			}
		})
	}
}

// A core image that is a symlink could point anywhere; it is refused, not
// followed.
func TestCheckRecordedLSMLabelsRefusesSymlinkedImage(t *testing.T) {
	dir, other := t.TempDir(), t.TempDir()
	writeCoreImage(t, other, "elsewhere.img", coreEntry(true, "cri-containerd.apparmor.d"))
	if err := os.Symlink(filepath.Join(other, "elsewhere.img"), filepath.Join(dir, "core-1.img")); err != nil {
		t.Fatal(err)
	}
	if err := CheckRecordedLSMLabels(dir, "apparmor:cri-containerd.apparmor.d"); err == nil {
		t.Fatal("CheckRecordedLSMLabels followed a symlinked core image")
	}
}

func TestRemoveKdatCache(t *testing.T) {
	t.Run("removes the cache", func(t *testing.T) {
		root := t.TempDir()
		mustMkdir(t, filepath.Join(root, "run"))
		mustWrite(t, filepath.Join(root, "run", "criu.kdat"))
		if err := RemoveKdatCache(root); err != nil {
			t.Fatalf("RemoveKdatCache: %v", err)
		}
		if _, err := os.Lstat(filepath.Join(root, "run", "criu.kdat")); !os.IsNotExist(err) {
			t.Fatalf("cache still there: %v", err)
		}
	})
	t.Run("no cache, no /run", func(t *testing.T) {
		root := t.TempDir()
		if err := RemoveKdatCache(root); err != nil {
			t.Fatalf("RemoveKdatCache without /run: %v", err)
		}
		mustMkdir(t, filepath.Join(root, "run"))
		if err := RemoveKdatCache(root); err != nil {
			t.Fatalf("RemoveKdatCache without the cache: %v", err)
		}
	})
	t.Run("removes a symlinked cache, not its target", func(t *testing.T) {
		root, outside := t.TempDir(), t.TempDir()
		target := filepath.Join(outside, "criu.kdat")
		mustWrite(t, target)
		mustMkdir(t, filepath.Join(root, "run"))
		if err := os.Symlink(target, filepath.Join(root, "run", "criu.kdat")); err != nil {
			t.Fatal(err)
		}
		if err := RemoveKdatCache(root); err != nil {
			t.Fatalf("RemoveKdatCache: %v", err)
		}
		if _, err := os.Lstat(filepath.Join(root, "run", "criu.kdat")); !os.IsNotExist(err) {
			t.Fatalf("symlink still there: %v", err)
		}
		if _, err := os.Stat(target); err != nil {
			t.Fatalf("symlink target was touched: %v", err)
		}
	})
	t.Run("/run symlinked outside the root", func(t *testing.T) {
		root, outside := t.TempDir(), t.TempDir()
		target := filepath.Join(outside, "criu.kdat")
		mustWrite(t, target)
		if err := os.Symlink(outside, filepath.Join(root, "run")); err != nil {
			t.Fatal(err)
		}
		if err := RemoveKdatCache(root); err != nil {
			t.Fatalf("RemoveKdatCache: %v", err)
		}
		if _, err := os.Stat(target); err != nil {
			t.Fatalf("file outside the root was removed: %v", err)
		}
	})
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("cache"), 0o644); err != nil {
		t.Fatal(err)
	}
}
