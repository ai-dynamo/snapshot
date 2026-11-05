//go:build integration

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package crdinstaller_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	snapshotv1 "github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/ai-dynamo/snapshot/api/v1alpha1/crds"
)

const podGroupNamespace = "default"

// statusCase writes a group's member statuses twice through /status: the first
// write, with before applied, must be accepted; the second, with after
// applied, is accepted when refusal is empty and refused with it otherwise.
type statusCase[M any] struct {
	name    string
	before  func(*[]M)
	after   func(*[]M)
	refusal string
}

func TestPodGroupSnapshotAdmission(t *testing.T) {
	cl := startPodGroupEnv(t)

	for _, tc := range []struct {
		name    string
		change  func(*snapshotv1.PodGroupSnapshotSpec)
		refusal string
	}{
		{"one-member-without-template", nil, ""},
		{"256-members-at-maximum-lengths", func(s *snapshotv1.PodGroupSnapshotSpec) {
			s.Members = longestSnapshotMembers()
		}, ""},
		{"template-with-both-settings", func(s *snapshotv1.PodGroupSnapshotSpec) {
			s.Members[0].PodSnapshotTemplate = &snapshotv1.PodGroupMemberTemplate{
				Containers: []snapshotv1.ContainerSnapshotConfig{
					config(fileProbe("/tmp/ready"), httpPostRestore("/resume", intstr.FromInt32(8000))),
				},
			}
		}, ""},
		{"template-with-labels-and-annotations", func(s *snapshotv1.PodGroupSnapshotSpec) {
			s.Members[0].PodSnapshotTemplate = &snapshotv1.PodGroupMemberTemplate{
				Metadata: &snapshotv1.PodSnapshotTemplateMetadata{
					Labels:      map[string]string{"example.com/team": "inference"},
					Annotations: map[string]string{"example.com/role": "head"},
				},
			}
		}, ""},
		{"template-names-another-container", func(s *snapshotv1.PodGroupSnapshotSpec) {
			s.Members[0].PodSnapshotTemplate = &snapshotv1.PodGroupMemberTemplate{
				Containers: []snapshotv1.ContainerSnapshotConfig{{Name: "sidecar"}},
			}
		}, "podSnapshotTemplate.containers must name containers in source.podRef.containers"},
		{"template-sets-group-label", func(s *snapshotv1.PodGroupSnapshotSpec) {
			s.Members[0].PodSnapshotTemplate = templateWithLabel(snapshotv1.PodGroupLabel)
		}, "metadata.labels must not set controller-owned label " + snapshotv1.PodGroupLabel},
		{"template-sets-member-label", func(s *snapshotv1.PodGroupSnapshotSpec) {
			s.Members[0].PodSnapshotTemplate = templateWithLabel(snapshotv1.PodGroupMemberLabel)
		}, "metadata.labels must not set controller-owned label " + snapshotv1.PodGroupMemberLabel},
		{"template-sets-snapshotjob-label", func(s *snapshotv1.PodGroupSnapshotSpec) {
			s.Members[0].PodSnapshotTemplate = templateWithLabel(snapshotv1.SnapshotJobOwnerLabel)
		}, "metadata.labels must not set controller-owned label " + snapshotv1.SnapshotJobOwnerLabel},
		{"template-sets-snapshotjob-uid-label", func(s *snapshotv1.PodGroupSnapshotSpec) {
			s.Members[0].PodSnapshotTemplate = templateWithLabel(snapshotv1.SnapshotJobOwnerUIDLabel)
		}, "metadata.labels must not set controller-owned label " + snapshotv1.SnapshotJobOwnerUIDLabel},
		{"no-members", func(s *snapshotv1.PodGroupSnapshotSpec) { s.Members = nil }, "spec.members: Required value"},
		{"257-members", func(s *snapshotv1.PodGroupSnapshotSpec) {
			s.Members = snapshotMembers(257)
		}, "must have at most 256 items"},
		{"duplicate-ids", func(s *snapshotv1.PodGroupSnapshotSpec) {
			s.Members = snapshotMembers(2)
			s.Members[1].ID = s.Members[0].ID
		}, "member IDs must be unique"},
		{"id-not-a-dns-label", func(s *snapshotv1.PodGroupSnapshotSpec) {
			s.Members[0].ID = "Worker_0"
		}, "spec.members[0].id in body should match"},
		{"source-without-uid", func(s *snapshotv1.PodGroupSnapshotSpec) {
			s.Members[0].Source.PodRef.UID = ""
		}, "member sources must name the Pod UID"},
		{"zero-deadline", func(s *snapshotv1.PodGroupSnapshotSpec) {
			s.DeadlineSeconds = 0
		}, "spec.deadlineSeconds in body should be greater than or equal to 1"},
		{"deadline-of-a-day", func(s *snapshotv1.PodGroupSnapshotSpec) { s.DeadlineSeconds = 86400 }, ""},
		{"deadline-over-a-day", func(s *snapshotv1.PodGroupSnapshotSpec) {
			s.DeadlineSeconds = 86401
		}, "spec.deadlineSeconds in body should be less than or equal to 86400"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := newPodGroupSnapshot(tc.name, 1)
			if tc.change != nil {
				tc.change(&snapshot.Spec)
			}
			requireAdmission(t, cl.Create(t.Context(), snapshot), tc.refusal)
		})
	}

	t.Run("name-fits-a-label-value", func(t *testing.T) {
		require.NoError(t, cl.Create(t.Context(), newPodGroupSnapshot(pad("long-name", 63), 1)))
		requireAdmission(t, cl.Create(t.Context(), newPodGroupSnapshot(pad("longer-name", 64), 1)),
			"metadata.name must be no more than 63 characters")
	})

	t.Run("spec-is-immutable", func(t *testing.T) {
		snapshot := newPodGroupSnapshot("immutable", 2)
		require.NoError(t, cl.Create(t.Context(), snapshot))
		snapshot.Spec.DeadlineSeconds++
		requireAdmission(t, cl.Update(t.Context(), snapshot), "spec is immutable")
	})

	// A member's position is its rank, so reordering the members is a change
	// to the spec even though every member is still there.
	t.Run("members-cannot-be-reordered", func(t *testing.T) {
		snapshot := newPodGroupSnapshot("reorder", 2)
		require.NoError(t, cl.Create(t.Context(), snapshot))
		snapshot.Spec.Members[0], snapshot.Spec.Members[1] = snapshot.Spec.Members[1], snapshot.Spec.Members[0]
		requireAdmission(t, cl.Update(t.Context(), snapshot), "spec is immutable")
	})

	// The status rules compare every entry with every member, so the largest
	// group must stay within the API server's runtime cost limits. Its 256
	// messages, each at the limit, must also fit in one object.
	t.Run("status-256-entries-at-maximum-lengths", func(t *testing.T) {
		snapshot := newPodGroupSnapshot("status-256-entries", 0)
		snapshot.Spec.Members = longestSnapshotMembers()
		require.NoError(t, cl.Create(t.Context(), snapshot))
		for i, m := range snapshot.Spec.Members {
			snapshot.Status.Members = append(snapshot.Status.Members,
				snapshotv1.PodGroupSnapshotMemberStatus{ID: m.ID, Rank: int32(i)})
		}
		require.NoError(t, cl.Status().Update(t.Context(), snapshot))
		for i := range snapshot.Status.Members {
			snapshot.Status.Members[i].Phase = snapshotv1.CheckpointPhaseAdmitted
			snapshot.Status.Members[i].Message = strings.Repeat("x", 1024)
		}
		require.NoError(t, cl.Status().Update(t.Context(), snapshot))
	})

	t.Run("phase-defaults-to-pending", func(t *testing.T) {
		snapshot := newPodGroupSnapshot("phase-default", 1)
		require.NoError(t, cl.Create(t.Context(), snapshot))
		snapshot.Status.Members = []snapshotv1.PodGroupSnapshotMemberStatus{{ID: "worker-0"}}
		require.NoError(t, cl.Status().Update(t.Context(), snapshot))
		stored := &snapshotv1.PodGroupSnapshot{}
		require.NoError(t, cl.Get(t.Context(), client.ObjectKeyFromObject(snapshot), stored))
		assert.Equal(t, snapshotv1.CheckpointPhasePending, stored.Status.Members[0].Phase)
	})

	type member = snapshotv1.PodGroupSnapshotMemberStatus
	setPhase := func(phase snapshotv1.CheckpointPhase) func(*[]member) {
		return func(ms *[]member) { (*ms)[0].Phase = phase }
	}
	const finalCheckpoint = "Checkpointed and Failed are final phases"
	cases := []statusCase[member]{
		{"undeclared-id", nil, func(ms *[]member) {
			*ms = append(*ms, member{ID: "worker-9"})
		}, "status.members rank must be the member's position in spec.members"},
		{"remove-an-entry", nil, func(ms *[]member) { *ms = (*ms)[:1] }, "status.members entries cannot be removed"},
		{"rank-not-its-position", nil, func(ms *[]member) {
			(*ms)[0].Rank = 1
		}, "status.members rank must be the member's position in spec.members"},
		{"rank-out-of-range", nil, func(ms *[]member) {
			(*ms)[1].Rank = 2
		}, "status.members rank must be the member's position in spec.members"},
		{"remove-every-entry", nil, func(ms *[]member) { *ms = nil }, "status.members entries cannot be removed"},
		{"pending-can-advance", nil, setPhase(snapshotv1.CheckpointPhaseAdmitted), ""},
		{"checkpointed-is-final", setPhase(snapshotv1.CheckpointPhaseCheckpointed), setPhase(snapshotv1.CheckpointPhaseFailed), finalCheckpoint},
		{"failed-is-final", setPhase(snapshotv1.CheckpointPhaseFailed), setPhase(snapshotv1.CheckpointPhasePending), finalCheckpoint},
		{"reason-not-camelcase", nil, func(ms *[]member) {
			(*ms)[0].Reason = "member failed"
		}, "members[0].reason in body should match"},
		{"message-at-the-limit", nil, func(ms *[]member) {
			(*ms)[0].Message = strings.Repeat("x", 1024)
		}, ""},
		{"message-too-long", nil, func(ms *[]member) {
			(*ms)[0].Message = strings.Repeat("x", 1025)
		}, "status.members[0].message: Too long"},
		{"node-name-too-long", nil, func(ms *[]member) {
			(*ms)[0].NodeName = strings.Repeat("x", 254)
		}, "status.members[0].nodeName: Too long"},
	}
	cases = append(cases, writeOnceCases("podSnapshot",
		func(m *member, v int) { m.PodSnapshot = objectIdentity("pod-snapshot", v) },
		func(m *member) { m.PodSnapshot = nil })...)
	cases = append(cases, writeOnceCases("content",
		func(m *member, v int) { m.Content = objectIdentity("content", v) },
		func(m *member) { m.Content = nil })...)
	cases = append(cases, writeOnceCases("startedAt",
		func(m *member, v int) { m.StartedAt = startedAt(v) },
		func(m *member) { m.StartedAt = nil })...)
	for _, tc := range cases {
		t.Run("status-"+tc.name, func(t *testing.T) {
			snapshot := newPodGroupSnapshot(strings.ToLower("status-"+tc.name), 2)
			require.NoError(t, cl.Create(t.Context(), snapshot))
			for i, m := range snapshot.Spec.Members {
				snapshot.Status.Members = append(snapshot.Status.Members, member{ID: m.ID, Rank: int32(i)})
			}
			if tc.before != nil {
				tc.before(&snapshot.Status.Members)
			}
			require.NoError(t, cl.Status().Update(t.Context(), snapshot))
			tc.after(&snapshot.Status.Members)
			requireAdmission(t, cl.Status().Update(t.Context(), snapshot), tc.refusal)
		})
	}
}

