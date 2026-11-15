// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agentauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ai-dynamo/snapshot/coordinator/server"
	"github.com/ai-dynamo/snapshot/coordinator/session"
	"github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
)

func TestNewValidatesConfig(t *testing.T) {
	h := newHarness(t)
	for name, modify := range map[string]func(*Config){
		"no Placement":                func(c *Config) { c.Placement = nil },
		"no TokenReview client":       func(c *Config) { c.TokenReviews = nil },
		"no REST config":              func(c *Config) { c.RESTConfig = nil },
		"no audience":                 func(c *Config) { c.Audience = "" },
		"no ServiceAccount namespace": func(c *Config) { c.ServiceAccount.Namespace = "" },
		"no ServiceAccount name":      func(c *Config) { c.ServiceAccount.Name = "" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := h.config()
			modify(&cfg)
			_, err := New(t.Context(), cfg)
			assert.ErrorIs(t, err, session.ErrInvalid)
		})
	}
	t.Run("no Clock", func(t *testing.T) {
		cfg := h.config()
		cfg.Clock = nil
		_, err := New(t.Context(), cfg)
		assert.NoError(t, err)
	})
}

func TestNewReadsTheIssuerAndItsKeys(t *testing.T) {
	h := newHarness(t)
	for name, tc := range map[string]struct {
		host func(*testing.T) string
		says string
	}{
		"a malformed host": {host: func(*testing.T) string { return "http://[::1" }},
		"no API server": {
			host: func(*testing.T) string { return "http://127.0.0.1:1" },
			says: "read the ServiceAccount token issuer",
		},
		"no discovery document": {host: notFound, says: "read the ServiceAccount token issuer"},
		"a discovery document with no issuer": {
			host: noIssuer,
			says: "the discovery document names no issuer",
		},
		"no key set": {
			host: keySetAnswering(unavailable),
			says: "read the ServiceAccount token issuer's keys",
		},
		"no key that can verify a signature": {
			host: keySetAnswering(func(w http.ResponseWriter, _ jose.JSONWebKeySet) {
				writeJSON(w, map[string]any{"keys": []any{}})
			}),
			says: "read the ServiceAccount token issuer's keys",
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := h.config()
			cfg.RESTConfig = &rest.Config{Host: tc.host(t)}
			_, err := New(t.Context(), cfg)
			require.Error(t, err)
			assert.ErrorContains(t, err, tc.says)
			assert.NotErrorIs(t, err, session.ErrInvalid)
		})
	}
}

// The API server serves issuer discovery only when its issuer is an https
// URL, and answers 404 otherwise.
func TestNewNamesTheHTTPSIssuerRequirement(t *testing.T) {
	h := newHarness(t)
	cfg := h.config()
	host := notFound(t)
	cfg.RESTConfig = &rest.Config{Host: host}
	_, err := New(t.Context(), cfg)
	require.EqualError(t, err, "read the ServiceAccount token issuer: GET "+host+discoveryPath+": 404 Not Found: "+
		"the API server serves it only when its --service-account-issuer is an https URL")
}

// noIssuer serves a discovery document that names no issuer.
func noIssuer(t *testing.T) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"jwks_uri": "https://issuer.invalid/openid/v1/jwks"})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// notFound serves no discovery document.
func notFound(t *testing.T) string {
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	return srv.URL
}

// keySetAnswering returns an issuer that answers every request for its key set
// with respond.
func keySetAnswering(respond func(http.ResponseWriter, jose.JSONWebKeySet)) func(*testing.T) string {
	return func(t *testing.T) string {
		is := newIssuer(t)
		is.respondWith(respond)
		return is.url()
	}
}

func TestAuthorizeAdmits(t *testing.T) {
	h := newHarness(t)
	token := h.token()
	require.NoError(t, h.authorize(member, token))
	actions := h.reviewer.Actions()
	require.Len(t, actions, 1)
	review := actions[0].(k8stesting.CreateAction).GetObject().(*authenticationv1.TokenReview)
	assert.Equal(t, authenticationv1.TokenReviewSpec{Token: token, Audiences: []string{audience}}, review.Spec)
}

