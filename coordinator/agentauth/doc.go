// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package agentauth authorizes the Snapshot agents that connect to a
// coordinator hosted on Kubernetes, by their Pod-bound ServiceAccount tokens.
// Its Authorizer is a server.Authorizer. A token must name its Pod's node,
// which Kubernetes adds by default since 1.30, and always since 1.32.
//
// The host gives it a Placement, which reports the node each member of an
// attempt runs on, and its ServiceAccount needs RBAC to create TokenReviews
// and to get the API server's ServiceAccount issuer discovery endpoints,
// /.well-known/openid-configuration and /openid/v1/jwks. Kubernetes grants
// the second to every ServiceAccount, through its default
// system:service-account-issuer-discovery binding.
//
// The Authorizer says why it refuses a member in the error it returns, which
// the coordinator logs and which never holds the token. It logs only failed
// TokenReviews itself.
package agentauth
