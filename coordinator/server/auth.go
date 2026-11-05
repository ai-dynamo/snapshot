// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"strconv"
	"time"

	"github.com/ai-dynamo/snapshot/coordinator/session"
	"github.com/go-logr/logr"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"k8s.io/apimachinery/pkg/types"
)

// The errors of the Authorizer contract. The first two refuse a client; the
// last two mean it may succeed later.
var (
	// ErrUnauthenticated reports a token that does not authenticate an agent.
	ErrUnauthenticated = errors.New("unauthenticated")

	// ErrPermissionDenied reports an agent that may not act for the member,
	// such as one on another node than the member's Pod.
	ErrPermissionDenied = errors.New("permission denied")

	// ErrUnavailable reports that authorization could not be decided now, such
	// as when a TokenReview failed.
	ErrUnavailable = errors.New("authorization unavailable")
)

// Authorizer decides whether the presenter of a token may act for a member of
// an attempt, typically by checking the agent's ServiceAccount token with a
// TokenReview and that the member's Pod runs on the agent's node. It is
// consulted once per connection, before the connection reaches anything, and
// it admits the connection only by returning nil. An error is one of:
//
//   - ErrUnauthenticated or ErrPermissionDenied: a refusal;
//   - ErrUnavailable: a transient failure;
//   - session.ErrNotFound: the attempt is not known yet, including when the
//     Authorizer's own cache lags.
//
// Any other error counts as ErrUnavailable, and is logged as a failure of the
// Authorizer. NATS reports every refusal to a client the same way, so a client
// retries all of them until its own deadline; the distinction decides what the
// coordinator logs.
//
// A member's grace period keeps running while its new connection is being
// authorized, so Authorize must answer well within AuthorizeTimeout, for
// example by caching TokenReviews.
type Authorizer interface {
	Authorize(ctx context.Context, uid types.UID, member, token string) error
}

// AuthorizeTimeout bounds authenticating a member's connection: the call to the
// Authorizer, and failing a session the member joined under an earlier
// coordinator. It is shorter than the time a client gives the coordinator to
// answer a connection, the session client's ClientConnectTimeout, so a slow
// TokenReview is answered before the client gives up and connects again, which
// would ask the Authorizer again.
const AuthorizeTimeout = 3 * time.Second

// authenticator is the embedded server's client authentication.
type authenticator struct{ s *Server }

// Check implements natsserver.Authentication. It admits the coordinator's own
// in-process connections, and a member's connection once the Authorizer admits
// it, the coordinator is ready, and the attempt has a session with that
// member, granting it session.MemberPermissions. It refuses every other
// client.
func (a authenticator) Check(c natsserver.ClientAuthentication) bool {
	s := a.s
	opts := c.GetOpts()
	if user, ok := s.internal(c, opts); ok {
		c.RegisterUser(user)
		return true
	}
	if !s.ready.Load() {
		return false
	}
	attempt, member, ok := session.ParseUserName(opts.Username)
	if !ok {
		s.log.Info("Refused a client that does not name a member")
		return false
	}
	uid := types.UID(attempt)
	log := s.log.WithValues("attempt", attempt, "member", member)
	epoch, err := parseEpoch(opts.Password)
	if err != nil {
		log.Info("Refused a member that presented a malformed epoch")
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), AuthorizeTimeout)
	defer cancel()
	if !s.authorize(ctx, log, uid, member, opts.Token) || !s.admit(ctx, log, uid, member, epoch) {
		return false
	}
	p := session.MemberPermissions(attempt, member)
	c.RegisterUser(&natsserver.User{
		Username: opts.Username,
		Account:  s.account,
		Permissions: &natsserver.Permissions{
			Publish:   &natsserver.SubjectPermission{Allow: p.Publish},
			Subscribe: &natsserver.SubjectPermission{Allow: p.Subscribe},
		},
	})
	return true
}

// internal returns the user of one of the coordinator's in-process connections,
// which present the coordinator's token over an in-process pipe.
func (s *Server) internal(c natsserver.ClientAuthentication, opts *natsserver.ClientOpts) (*natsserver.User, bool) {
	if subtle.ConstantTimeCompare([]byte(opts.Token), []byte(s.token)) != 1 {
		return nil, false
	}
	if addr := c.RemoteAddress(); addr == nil || addr.Network() != "pipe" {
		return nil, false
	}
	switch opts.Username {
	case coordinatorUser:
		return &natsserver.User{Username: coordinatorUser, Account: s.account}, true
	case eventsUser:
		return &natsserver.User{Username: eventsUser, Account: s.ns.SystemAccount()}, true
	}
	return nil, false
}

// parseEpoch reads the epoch a member presents as its password: empty until
// it has read one.
func parseEpoch(password string) (uint64, error) {
	if password == "" {
		return 0, nil
	}
	return strconv.ParseUint(password, 10, 64)
}

// authorize asks the Authorizer, and logs a refusal by its kind.
func (s *Server) authorize(ctx context.Context, log logr.Logger, uid types.UID, member, token string) bool {
	err := s.cfg.Authorizer.Authorize(ctx, uid, member, token)
	switch {
	case err == nil:
		return true
	case errors.Is(err, ErrUnauthenticated), errors.Is(err, ErrPermissionDenied):
		log.Info("Refused a member the Authorizer refused", "reason", err.Error())
	case errors.Is(err, ErrUnavailable), errors.Is(err, session.ErrNotFound):
		log.V(1).Info("Could not authorize a member yet", "reason", err.Error())
	default:
		log.Error(err, "The Authorizer failed outside its contract; the member will retry")
	}
	return false
}

// admit decides whether an authorized member may connect, following the epoch
// it presents. A member that presents none is joining, and needs the attempt's
// session. A member that presents an epoch is reconnecting: to the session it
// joined if the epoch matches; to a session this coordinator recreated after
// losing the one it joined if not, which it fails with
// session.ReasonCoordinatorLost before admitting the member; or to no session
// at all, which the member finds out once admitted. Either way, a member that
// lost its session is never admitted to a session it could write to.
func (s *Server) admit(ctx context.Context, log logr.Logger, uid types.UID, member string, epoch uint64) bool {
	a := s.lookup(uid)
	switch {
	case a == nil && epoch == 0:
		log.V(1).Info("Refused a member of an attempt with no session yet")
		return false
	case a == nil:
		return true
	case !a.has(member):
		log.Info("Refused a client that is not a member of the attempt")
		return false
	case epoch != 0 && epoch != a.epoch:
		if err := s.endWith(ctx, a, []byte(session.ReasonCoordinatorLost)); err != nil && !ended(err) {
			log.Error(err, "Failing a session whose member joined it under an earlier coordinator failed")
			return false
		}
		log.Info("Failed a session whose member joined it under an earlier coordinator")
	}
	return true
}
