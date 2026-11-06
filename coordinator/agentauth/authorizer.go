// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agentauth

import (
	"context"
	"errors"
	"fmt"

	"github.com/ai-dynamo/snapshot/coordinator/server"
	"github.com/ai-dynamo/snapshot/coordinator/session"
	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-logr/logr"
	"golang.org/x/sync/singleflight"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/cache"
	authenticationv1client "k8s.io/client-go/kubernetes/typed/authentication/v1"
	"k8s.io/client-go/rest"
	"k8s.io/utils/clock"
)

// Placement reports where the members of an attempt run.
type Placement interface {
	// MemberNodes returns the node of each member of the attempt uid, by
	// member ID; a member whose node is not known yet maps to "". It returns
	// an error wrapping session.ErrNotFound when the attempt is not known.
	MemberNodes(ctx context.Context, uid types.UID) (map[string]string, error)
}

// Config configures an Authorizer. Every field but Clock and Log is required.
type Config struct {
	// Placement reports the node each member of an attempt runs on.
	Placement Placement

	// TokenReviews reviews the tokens that pass every other check.
	TokenReviews authenticationv1client.TokenReviewInterface

	// RESTConfig reaches the API server, which publishes its ServiceAccount
	// token issuer and the issuer's keys.
	RESTConfig *rest.Config

	// Audience is the audience the agents' tokens are issued for.
	Audience string

	// ServiceAccount is the agents' ServiceAccount.
	ServiceAccount types.NamespacedName

	// Clock tells the time tokens and cached reviews expire by. It defaults to
	// the real clock.
	Clock clock.PassiveClock

	// Log receives failed TokenReviews, never a token. The zero Logger
	// discards everything.
	Log logr.Logger
}

// validate reports a missing field of cfg.
func (cfg Config) validate() error {
	switch {
	case cfg.Placement == nil:
		return fmt.Errorf("an agent Authorizer needs a Placement: %w", session.ErrInvalid)
	case cfg.TokenReviews == nil:
		return fmt.Errorf("an agent Authorizer needs a TokenReview client: %w", session.ErrInvalid)
	case cfg.RESTConfig == nil:
		return fmt.Errorf("an agent Authorizer needs the API server's REST config: %w", session.ErrInvalid)
	case cfg.Audience == "":
		return fmt.Errorf("an agent Authorizer needs an audience: %w", session.ErrInvalid)
	case cfg.ServiceAccount.Namespace == "" || cfg.ServiceAccount.Name == "":
		return fmt.Errorf("an agent Authorizer needs the agents' ServiceAccount: %w", session.ErrInvalid)
	}
	return nil
}

// Authorizer admits an agent for a member of an attempt when the agent's token
// is a token of the agents' ServiceAccount for the audience, bound to a Pod on
// the node the member runs on, and the API server still accepts it.
type Authorizer struct {
	cfg Config

	// user is the name the agents' tokens authenticate as.
	user string

	// verifier checks a token's signature, issuer, audience, and lifetime.
	verifier *oidc.IDTokenVerifier

	// reviews holds TokenReviews' answers by the token's SHA-256, and flights
	// runs one TokenReview at a time for each token.
	reviews *cache.LRUExpireCache
	flights singleflight.Group
}

var _ server.Authorizer = (*Authorizer)(nil)

// New validates cfg and returns an Authorizer. It reads the API server's
// ServiceAccount token issuer within ctx, and fetches the issuer's keys when a
// token first needs them.
func New(ctx context.Context, cfg Config) (*Authorizer, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.RealClock{}
	}
	verifier, err := newVerifier(ctx, cfg.RESTConfig, cfg.Audience, cfg.Clock.Now)
	if err != nil {
		return nil, err
	}
	return &Authorizer{
		cfg:      cfg,
		user:     serviceAccountUser(cfg.ServiceAccount),
		verifier: verifier,
		reviews:  cache.NewLRUExpireCacheWithClock(maxReviews, cfg.Clock),
	}, nil
}

// Authorize implements server.Authorizer. It verifies the token locally, then
// checks it against where the attempt's members run, and only then asks the
// API server, so a token that fails either check costs no TokenReview. It
// logs no refusal: the coordinator logs the error, which says why and never
// holds the token.
func (a *Authorizer) Authorize(ctx context.Context, uid types.UID, member, token string) error {
	id, err := a.verify(ctx, token)
	if err != nil {
		return err
	}
	if err := a.checkPlacement(ctx, uid, member, id); err != nil {
		return err
	}
	return a.review(ctx, token, id)
}

// checkPlacement checks that the token is the agents' ServiceAccount's, and
// that member belongs to the attempt uid and runs on the node the token is
// bound to.
func (a *Authorizer) checkPlacement(ctx context.Context, uid types.UID, member string, id identity) error {
	if id.serviceAccount != a.cfg.ServiceAccount {
		return fmt.Errorf("%w: the token is ServiceAccount %s's, not the agents' %s", server.ErrPermissionDenied,
			id.serviceAccount, a.cfg.ServiceAccount)
	}
	nodes, err := a.cfg.Placement.MemberNodes(ctx, uid)
	switch {
	case errors.Is(err, session.ErrNotFound):
		return err
	case err != nil:
		return fmt.Errorf("%w: reading where the attempt's members run: %v", server.ErrUnavailable, err)
	}
	node, ok := nodes[member]
	switch {
	case !ok:
		return fmt.Errorf("%w: %q is not a member of the attempt", server.ErrPermissionDenied, member)
	case node == "":
		// The group's status records a member's node once it is scheduled.
		return fmt.Errorf("%w: the node of member %q is not known yet", server.ErrUnavailable, member)
	case node != id.node:
		return fmt.Errorf("%w: member %q runs on node %q, not on the token's node %q", server.ErrPermissionDenied,
			member, node, id.node)
	}
	return nil
}