// The issuer may sign with a key of any type: the verifier allows every
// asymmetric algorithm, whatever the algorithms the discovery document names
// and the keys New read.
func TestAuthorizeAllowsEveryAsymmetricAlgorithm(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	require.NoError(t, err)
	p521, err := ecdsa.GenerateKey(elliptic.P521(), rand.Reader)
	require.NoError(t, err)
	_, edKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	for _, k := range []jose.SigningKey{
		{Algorithm: jose.RS256, Key: rsaKey},
		{Algorithm: jose.RS384, Key: rsaKey},
		{Algorithm: jose.RS512, Key: rsaKey},
		{Algorithm: jose.PS256, Key: rsaKey},
		{Algorithm: jose.PS384, Key: rsaKey},
		{Algorithm: jose.PS512, Key: rsaKey},
		{Algorithm: jose.ES384, Key: p384},
		{Algorithm: jose.ES512, Key: p521},
		{Algorithm: jose.EdDSA, Key: edKey},
	} {
		t.Run(string(k.Algorithm), func(t *testing.T) {
			// The issuer names ES256 only, and New read only key-0, an ES256
			// key.
			h := newHarness(t)
			h.issuer.publish("key-1", k)
			h.clock.Step(minKeyFetchInterval)
			require.NoError(t, h.authorize(member, h.signedWith("key-1")))
		})
	}
}

func TestAuthorizeRefusesTokensThatDoNotVerify(t *testing.T) {
	for name, token := range map[string]func(*harness) string{
		"not a token": func(*harness) string { return "not-a-token" },
		"unsigned": func(h *harness) string {
			return unsigned(t, h.issuer.claims(h.clock.Now()))
		},
		"signed with a key the issuer does not publish": func(h *harness) string {
			return h.issuer.sign(t, "key-unknown", h.issuer.claims(h.clock.Now()))
		},
		"signed with HMAC": func(h *harness) string {
			secret := jose.SigningKey{Algorithm: jose.HS256, Key: []byte("a-secret-of-at-least-32-bytes-long")}
			return signWith(t, secret, "key-0", h.issuer.claims(h.clock.Now()), nil)
		},
		"another issuer":   func(h *harness) string { return h.token(set("iss", "https://issuer.invalid")) },
		"another audience": func(h *harness) string { return h.token(set("aud", []string{"another-audience"})) },
		"expired": func(h *harness) string {
			return h.token(set("exp", h.clock.Now().Add(-time.Second).Unix()))
		},
		"no expiry": func(h *harness) string { return h.token(func(c tokenClaims) { delete(c, "exp") }) },
		"not valid yet": func(h *harness) string {
			// The verifier allows five minutes of clock skew.
			return h.token(set("nbf", h.clock.Now().Add(6*time.Minute).Unix()))
		},
		"not bound to a Pod": func(h *harness) string {
			return h.token(func(c tokenClaims) { delete(c.kubernetes(), "pod") })
		},
		"bound to a Pod with no node": func(h *harness) string {
			return h.token(func(c tokenClaims) { delete(c.kubernetes(), "node") })
		},
		"bound to a Secret": func(h *harness) string {
			return h.token(func(c tokenClaims) {
				k := c.kubernetes()
				delete(k, "pod")
				delete(k, "node")
				k["secret"] = map[string]any{"name": "agent-token", "uid": "secret-uid"}
			})
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			err := h.authorize(member, token(h))
			require.ErrorIs(t, err, server.ErrUnauthenticated)
			assert.Zero(t, h.placement.asked.Load(), "asked where the members run")
			assert.Zero(t, h.reviewer.calls.Load(), "reviewed the token")
		})
	}
}

// set returns a modifier that sets the claim name to value.
func set(name string, value any) func(tokenClaims) {
	return func(c tokenClaims) { c[name] = value }
}

