// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"flag"
	"fmt"

	"k8s.io/apimachinery/pkg/labels"
)

// podGroupConfig turns on the PodGroupSnapshot controller and tells it how to
// find the node agent Pods, whose readiness and group protocols admission
// checks on each member's node.
type podGroupConfig struct {
	enabled        bool
	agentNamespace string
	agentSelector  string
}

func bindPodGroupFlags(flags *flag.FlagSet) *podGroupConfig {
	cfg := &podGroupConfig{}
	flags.BoolVar(&cfg.enabled, "enable-pod-groups", false,
		"Run the PodGroupSnapshot controller (alpha); requires --agent-namespace and --agent-selector")
	flags.StringVar(&cfg.agentNamespace, "agent-namespace", "",
		"Namespace of the node agent Pods that pod-group admission checks")
	flags.StringVar(&cfg.agentSelector, "agent-selector", "",
		"Label selector of the node agent Pods that pod-group admission checks")
	return cfg
}

// selector returns the agent Pods' label selector. It refuses an empty one,
// which would take every Pod in the namespace for an agent.
func (c *podGroupConfig) selector() (labels.Selector, error) {
	if c.agentNamespace == "" {
		return nil, errors.New("--agent-namespace is required with --enable-pod-groups")
	}
	selector, err := labels.Parse(c.agentSelector)
	if err != nil {
		return nil, fmt.Errorf("parse --agent-selector: %w", err)
	}
	if selector.Empty() {
		return nil, errors.New("--agent-selector is required with --enable-pod-groups")
	}
	return selector, nil
}
