//go:build integration

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package groups

import (
	"context"
	"crypto/tls"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/ai-dynamo/snapshot/coordinator"
	"github.com/ai-dynamo/snapshot/coordinator/agentauth"
	"github.com/ai-dynamo/snapshot/coordinator/coordinatortest"
	"github.com/ai-dynamo/snapshot/coordinator/internal/testcert"
	"github.com/ai-dynamo/snapshot/coordinator/server"
	"github.com/ai-dynamo/snapshot/coordinator/session"
	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	clocktesting "k8s.io/utils/clock/testing"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// The tests run each node's coordinator, from coordinatortest, and a manager
// running Setup for it, against one envtest API server, which no operator
// runs against: each test writes the group resources' status itself. Every
// test names its own nodes, so no host serves another test's attempts.

var (
	restConfig *rest.Config
	scheme     = runtime.NewScheme()
	k8s        client.Client
)

// timeout bounds every wait for the hosts to act. A final attempt's session is
// looked at again every few seconds, which the longest waits cover.
const timeout = 20 * time.Second

func TestMain(m *testing.M) {
	ctrllog.SetLogger(logr.Discard())
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		panic(err)
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "api", "v1alpha1", "crds")},
		ErrorIfCRDPathMissing: true,
	}
	var err error
	if restConfig, err = env.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "start envtest: %v\n", err)
		os.Exit(1)
	}
	if k8s, err = client.New(restConfig, client.Options{Scheme: scheme}); err != nil {
		fmt.Fprintf(os.Stderr, "create the test client: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	if err := env.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "stop envtest: %v\n", err)
	}
	os.Exit(code)
}

// host is one node's coordinator, with Setup keeping its sessions.
type host struct {
	node     string
	coord    *coordinatortest.Coordinator
	clock    *clocktesting.FakePassiveClock
	cache    client.Reader
	attempts *Attempts
}

// startHost runs NewAttempts and Setup for coord, the coordinator of node, in
// a manager of its own, configured by modify, with a fake clock that starts
// now, and stops the manager when the test ends.
func startHost(t *testing.T, node string, coord *coordinatortest.Coordinator,
	modify ...func(*manager.Options)) *host {
	t.Helper()
	mgr := newManager(t, modify...)
	h := &host{node: node, coord: coord, clock: clocktesting.NewFakePassiveClock(time.Now()), cache: mgr.GetCache()}
	var err error
	h.attempts, err = NewAttempts(context.Background(), mgr)
	require.NoError(t, err)
	require.NoError(t, Setup(mgr, h.attempts, Config{Server: coord.Server, Node: node, Clock: h.clock}))
	run(t, mgr)
	return h
}

// newManager returns a manager of the test's own, configured by modify.
func newManager(t *testing.T, modify ...func(*manager.Options)) manager.Manager {
	t.Helper()
	opts := manager.Options{
		Scheme:  scheme,
		Logger:  logr.Discard(),
		Metrics: metricsserver.Options{BindAddress: "0"},
		// Every host runs the same controller in one process.
		Controller: config.Controller{SkipNameValidation: ptr.To(true)},
	}
	for _, m := range modify {
		m(&opts)
	}
	mgr, err := manager.New(restConfig, opts)
	require.NoError(t, err)
	return mgr
}

// run starts mgr, and stops it when the test ends.
func run(t *testing.T, mgr manager.Manager) {
	t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	t.Cleanup(func() {
		stop()
		require.NoError(t, <-done)
	})
}

// nodes returns n node names of the test's own.
func nodes(t *testing.T, n int) []string {
	name := strings.ToLower(regexp.MustCompile(`[^A-Za-z0-9]+`).ReplaceAllString(t.Name(), "-"))
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s-%d", name, i)
	}
	return out
}

// member is one member of a test's group: its ID and its node.
type member struct{ id, node string }

