// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package groups keeps the coordinator's sessions in step with the group
// resources, PodGroupSnapshots and PodGroupRestores: it creates, cancels, and
// removes the sessions of the attempts its node serves, those whose rank-0
// member runs on it, as the group's status records. It is the only package
// of the coordinator that knows the kinds' shapes, and Attempts.MemberNodes
// answers the Authorizer's lookup from them.
//
// The server needs an Authorizer, which reads the attempts, so a host builds
// its parts in this order, all before it starts its manager:
//
//  1. NewAttempts, which registers the cache indexes the attempts are read by;
//  2. agentauth.New, with the attempts as its Placement;
//  3. server.New, with that Authorizer;
//  4. Setup, with the attempts and a Config that names the server and the
//     host's node.
//
// The host configures logging on the manager, which the package logs through.
// Sessions live only in the coordinator's memory, so after a restart the
// startup ensures the session of every unfinished attempt the node serves
// before it marks the server ready.
//
// The host's ServiceAccount needs get, list, and watch on both kinds,
// cluster-wide, as a ClusterRole grants them:
//
//	rules:
//	- apiGroups: ["nvidia.com"]
//	  resources: ["podgroupsnapshots", "podgrouprestores"]
//	  verbs: ["get", "list", "watch"]
package groups
