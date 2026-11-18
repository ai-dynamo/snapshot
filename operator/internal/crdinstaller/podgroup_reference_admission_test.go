//go:build integration

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package crdinstaller_test

import "testing"

func TestPodGroupReferenceNamesAdmission(t *testing.T) {
	cl := startPodGroupEnv(t)
	for _, field := range []string{"snapshot", "target"} {
		for _, tc := range []struct {
			name  string
			value string
			valid bool
		}{
			{"slash", "bad/name", false},
			{"percent", "bad%name", false},
			{"dot", ".", false},
			{"double-dot", "..", false},
			{"uppercase", "Bad", false},
			{"dotted", "group.example", true},
		} {
			t.Run(field+"-"+tc.name, func(t *testing.T) {
				restore := newPodGroupRestore("reference-"+field+"-"+tc.name, 1)
				path := "spec.snapshotRef.name"
				if field == "snapshot" {
					restore.Spec.SnapshotRef.Name = tc.value
				} else {
					restore.Spec.Targets[0].PodRef.Name = tc.value
					path = "spec.targets[0].podRef.name"
				}
				refusal := ""
				if !tc.valid {
					refusal = path
				}
				requireAdmission(t, cl.Create(t.Context(), restore), refusal)
			})
		}
	}
}
