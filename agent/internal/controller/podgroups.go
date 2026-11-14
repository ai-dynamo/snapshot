// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/go-logr/logr"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"

	"github.com/ai-dynamo/snapshot/agent/internal/group"
	"github.com/ai-dynamo/snapshot/coordinator"
	"github.com/ai-dynamo/snapshot/coordinator/session"
)

// The agent's own reasons for a pod group member, besides the session's.
const (
	// reasonGroupPending is the agent's wait on nvidia.com/Restored while a
	// group target waits for its PodGroupRestore to pin it. Like
	// SnapshotPending, it is internal to the agent: the Pod contract does not
	// name it, and podcontract.ClassifyRestoreOutcome reads it as not final.
	reasonGroupPending = "GroupPending"

	// reasonInvalidPodGroupMember fails a PodSnapshotContent whose PodSnapshot
	// names a pod group it is not a member of.
	reasonInvalidPodGroupMember = "InvalidPodGroupMember"

	// reasonCoordinatorUnreachable fails a member that could not join its
	// attempt although the attempt's deadline had not passed. A member that
	// never joined has changed nothing.
	reasonCoordinatorUnreachable = "CoordinatorUnreachable"

	// reasonCheckpointInterrupted and reasonRestoreInterrupted fail a member
	// that had started when its agent restarted. A started member is never
	// checkpointed or restored again: the agent cannot tell how far it got.
	reasonCheckpointInterrupted = "CheckpointInterrupted"
	reasonRestoreInterrupted    = "RestoreInterrupted"
)

// groupFailTimeout bounds failing an attempt the controller joined but did not
// hand to the executor, which may have to wait for the coordinator.
const groupFailTimeout = 30 * time.Second

// podGroups is the agent's part in pod groups, set by EnablePodGroups.
type podGroups struct {
	// configFn and joinFn are Locator.Config and group.Join. Tests replace
	// them.
	configFn func(ctx context.Context, attempt, member string, sessionNode group.SessionNode) (coordinator.Config, error)
	joinFn   func(context.Context, coordinator.Config) (*group.Member, error)

	// restores is the PodGroupRestore informer's indexer, by UID (uidIndex).
	// Run sets it.
	restores cache.Indexer

	// captureMembers holds, by PodSnapshotContent name, the content's part in
	// a pod group, or nil for a content captured on its own: a member's
	// PodSnapshot and its group never change it.
	captureMembers sync.Map

	// captureOutcomes and restoreOutcomes hold, by PodSnapshotContent name
	// and by restore Pod UID, the end of an attempt this agent ran, until its
	// report is written. A started member never runs again, so a report that
	// failed to land is sent again from here.
	captureOutcomes sync.Map
	restoreOutcomes sync.Map

	// leaving counts the members that wait in Leave for their session to end.
	leaving sync.WaitGroup
}

// EnablePodGroups makes the agent take part in pod groups: it checkpoints the
// members of PodGroupSnapshots and restores the targets of PodGroupRestores on
// its node, and joins their attempts through locator, which reads the agent
// Pods with the controller's client unless it has its own. Without it, nothing
// of pod groups runs. Call it before Run.
func (w *NodeController) EnablePodGroups(locator group.Locator) {
	if locator.Client == nil {
		locator.Client = w.clientset
	}
	w.podGroups = &podGroups{configFn: locator.Config, joinFn: group.Join}
}

// joinAttempt joins the attempt, the UID of its group resource, as member. It
// waits for the coordinator's address, and then for the coordinator to admit
// the member, until ctx is done.
func (w *NodeController) joinAttempt(ctx context.Context, attempt ktypes.UID, member string, sessionNode group.SessionNode) (*group.Member, error) {
	cfg, err := w.podGroups.configFn(ctx, string(attempt), member, sessionNode)
	if err != nil {
		return nil, err
	}
	return w.podGroups.joinFn(ctx, cfg)
}

// leaveAttempt leaves g's attempt off the queue worker. Leave keeps the
// connection until the session ends, which the slowest member, or the
// session's deadline, decides; ctx is the agent's, so a shutdown closes it.
func (w *NodeController) leaveAttempt(ctx context.Context, log logr.Logger, g *group.Member) {
	w.podGroups.leaving.Add(1)
	go func() {
		defer w.podGroups.leaving.Done()
		if err := g.Leave(ctx); err != nil && ctx.Err() == nil {
			log.Info("The pod group's attempt ended without every member done", "attempt", g.Attempt(), "error", err.Error())
		}
	}()
}

// failAttempt ends g's attempt for every member and closes the connection.
// The executor does this itself for a capture or restore that fails, so it is
// the safety net for a member that the controller joined and that failed
// outside the executor. Failing an attempt that has ended changes nothing.
func failAttempt(ctx context.Context, log logr.Logger, g *group.Member) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), groupFailTimeout)
	defer cancel()
	if err := g.Fail(ctx); err != nil {
		log.Error(err, "Failed to fail the pod group's attempt", "attempt", g.Attempt())
	}
}

// sessionEndReason returns the reason of the session's end when err comes
// from it: the member failed because the attempt ended, so it reports the
// attempt's reason. It returns "" for a member that failed on its own.
func sessionEndReason(err error) string {
	var abort *session.AbortError
	if errors.As(err, &abort) {
		return abort.Reason
	}
	return ""
}

// attemptEndReason returns the group's reason for a member that failed with
// err: the session's when err comes from its end, or DeadlineExceeded once the
// attempt's deadline has passed, which ends the session too. It returns ""
// for a member that failed on its own.
func attemptEndReason(err error, deadline, now time.Time) string {
	if reason := sessionEndReason(err); reason != "" {
		return reason
	}
	if !now.Before(deadline) {
		return session.ReasonDeadlineExceeded
	}
	return ""
}

// joinFailureReason returns the reason of a member that could not join its
// attempt, and whether it is the group's (see attemptEndReason).
func joinFailureReason(err error, deadline, now time.Time) (string, bool) {
	if reason := attemptEndReason(err, deadline, now); reason != "" {
		return reason, true
	}
	return reasonCoordinatorUnreachable, false
}

// attemptContext bounds a member's part in an attempt by the attempt's
// deadline, and by limit when it is set and earlier.
func attemptContext(ctx context.Context, deadline time.Time, limit time.Duration) (context.Context, context.CancelFunc) {
	if limit > 0 {
		if byLimit := time.Now().Add(limit); byLimit.Before(deadline) {
			deadline = byLimit
		}
	}
	return context.WithDeadline(ctx, deadline)
}
