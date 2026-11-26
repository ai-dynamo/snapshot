// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
	"k8s.io/utils/ptr"

	snapshotv1alpha1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
)

func validAgentConfig() *AgentConfig {
	return &AgentConfig{
		Storage: StorageSpec{
			Type:     "pvc",
			BasePath: "/checkpoints",
		},
		Restore: RestoreSpec{
			TimeoutSeconds: 60,
		},
		PageBroker: PageBrokerSpec{ControlSocketPath: "/pagebroker/control/pagebroker.sock"},
	}
}

// The key an admin flips in the ConfigMap has to be the key the agent reads,
// and an agent whose ConfigMap predates it has to keep the gate on.
func TestRestoreSpecParsesSkipCompatCheck(t *testing.T) {
	cases := map[string]bool{
		"restore:\n  skipCompatCheck: true\n":  true,
		"restore:\n  skipCompatCheck: false\n": false,
		"restore:\n  timeoutSeconds: 60\n":     false,
	}
	for document, want := range cases {
		cfg := &AgentConfig{}
		if err := yaml.Unmarshal([]byte(document), cfg); err != nil {
			t.Fatalf("unmarshal %q: %v", document, err)
		}
		if cfg.Restore.SkipCompatCheck != want {
			t.Errorf("%q parsed skipCompatCheck = %v, want %v", document, cfg.Restore.SkipCompatCheck, want)
		}
	}
}

func TestAgentConfigValidateRequiresFixedStorageBasePath(t *testing.T) {
	for _, basePath := range []string{"checkpoints", " /checkpoints ", "/checkpoints/../other", "/other"} {
		cfg := validAgentConfig()
		cfg.Storage.BasePath = basePath
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate accepted storage base path %q", basePath)
		}
	}
}

func TestAgentConfigValidateRequiresPageBrokerControlSocket(t *testing.T) {
	cfg := validAgentConfig()
	cfg.PageBroker.ControlSocketPath = ""

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for missing PageBroker control socket")
	}
}

func TestPageBrokerRestoreMode(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  string
	}{
		{"", "direct"}, {"  ", "direct"}, {"staged", "staged"}, {"direct", "direct"}, {" DIRECT ", "direct"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			cfg := validAgentConfig()
			cfg.PageBroker.RestoreMode = tc.input
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			if cfg.PageBroker.RestoreMode != tc.want {
				t.Fatalf("mode = %q, want %q", cfg.PageBroker.RestoreMode, tc.want)
			}
		})
	}
	cfg := validAgentConfig()
	cfg.PageBroker.RestoreMode = "invalid"
	if err := cfg.Validate(); err == nil {
		t.Fatal("accepted invalid restore mode")
	}
}

// A config written before checkpoint.timeoutSeconds existed must still load, and must come up with
// the guard on rather than unbounded.
func TestCheckpointSpecDefaultsWhenAbsent(t *testing.T) {
	var spec CheckpointSpec
	require.NoError(t, yaml.Unmarshal([]byte("{}"), &spec))

	require.NoError(t, spec.Validate())
	assert.Equal(t, time.Duration(DefaultCheckpointTimeoutSeconds)*time.Second, spec.CheckpointTimeout())
}

func TestCheckpointSpecRejectsAnExplicitNonPositiveTimeout(t *testing.T) {
	for _, raw := range []string{"timeoutSeconds: 0\n", "timeoutSeconds: -1\n"} {
		var spec CheckpointSpec
		require.NoError(t, yaml.Unmarshal([]byte(raw), &spec))
		require.Error(t, spec.Validate(), raw)
	}
}

func TestCheckpointSpecHonoursAnExplicitTimeout(t *testing.T) {
	var spec CheckpointSpec
	require.NoError(t, yaml.Unmarshal([]byte("timeoutSeconds: 45\n"), &spec))

	require.NoError(t, spec.Validate())
	assert.Equal(t, 45*time.Second, spec.CheckpointTimeout())
}

// time.Duration counts nanoseconds in an int64, so a large enough seconds value wraps negative and
// every call site reads that as "no timeout" — the bound silently disappears. Reject it instead.
func TestTimeoutsRejectValuesThatOverflowDuration(t *testing.T) {
	overflow := int(maxTimeoutSeconds) + 1
	require.Less(t, time.Duration(overflow)*time.Second, time.Duration(0),
		"the test value must actually overflow, or this proves nothing")

	checkpoint := CheckpointSpec{TimeoutSeconds: &overflow}
	require.Error(t, checkpoint.Validate())

	restore := RestoreSpec{TimeoutSeconds: overflow}
	require.Error(t, restore.Validate())
}

func TestTimeoutsAcceptTheLargestRepresentableValue(t *testing.T) {
	limit := int(maxTimeoutSeconds)
	require.Positive(t, time.Duration(limit)*time.Second)

	checkpoint := CheckpointSpec{TimeoutSeconds: &limit}
	require.NoError(t, checkpoint.Validate())

	restore := RestoreSpec{TimeoutSeconds: limit}
	require.NoError(t, restore.Validate())
}

// Absent and explicit zero are different answers: zero asks for no retry, absent
// expresses no preference and takes the default.
func TestResolveRetryLimit(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured *int
		requested  *int32
		want       int
	}{
		{"neither set takes the default", nil, nil, DefaultCheckpointRetryLimit},
		{"the cluster default applies when the request is silent", ptr.To(3), nil, 3},
		{"a configured zero is honoured", ptr.To(0), nil, 0},
		{"an explicit request wins", ptr.To(3), ptr.To(int32(1)), 1},
		{"an explicit zero is a request, not absence", ptr.To(3), ptr.To(int32(0)), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &CheckpointSpec{DefaultRetryLimit: tc.configured}
			if got := c.ResolveRetryLimit(tc.requested); got != tc.want {
				t.Errorf("ResolveRetryLimit() = %d, want %d", got, tc.want)
			}
		})
	}
}

// The cluster default is bounded like the spec fields it stands in for. Without
// the upper bound it could hand a request more attempts than the request would
// have been allowed to ask for.
func TestCheckpointSpecBoundsTheDefaultRetryLimit(t *testing.T) {
	for _, tc := range []struct {
		limit int
		valid bool
	}{
		{-1, false},
		{0, true},
		{snapshotv1alpha1.MaxCheckpointRetryLimit, true},
		{snapshotv1alpha1.MaxCheckpointRetryLimit + 1, false},
	} {
		err := (&CheckpointSpec{DefaultRetryLimit: ptr.To(tc.limit)}).Validate()
		if tc.valid && err != nil {
			t.Errorf("defaultRetryLimit %d should validate, got %v", tc.limit, err)
		}
		if !tc.valid && err == nil {
			t.Errorf("defaultRetryLimit %d should not validate", tc.limit)
		}
	}
}

func TestCheckpointSpecParsesFreezeTimeout(t *testing.T) {
	for _, raw := range []string{"freezeTimeoutSeconds: 0", "freezeTimeoutSeconds: -1", "freezeTimeoutSeconds: 4294967296"} {
		var spec CheckpointSpec
		err := yaml.Unmarshal([]byte(raw), &spec)
		if err == nil {
			err = spec.Validate()
		}
		require.Error(t, err, raw)
	}
	for _, value := range []uint32{1, 300, math.MaxUint32} {
		var spec CheckpointSpec
		require.NoError(t, yaml.Unmarshal([]byte(fmt.Sprintf("freezeTimeoutSeconds: %d", value)), &spec))
		require.NoError(t, spec.Validate())
		require.Equal(t, value, *spec.FreezeTimeoutSeconds)
	}
}
