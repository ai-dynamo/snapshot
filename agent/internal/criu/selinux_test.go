// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package criu

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-logr/logr/testr"
)

func TestIsSELinuxLabel(t *testing.T) {
	for label, want := range map[string]bool{
		"system_u:system_r:container_t:s0:c2,c28": true,
		"system_u:system_r:spc_t:s0":              true,
		"unconfined":                              false,
		"cri-containerd.apparmor.d (enforce)":     false,
		"kernel":                                  false,
		"":                                        false,
	} {
		if got := IsSELinuxLabel(label); got != want {
			t.Errorf("IsSELinuxLabel(%q) = %t, want %t", label, got, want)
		}
	}
}

func TestProvideSELinuxConfigKeepsAnExistingConfig(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, selinuxConfigFilename)
	if err := os.WriteFile(config, []byte("SELINUX=enforcing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	undo, err := provideSELinuxConfig(dir, testr.New(t))
	if err != nil {
		t.Fatalf("provideSELinuxConfig: %v", err)
	}
	if err := undo(); err != nil {
		t.Fatalf("undo: %v", err)
	}
	if data, err := os.ReadFile(config); err != nil || string(data) != "SELINUX=enforcing\n" {
		t.Fatalf("config after undo = %q, %v", data, err)
	}
}

func TestProvideSELinuxConfigRefusesASymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "elsewhere")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "selinux")
	if err := os.Symlink(target, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := provideSELinuxConfig(dir, testr.New(t)); err == nil || !strings.Contains(err.Error(), "is not a directory") {
		t.Fatalf("provideSELinuxConfig = %v, want a not-a-directory error", err)
	}
}

func TestProvideSELinuxConfigMountsAndRemovesIt(t *testing.T) {
	if os.Getenv("SNAPSHOT_PRIVILEGED_TESTS") != "1" {
		t.Skip("set SNAPSHOT_PRIVILEGED_TESTS=1 in a privileged Linux environment")
	}
	for name, dirExists := range map[string]bool{"missing dir": false, "dir without config": true} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "selinux")
			other := filepath.Join(dir, "semanage.conf")
			if dirExists {
				if err := os.Mkdir(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(other, nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			undo, err := provideSELinuxConfig(dir, testr.New(t))
			if err != nil {
				t.Fatalf("provideSELinuxConfig: %v", err)
			}
			config := filepath.Join(dir, selinuxConfigFilename)
			if _, err := os.Stat(config); err != nil {
				t.Fatalf("config while mounted: %v", err)
			}
			if err := undo(); err != nil {
				t.Fatalf("undo: %v", err)
			}
			if _, err := os.Stat(config); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("config after undo: %v, want it gone", err)
			}
			_, err = os.Stat(other)
			if dirExists && err != nil {
				t.Fatalf("existing file after undo: %v", err)
			}
			if _, err := os.Stat(dir); !dirExists && !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("created dir after undo: %v, want it removed", err)
			}
		})
	}
}
