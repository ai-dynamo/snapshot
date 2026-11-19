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

// inspectWithRelayFunc is overridden in tests: cuda.InspectCuInterpose runs
// the cuInterpose coordinator in the source's namespaces, which a unit test
// cannot satisfy.
var inspectWithRelayFunc = cuda.InspectCuInterpose

// admitCapture runs a capture's admission: the member publishes its Pod IP, its
// Pod's UID, its captured container and whether it runs a cuInterpose relay,
// reads every other member's, inspects with the other relays, whose leader
// validates the group's topology and records which relays share GPU memory
// over NVLink, and passes the admitted barrier, after which the attempt
// commits. It returns the ranks of the members whose Pods share GPU memory
// over NVLink with this member's.
func admitCapture(ctx context.Context, g *group.Member, podIP, podUID, container string, state *types.CheckpointContainerSnapshot) ([]int, error) {
	ip, err := netip.ParseAddr(podIP)
	if err != nil {
		return nil, fmt.Errorf("the Pod's IP: %w", err)
	}
	runsRelay := state.CuInterpose.UsesCoordinator()
	admission := group.Admission{PodIP: ip, PodUID: podUID, Containers: []string{container}, RunsRelay: runsRelay}
	if err := g.Admit(ctx, admission); err != nil {
		return nil, err
	}
	var fabricPeers []int
	if runsRelay {
		err := withRelay(ctx, g, func(relay *cuda.CuInterposeRelay) error {
			return inspectWithRelayFunc(ctx, coordinatorTarget(state.PID, state.CuInterpose.PIDs, relay))
		})
		if err != nil {
			return nil, fmt.Errorf("inspect cuinterpose: %w", err)
		}
		if fabricPeers, err = g.FabricPeers(ctx); err != nil {
			return nil, err
		}
	}
	if err := g.Barrier(ctx, group.Admitted); err != nil {
		return nil, err
	}
	return fabricPeers, nil
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

// groupManifest records g's part in the capture, once it has been admitted,
// with fabricPeers, the ranks of the members whose Pods share GPU memory over
// NVLink with this member's.
func groupManifest(g *group.Member, fabricPeers []int) *types.PodGroupManifest {
	ids := g.MemberIDs()
	members := make([]types.PodGroupManifestMember, len(ids))
	for rank, a := range g.Admissions() {
		members[rank] = types.PodGroupManifestMember{ID: ids[rank], PodUID: a.PodUID, Containers: a.Containers, PodIP: a.PodIP}
	}
	return &types.PodGroupManifest{
		Version:     types.PodGroupManifestVersion,
		CaptureUID:  g.Attempt(),
		MemberID:    g.ID(),
		Members:     members,
		FabricPeers: fabricPeers,
	}
}

// admitRestore runs a restore's admission: the member checks that its artifact
// is its own part of a group capture, publishes its new Pod IP, the capture it
// restores, whether it runs a cuInterpose relay and, if its Pod shared GPU
// memory over NVLink at capture, where gpus, its restore container's GPUs, are
// in their NVLink fabric. It reads every other member's, checks that the
// members that shared GPU memory can share it again, and passes the admitted
// barrier, after which the attempt commits.
func admitRestore(ctx context.Context, g *group.Member, targetPodIP string, manifest *types.CheckpointManifest, gpus []string) error {
	captured := manifest.PodGroup
	if err := checkGroupArtifact(captured, g.ID(), g.Rank(), len(g.MemberIDs())); err != nil {
		return err
	}
	ip, err := netip.ParseAddr(targetPodIP)
	if err != nil {
		return fmt.Errorf("the restore Pod's IP: %w", err)
	}
	admission := group.Admission{PodIP: ip, CaptureID: captured.CaptureUID, RunsRelay: manifest.CuInterpose.UsesCoordinator()}
	// A member whose Pod shared no GPU memory over NVLink needs no fabric.
	if len(captured.FabricPeers) > 0 {
		fabric, err := readGPUFabricFunc(ctx, gpus)
		if err != nil {
			return fmt.Errorf("read the NVLink fabric of the restore Pod's GPUs: %w", err)
		}
		admission.Fabric = &group.FabricAdmission{Peers: captured.FabricPeers, GPUs: fabric}
	}
	if err := g.Admit(ctx, admission); err != nil {
		return err
	}
	if err := checkFabric(g.Rank(), g.Admissions()); err != nil {
		return err
	}
	return g.Barrier(ctx, group.Admitted)
}

// checkGroupArtifact refuses an artifact that is not member's part, at rank,
// of a group capture whose number of members is members, in the version of
// its manifest this agent restores, or whose fabric peers are not other
// members' ranks.
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
	for _, peer := range captured.FabricPeers {
		if peer < 0 || peer >= members || peer == rank {
			return fmt.Errorf("the artifact lists rank %d as a member it shared GPU memory with, "+
				"which is not another member's rank", peer)
		}
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