// createCapture creates a PodGroupSnapshot of members, in rank order, whose
// member PodSnapshots are pinned, so its attempt has member work, unless
// pinned is false. With finalizer, deleting it leaves it until the test
// removes the in-use finalizer.
func createCapture(t *testing.T, members []member, pinned, finalizer bool) *v1alpha1.PodGroupSnapshot {
	t.Helper()
	ctx := within(t)
	g := &v1alpha1.PodGroupSnapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", GenerateName: "group-"},
		Spec:       v1alpha1.PodGroupSnapshotSpec{DeadlineSeconds: 600},
	}
	if finalizer {
		g.Finalizers = []string{v1alpha1.PodGroupInUseFinalizer}
	}
	for _, m := range members {
		g.Spec.Members = append(g.Spec.Members, v1alpha1.PodGroupSnapshotMember{ID: m.id,
			Source: v1alpha1.PodSnapshotSource{PodRef: v1alpha1.PodReference{Name: "pod-" + m.id,
				UID: types.UID("pod-uid-" + m.id), Containers: []string{"main"}}}})
	}
	require.NoError(t, k8s.Create(ctx, g))
	for rank, m := range members {
		status := v1alpha1.PodGroupSnapshotMemberStatus{ID: m.id, Rank: int32(rank), NodeName: m.node,
			Phase: v1alpha1.CheckpointPhaseCheckpointing}
		if pinned {
			status.PodSnapshot = &v1alpha1.ObjectIdentity{Name: g.Name + "-" + m.id,
				UID: types.UID("podsnapshot-uid-" + m.id)}
		}
		g.Status.Members = append(g.Status.Members, status)
	}
	require.NoError(t, k8s.Status().Update(ctx, g))
	return g
}

// checkpointed makes every member of g Checkpointed, which makes its attempt
// final.
func checkpointed(t *testing.T, g *v1alpha1.PodGroupSnapshot) {
	t.Helper()
	for i := range g.Status.Members {
		g.Status.Members[i].Phase = v1alpha1.CheckpointPhaseCheckpointed
	}
	require.NoError(t, k8s.Status().Update(within(t), g))
}

// readySnapshot creates a PodGroupSnapshot of the member IDs ids, in rank
// order, and makes it Ready, as it is when the operator admits a restore of
// it. No node serves its own attempt.
func readySnapshot(t *testing.T, ids []string) *v1alpha1.PodGroupSnapshot {
	t.Helper()
	members := make([]member, len(ids))
	for i, id := range ids {
		members[i] = member{id: id}
	}
	g := createCapture(t, members, true, false)
	for i := range g.Status.Members {
		g.Status.Members[i].Phase = v1alpha1.CheckpointPhaseCheckpointed
	}
	meta.SetStatusCondition(&g.Status.Conditions, metav1.Condition{Type: v1alpha1.PodGroupConditionReady,
		Status: metav1.ConditionTrue, Reason: string(v1alpha1.CheckpointPhaseCheckpointed)})
	require.NoError(t, k8s.Status().Update(within(t), g))
	return g
}

// createRestore creates a PodGroupRestore of snapshot whose targets, listed in
// the order of members, are pinned to their nodes.
func createRestore(t *testing.T, snapshot *v1alpha1.PodGroupSnapshot, members []member) *v1alpha1.PodGroupRestore {
	t.Helper()
	ctx := within(t)
	r := &v1alpha1.PodGroupRestore{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", GenerateName: "restore-"},
		Spec: v1alpha1.PodGroupRestoreSpec{
			SnapshotRef:     v1alpha1.ObjectIdentity{Name: snapshot.Name, UID: snapshot.UID},
			DeadlineSeconds: 600,
		},
	}
	for _, m := range members {
		r.Spec.Targets = append(r.Spec.Targets, v1alpha1.PodGroupRestoreTarget{ID: m.id,
			PodRef: v1alpha1.TargetPodReference{Name: "restore-pod-" + m.id}})
	}
	require.NoError(t, k8s.Create(ctx, r))
	for _, m := range members {
		r.Status.Members = append(r.Status.Members, v1alpha1.PodGroupRestoreMemberStatus{ID: m.id,
			NodeName: m.node, Phase: v1alpha1.RestorePhaseAdmitted,
			Pod: &v1alpha1.ObjectIdentity{Name: "restore-pod-" + m.id, UID: types.UID("restore-pod-uid-" + m.id)}})
	}
	require.NoError(t, k8s.Status().Update(ctx, r))
	return r
}

// release removes the in-use finalizer from obj, so a deleted resource goes.
func release(t *testing.T, obj client.Object) {
	t.Helper()
	ctx := within(t)
	require.NoError(t, k8s.Get(ctx, client.ObjectKeyFromObject(obj), obj))
	patch := client.MergeFrom(obj.DeepCopyObject().(client.Object))
	obj.SetFinalizers(nil)
	require.NoError(t, k8s.Patch(ctx, obj, patch))
}

