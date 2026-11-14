// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/go-logr/logr"

	"github.com/ai-dynamo/snapshot/agent/internal/group"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

// groupFailTimeout bounds failing an attempt after a member's capture or
// restore failed, which may have to wait for the coordinator.
const groupFailTimeout = 30 * time.Second

// admitCapture runs a capture's admission: the member publishes its Pod IP,
// reads every other member's, and passes the admitted barrier, after which the
// attempt commits.
func admitCapture(ctx context.Context, g *group.Member, podIP string) error {
	ip, err := netip.ParseAddr(podIP)
	if err != nil {
		return fmt.Errorf("the Pod's IP: %w", err)
	}
	if err := g.Admit(ctx, group.Admission{PodIP: ip}); err != nil {
		return err
	}
	return g.Barrier(ctx, group.Admitted)
}

// groupManifest records g's part in the capture, once it has been admitted.
func groupManifest(g *group.Member) *types.GroupManifest {
	podIPs := make([]netip.Addr, len(g.Admissions()))
	for rank, a := range g.Admissions() {
		podIPs[rank] = a.PodIP
	}
	return &types.GroupManifest{CaptureID: g.Attempt(), Member: g.ID(), Rank: g.Rank(), PodIPs: podIPs}
}

// admitRestore runs a restore's admission: the member checks that its artifact
// is its own part of a group capture, publishes its new Pod IP and the capture
// it restores, reads every other member's, and passes the admitted barrier,
// after which the attempt commits.
func admitRestore(ctx context.Context, g *group.Member, targetPodIP string, captured *types.GroupManifest) error {
	if err := checkGroupArtifact(captured, g.ID(), g.Rank()); err != nil {
		return err
	}
	ip, err := netip.ParseAddr(targetPodIP)
	if err != nil {
		return fmt.Errorf("the restore Pod's IP: %w", err)
	}
	if err := g.Admit(ctx, group.Admission{PodIP: ip, CaptureID: captured.CaptureID}); err != nil {
		return err
	}
	return g.Barrier(ctx, group.Admitted)
}

// checkGroupArtifact refuses an artifact that is not member's part, at rank,
// of a group capture.
func checkGroupArtifact(captured *types.GroupManifest, member string, rank int) error {
	switch {
	case captured == nil:
		return errors.New("the artifact was not captured as a member of a pod group")
	case captured.Member != member || captured.Rank != rank:
		return fmt.Errorf("the artifact is member %s at rank %d of its capture, not member %s at rank %d",
			captured.Member, captured.Rank, member, rank)
	}
	return nil
}

// failGroup ends g's attempt for every member, after this member's capture or
// restore failed.
func failGroup(ctx context.Context, log logr.Logger, g *group.Member) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), groupFailTimeout)
	defer cancel()
	if err := g.Fail(ctx); err != nil {
		log.Error(err, "Failed to fail the pod group's attempt", "attempt", g.Attempt())
	}
}

// recordCommit calls committed, the caller's record that the member started,
// once the attempt has committed. This member has changed nothing yet, so a
// record that cannot be written leaves it as Snapshot found it, and fails the
// attempt for the members that already started.
func recordCommit(ctx context.Context, committed func(context.Context) error) error {
	if committed == nil {
		return nil
	}
	return committed(ctx)
}
