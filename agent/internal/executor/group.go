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

	"github.com/ai-dynamo/snapshot/agent/internal/cuda"
	"github.com/ai-dynamo/snapshot/agent/internal/group"
	"github.com/ai-dynamo/snapshot/agent/internal/types"
)

// groupFailTimeout bounds failing an attempt after a member's capture or
// restore failed, which may have to wait for the coordinator.
const groupFailTimeout = 30 * time.Second

// admitCapture runs a capture's admission: the member publishes its Pod IP, its
// Pod's UID, its captured container and whether it runs a cuInterpose relay,
// reads every other member's, inspects with the other relays, whose leader
// validates the group's topology, and passes the admitted barrier, after which
// the attempt commits.
func admitCapture(ctx context.Context, g *group.Member, podIP, podUID, container string, state *types.CheckpointContainerSnapshot) error {
	ip, err := netip.ParseAddr(podIP)
	if err != nil {
		return fmt.Errorf("the Pod's IP: %w", err)
	}
	runsRelay := state.CuInterpose.UsesCoordinator()
	admission := group.Admission{PodIP: ip, PodUID: podUID, Containers: []string{container}, RunsRelay: runsRelay}
	if err := g.Admit(ctx, admission); err != nil {
		return err
	}
	if runsRelay {
		err := withRelay(ctx, g, func(relay *cuda.CuInterposeRelay) error {
			return cuda.InspectCuInterpose(ctx, coordinatorTarget(state.PID, state.CuInterpose.PIDs, relay))
		})
		if err != nil {
			return fmt.Errorf("inspect cuinterpose: %w", err)
		}
	}
	return g.Barrier(ctx, group.Admitted)
}

// withRelay runs a cuInterpose coordinator through run, as g's relay when g is
// set, while the agent serves the relay's session socket.
func withRelay(ctx context.Context, g *group.Member, run func(*cuda.CuInterposeRelay) error) error {
	if g == nil {
		return run(nil)
	}
	relay, err := g.OpenRelay(ctx)
	if err != nil {
		return err
	}
	if relay == nil {
		return run(nil)
	}
	err = run(&cuda.CuInterposeRelay{Session: relay.File, Args: relay.Args})
	return errors.Join(err, relay.Close())
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
// is its own part of a group capture, publishes its new Pod IP, the capture it
// restores and whether it runs a cuInterpose relay, reads every other member's,
// and passes the admitted barrier, after which the attempt commits.
func admitRestore(ctx context.Context, g *group.Member, targetPodIP string, manifest *types.CheckpointManifest) error {
	captured := manifest.PodGroup
	if err := checkGroupArtifact(captured, g.ID(), g.Rank(), len(g.MemberIDs())); err != nil {
		return err
	}
	ip, err := netip.ParseAddr(targetPodIP)
	if err != nil {
		return fmt.Errorf("the restore Pod's IP: %w", err)
	}
	admission := group.Admission{PodIP: ip, CaptureID: captured.CaptureUID, RunsRelay: manifest.CuInterpose.UsesCoordinator()}
	if err := g.Admit(ctx, admission); err != nil {
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