// awaitHeld waits until h's coordinator holds the session of uid, or no
// longer does.
func (h *host) awaitHeld(t *testing.T, uid types.UID, want bool) {
	t.Helper()
	require.Eventually(t, func() bool {
		_, holds := h.coord.Ended(uid)
		return holds == want
	}, timeout, 10*time.Millisecond, "the coordinator of %s holding the session of %s: want %t", h.node, uid, want)
}

// awaitSeen waits until h's cache shows what seen reports true of the attempt
// uid, so its reconciler has had the event that showed it.
func (h *host) awaitSeen(t *testing.T, uid types.UID, seen func(*attempt) bool) {
	t.Helper()
	require.Eventually(t, func() bool {
		a, err := h.attempts.get(context.Background(), uid)
		return err == nil && seen(a)
	}, timeout, 10*time.Millisecond)
}

// requireNotHeld fails the test if h's coordinator holds the session of uid
// within a moment.
func (h *host) requireNotHeld(t *testing.T, uid types.UID) {
	t.Helper()
	require.Never(t, func() bool {
		_, holds := h.coord.Ended(uid)
		return holds
	}, 300*time.Millisecond, 10*time.Millisecond, "the coordinator of %s must not hold the session of %s",
		h.node, uid)
}

// connect connects member id of the attempt uid to h's coordinator, and closes
// it when the test ends.
func (h *host) connect(t *testing.T, uid types.UID, id string) coordinator.Session {
	t.Helper()
	s, err := coordinator.Connect(within(t), h.coord.Config(uid, id))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// awaitEnd waits until s's session has ended, and returns how.
func awaitEnd(t *testing.T, s coordinator.Session) error {
	t.Helper()
	select {
	case <-s.Done():
		return s.Err()
	case <-time.After(timeout):
		t.Fatal("the session did not end")
		return nil
	}
}

// within returns a context the test bounds its calls with.
func within(t *testing.T) context.Context {
	ctx, stop := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(stop)
	return ctx
}

// TestSessionNode covers where a session lives: on the coordinator of the
// node of its attempt's rank-0 member, once member work exists, and nowhere
// else. A capture's ranks are its spec order; a restore's are its
// PodGroupSnapshot's.
func TestSessionNode(t *testing.T) {
	names := nodes(t, 2)
	hosts := []*host{startHost(t, names[0], coordinatortest.Start(t)), startHost(t, names[1], coordinatortest.Start(t))}

	t.Run("a capture", func(t *testing.T) {
		g := createCapture(t, []member{{"a", names[1]}, {"b", names[0]}}, true, false)
		hosts[1].awaitHeld(t, g.UID, true)
		hosts[0].awaitSeen(t, g.UID, func(a *attempt) bool { return a.memberWork })
		hosts[0].requireNotHeld(t, g.UID)
		assert.Equal(t, 1, hosts[1].connect(t, g.UID, "b").Rank())
	})

	t.Run("a restore", func(t *testing.T) {
		r := createRestore(t, readySnapshot(t, []string{"a", "b"}), []member{{"b", names[1]}, {"a", names[0]}})
		hosts[0].awaitHeld(t, r.UID, true)
		hosts[1].awaitSeen(t, r.UID, func(a *attempt) bool { return a.memberWork })
		hosts[1].requireNotHeld(t, r.UID)
		assert.Equal(t, 1, hosts[0].connect(t, r.UID, "b").Rank())
	})

	t.Run("no session before member work exists", func(t *testing.T) {
		g := createCapture(t, []member{{"a", names[0]}}, false, false)
		hosts[0].awaitSeen(t, g.UID, func(a *attempt) bool { return a.sessionNode == names[0] })
		hosts[0].requireNotHeld(t, g.UID)
	})
}

// TestHostOrder builds a host in the order the package doc gives, with the
// real agentauth.Authorizer reading the attempts as its Placement: an agent's
// Pod-bound token admits it for a member on its node, and only for one.
func TestHostOrder(t *testing.T) {
	ctx := within(t)
	names := nodes(t, 2)
	node := names[0]
	agents := types.NamespacedName{Namespace: "snapshot-agents", Name: "snapshot-agent"}
	const audience = "snapshot-coordinator"
	admin, err := kubernetes.NewForConfig(restConfig)
	require.NoError(t, err)
	_, err = admin.CoreV1().Namespaces().Create(ctx,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: agents.Namespace}}, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = admin.CoreV1().ServiceAccounts(agents.Namespace).Create(ctx,
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: agents.Name}}, metav1.CreateOptions{})
	require.NoError(t, err)
	agent, err := admin.CoreV1().Pods(agents.Namespace).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "snapshot-agent-0"},
		Spec: corev1.PodSpec{ServiceAccountName: agents.Name, NodeName: node,
			Containers: []corev1.Container{{Name: "agent", Image: "agent"}}},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	token, err := admin.CoreV1().ServiceAccounts(agents.Namespace).CreateToken(ctx, agents.Name,
		&authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{
			Audiences:         []string{audience},
			ExpirationSeconds: ptr.To[int64](600),
			BoundObjectRef: &authenticationv1.BoundObjectReference{
				Kind: "Pod", APIVersion: "v1", Name: agent.Name, UID: agent.UID,
			},
		}}, metav1.CreateOptions{})
	require.NoError(t, err)

	mgr := newManager(t)
	attempts, err := NewAttempts(ctx, mgr)
	require.NoError(t, err)
	auth, err := agentauth.New(ctx, agentauth.Config{
		Placement:      attempts,
		TokenReviews:   admin.AuthenticationV1().TokenReviews(),
		RESTConfig:     restConfig,
		Audience:       audience,
		ServiceAccount: agents,
	})
	require.NoError(t, err)
	roots, cert := testcert.New(t)
	srv, err := server.New(server.Config{
		Host:       "127.0.0.1",
		Port:       natsserver.RANDOM_PORT,
		TLS:        &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		Authorizer: auth,
	})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, srv.Shutdown(context.Background())) })
	require.NoError(t, Setup(mgr, attempts, Config{Server: srv, Node: node}))
	run(t, mgr)

	g := createCapture(t, []member{{"a", node}, {"b", names[1]}}, true, false)
	require.Eventually(t, func() bool {
		_, holds := srv.Ended(g.UID)
		return holds && srv.Ready()
	}, timeout, 10*time.Millisecond)
	cfg := coordinator.Config{
		URLs:       []string{"tls://" + srv.Addr().String()},
		RootCAs:    roots,
		ServerName: "localhost",
		Attempt:    string(g.UID),
		Member:     "a",
		Token:      func() (string, error) { return token.Status.Token, nil },
	}
	s, err := coordinator.Connect(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	assert.Equal(t, 0, s.Rank())

	cfg.Member = "b"
	refused, cancelRefused := context.WithTimeout(ctx, 2*time.Second)
	defer cancelRefused()
	_, err = coordinator.Connect(refused, cfg)
	require.ErrorIs(t, err, session.ErrForbidden, "the agent's token must not admit it for a member on another node")
}

