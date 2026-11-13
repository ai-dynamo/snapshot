// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package agentauth

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/ai-dynamo/snapshot/coordinator/server"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// maxReviewAge caps how long a TokenReview's admission is reused, so a token
// whose Pod was deleted is refused within it. It is the default of
// kube-apiserver's own cache of token webhooks' answers.
const maxReviewAge = 2 * time.Minute

// maxRefusalAge caps how long a TokenReview's refusal is reused. The API server
// also refuses a token when it fails to read the token's Pod, so this is well
// under server.DefaultGrace: a refusal given in error is reviewed again while
// the member can still connect. A refused agent's retries still cost at most
// one TokenReview per token every maxRefusalAge.
const maxRefusalAge = 5 * time.Second

// maxReviews bounds the TokenReview answers held. Only tokens the issuer
// signed for the agents' ServiceAccount, bound to the node of a member they
// name, reach a TokenReview, so this is far more than the agents that connect
// to one coordinator within maxReviewAge.
const maxReviews = 1024

// review reports whether the API server accepts the token, which a TokenReview
// decides: the Pod the token is bound to still exists, and the token has not
// been invalidated. It reuses an admission until the token expires or for
// maxReviewAge, a refusal for maxRefusalAge, and never a failure to ask, and
// runs one TokenReview at a time for each token. That TokenReview runs under a
// context of its own, bounded by server.AuthorizeTimeout, so it serves every
// caller waiting for it, and is reused, even once the caller that started it
// has given up.
func (a *Authorizer) review(ctx context.Context, token string, id identity) error {
	key := sha256.Sum256([]byte(token))
	flight := a.flights.DoChan(string(key[:]), func() (any, error) {
		// Look here, inside the flight, so a call that missed the answer an
		// earlier flight was adding finds it rather than asking again.
		if answer, ok := a.reviews.Get(key); ok {
			err, _ := answer.(error) // An admission is a nil error.
			return nil, err
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), server.AuthorizeTimeout)
		defer cancel()
		err := a.tokenReview(ctx, token, id)
		if !errors.Is(err, server.ErrUnavailable) {
			ttl := min(id.expiry.Sub(a.cfg.Clock.Now()), maxReviewAge)
			if err != nil {
				ttl = min(ttl, maxRefusalAge)
			}
			a.reviews.Add(key, err, ttl)
		}
		return nil, err
	})
	select {
	case r := <-flight:
		return r.Err
	case <-ctx.Done():
		return fmt.Errorf("%w: waiting for a TokenReview: %w", server.ErrUnavailable, context.Cause(ctx))
	}
}

// tokenReview asks the API server whether it accepts the token, which id
// describes, for the audience, as the agents' ServiceAccount.
func (a *Authorizer) tokenReview(ctx context.Context, token string, id identity) error {
	r, err := a.cfg.TokenReviews.Create(ctx, &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{Token: token, Audiences: []string{a.cfg.Audience}},
	}, metav1.CreateOptions{})
	switch {
	case err != nil:
		a.cfg.Log.Error(err, "A TokenReview failed", "pod", id.pod)
		return fmt.Errorf("%w: a TokenReview failed: %w", server.ErrUnavailable, err)
	case !r.Status.Authenticated:
		return fmt.Errorf("%w: the API server does not accept the token of Pod %s: %s", server.ErrUnauthenticated,
			id.pod, r.Status.Error)
	case r.Status.User.Username != a.user:
		return fmt.Errorf("%w: the token of Pod %s authenticates %q, not %q", server.ErrUnauthenticated,
			id.pod, r.Status.User.Username, a.user)
	}
	return nil
}