func TestAuthorizeRefusesUnexpectedHeaders(t *testing.T) {
	for name, opts := range map[string]*jose.SignerOptions{
		"jwk":  {EmbedJWK: true},
		"x5c":  (&jose.SignerOptions{}).WithHeader("x5c", []string{"bm90LWEtY2VydGlmaWNhdGU="}),
		"crit": (&jose.SignerOptions{}).WithHeader("crit", []string{"exp"}).WithHeader("exp", 0),
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			err := h.authorize(member, h.tokenWithHeader(opts))
			require.ErrorIs(t, err, server.ErrUnauthenticated)
			assert.ErrorContains(t, err, "the token's header holds a parameter other than alg, kid and typ")
			assert.Zero(t, h.placement.asked.Load(), "asked where the members run")
		})
	}
	t.Run("alg, kid and typ", func(t *testing.T) {
		h := newHarness(t)
		require.NoError(t, h.authorize(member, h.token()))
	})
}

// GHSA-mgcc-cww5-fp2q: go-jose up to v4.1.5 spent seconds validating an
// RSA key with a huge prime, which a token's jwk header can carry, before any
// other check. Both the header check and go-jose itself refuse it at once.
func TestAuthorizeRefusesAHugeKeyInTheHeaderQuickly(t *testing.T) {
	h := newHarness(t)
	token := hugeKeyToken(t)
	for name, refuse := range map[string]func() error{
		"Authorize": func() error { return h.authorize(member, token) },
		"the verifier, past the header check": func() error {
			_, err := h.auth.verifier.Verify(t.Context(), token)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			require.Error(t, refuse())
			assert.Less(t, time.Since(start), 100*time.Millisecond)
		})
	}
}

// hugeKeyToken returns a token whose header carries, as its jwk, an RSA
// private key with a 16,384-bit prime, built as the advisory's proof of
// concept builds it. Its algorithm is the issuer's, so that go-jose parses the
// key.
func hugeKeyToken(t *testing.T) string {
	t.Helper()
	p := new(big.Int).Lsh(big.NewInt(1), 16384-1)
	p.SetBit(p, 0, 1)
	n := new(big.Int).Lsh(big.NewInt(1), 2048)
	enc := base64.RawURLEncoding.EncodeToString
	return withHeader(t, map[string]any{
		"alg": "ES256",
		"jwk": map[string]string{
			"kty": "RSA",
			"n":   enc(n.Bytes()),
			"e":   enc(big.NewInt(65537).Bytes()),
			"d":   enc(big.NewInt(3).Bytes()),
			"p":   enc(p.Bytes()),
			"q":   enc(big.NewInt(3).Bytes()),
		},
	}, tokenClaims{})
}

// go-jose quotes a token's header in its errors. The coordinator logs the
// error Authorize returns on every connection, so that error holds none of
// it, and the Authorizer logs it at V(1) only, cut short.
func TestRefusalsHoldNoTextTheClientChose(t *testing.T) {
	h := newHarness(t)
	text := strings.Repeat("text the client chose ", 150)
	token := withHeader(t, map[string]any{"alg": text, "kid": "key-0"}, h.issuer.claims(h.clock.Now()))
	err := h.authorize(member, token)
	require.ErrorIs(t, err, server.ErrUnauthenticated)
	assert.NotContains(t, err.Error(), "text the client chose")

	h.mu.Lock()
	defer h.mu.Unlock()
	require.Len(t, h.logs, 1)
	assert.Contains(t, h.logs[0], `"level"=1 "msg"="Refused a token"`)
	assert.Contains(t, h.logs[0], "text the client chose")
	assert.Less(t, len(h.logs[0]), 2*maxCause)
}

func TestTruncate(t *testing.T) {
	assert.Equal(t, "short", truncate("short", 5))
	assert.Equal(t, "sho...", truncate("short", 3))
	assert.Equal(t, "h...", truncate("héllo", 2), "cut within a rune")
}

// unsigned returns a token with the claims c and no signature.
func unsigned(t *testing.T, c tokenClaims) string {
	return withHeader(t, map[string]any{"alg": "none", "typ": "JWT"}, c)
}

// withHeader returns a token with header and the claims c, and no signature.
func withHeader(t *testing.T, header map[string]any, c tokenClaims) string {
	t.Helper()
	h, err := json.Marshal(header)
	require.NoError(t, err)
	payload, err := json.Marshal(c)
	require.NoError(t, err)
	enc := base64.RawURLEncoding.EncodeToString
	return enc(h) + "." + enc(payload) + "."
}