// TestTemplatesReserveEachOthersOwnershipLabels checks that neither kind's
// PodSnapshot template can set the labels the other kind's controller selects
// its PodSnapshots by.
func TestTemplatesReserveEachOthersOwnershipLabels(t *testing.T) {
	cl := startPodGroupEnv(t)
	for _, tc := range []struct {
		name, label string
		refused     bool
	}{
		{"group", snapshotv1.PodGroupLabel, true},
		{"group-member", snapshotv1.PodGroupMemberLabel, true},
		{"job", snapshotv1.SnapshotJobOwnerLabel, true},
		{"job-uid", snapshotv1.SnapshotJobOwnerUIDLabel, true},
		{"another", "example.com/team", false},
	} {
		t.Run("snapshotjob-"+tc.name, func(t *testing.T) {
			job := newSettingsJob("label-"+tc.name, snapshotv1.ContainerSnapshotConfig{Name: "main"})
			job.Namespace = podGroupNamespace
			job.Spec.PodSnapshotTemplate.Metadata = &snapshotv1.PodSnapshotTemplateMetadata{
				Labels: map[string]string{tc.label: "x"},
			}
			refusal := ""
			if tc.refused {
				refusal = "metadata.labels must not set controller-owned label " + tc.label
			}
			requireAdmission(t, cl.Create(t.Context(), job), refusal)
		})
	}
}

