// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"flag"
	"fmt"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/ai-dynamo/snapshot/agent/internal/group"
)

// podGroupConfig turns on the agent's part in pod groups, and tells it how to
// reach the coordinator of a group: the agent Pod on the node of the group's
// rank-0 member. Without --enable-pod-groups none of it is required, and
// nothing of pod groups runs.
type podGroupConfig struct {
	enabled        bool
	agentNamespace string
	agentSelector  string
	serverName     string
	port           int
}

func bindPodGroupFlags(flags *flag.FlagSet) *podGroupConfig {
	cfg := &podGroupConfig{}
	flags.BoolVar(&cfg.enabled, "enable-pod-groups", false,
		"Checkpoint and restore the members of pod groups on this node (alpha); requires --agent-namespace, --agent-selector and --coordinator-server-name")
	flags.StringVar(&cfg.agentNamespace, "agent-namespace", "",
		"Namespace of the node agent Pods, whose coordinators hold the pod groups' sessions")
	flags.StringVar(&cfg.agentSelector, "agent-selector", "",
		"Label selector of the node agent Pods")
	flags.StringVar(&cfg.serverName, "coordinator-server-name", "",
		"Name that the coordinators' serving certificate carries")
	flags.IntVar(&cfg.port, "coordinator-port", group.DefaultPort,
		"Port the coordinators accept agents on, on each node's IP")
	return cfg
}

// locator returns the Locator the agent joins its members' attempts with. It
// refuses a configuration that cannot find or verify a coordinator.
func (c *podGroupConfig) locator(log logr.Logger) (group.Locator, error) {
	switch {
	case c.agentNamespace == "":
		return group.Locator{}, errors.New("--agent-namespace is required with --enable-pod-groups")
	case c.serverName == "":
		return group.Locator{}, errors.New("--coordinator-server-name is required with --enable-pod-groups")
	case c.port <= 0 || c.port > 65535:
		return group.Locator{}, fmt.Errorf("--coordinator-port %d is not a port", c.port)
	}
	selector, err := labels.Parse(c.agentSelector)
	if err != nil {
		return group.Locator{}, fmt.Errorf("parse --agent-selector: %w", err)
	}
	if selector.Empty() {
		// An empty selector would take every Pod in the namespace for an agent.
		return group.Locator{}, errors.New("--agent-selector is required with --enable-pod-groups")
	}
	return group.Locator{
		Namespace:  c.agentNamespace,
		Selector:   selector,
		Port:       c.port,
		ServerName: c.serverName,
		Log:        log,
	}, nil
}
