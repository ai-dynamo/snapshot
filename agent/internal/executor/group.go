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

// admitCapture runs a capture's admission: the member publishes its Pod IP, its
// Pod's UID and its captured container, reads every other member's, and passes
// the admitted barrier, after which the attempt commits.
func admitCapture(ctx context.Context, g *group.Member, podIP, podUID, container string) error {
	ip, err := netip.ParseAddr(podIP)
	if err != nil {
		return fmt.Errorf("the Pod's IP: %w", err)
	}
	if err := g.Admit(ctx, group.Admission{PodIP: ip, PodUID: podUID, Containers: []string{container}}); err != nil {
		return err
	}
	return g.Barrier(ctx, group.Admitted)
}

// groupManifest records g's part in the capture, once it has been admitted.
func groupManifest(g *group.Member) *types.PodGroupManifest {
	ids := g.MemberIDs()
	members := make([]types.PodGroupManifestMember, len(ids))
	for rank, a := range g.Admissions() {
		members[rank] = types.PodGroupManifestMember{ID: ids[rank], PodUID: a.PodUID, Containers: a.Containers, PodIP: a.PodIP}
	}
	return &types.PodGroupManifest{
		Version:    types.PodGroupManifestVersion,
		CaptureUID: g.Attempt(),
		MemberID:   g.ID(),
		Members:    members,
	}
}

// admitRestore runs a restore's admission: the member checks that its artifact
// is its own part of a group capture, publishes its new Pod IP and the capture
// it restores, reads every other member's, runs check, and passes the admitted
// barrier, after which the attempt commits. check can read every member's
// admission, and its failure refuses the attempt.
func admitRestore(ctx context.Context, g *group.Member, targetPodIP string, captured *types.PodGroupManifest,
	check func() error,
) error {
	if err := checkGroupArtifact(captured, g.ID(), g.Rank(), len(g.MemberIDs())); err != nil {
		return err
	}
	ip, err := netip.ParseAddr(targetPodIP)
	if err != nil {
		return fmt.Errorf("the restore Pod's IP: %w", err)
	}
	if err := g.Admit(ctx, group.Admission{PodIP: ip, CaptureID: captured.CaptureUID}); err != nil {
		return err
	}
	if err := check(); err != nil {
		return err
	}
	return g.Barrier(ctx, group.Admitted)
}

// checkGroupArtifact refuses an artifact that is not member's part, at rank,
// of a group capture whose number of members is members, in the version of
// its manifest this agent restores.
func checkGroupArtifact(captured *types.PodGroupManifest, member string, rank, members int) error {
	switch {
	case captured == nil:
		return errors.New("the artifact was not captured as a member of a pod group")
	case captured.Version != types.PodGroupManifestVersion:
		return fmt.Errorf("the artifact's pod group section has version %d, and this agent restores version %d",
			captured.Version, types.PodGroupManifestVersion)
	case len(captured.Members) != members:
		return fmt.Errorf("the capture had %d members, the restore has %d", len(captured.Members), members)
	case captured.MemberID != member || captured.Rank() != rank:
		return fmt.Errorf("the artifact is member %s at rank %d of its capture, not member %s at rank %d",
			captured.MemberID, captured.Rank(), member, rank)
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