func TestPodGroupRestoreAdmission(t *testing.T) {
	cl := startPodGroupEnv(t)

	for _, tc := range []struct {
		name    string
		change  func(*snapshotv1.PodGroupRestoreSpec)
		refusal string
	}{
		{"target-without-uid-or-mappings", nil, ""},
		{"one-container-mapping", func(s *snapshotv1.PodGroupRestoreSpec) {
			s.Targets[0].ContainerMappings = []snapshotv1.ContainerMapping{{Source: "main", Destination: "server"}}
		}, ""},
		{"two-container-mappings", func(s *snapshotv1.PodGroupRestoreSpec) {
			s.Targets[0].ContainerMappings = []snapshotv1.ContainerMapping{
				{Source: "main", Destination: "server"},
				{Source: "helper", Destination: "helper"},
			}
		}, "spec.targets[0].containerMappings: Too many: 2: must have at most 1 item"},
		{"256-targets-at-maximum-lengths", func(s *snapshotv1.PodGroupRestoreSpec) {
			s.Targets = longestRestoreTargets()
		}, ""},
		{"no-targets", func(s *snapshotv1.PodGroupRestoreSpec) { s.Targets = nil }, "spec.targets: Required value"},
		{"257-targets", func(s *snapshotv1.PodGroupRestoreSpec) {
			s.Targets = restoreTargets(257)
		}, "must have at most 256 items"},
		{"duplicate-ids", func(s *snapshotv1.PodGroupRestoreSpec) {
			s.Targets = restoreTargets(2)
			s.Targets[1].ID = s.Targets[0].ID
		}, "spec.targets[1]: Duplicate value"},
		{"id-not-a-dns-label", func(s *snapshotv1.PodGroupRestoreSpec) {
			s.Targets[0].ID = "Worker_0"
		}, "spec.targets[0].id in body should match"},
		{"snapshot-ref-without-uid", func(s *snapshotv1.PodGroupRestoreSpec) {
			s.SnapshotRef.UID = ""
		}, "spec.snapshotRef.uid in body should be at least 1 chars long"},
		{"destination-not-a-dns-label", func(s *snapshotv1.PodGroupRestoreSpec) {
			s.Targets[0].ContainerMappings = []snapshotv1.ContainerMapping{{Source: "main", Destination: "Main"}}
		}, "spec.targets[0].containerMappings[0].destination in body should match"},
		{"zero-deadline", func(s *snapshotv1.PodGroupRestoreSpec) {
			s.DeadlineSeconds = 0
		}, "spec.deadlineSeconds in body should be greater than or equal to 1"},
		{"deadline-of-a-day", func(s *snapshotv1.PodGroupRestoreSpec) { s.DeadlineSeconds = 86400 }, ""},
		{"deadline-over-a-day", func(s *snapshotv1.PodGroupRestoreSpec) {
			s.DeadlineSeconds = 86401
		}, "spec.deadlineSeconds in body should be less than or equal to 86400"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restore := newPodGroupRestore(tc.name, 1)
			if tc.change != nil {
				tc.change(&restore.Spec)
			}
			requireAdmission(t, cl.Create(t.Context(), restore), tc.refusal)
		})
	}

	t.Run("spec-is-immutable", func(t *testing.T) {
		restore := newPodGroupRestore("immutable", 2)
		require.NoError(t, cl.Create(t.Context(), restore))
		restore.Spec.Targets[0].PodRef.UID = "pinned-by-the-caller"
		requireAdmission(t, cl.Update(t.Context(), restore), "spec is immutable")
	})

	t.Run("status-256-entries-at-maximum-lengths", func(t *testing.T) {
		restore := newPodGroupRestore("status-256-entries", 0)
		restore.Spec.Targets = longestRestoreTargets()
		require.NoError(t, cl.Create(t.Context(), restore))
		for _, target := range restore.Spec.Targets {
			restore.Status.Members = append(restore.Status.Members, snapshotv1.PodGroupRestoreMemberStatus{ID: target.ID})
		}
		require.NoError(t, cl.Status().Update(t.Context(), restore))
		for i := range restore.Status.Members {
			restore.Status.Members[i].Phase = snapshotv1.RestorePhaseAdmitted
			restore.Status.Members[i].Message = strings.Repeat("x", 1024)
		}
		require.NoError(t, cl.Status().Update(t.Context(), restore))
	})

	t.Run("phase-defaults-to-pending", func(t *testing.T) {
		restore := newPodGroupRestore("phase-default", 1)
		require.NoError(t, cl.Create(t.Context(), restore))
		restore.Status.Members = []snapshotv1.PodGroupRestoreMemberStatus{{ID: "worker-0"}}
		require.NoError(t, cl.Status().Update(t.Context(), restore))
		stored := &snapshotv1.PodGroupRestore{}
		require.NoError(t, cl.Get(t.Context(), client.ObjectKeyFromObject(restore), stored))
		assert.Equal(t, snapshotv1.RestorePhasePending, stored.Status.Members[0].Phase)
	})

	type member = snapshotv1.PodGroupRestoreMemberStatus
	setPhase := func(phase snapshotv1.RestorePhase) func(*[]member) {
		return func(ms *[]member) { (*ms)[0].Phase = phase }
	}
	setIP := func(ip string) func(*[]member) {
		return func(ms *[]member) { (*ms)[0].TargetPodIP = ip }
	}
	const finalRestore = "Released and Failed are final phases"
	const notIPv4 = "members[0].targetPodIP in body must be of type ipv4"
	cases := []statusCase[member]{
		{"undeclared-id", nil, func(ms *[]member) {
			*ms = append(*ms, member{ID: "worker-9"})
		}, "status.members must name targets of spec.targets"},
		{"remove-an-entry", nil, func(ms *[]member) { *ms = (*ms)[:1] }, "status.members entries cannot be removed"},
		{"remove-every-entry", nil, func(ms *[]member) { *ms = nil }, "status.members entries cannot be removed"},
		{"pending-can-advance", nil, setPhase(snapshotv1.RestorePhaseAdmitted), ""},
		{"released-is-final", setPhase(snapshotv1.RestorePhaseReleased), setPhase(snapshotv1.RestorePhaseFailed), finalRestore},
		{"failed-is-final", setPhase(snapshotv1.RestorePhaseFailed), setPhase(snapshotv1.RestorePhaseRestoring), finalRestore},
		{"target-pod-ip-ipv6", nil, setIP("fd00::1"), notIPv4},
		{"target-pod-ip-not-an-ip", nil, setIP("node-a"), notIPv4},
		{"reason-not-camelcase", nil, func(ms *[]member) {
			(*ms)[0].Reason = "member failed"
		}, "members[0].reason in body should match"},
		{"message-at-the-limit", nil, func(ms *[]member) {
			(*ms)[0].Message = strings.Repeat("x", 1024)
		}, ""},
		{"message-too-long", nil, func(ms *[]member) {
			(*ms)[0].Message = strings.Repeat("x", 1025)
		}, "status.members[0].message: Too long"},
		{"two-containers", nil, func(ms *[]member) {
			(*ms)[0].Containers = []snapshotv1.PodGroupRestoreContainerStatus{
				{Name: "server", ContainerID: "containerd://1"},
				{Name: "helper", ContainerID: "containerd://2"},
			}
		}, "status.members[0].containers: Too many: 2: must have at most 1 item"},
	}
	cases = append(cases, writeOnceCases("pod",
		func(m *member, v int) { m.Pod = objectIdentity("pod", v) },
		func(m *member) { m.Pod = nil })...)
	cases = append(cases, writeOnceCases("nodeName",
		func(m *member, v int) { m.NodeName = fmt.Sprintf("node-%d", v) },
		func(m *member) { m.NodeName = "" })...)
	cases = append(cases, writeOnceCases("containers",
		func(m *member, v int) {
			m.Containers = []snapshotv1.PodGroupRestoreContainerStatus{
				{Name: "server", ContainerID: fmt.Sprintf("containerd://%d", v)},
			}
		},
		func(m *member) { m.Containers = nil })...)
	cases = append(cases, writeOnceCases("targetPodIP",
		func(m *member, v int) { m.TargetPodIP = fmt.Sprintf("10.0.0.%d", v) },
		func(m *member) { m.TargetPodIP = "" })...)
	cases = append(cases, writeOnceCases("startedAt",
		func(m *member, v int) { m.StartedAt = startedAt(v) },
		func(m *member) { m.StartedAt = nil })...)
	for _, tc := range cases {
		t.Run("status-"+tc.name, func(t *testing.T) {
			restore := newPodGroupRestore(strings.ToLower("status-"+tc.name), 2)
			require.NoError(t, cl.Create(t.Context(), restore))
			for _, target := range restore.Spec.Targets {
				restore.Status.Members = append(restore.Status.Members, member{ID: target.ID})
			}
			if tc.before != nil {
				tc.before(&restore.Status.Members)
			}
			require.NoError(t, cl.Status().Update(t.Context(), restore))
			tc.after(&restore.Status.Members)
			requireAdmission(t, cl.Status().Update(t.Context(), restore), tc.refusal)
		})
	}
}

