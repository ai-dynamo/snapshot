// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package coordinatortls is the operator's built-in issuer of the TLS
// certificate that SNEP-324's Snapshot coordinator serves, so that installing
// Snapshot does not require cert-manager.
//
// The coordinator and the agents depend only on mounted files, never on who
// made them: the coordinator serves tls.crt and tls.key, and the agents trust
// the CAs in ca.crt for the server name. The Issuer writes three resources in
// one namespace, and nothing else:
//
//   - The CA Secret, of type Opaque: ca.crt and ca.key and, after the CA is
//     replaced, previous.crt, the CA certificate it replaced, until that
//     expires. Only the Issuer reads it; it is not part of what the
//     coordinator or the agents consume.
//   - The serving Secret, of type kubernetes.io/tls: tls.crt and tls.key. The
//     certificate names the server name as its only DNS name.
//   - The trust bundle ConfigMap: ca.crt, the CA certificates the agents
//     trust, which are exactly the CA Secret's ca.crt and previous.crt. The
//     Issuer builds it from the CA Secret alone, never from its own content,
//     so anything else written into it is rewritten at the next check.
//
// The Issuer runs on the leader. It checks the resources when it starts and
// then every minute, and replaces each one that is missing or not valid, so a
// resource that is deleted or edited is repaired. The keys are ECDSA P-256.
// The CA is valid for 10 years and the serving certificate for 1 year, and
// each is replaced 90 days before it expires. A certificate that is not valid
// yet is replaced too.
//
// # Replacing the CA
//
// A new CA is issued when the CA is within 90 days of expiry, or at any age
// when the CA Secret is lost or damaged. The CA it replaces becomes
// previous.crt if it was readable and has not expired; a CA Secret that was
// lost or damaged leaves no previous CA, so that case loses the overlap
// described below. The Issuer then writes the CA Secret, the bundle and a
// serving certificate signed by the new CA, in that order, so the bundle that
// trusts the new CA is published before the certificate it signs. Once
// previous.crt expires, the Issuer drops it from the CA Secret, then from the
// bundle.
//
// Replacing the CA can disrupt connection attempts that reconnect before both
// sides have reloaded: every coordinator its serving certificate, and every
// agent its bundle. Both reload from their mounted files once the kubelet
// refreshes them, on each node's own schedule, so publishing the bundle first
// does not make agents trust the new CA first. Keeping the previous CA in the
// bundle removes half of that window: an agent with the new bundle still
// accepts a coordinator that serves the old certificate. A two-phase rotation,
// which would hold the new serving certificate back until every bundle has
// refreshed, would remove the rest.
//
// # RBAC
//
// The operator needs, in the Issuer's namespace, create on Secrets and
// ConfigMaps, which cannot be limited to names, and get and update on the
// two named Secrets and the named ConfigMap:
//
//	rules:
//	- apiGroups: [""]
//	  resources: ["secrets", "configmaps"]
//	  verbs: ["create"]
//	- apiGroups: [""]
//	  resources: ["secrets"]
//	  resourceNames: ["<CA Secret>", "<serving Secret>"]
//	  verbs: ["get", "update"]
//	- apiGroups: [""]
//	  resources: ["configmaps"]
//	  resourceNames: ["<trust bundle ConfigMap>"]
//	  verbs: ["get", "update"]
package coordinatortls
