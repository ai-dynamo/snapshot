// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agentauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ai-dynamo/snapshot/coordinator/server"
	"github.com/ai-dynamo/snapshot/coordinator/session"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
	clocktesting "k8s.io/utils/clock/testing"
)

// The audience, ServiceAccount, attempt, and members of the tests.
const (
	audience           = "snapshot-coordinator"
	attempt  types.UID = "attempt-uid"
	member             = "member-0"
	node               = "node-0"
)

var agents = types.NamespacedName{Namespace: "snapshot", Name: "snapshot-agent"}

// placement is a fake Placement: the attempt's members' nodes, by attempt.
type placement struct {
	asked atomic.Int32

	mu    sync.Mutex
	nodes map[types.UID]map[string]string
	err   error
}

// MemberNodes implements Placement.
func (p *placement) MemberNodes(_ context.Context, uid types.UID) (map[string]string, error) {
	p.asked.Add(1)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return nil, p.err
	}
	nodes, ok := p.nodes[uid]
	if !ok {
		return nil, fmt.Errorf("attempt %s: %w", uid, session.ErrNotFound)
	}
	return nodes, nil
}

// issuer serves the ServiceAccount issuer discovery endpoints an API server
// does, with keys of its own, and signs tokens with them.
type issuer struct {
	srv *httptest.Server

	mu   sync.Mutex
	keys map[string]*ecdsa.PrivateKey // by key ID; signs with "key-0"
	// hold, when set, holds each request for the key set until it is closed,
	// and respond, when set, answers it in place of the key set.
	hold    chan struct{}
	respond func(http.ResponseWriter, jose.JSONWebKeySet)

	// keyFetches counts the requests for the key set.
	keyFetches atomic.Int32
}

// newIssuer starts an issuer with one key, "key-0".
func newIssuer(t *testing.T) *issuer {
	t.Helper()
	is := &issuer{keys: map[string]*ecdsa.PrivateKey{}}
	is.addKey(t, "key-0")
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+discoveryPath, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                                is.url(),
			"jwks_uri":                              "https://issuer.invalid/openid/v1/jwks",
			"response_types_supported":              []string{"id_token"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"ES256"},
		})
	})
	mux.HandleFunc("GET "+keysPath, func(w http.ResponseWriter, r *http.Request) {
		is.keyFetches.Add(1)
		is.mu.Lock()
		hold, respond := is.hold, is.respond
		var set jose.JSONWebKeySet
		for id, k := range is.keys {
			set.Keys = append(set.Keys, jose.JSONWebKey{Key: &k.PublicKey, KeyID: id, Algorithm: "ES256", Use: "sig"})
		}
		is.mu.Unlock()
		if hold != nil {
			select {
			case <-hold:
			case <-r.Context().Done():
				return
			}
		}
		if respond != nil {
			respond(w, set)
			return
		}
		writeJSON(w, set)
	})
	is.srv = httptest.NewServer(mux)
	t.Cleanup(is.srv.Close)
	return is
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// url is the issuer's URL, which is also where it serves the endpoints.
func (is *issuer) url() string { return is.srv.URL }

// addKey publishes a new key under id.
func (is *issuer) addKey(t *testing.T, id string) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	is.mu.Lock()
	defer is.mu.Unlock()
	is.keys[id] = k
}

// holdKeys holds every request for the key set until release is called, which
// the test's cleanup also does.
func (is *issuer) holdKeys(t *testing.T) (release func()) {
	t.Helper()
	hold := make(chan struct{})
	is.mu.Lock()
	defer is.mu.Unlock()
	is.hold = hold
	release = sync.OnceFunc(func() {
		is.mu.Lock()
		defer is.mu.Unlock()
		is.hold = nil
		close(hold)
	})
	t.Cleanup(release)
	return release
}