// startPodGroupEnv installs every embedded CRD, as the crd-installer does. The
// install is itself the CEL cost check: the API server refuses a CRD whose
// rules could exceed the cost budget.
func startPodGroupEnv(t *testing.T) client.Client {
	t.Helper()
	var definitions []*apiextensionsv1.CustomResourceDefinition
	for _, manifest := range crds.All() {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		require.NoError(t, yaml.Unmarshal([]byte(manifest), crd))
		definitions = append(definitions, crd)
	}
	env := &envtest.Environment{CRDs: definitions}
	t.Cleanup(func() { assert.NoError(t, env.Stop()) })
	cfg, err := env.Start()
	require.NoError(t, err, "install the embedded CRDs")
	scheme := runtime.NewScheme()
	require.NoError(t, snapshotv1.AddToScheme(scheme))
	cl, err := client.New(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)
	return cl
}

// writeOnceCases checks that set can give a member field a value once, and
// that neither changing nor clearing it afterwards is admitted.
func writeOnceCases[M any](field string, set func(m *M, value int), clear func(m *M)) []statusCase[M] {
	first := func(ms *[]M) { set(&(*ms)[0], 1) }
	refusal := field + " is write-once"
	return []statusCase[M]{
		{field + "-set", nil, first, ""},
		{field + "-change", first, func(ms *[]M) { set(&(*ms)[0], 2) }, refusal},
		{field + "-clear", first, func(ms *[]M) { clear(&(*ms)[0]) }, refusal},
	}
}

