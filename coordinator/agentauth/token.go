// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agentauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/ai-dynamo/snapshot/coordinator/server"
	"github.com/coreos/go-oidc/v3/oidc"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
)

// The API server's ServiceAccount issuer discovery endpoints.
const (
	discoveryPath = "/.well-known/openid-configuration"
	keysPath      = "/openid/v1/jwks"
)

// maxDiscovery bounds the discovery document read, which is a few hundred
// bytes.
const maxDiscovery = 1 << 20

// identity is what a verified token says about the agent that presents it.
type identity struct {
	serviceAccount, pod types.NamespacedName
	node                string
	expiry              time.Time
}

// claims are the claims Kubernetes adds to a ServiceAccount token. A token
// bound to a Pod names the Pod and the Pod's node.
type claims struct {
	Kubernetes struct {
		Namespace      string `json:"namespace"`
		ServiceAccount ref    `json:"serviceaccount"`
		Pod            ref    `json:"pod"`
		Node           ref    `json:"node"`
	} `json:"kubernetes.io"`
}

// ref names an object in claims.
type ref struct {
	Name string `json:"name"`
}

// discovery is what an Authorizer reads of the issuer's discovery document.
type discovery struct {
	Issuer     string   `json:"issuer"`
	Algorithms []string `json:"id_token_signing_alg_values_supported"`
}

// newVerifier returns a verifier of the API server's ServiceAccount tokens for
// audience. It reads the issuer from the API server's discovery document,
// within ctx, and fetches the issuer's keys from the API server, again for a
// key it does not hold yet. Both come from the API server itself, through the
// client cfg configures, rather than from the document's jwks_uri, which may
// name a public endpoint the agents cannot reach.
func newVerifier(ctx context.Context, cfg *rest.Config, audience string,
	now func() time.Time) (*oidc.IDTokenVerifier, error) {
	cfg = rest.CopyConfig(cfg)
	// A fetch of the keys runs apart from the Authorize calls that wait for
	// it, so only the client's timeout bounds it.
	cfg.Timeout = server.AuthorizeTimeout
	client, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, fmt.Errorf("build a client of the API server: %w", err)
	}
	host, _, err := rest.DefaultServerUrlFor(cfg)
	if err != nil {
		return nil, fmt.Errorf("find the API server: %w", err)
	}
	d, err := readDiscovery(ctx, client, host.JoinPath(discoveryPath).String())
	if err != nil {
		return nil, fmt.Errorf("read the ServiceAccount token issuer: %w", err)
	}
	keys := oidc.NewRemoteKeySet(oidc.ClientContext(ctx, client), host.JoinPath(keysPath).String())
	return oidc.NewVerifier(d.Issuer, keys, &oidc.Config{
		ClientID:             audience,
		SupportedSigningAlgs: d.Algorithms,
		Now:                  now,
	}), nil
}

// readDiscovery gets the discovery document at url.
func readDiscovery(ctx context.Context, client *http.Client, url string) (discovery, error) {
	var d discovery
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return d, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return d, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return d, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxDiscovery)).Decode(&d); err != nil {
		return d, fmt.Errorf("GET %s: %w", url, err)
	}
	if d.Issuer == "" {
		return d, errors.New("the discovery document names no issuer")
	}
	return d, nil
}

// verify checks the token's signature, issuer, audience, and lifetime, and
// that it is bound to a Pod and the Pod's node, and returns what it says.
func (a *Authorizer) verify(ctx context.Context, token string) (identity, error) {
	t, err := a.verifier.Verify(ctx, token)
	switch {
	case err != nil && ctx.Err() != nil:
		// The keys the token needs could not be fetched in time.
		return identity{}, fmt.Errorf("%w: %v", server.ErrUnavailable, err)
	case err != nil:
		return identity{}, fmt.Errorf("%w: %v", server.ErrUnauthenticated, err)
	}
	var c claims
	if err := t.Claims(&c); err != nil {
		return identity{}, fmt.Errorf("%w: %v", server.ErrUnauthenticated, err)
	}
	k := c.Kubernetes
	if k.Pod.Name == "" || k.Node.Name == "" {
		return identity{}, fmt.Errorf("%w: the token is not bound to a Pod and its node", server.ErrUnauthenticated)
	}
	return identity{
		serviceAccount: types.NamespacedName{Namespace: k.Namespace, Name: k.ServiceAccount.Name},
		pod:            types.NamespacedName{Namespace: k.Namespace, Name: k.Pod.Name},
		node:           k.Node.Name,
		expiry:         t.Expiry,
	}, nil
}

// serviceAccountUser returns the user name a token of sa authenticates as.
func serviceAccountUser(sa types.NamespacedName) string {
	return "system:serviceaccount:" + sa.Namespace + ":" + sa.Name
}