func TestAuthorizeChecksIdentityAndPlacement(t *testing.T) {
	for name, tc := range map[string]struct {
		setup  func(*placement)
		modify func(tokenClaims)
		member string
		want   error
	}{
		"another ServiceAccount": {
			modify: func(c tokenClaims) { c.kubernetes()["serviceaccount"] = map[string]any{"name": "workload"} },
			want:   server.ErrPermissionDenied,
		},
		"the agents' ServiceAccount name in another namespace": {
			modify: func(c tokenClaims) { c.kubernetes()["namespace"] = "workloads" },
			want:   server.ErrPermissionDenied,
		},
		"an attempt not known yet": {
			setup: func(p *placement) { p.nodes = nil },
			want:  session.ErrNotFound,
		},
		"no answer from the Placement": {
			setup: func(p *placement) { p.err = errors.New("the cache is not synced") },
			want:  server.ErrUnavailable,
		},
		"not a member of the attempt": {
			member: "member-9",
			want:   server.ErrPermissionDenied,
		},
		"a member on another node": {
			setup: func(p *placement) { p.nodes[attempt][member] = "node-1" },
			want:  server.ErrPermissionDenied,
		},
		"a member whose node is not known yet": {
			setup: func(p *placement) { p.nodes[attempt][member] = "" },
			want:  server.ErrUnavailable,
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			if tc.setup != nil {
				tc.setup(h.placement)
			}
			var modify []func(tokenClaims)
			if tc.modify != nil {
				modify = append(modify, tc.modify)
			}
			if tc.member == "" {
				tc.member = member
			}
			err := h.authorize(tc.member, h.token(modify...))
			require.ErrorIs(t, err, tc.want)
			for _, other := range []error{server.ErrUnauthenticated, server.ErrPermissionDenied,
				server.ErrUnavailable, session.ErrNotFound} {
				if other != tc.want {
					assert.NotErrorIs(t, err, other)
				}
			}
			assert.Zero(t, h.reviewer.calls.Load(), "reviewed the token")
		})
	}
}

// A member reconnecting to an attempt the coordinator does not hold, such as
// after its group was removed, must be admitted, so that it learns that its
// session is gone, with session.ErrCoordinatorLost. Its token is still checked.
func TestAuthorizeAMemberOfAnAttemptNotHeld(t *testing.T) {
	t.Run("admits a valid token for a removed group", func(t *testing.T) {
		h := newHarness(t)
		h.placement.nodes = nil
		require.NoError(t, h.authorizeNotHeld(member, h.token()))
		assert.Zero(t, h.placement.asked.Load(), "asked where the members run")
		assert.EqualValues(t, 1, h.reviewer.calls.Load())
	})
	t.Run("refuses a token that does not verify", func(t *testing.T) {
		h := newHarness(t)
		h.placement.nodes = nil
		err := h.authorizeNotHeld(member, h.token(set("aud", []string{"another-audience"})))
		require.ErrorIs(t, err, server.ErrUnauthenticated)
		assert.Zero(t, h.reviewer.calls.Load(), "reviewed the token")
	})
	t.Run("refuses a token of another ServiceAccount", func(t *testing.T) {
		h := newHarness(t)
		h.placement.nodes = nil
		workload := func(c tokenClaims) { c.kubernetes()["serviceaccount"] = map[string]any{"name": "workload"} }
		require.ErrorIs(t, h.authorizeNotHeld(member, h.token(workload)), server.ErrPermissionDenied)
		assert.Zero(t, h.reviewer.calls.Load(), "reviewed the token")
	})
	t.Run("refuses a token the API server does not accept", func(t *testing.T) {
		h := newHarness(t)
		h.placement.nodes = nil
		h.reviewer.answerWith(authenticationv1.TokenReviewStatus{Error: "pod not found"}, nil)
		require.ErrorIs(t, h.authorizeNotHeld(member, h.token()), server.ErrUnauthenticated)
		assert.EqualValues(t, 1, h.reviewer.calls.Load())
	})
}

