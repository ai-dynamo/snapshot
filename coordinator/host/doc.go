// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package host runs SNEP-324's Snapshot coordinator in an agent's Pod. Run
// wires package groups, package agentauth and package server together in a
// controller-runtime manager, in the order package groups gives, and serves the
// health endpoint the Pod's probes read. Every agent runs it, in an
// unprivileged container of its own, and it serves the sessions of the attempts
// whose rank-0 member runs on the agent's node.
//
// The host needs:
//
//   - RBAC: its ServiceAccount, the agents', needs get, list and watch on
//     PodGroupSnapshots and PodGroupRestores, cluster-wide, and create on
//     TokenReviews, as the ClusterRole below grants. It also reads the API
//     server's ServiceAccount issuer, which Kubernetes's default
//     system:service-account-issuer-discovery binding lets every
//     ServiceAccount do.
//   - Files: tls.crt and tls.key in Config.TLSDir, the client listener's
//     serving certificate and its key, as the volume of a kubernetes.io/tls
//     Secret holds them. A certificate replaced there is served to new
//     connections without a restart.
//   - Ports: Config.Port for the agents and Config.HealthPort for the health
//     endpoint, both on Config.Host, the node's IP, and on no other address.
//     The agent's Pod uses the host's network, so both are ports on the node,
//     which a NetworkPolicy may not cover: TLS and the token check guard the
//     client port. The health endpoint serves /healthz, which is ok while the
//     process runs, and /readyz, which is ok once the coordinator has ensured
//     the sessions its node serves and admits clients. Run opens no other
//     port, and serves no metrics.
//   - A writable temporary directory, $TMPDIR or else /tmp: JetStream checks
//     that it can write there, although every bucket lives in memory.
//
// The ClusterRole:
//
//	rules:
//	- apiGroups: ["nvidia.com"]
//	  resources: ["podgroupsnapshots", "podgrouprestores"]
//	  verbs: ["get", "list", "watch"]
//	- apiGroups: ["authentication.k8s.io"]
//	  resources: ["tokenreviews"]
//	  verbs: ["create"]
package host