// visibleLabel is the label a host that hideSnapshots configures waits for
// before its cache shows a PodGroupSnapshot.
const visibleLabel = "groups.coordinator.test/visible"

// hideSnapshots configures a host's cache to show a PodGroupSnapshot, to its
// reads and its events alike, only once the snapshot carries visibleLabel, as
// an informer that lags the others would.
func hideSnapshots(opts *manager.Options) {
	opts.Cache.ByObject = map[client.Object]cache.ByObject{
		&v1alpha1.PodGroupSnapshot{}: {Label: labels.SelectorFromSet(labels.Set{visibleLabel: "true"})},
	}
}

// showSnapshot labels snapshot with visibleLabel, which shows it to a host that
// hideSnapshots configures.
func showSnapshot(t *testing.T, snapshot *v1alpha1.PodGroupSnapshot) {
	t.Helper()
	patch := client.MergeFrom(snapshot.DeepCopy())
	snapshot.Labels = map[string]string{visibleLabel: "true"}
	require.NoError(t, k8s.Patch(within(t), snapshot, patch))
}

// TestRestoreAndSnapshotOrder covers a restore whose attempt reads its
// PodGroupSnapshot, which the host's cache may show after the restore, since
// client-go promises no order between informers. The snapshot's own event then
// reconciles the restore, with no further event of the restore's.
func TestRestoreAndSnapshotOrder(t *testing.T) {
	names := nodes(t, 2)
	members := func(node string) []member { return []member{{"a", node}, {"b", node}} }

	t.Run("the snapshot shows first", func(t *testing.T) {
		h := startHost(t, names[0], coordinatortest.Start(t))
		r := createRestore(t, readySnapshot(t, []string{"a", "b"}), members(names[0]))
		h.awaitHeld(t, r.UID, true)
	})

	t.Run("the restore shows first", func(t *testing.T) {
		h := startHost(t, names[1], coordinatortest.Start(t), hideSnapshots)
		snapshot := readySnapshot(t, []string{"a", "b"})
		r := createRestore(t, snapshot, members(names[1]))
		require.Eventually(t, func() bool {
			var cached v1alpha1.PodGroupRestore
			err := h.cache.Get(context.Background(), client.ObjectKeyFromObject(r), &cached)
			return err == nil && len(cached.Status.Members) > 0 && cached.Status.Members[0].Pod != nil
		}, timeout, 10*time.Millisecond, "the host's cache must show the pinned restore")
		_, err := h.attempts.get(context.Background(), r.UID)
		require.ErrorIs(t, err, session.ErrNotFound, "the host's cache must hide the snapshot")
		h.requireNotHeld(t, r.UID)

		showSnapshot(t, snapshot)
		h.awaitHeld(t, r.UID, true)
	})
}