func TestAuthorizeReviewsTheToken(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Group: "authentication.k8s.io", Resource: "tokenreviews"},
		"", errors.New("RBAC: not allowed"))
	for name, tc := range map[string]struct {
		status authenticationv1.TokenReviewStatus
		err    error
		want   error
		says   string
	}{
		"not authenticated": {
			status: authenticationv1.TokenReviewStatus{Error: "service account token has been invalidated"},
			want:   server.ErrUnauthenticated,
			says: "the API server does not accept the token of Pod snapshot/snapshot-agent-abcde: " +
				"service account token has been invalidated",
		},
		"authenticated as someone else": {
			status: authenticationv1.TokenReviewStatus{
				Authenticated: true,
				User:          authenticationv1.UserInfo{Username: "system:serviceaccount:snapshot:workload"},
			},
			want: server.ErrUnauthenticated,
			says: `the token of Pod snapshot/snapshot-agent-abcde authenticates "system:serviceaccount:snapshot:workload"`,
		},
		"authenticated for another audience": {
			status: authenticationv1.TokenReviewStatus{
				Authenticated: true,
				User:          accepted.User,
				Audiences:     []string{"another-audience"},
			},
			want: server.ErrUnauthenticated,
			says: `the API server does not accept the token of Pod snapshot/snapshot-agent-abcde for the audience ` +
				`"snapshot-coordinator"`,
		},
		"authenticated for the API server only": {
			status: authenticationv1.TokenReviewStatus{Authenticated: true, User: accepted.User},
			want:   server.ErrUnauthenticated,
			says: `the API server does not accept the token of Pod snapshot/snapshot-agent-abcde for the audience ` +
				`"snapshot-coordinator"`,
		},
		"a failed TokenReview": {err: forbidden, want: server.ErrUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.reviewer.answerWith(tc.status, tc.err)
			err := h.authorize(member, h.token())
			require.ErrorIs(t, err, tc.want)
			assert.ErrorContains(t, err, tc.says)
			assert.EqualValues(t, 1, h.reviewer.calls.Load())
		})
	}
}

func TestAuthorizeReusesReviews(t *testing.T) {
	t.Run("an admission", func(t *testing.T) {
		h := newHarness(t)
		token := h.token()
		require.NoError(t, h.authorize(member, token))
		require.NoError(t, h.authorize(member, token))
		assert.EqualValues(t, 1, h.reviewer.calls.Load())

		// A reused admission still checks the member's node.
		h.placement.nodes[attempt]["member-1"] = "node-1"
		require.ErrorIs(t, h.authorize("member-1", token), server.ErrPermissionDenied)
		assert.EqualValues(t, 1, h.reviewer.calls.Load())
	})
	t.Run("a refusal, within maxRefusalAge", func(t *testing.T) {
		h := newHarness(t)
		h.reviewer.answerWith(authenticationv1.TokenReviewStatus{Error: "pod not found"}, nil)
		token := h.token()
		require.ErrorIs(t, h.authorize(member, token), server.ErrUnauthenticated)
		h.reviewer.answerWith(accepted, nil)
		h.clock.Step(maxRefusalAge)
		require.ErrorIs(t, h.authorize(member, token), server.ErrUnauthenticated)
		assert.EqualValues(t, 1, h.reviewer.calls.Load())

		// Another token is reviewed on its own.
		require.NoError(t, h.authorize(member, h.token()))
		assert.EqualValues(t, 2, h.reviewer.calls.Load())
	})
	t.Run("never a failed TokenReview", func(t *testing.T) {
		h := newHarness(t)
		h.reviewer.answerWith(authenticationv1.TokenReviewStatus{}, apierrors.NewServiceUnavailable("etcd is down"))
		token := h.token()
		require.ErrorIs(t, h.authorize(member, token), server.ErrUnavailable)
		h.reviewer.answerWith(accepted, nil)
		require.NoError(t, h.authorize(member, token))
		assert.EqualValues(t, 2, h.reviewer.calls.Load())
	})
}