// requireAdmission requires err to admit the write when refusal is empty, and
// otherwise to refuse it as Invalid with a message containing refusal.
func requireAdmission(t *testing.T, err error, refusal string) {
	t.Helper()
	if refusal == "" {
		require.NoError(t, err)
		return
	}
	require.True(t, apierrors.IsInvalid(err), "want Invalid, got %v", err)
	assert.Contains(t, err.Error(), refusal)
}

func newPodGroupSnapshot(name string, members int) *snapshotv1.PodGroupSnapshot {
	return &snapshotv1.PodGroupSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: podGroupNamespace},
		Spec:       snapshotv1.PodGroupSnapshotSpec{DeadlineSeconds: 1800, Members: snapshotMembers(members)},
	}
}

func snapshotMembers(n int) []snapshotv1.PodGroupSnapshotMember {
	members := make([]snapshotv1.PodGroupSnapshotMember, n)
	for i := range members {
		members[i] = snapshotv1.PodGroupSnapshotMember{
			ID: fmt.Sprintf("worker-%d", i),
			Source: snapshotv1.PodSnapshotSource{PodRef: snapshotv1.PodReference{
				Name:       fmt.Sprintf("llm-worker-%d", i),
				UID:        types.UID(fmt.Sprintf("source-uid-%d", i)),
				Containers: []string{"main"},
			}},
		}
	}
	return members
}