// respondWith answers every request for the key set with respond, or with the
// key set again when respond is nil.
func (is *issuer) respondWith(respond func(http.ResponseWriter, jose.JSONWebKeySet)) {
	is.mu.Lock()
	defer is.mu.Unlock()
	is.respond = respond
}

// unavailable answers a request as an API server that cannot serve it does.
func unavailable(w http.ResponseWriter, _ jose.JSONWebKeySet) {
	http.Error(w, "the server is currently unable to handle the request", http.StatusServiceUnavailable)
}

// tokenClaims are the claims of a token, as JSON; modifiers change them.
type tokenClaims map[string]any

// kubernetes returns the kubernetes.io claims.
func (c tokenClaims) kubernetes() map[string]any { return c["kubernetes.io"].(map[string]any) }

// tokenSerial makes every token unique, as the API server's token IDs do.
var tokenSerial atomic.Int64

// claims returns the claims the API server gives a token of the agents'
// ServiceAccount, for the audience, bound to a Pod on node, issued at now and
// valid for an hour.
func (is *issuer) claims(now time.Time) tokenClaims {
	return tokenClaims{
		"iss": is.url(),
		"sub": serviceAccountUser(agents),
		"aud": []string{audience},
		"iat": now.Unix(),
		"nbf": now.Unix(),
		"exp": now.Add(time.Hour).Unix(),
		"jti": fmt.Sprintf("token-%d", tokenSerial.Add(1)),
		"kubernetes.io": map[string]any{
			"namespace":      agents.Namespace,
			"serviceaccount": map[string]any{"name": agents.Name, "uid": "sa-uid"},
			"pod":            map[string]any{"name": "snapshot-agent-abcde", "uid": "pod-uid"},
			"node":           map[string]any{"name": node, "uid": "node-uid"},
		},
	}
}

// sign signs c with the issuer's key under id, or, when it has none, with a
// key it never publishes.
func (is *issuer) sign(t *testing.T, id string, c tokenClaims) string {
	t.Helper()
	is.mu.Lock()
	k, ok := is.keys[id]
	is.mu.Unlock()
	if !ok {
		return forge(t, id, c)
	}
	return signWith(t, k, id, c)
}

// forge signs c with a new key, which no issuer publishes, under the key ID
// id, which may be the ID of a key the issuer does publish.
func forge(t *testing.T, id string, c tokenClaims) string {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	return signWith(t, k, id, c)
}

// signWith signs c with k, under the key ID id.
func signWith(t *testing.T, k *ecdsa.PrivateKey, id string, c tokenClaims) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: jose.JSONWebKey{Key: k, KeyID: id}},
		(&jose.SignerOptions{}).WithType("JWT"))
	require.NoError(t, err)
	token, err := jwt.Signed(signer).Claims(map[string]any(c)).Serialize()
	require.NoError(t, err)
	return token
}

// reviewer is a fake TokenReview API: client-go's fake clientset, which
// answers every review with status and err, by default accepted. It counts the reviews, and holds each until
// release closes, when set, or until its context ends, as a real client would.
type reviewer struct {
	*fake.Clientset

	calls   atomic.Int32
	release chan struct{}

	mu     sync.Mutex
	status authenticationv1.TokenReviewStatus
	err    error
}

// accepted is the TokenReview answer that accepts a token as the agents'
// ServiceAccount's.
var accepted = authenticationv1.TokenReviewStatus{
	Authenticated: true,
	User:          authenticationv1.UserInfo{Username: serviceAccountUser(agents)},
	Audiences:     []string{audience},
}

func newReviewer() *reviewer {
	r := &reviewer{Clientset: fake.NewClientset(), status: accepted}
	r.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authenticationv1.TokenReview).DeepCopy()
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.err != nil {
			return true, nil, r.err
		}
		review.Status = r.status
		return true, review, nil
	})
	return r
}