func TestReviewsExpire(t *testing.T) {
	t.Run("an admission, after maxReviewAge", func(t *testing.T) {
		h := newHarness(t)
		token := h.token()
		require.NoError(t, h.authorize(member, token))
		h.clock.Step(maxReviewAge)
		require.NoError(t, h.authorize(member, token))
		assert.EqualValues(t, 1, h.reviewer.calls.Load())
		h.clock.Step(time.Nanosecond)
		require.NoError(t, h.authorize(member, token))
		assert.EqualValues(t, 2, h.reviewer.calls.Load())
	})
	t.Run("a refusal, after maxRefusalAge", func(t *testing.T) {
		// A refusal given in error is reviewed again while the member can
		// still connect.
		require.Less(t, maxRefusalAge, server.DefaultGrace)
		h := newHarness(t)
		h.reviewer.answerWith(authenticationv1.TokenReviewStatus{Error: "pod not found"}, nil)
		token := h.token()
		require.ErrorIs(t, h.authorize(member, token), server.ErrUnauthenticated)
		h.reviewer.answerWith(accepted, nil)
		h.clock.Step(maxRefusalAge + time.Nanosecond)
		require.NoError(t, h.authorize(member, token))
		assert.EqualValues(t, 2, h.reviewer.calls.Load())
	})
	t.Run("an admission, when the token expires first", func(t *testing.T) {
		// Authorize refuses an expired token before it reviews it, so this
		// asks review itself.
		h := newHarness(t)
		id := identity{expiry: h.clock.Now().Add(maxReviewAge / 2)}
		require.NoError(t, h.auth.review(t.Context(), "token", id))
		h.clock.SetTime(id.expiry)
		require.NoError(t, h.auth.review(t.Context(), "token", id))
		assert.EqualValues(t, 1, h.reviewer.calls.Load())
		h.clock.Step(time.Nanosecond)
		require.NoError(t, h.auth.review(t.Context(), "token", id))
		assert.EqualValues(t, 2, h.reviewer.calls.Load())
	})
}

func TestReviewsAreBounded(t *testing.T) {
	h := newHarness(t)
	tokens := make([]string, maxReviews+1)
	for i := range tokens {
		tokens[i] = h.token()
		require.NoError(t, h.authorize(member, tokens[i]))
	}
	require.EqualValues(t, maxReviews+1, h.reviewer.calls.Load())

	// The most recent review is still held, and the oldest is gone.
	require.NoError(t, h.authorize(member, tokens[maxReviews]))
	assert.EqualValues(t, maxReviews+1, h.reviewer.calls.Load())
	require.NoError(t, h.authorize(member, tokens[0]))
	assert.EqualValues(t, maxReviews+2, h.reviewer.calls.Load())
}

func TestConcurrentRequestsShareOneReview(t *testing.T) {
	h := newHarness(t)
	h.reviewer.release = make(chan struct{})
	token := h.token()
	const n = 8
	errs := make(chan error, n)
	for range n {
		go func() { errs <- h.authorize(member, token) }()
	}
	// Hold the first review until every request has passed the Placement,
	// on its way to the review.
	require.Eventually(t, func() bool {
		return h.placement.asked.Load() == n && h.reviewer.calls.Load() == 1
	}, time.Second, time.Millisecond)
	close(h.reviewer.release)
	for range n {
		require.NoError(t, <-errs)
	}
	assert.EqualValues(t, 1, h.reviewer.calls.Load())
}

func TestAReviewOutlivesTheCallThatStartedIt(t *testing.T) {
	h := newHarness(t)
	h.reviewer.release = make(chan struct{})
	token := h.token()

	// The first call gives up while its review is held.
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	err := h.authorizeWithin(ctx, member, token, true)
	require.ErrorIs(t, err, server.ErrUnavailable)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	// A second call is admitted once the review it shares goes through.
	second := make(chan error, 1)
	go func() { second <- h.authorize(member, token) }()
	require.Eventually(t, func() bool { return h.placement.asked.Load() == 2 }, time.Second, time.Millisecond)
	close(h.reviewer.release)
	require.NoError(t, <-second)

	// A third reuses its answer.
	require.NoError(t, h.authorize(member, token))
	assert.EqualValues(t, 1, h.reviewer.calls.Load())
}

