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

// maxDocument bounds a document read from the API server: the discovery
// document is a few hundred bytes, and the key set a few kilobytes.
const maxDocument = 1 << 20

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
// cfg's audience. Within ctx, it reads the issuer from the API server's
// discovery document, and then the issuer's keys. Both come from the API
// server itself, through the client cfg.RESTConfig configures, rather than
// from the document's jwks_uri, which may name a public endpoint the agents
// cannot reach.
func newVerifier(ctx context.Context, cfg Config) (*oidc.IDTokenVerifier, error) {
	client, err := rest.HTTPClientFor(cfg.RESTConfig)
	if err != nil {
		return nil, fmt.Errorf("build a client of the API server: %w", err)
	}
	host, _, err := rest.DefaultServerUrlFor(cfg.RESTConfig)
	if err != nil {
		return nil, fmt.Errorf("find the API server: %w", err)
	}
	var d discovery
	if err := getJSON(ctx, client, host.JoinPath(discoveryPath).String(), &d); err != nil {
		return nil, fmt.Errorf("read the ServiceAccount token issuer: %w", err)
	}
	if d.Issuer == "" {
		return nil, errors.New("read the ServiceAccount token issuer: the discovery document names no issuer")
	}
	keys, err := newKeySet(ctx, client, host.JoinPath(keysPath).String(), cfg)
	if err != nil {
		return nil, fmt.Errorf("read the ServiceAccount token issuer's keys: %w", err)
	}
	return oidc.NewVerifier(d.Issuer, keys, &oidc.Config{
		ClientID:             cfg.Audience,
		SupportedSigningAlgs: d.Algorithms,
		Now:                  cfg.Clock.Now,
	}), nil
}

// getJSON gets the JSON document at url through client, within ctx, and
// decodes it into v.
func getJSON(ctx context.Context, client *http.Client, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxDocument)).Decode(v); err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}
	return nil
}

// verify checks the token's signature, issuer, audience, and lifetime, and
// that it is bound to a Pod and the Pod's node, and returns what it says.
func (a *Authorizer) verify(ctx context.Context, token string) (identity, error) {
	t, err := a.verifier.Verify(ctx, token)
	switch {
	case err != nil && ctx.Err() != nil:
		// The caller gave up waiting for a fetch of the issuer's keys.
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
