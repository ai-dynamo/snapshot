// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestInheritedNamespaceFile(t *testing.T) {
	file, err := inheritedNamespaceFile(-1, "pid")
	if err != nil || file != nil {
		t.Fatalf("legacy descriptor = %v, %v", file, err)
	}
	for _, fd := range []int{-2, 0, 1, 2, 1000000} {
		if _, err := inheritedNamespaceFile(fd, "pid"); err == nil {
			t.Fatalf("accepted descriptor %d", fd)
		}
	}
	original, err := os.Open("/proc/self/ns/pid")
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	fd, err := unix.FcntlInt(original.Fd(), unix.F_DUPFD_CLOEXEC, 3)
	if err != nil {
		t.Fatal(err)
	}
	inherited, err := inheritedNamespaceFile(fd, "pid")
	if err != nil {
		t.Fatal(err)
	}
	if err := inherited.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := inherited.Stat(); err == nil {
		t.Fatal("inherited descriptor remains open")
	}
	if _, err := original.Stat(); err != nil {
		t.Fatal(err)
	}
}
