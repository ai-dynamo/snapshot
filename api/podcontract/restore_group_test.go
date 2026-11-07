// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podcontract

import (
	"testing"

	"k8s.io/apimachinery/pkg/types"
)

func TestRestoreGroupBindingBinds(t *testing.T) {
	const name, uid = "llm-tp8-r1", types.UID("uid-r1")
	binding := func(name, uid string) map[string]string {
		return map[string]string{RestoreGroupAnnotation: name, RestoreGroupUIDAnnotation: uid}
	}
	for _, tc := range []struct {
		name        string
		annotations map[string]string
		want        bool
	}{
		{"both annotations match", binding(name, string(uid)), true},
		{"no UID", map[string]string{RestoreGroupAnnotation: name}, false},
		{"an empty UID", binding(name, ""), false},
		{"another UID under the same name", binding(name, "uid-r0"), false},
		{"another name", binding("llm-tp8-r0", string(uid)), false},
		{"a UID without a name", map[string]string{RestoreGroupUIDAnnotation: string(uid)}, false},
		{"no annotations", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binding := RestoreGroupBindingOf(tc.annotations)
			if got := binding.Binds(name, uid); got != tc.want {
				t.Fatalf("%+v.Binds(%q, %q) = %v, want %v", binding, name, uid, got, tc.want)
			}
		})
	}

	t.Run("nothing binds to an empty name and UID", func(t *testing.T) {
		if RestoreGroupBindingOf(nil).Binds("", "") {
			t.Fatal("an absent binding binds the Pod to an empty name and UID")
		}
	})
}
