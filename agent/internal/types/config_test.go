// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
	"k8s.io/utils/ptr"
)

func validAgentConfig() *AgentConfig {
	return &AgentConfig{
		Storage: StorageSpec{
			Type:     "pvc",
			BasePath: "/checkpoints",
		},
		Restore: RestoreSpec{
			RestoreTimeoutSeconds: 60,
		},
		PageBroker: PageBrokerSpec{ControlSocketPath: "/pagebroker/control/pagebroker.sock"},
	}
}

// The key an admin flips in the ConfigMap has to be the key the agent reads,
// and an agent whose ConfigMap predates it has to keep the gate on.
func TestRestoreSpecParsesSkipCompatCheck(t *testing.T) {
	cases := map[string]bool{
		"restore:\n  skipCompatCheck: true\n":     true,
		"restore:\n  skipCompatCheck: false\n":    false,
		"restore:\n  restoreTimeoutSeconds: 60\n": false,
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

// A config written before checkpointTimeoutSeconds existed must still load, and must come up with
// the guard on rather than unbounded.
func TestCheckpointSpecDefaultsWhenAbsent(t *testing.T) {
	var spec CheckpointSpec
	require.NoError(t, yaml.Unmarshal([]byte("{}"), &spec))

	require.NoError(t, spec.Validate())
	assert.Equal(t, time.Duration(DefaultCheckpointTimeoutSeconds)*time.Second, spec.CheckpointTimeout())
}

func TestCheckpointSpecRejectsAnExplicitNonPositiveTimeout(t *testing.T) {
	for _, raw := range []string{"checkpointTimeoutSeconds: 0\n", "checkpointTimeoutSeconds: -1\n"} {
		var spec CheckpointSpec
		require.NoError(t, yaml.Unmarshal([]byte(raw), &spec))
		require.Error(t, spec.Validate(), raw)
	}
}

func TestCheckpointSpecHonoursAnExplicitTimeout(t *testing.T) {
	var spec CheckpointSpec
	require.NoError(t, yaml.Unmarshal([]byte("checkpointTimeoutSeconds: 45\n"), &spec))

	require.NoError(t, spec.Validate())
	assert.Equal(t, 45*time.Second, spec.CheckpointTimeout())
}

// time.Duration counts nanoseconds in an int64, so a large enough seconds value wraps negative and
// every call site reads that as "no timeout" — the bound silently disappears. Reject it instead.
func TestTimeoutsRejectValuesThatOverflowDuration(t *testing.T) {
	overflow := int(maxTimeoutSeconds) + 1
	require.Less(t, time.Duration(overflow)*time.Second, time.Duration(0),
		"the test value must actually overflow, or this proves nothing")

	checkpoint := CheckpointSpec{CheckpointTimeoutSeconds: &overflow}
	require.Error(t, checkpoint.Validate())

	restore := RestoreSpec{RestoreTimeoutSeconds: overflow}
	require.Error(t, restore.Validate())
}

func TestTimeoutsAcceptTheLargestRepresentableValue(t *testing.T) {
	limit := int(maxTimeoutSeconds)
	require.Positive(t, time.Duration(limit)*time.Second)

	checkpoint := CheckpointSpec{CheckpointTimeoutSeconds: &limit}
	require.NoError(t, checkpoint.Validate())

	restore := RestoreSpec{RestoreTimeoutSeconds: limit}
	require.NoError(t, restore.Validate())
}

// Absent and explicit zero are different answers: zero asks for no retry, absent
// expresses no preference and defers to the cluster default.
func TestResolveRetryLimit(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured *int
		requested  *int32
		want       int
	}{
		{"neither set falls back to the default", nil, nil, DefaultCheckpointRetryLimit},
		{"cluster default applies when the request is silent", ptr.To(3), nil, 3},
		{"an explicit request wins", ptr.To(3), ptr.To(int32(1)), 1},
		{"an explicit zero is a request, not absence", ptr.To(3), ptr.To(int32(0)), 0},
		{"a cluster zero is honoured", ptr.To(0), nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &CheckpointSpec{RetryLimit: tc.configured}
			if got := c.ResolveRetryLimit(tc.requested); got != tc.want {
				t.Errorf("ResolveRetryLimit() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestCheckpointSpecRejectsANegativeRetryLimit(t *testing.T) {
	c := &CheckpointSpec{RetryLimit: ptr.To(-1)}
	if err := c.Validate(); err == nil {
		t.Error("a negative retryLimit should not validate")
	}
	if err := (&CheckpointSpec{RetryLimit: ptr.To(0)}).Validate(); err != nil {
		t.Errorf("zero retryLimit should validate, got %v", err)
	}
}