// longestSnapshotMembers is the most costly spec a PodGroupSnapshot admits:
// 256 members, with IDs and Pod names at their maximum lengths, and each with
// a template carrying both settings, the probe's file path at its maximum.
func longestSnapshotMembers() []snapshotv1.PodGroupSnapshotMember {
	members := snapshotMembers(256)
	for i := range members {
		members[i].ID = pad(members[i].ID, 63)
		members[i].Source.PodRef.Name = pad(members[i].Source.PodRef.Name, 253)
		members[i].PodSnapshotTemplate = &snapshotv1.PodGroupMemberTemplate{
			Metadata: &snapshotv1.PodSnapshotTemplateMetadata{Labels: map[string]string{"example.com/team": "inference"}},
			Containers: []snapshotv1.ContainerSnapshotConfig{config(
				fileProbe(pad("/ready", 4096)),
				httpPostRestore("/resume", intstr.FromString("abcdefghijklmno")),
			)},
		}
	}
	return members
}

func templateWithLabel(label string) *snapshotv1.PodGroupMemberTemplate {
	return &snapshotv1.PodGroupMemberTemplate{
		Metadata: &snapshotv1.PodSnapshotTemplateMetadata{Labels: map[string]string{label: "x"}},
	}
}

func newPodGroupRestore(name string, targets int) *snapshotv1.PodGroupRestore {
	return &snapshotv1.PodGroupRestore{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: podGroupNamespace},
		Spec: snapshotv1.PodGroupRestoreSpec{
			SnapshotRef:     snapshotv1.ObjectIdentity{Name: "llm-tp8", UID: "snapshot-uid"},
			DeadlineSeconds: 1500,
			Targets:         restoreTargets(targets),
		},
	}
}

