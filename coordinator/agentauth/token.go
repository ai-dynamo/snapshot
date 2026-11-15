// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agentauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
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

// maxCause bounds the cause of a refusal that an Authorizer logs. A client
// chooses part of it, such as its token's header.
const maxCause = 256

// algorithms are the JWS algorithms a token may be signed with: every
// asymmetric one go-oidc supports, and never HMAC. Only the issuer's keys
// verify a signature, so this admits no token they do not, and the issuer may
// sign with a key of another type without a restart.
var algorithms = []string{
	oidc.RS256, oidc.RS384, oidc.RS512,
	oidc.PS256, oidc.PS384, oidc.PS512,
	oidc.ES256, oidc.ES384, oidc.ES512,
	oidc.EdDSA,
}

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
	Issuer string `json:"issuer"`
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
	err = getJSON(ctx, client, host.JoinPath(discoveryPath).String(), &d)
	if e, ok := errors.AsType[*statusError](err); ok && e.code == http.StatusNotFound {
		return nil, fmt.Errorf("read the ServiceAccount token issuer: %w: the API server serves it only when its "+
			"--service-account-issuer is an https URL", err)
	}
	if err != nil {
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
		SupportedSigningAlgs: algorithms,
		Now:                  cfg.Clock.Now,
	}), nil
}

// statusError reports a GET that the server answered with a status other than
// 200 OK.
type statusError struct {
	url, status string
	code        int
}

func (e *statusError) Error() string { return "GET " + e.url + ": " + e.status }

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
		return &statusError{url: url, status: resp.Status, code: resp.StatusCode}
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxDocument)).Decode(v); err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}
	return nil
}

// allowedHeader holds the JWS header parameters a token may carry. The API
// server sets only alg and kid. Any other, such as jwk, x5c or crit, only
// gives the JOSE parser more work, on a path any client reaches without
// credentials.
var allowedHeader = map[string]bool{"alg": true, "kid": true, "typ": true}

// checkHeader refuses a token whose protected header holds a parameter
// allowedHeader does not. It reads the header with the standard library, so
// that such a token never reaches the JOSE parser.
func checkHeader(token string) error {
	encoded, _, _ := strings.Cut(token, ".")
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return errors.New("the token's header is not base64url")
	}
	var header map[string]json.RawMessage
	if err := json.Unmarshal(raw, &header); err != nil {
		return errors.New("the token's header is not a JSON object")
	}
	for name := range header {
		if !allowedHeader[name] {
			return errors.New("the token's header holds a parameter other than alg, kid and typ")
		}
	}
	return nil
}

// verify checks the token's header, signature, issuer, audience, and
// lifetime, and that it is bound to a Pod and the Pod's node, and returns what
// it says. The error it returns says why in fixed text: go-jose quotes the
// token's header in its errors, and the coordinator logs the error on every
// connection.
func (a *Authorizer) verify(ctx context.Context, token string) (identity, error) {
	if err := checkHeader(token); err != nil {
		return identity{}, fmt.Errorf("%w: %v", server.ErrUnauthenticated, err)
	}
	t, err := a.verifier.Verify(ctx, token)
	switch {
	case err != nil && ctx.Err() != nil:
		// The caller gave up waiting for a fetch of the issuer's keys.
		return identity{}, fmt.Errorf("%w: waiting for a fetch of the issuer's keys: %w", server.ErrUnavailable,
			context.Cause(ctx))
	case err != nil:
		return identity{}, a.refuse("the token does not verify", err)
	}
	var c claims
	if err := t.Claims(&c); err != nil {
		return identity{}, a.refuse("the token's claims are malformed", err)
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

// refuse returns ErrUnauthenticated, with reason, which is fixed text. It logs
// cause at V(1) only, cut to maxCause bytes.
func (a *Authorizer) refuse(reason string, cause error) error {
	a.cfg.Log.V(1).Info("Refused a token", "reason", reason, "cause", truncate(cause.Error(), maxCause))
	return fmt.Errorf("%w: %s", server.ErrUnauthenticated, reason)
}

// truncate returns s, cut to at most n bytes and marked as cut if it is.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "..."
}

// serviceAccountUser returns the user name a token of sa authenticates as.
func serviceAccountUser(sa types.NamespacedName) string {
	return "system:serviceaccount:" + sa.Namespace + ":" + sa.Name
}