// TestDeletion covers a group resource whose deletion starts: its session is
// cancelled before every member has arrived at the admitted barrier, kept
// open afterwards, and removed once the resource is gone.
func TestDeletion(t *testing.T) {
	names := nodes(t, 1)
	h := startHost(t, names[0], coordinatortest.Start(t))
	deleting := func(a *attempt) bool { return a.deleting }

	t.Run("before the admitted barrier completes, the session is cancelled", func(t *testing.T) {
		ctx := within(t)
		g := createCapture(t, []member{{"a", names[0]}, {"b", names[0]}}, true, true)
		h.awaitHeld(t, g.UID, true)
		a, b := h.connect(t, g.UID, "a"), h.connect(t, g.UID, "b")
		arrived := make(chan error, 1)
		go func() { arrived <- a.Barrier(ctx, session.BarrierAdmitted) }()
		require.NoError(t, k8s.Delete(ctx, g))
		require.ErrorIs(t, <-arrived, &session.AbortError{Reason: session.ReasonCancelled})
		require.ErrorIs(t, awaitEnd(t, b), &session.AbortError{Reason: session.ReasonCancelled})

		release(t, g)
		h.awaitHeld(t, g.UID, false)
	})

	t.Run("once it has completed, the session stays open", func(t *testing.T) {
		ctx := within(t)
		g := createCapture(t, []member{{"a", names[0]}, {"b", names[0]}}, true, true)
		h.awaitHeld(t, g.UID, true)
		a, b := h.connect(t, g.UID, "a"), h.connect(t, g.UID, "b")
		arrived := make(chan error, 1)
		go func() { arrived <- a.Barrier(ctx, session.BarrierAdmitted) }()
		require.NoError(t, b.Barrier(ctx, session.BarrierAdmitted))
		require.NoError(t, <-arrived)
		require.NoError(t, k8s.Delete(ctx, g))
		h.awaitSeen(t, g.UID, deleting)
		require.Never(t, func() bool {
			ended, _ := h.coord.Ended(g.UID)
			return ended
		}, 500*time.Millisecond, 10*time.Millisecond, "a committed session must stay open")

		release(t, g)
		h.awaitHeld(t, g.UID, false)
		require.Error(t, awaitEnd(t, a), "a member learns that its session is gone")
	})
}

// TestRemoval covers removing the session of a group resource that is gone,
// including when another resource takes its name.
func TestRemoval(t *testing.T) {
	names := nodes(t, 1)
	h := startHost(t, names[0], coordinatortest.Start(t))

	t.Run("a deleted resource's session is removed", func(t *testing.T) {
		g := createCapture(t, []member{{"a", names[0]}}, true, false)
		h.awaitHeld(t, g.UID, true)
		require.NoError(t, k8s.Delete(within(t), g))
		h.awaitHeld(t, g.UID, false)
	})

	t.Run("another resource takes its name", func(t *testing.T) {
		ctx := within(t)
		first := createCapture(t, []member{{"a", names[0]}}, true, false)
		h.awaitHeld(t, first.UID, true)
		require.NoError(t, k8s.Delete(ctx, first))
		require.Eventually(t, func() bool {
			return apierrors.IsNotFound(k8s.Get(ctx, client.ObjectKeyFromObject(first), &v1alpha1.PodGroupSnapshot{}))
		}, timeout, 10*time.Millisecond)
		second := &v1alpha1.PodGroupSnapshot{
			ObjectMeta: metav1.ObjectMeta{Namespace: first.Namespace, Name: first.Name},
			Spec:       first.Spec,
		}
		require.NoError(t, k8s.Create(ctx, second))
		second.Status = first.Status
		require.NoError(t, k8s.Status().Update(ctx, second))
		h.awaitHeld(t, second.UID, true)
		h.awaitHeld(t, first.UID, false)
	})
}