func restoreTargets(n int) []snapshotv1.PodGroupRestoreTarget {
	targets := make([]snapshotv1.PodGroupRestoreTarget, n)
	for i := range targets {
		targets[i] = snapshotv1.PodGroupRestoreTarget{
			ID:     fmt.Sprintf("worker-%d", i),
			PodRef: snapshotv1.TargetPodReference{Name: fmt.Sprintf("llm-worker-%d-r1", i)},
		}
	}
	return targets
}

// longestRestoreTargets is the largest spec a PodGroupRestore admits: 256
// targets, with IDs and Pod names at their maximum lengths.
func longestRestoreTargets() []snapshotv1.PodGroupRestoreTarget {
	targets := restoreTargets(256)
	for i := range targets {
		targets[i].ID = pad(targets[i].ID, 63)
		targets[i].PodRef.Name = pad(targets[i].PodRef.Name, 253)
	}
	return targets
}

func objectIdentity(name string, v int) *snapshotv1.ObjectIdentity {
	return &snapshotv1.ObjectIdentity{
		Name: fmt.Sprintf("%s-%d", name, v),
		UID:  types.UID(fmt.Sprintf("%s-uid-%d", name, v)),
	}
}

func startedAt(v int) *metav1.Time {
	t := metav1.NewTime(time.Date(2026, time.October, 16, 18, 2, v, 0, time.UTC))
	return &t
}

// pad lengthens s to n characters without leaving its DNS-label or path syntax.
func pad(s string, n int) string {
	return s + strings.Repeat("x", n-len(s))
}