// The harness checks, in every test, that no error Authorize returns, which
// the coordinator logs, and no log line holds any part of a token. This test
// returns every kind of refusal and failure, and checks that the Authorizer
// logs only the failed key fetch and the failed TokenReview above V(1).
func TestNoErrorOrLogHoldsTheToken(t *testing.T) {
	h := newHarness(t)
	now := h.clock.Now()
	for _, token := range []string{
		"not-a-token",
		unsigned(t, h.issuer.claims(now)),
		h.issuer.sign(t, "key-unknown", h.issuer.claims(now)),
		h.token(set("iss", "https://issuer.invalid")),
		h.token(set("aud", []string{"another-audience"})),
		h.token(set("exp", now.Add(-time.Second).Unix())),
		h.token(func(c tokenClaims) { delete(c.kubernetes(), "node") }),
	} {
		require.ErrorIs(t, h.authorize(member, token), server.ErrUnauthenticated)
	}

	workload := func(c tokenClaims) { c.kubernetes()["serviceaccount"] = map[string]any{"name": "workload"} }
	require.ErrorIs(t, h.authorize(member, h.token(workload)), server.ErrPermissionDenied)
	require.ErrorIs(t, h.authorize("member-9", h.token()), server.ErrPermissionDenied)
	h.placement.nodes[attempt]["member-1"] = "node-1"
	require.ErrorIs(t, h.authorize("member-1", h.token()), server.ErrPermissionDenied)
	h.placement.nodes[attempt]["member-2"] = ""
	require.ErrorIs(t, h.authorize("member-2", h.token()), server.ErrUnavailable)
	h.placement.err = errors.New("the cache is not synced")
	require.ErrorIs(t, h.authorize(member, h.token()), server.ErrUnavailable)
	h.placement.err = nil
	nodes := h.placement.nodes
	h.placement.nodes = nil
	require.ErrorIs(t, h.authorize(member, h.token()), session.ErrNotFound)
	h.placement.nodes = nodes

	h.reviewer.answerWith(authenticationv1.TokenReviewStatus{Error: "pod not found"}, nil)
	require.ErrorIs(t, h.authorize(member, h.token()), server.ErrUnauthenticated)
	h.reviewer.answerWith(authenticationv1.TokenReviewStatus{
		Authenticated: true,
		User:          authenticationv1.UserInfo{Username: "system:serviceaccount:snapshot:workload"},
	}, nil)
	require.ErrorIs(t, h.authorize(member, h.token()), server.ErrUnauthenticated)
	h.reviewer.answerWith(authenticationv1.TokenReviewStatus{}, apierrors.NewServiceUnavailable("etcd is down"))
	require.ErrorIs(t, h.authorize(member, h.token()), server.ErrUnavailable)

	h.reviewer.answerWith(accepted, nil)
	h.reviewer.release = make(chan struct{})
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, h.authorizeWithin(ctx, member, h.token(), true), server.ErrUnavailable)
	close(h.reviewer.release)

	// The keys: a caller gives up waiting for a fetch, another is refused
	// within the cooldown, after the fetch if it is still in flight, and
	// another after a failed fetch.
	h.clock.Step(minKeyFetchInterval)
	release := h.issuer.holdKeys(t)
	ctx, cancel = context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, h.authorizeWithin(ctx, member, h.signedWith("key-unknown"), true), server.ErrUnavailable)
	release()
	require.ErrorIs(t, h.authorize(member, h.signedWith("key-unknown")), server.ErrUnauthenticated)
	h.issuer.respondWith(unavailable)
	h.clock.Step(minKeyFetchInterval)
	require.ErrorIs(t, h.authorize(member, h.signedWith("key-unknown")), server.ErrUnauthenticated)

	logs := h.defaultLogs()
	require.Len(t, logs, 2)
	assert.Contains(t, logs[0], `"msg"="A TokenReview failed"`)
	assert.Contains(t, logs[1], `"msg"="A fetch of the issuer's keys failed"`)
}