// Create implements TokenReviewInterface.
func (r *reviewer) Create(ctx context.Context, review *authenticationv1.TokenReview,
	opts metav1.CreateOptions) (*authenticationv1.TokenReview, error) {
	r.calls.Add(1)
	if r.release != nil {
		select {
		case <-r.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return r.AuthenticationV1().TokenReviews().Create(ctx, review, opts)
}

// answerWith makes the reviewer answer every review with status and err.
func (r *reviewer) answerWith(status authenticationv1.TokenReviewStatus, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status, r.err = status, err
}

// harness is an Authorizer with fakes for everything it asks.
type harness struct {
	t         *testing.T
	clock     *clocktesting.FakeClock
	issuer    *issuer
	placement *placement
	reviewer  *reviewer
	auth      *Authorizer

	mu     sync.Mutex
	logs   []string
	tokens []string
	errs   []error
}

// newHarness returns a harness whose Placement puts member on node. It checks,
// when the test ends, that no goroutine is left, and that no error Authorize
// returned and no log line holds any part of any token the test presented.
func newHarness(t *testing.T) *harness {
	t.Helper()
	ignore := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, ignore) })
	h := &harness{
		t:         t,
		clock:     clocktesting.NewFakeClock(time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)),
		issuer:    newIssuer(t),
		placement: &placement{nodes: map[types.UID]map[string]string{attempt: {member: node}}},
		reviewer:  newReviewer(),
	}
	t.Cleanup(h.checkSecrets)
	auth, err := New(t.Context(), h.config())
	require.NoError(t, err)
	h.auth = auth
	return h
}

// config is the harness's Config.
func (h *harness) config() Config {
	return Config{
		Placement:      h.placement,
		TokenReviews:   h.reviewer,
		RESTConfig:     &rest.Config{Host: h.issuer.url()},
		Audience:       audience,
		ServiceAccount: agents,
		Clock:          h.clock,
		Log:            h.logger(),
	}
}

// logger records every line, at every verbosity.
func (h *harness) logger() logr.Logger {
	return funcr.New(func(prefix, args string) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.logs = append(h.logs, prefix+" "+args)
	}, funcr.Options{Verbosity: 9})
}

// checkSecrets fails the test if an error Authorize returned, which the
// coordinator logs, or a log line holds any part of a token presented.
func (h *harness) checkSecrets() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, token := range h.tokens {
		for _, part := range strings.Split(token, ".") {
			if len(part) < 8 {
				continue
			}
			for _, err := range h.errs {
				assert.NotContains(h.t, err.Error(), part, "an error holds part of a token")
			}
			for _, line := range h.logs {
				assert.NotContains(h.t, line, part, "a log line holds part of a token")
			}
		}
	}
}

// token returns a valid token of the agents' ServiceAccount, changed by
// modify, signed with the issuer's key.
func (h *harness) token(modify ...func(tokenClaims)) string {
	h.t.Helper()
	c := h.issuer.claims(h.clock.Now())
	for _, m := range modify {
		m(c)
	}
	return h.issuer.sign(h.t, "key-0", c)
}

// signedWith returns a valid token of the agents' ServiceAccount, signed with
// the issuer's key under id, or, when it has none, with a key it never
// publishes.
func (h *harness) signedWith(id string) string {
	h.t.Helper()
	return h.issuer.sign(h.t, id, h.issuer.claims(h.clock.Now()))
}

// authorize asks the Authorizer, within server.AuthorizeTimeout as the
// coordinator does, to admit token for member of attempt.
func (h *harness) authorize(member, token string) error {
	ctx, cancel := context.WithTimeout(h.t.Context(), server.AuthorizeTimeout)
	defer cancel()
	return h.authorizeWithin(ctx, member, token)
}

// authorizeWithin asks the Authorizer, within ctx, to admit token for member
// of attempt.
func (h *harness) authorizeWithin(ctx context.Context, member, token string) error {
	err := h.auth.Authorize(ctx, attempt, member, token)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tokens = append(h.tokens, token)
	if err != nil {
		h.errs = append(h.errs, err)
	}
	return err
}