// TestFinal covers a final attempt's session, which is removed once it has
// ended, or at the attempt's deadline, as the host's clock measures it.
func TestFinal(t *testing.T) {
	names := nodes(t, 1)
	h := startHost(t, names[0], coordinatortest.Start(t))

	t.Run("once its session has ended", func(t *testing.T) {
		ctx := within(t)
		g := createCapture(t, []member{{"a", names[0]}, {"b", names[0]}}, true, false)
		h.awaitHeld(t, g.UID, true)
		members := []coordinator.Session{h.connect(t, g.UID, "a"), h.connect(t, g.UID, "b")}
		checkpointed(t, g)
		h.awaitSeen(t, g.UID, func(a *attempt) bool { return a.final })
		h.requireNotEnded(t, g.UID)
		for _, s := range members {
			require.NoError(t, s.Leave(ctx))
		}
		h.awaitHeld(t, g.UID, false)
	})

	t.Run("at its deadline", func(t *testing.T) {
		g := createCapture(t, []member{{"a", names[0]}}, true, false)
		h.awaitHeld(t, g.UID, true)
		h.connect(t, g.UID, "a")
		checkpointed(t, g)
		h.awaitSeen(t, g.UID, func(a *attempt) bool { return a.final })
		h.requireNotEnded(t, g.UID)
		h.clock.SetTime(g.CreationTimestamp.Add(time.Duration(g.Spec.DeadlineSeconds) * time.Second))
		h.awaitHeld(t, g.UID, false)
	})
}

// requireNotEnded fails the test unless h's coordinator still holds the open
// session of uid a moment later.
func (h *host) requireNotEnded(t *testing.T, uid types.UID) {
	t.Helper()
	require.Never(t, func() bool {
		ended, holds := h.coord.Ended(uid)
		return ended || !holds
	}, 300*time.Millisecond, 10*time.Millisecond, "the session of %s must stay open", uid)
}

// TestRestart covers a host that starts while attempts are in flight, as after
// a restart, which lost every session: it ensures the session of every
// unfinished attempt its node serves before the coordinator admits clients,
// and of no other attempt.
func TestRestart(t *testing.T) {
	names := nodes(t, 2)
	here := names[0]
	served := []types.UID{
		createCapture(t, []member{{"a", here}, {"b", names[1]}}, true, false).UID,
		createRestore(t, readySnapshot(t, []string{"a", "b"}), []member{{"b", names[1]}, {"a", here}}).UID,
	}
	final := createCapture(t, []member{{"a", here}}, true, false)
	checkpointed(t, final)
	others := []types.UID{
		final.UID,
		createCapture(t, []member{{"a", names[1]}, {"b", here}}, true, false).UID,
		createCapture(t, []member{{"a", here}}, false, false).UID,
	}

	logs := &logLines{}
	coord := coordinatortest.StartUnready(t, func(cfg *server.Config) { cfg.Log = logs.logger() })
	h := startHost(t, here, coord)
	require.Eventually(t, coord.Ready, timeout, 10*time.Millisecond)
	for _, uid := range served {
		_, holds := coord.Ended(uid)
		assert.True(t, holds, "the session of %s", uid)
	}
	for _, uid := range others {
		h.requireNotHeld(t, uid)
	}
	lines := logs.all()
	ready := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, "Coordinator ready") })
	require.GreaterOrEqual(t, ready, 0)
	for _, uid := range served {
		opened := slices.IndexFunc(lines, func(l string) bool {
			return strings.Contains(l, "Session opened") && strings.Contains(l, string(uid))
		})
		assert.True(t, opened >= 0 && opened < ready, "the session of %s must open before the coordinator is ready",
			uid)
	}
}

// logLines records every log line.
type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) logger() logr.Logger {
	return funcr.New(func(prefix, args string) {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.lines = append(l.lines, prefix+" "+args)
	}, funcr.Options{})
}

func (l *logLines) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.lines)
}
