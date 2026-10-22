// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	snapshottypes "github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/go-logr/logr"
)

// The check is a guard, not a gate: it may only refuse what provably cannot
// fit, so every case it cannot measure has to let the capture through.
func TestCheckStagingCapacitySkipsWhatItCannotMeasure(t *testing.T) {
	for _, tc := range []struct {
		name        string
		stagingPath string
	}{
		{"no staging path configured", ""},
		{"staging path does not exist", "/nonexistent/staging"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &NodeController{
				log:    logr.Discard(),
				config: &snapshottypes.AgentConfig{PageBroker: snapshottypes.PageBrokerSpec{StagingPath: tc.stagingPath}},
			}

			if err := w.checkStagingCapacity(1); err != nil {
				t.Errorf("checkStagingCapacity() = %v, want nil", err)
			}
		})
	}
}

// A real directory with a readable filesystem must not refuse an ordinary
// capture, or the guard would block every capture on the node.
func TestCheckStagingCapacityAllowsAMeasurableNode(t *testing.T) {
	w := &NodeController{
		log:    logr.Discard(),
		config: &snapshottypes.AgentConfig{PageBroker: snapshottypes.PageBrokerSpec{StagingPath: t.TempDir()}},
	}

	// PID 1 is present in any container, so the estimate is readable here.
	if err := w.checkStagingCapacity(1); err != nil {
		t.Errorf("checkStagingCapacity() = %v, want nil", err)
	}
}
