//go:build integration

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ai-dynamo/snapshot/api/v1alpha1"
	"github.com/ai-dynamo/snapshot/coordinator"
	"github.com/ai-dynamo/snapshot/coordinator/internal/testcert"
	"github.com/ai-dynamo/snapshot/coordinator/session"
	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
)

// The test runs Run against an envtest API server, which no operator runs
// against: it writes the group resources' status itself. The coordinator acts
// as the agents' ServiceAccount, by impersonation, with the RBAC the package
// doc gives and Kubernetes's defaults.

const (
	node      = "node-0"
	otherNode = "node-1"
	audience  = "snapshot-coordinator"

	// timeout bounds every wait for the coordinator to act. certwatcher also
	// reads the certificate every 10 s, should it miss a change.
	timeout = 30 * time.Second
)

var agents = types.NamespacedName{Namespace: "snapshot-system", Name: "snapshot-agent"}

// TestRun runs a coordinator on node-0 from its start to its end: readiness,
// a client limited to TLS 1.2 refused, an agent admitted for the member on its
// node only, a session cancelled by its resource's deletion, a rotated
// certificate, and the shutdown.
func TestRun(t *testing.T) {
	ctrllog.SetLogger(logr.Discard())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	k := startCluster(t)
	token := k.agentToken(t)
	served := k.createCapture(t, false, member{"a", node}, member{"b", otherNode})

	roots, cert := testcert.New(t)
	dir := t.TempDir()
	writeCert(t, dir, cert)
	port, healthPort := freePort(t), freePort(t)
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	health := net.JoinHostPort("127.0.0.1", strconv.Itoa(healthPort))
	holdMetricsPort(t)
	logs := &logLines{}
	r := start(t, Config{
		RESTConfig:     k.agentConfig(),
		Node:           node,
		ServiceAccount: agents,
		Audience:       audience,
		Host:           "127.0.0.1",
		Port:           port,
		HealthPort:     healthPort,
		TLSDir:         dir,
		Log:            logs.logger(),
	})
	// connect connects an agent as member of the attempt uid, within timeout,
	// and closes the session when the test ends, not the step.
	connect := func(ctx context.Context, uid types.UID, member string) (coordinator.Session, error) {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		s, err := coordinator.Connect(ctx, coordinator.Config{
			URLs:       []string{"tls://" + addr},
			RootCAs:    func() (*x509.CertPool, error) { return roots, nil },
			ServerName: "localhost",
			Attempt:    string(uid),
			Member:     member,
			Token:      func() (string, error) { return token, nil },
		})
		if err == nil {
			t.Cleanup(func() { _ = s.Close() })
		}
		return s, err
	}
	// Each step needs the ones before it, so the first to fail ends the test.
	step := func(name string, f func(t *testing.T)) {
		t.Helper()
		if !t.Run(name, f) {
			t.FailNow()
		}
	}

	step("readiness waits for the startup to ensure the sessions", func(t *testing.T) {
		poll(t, "liveness", func() bool {
			r.requireRunning(t)
			return probe(health, "/healthz") == http.StatusOK
		})
		// Until the ServiceAccount may list the group resources, the cache
		// cannot sync, so the startup cannot ensure the sessions.
		for end := time.Now().Add(time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
			require.Equal(t, http.StatusOK, probe(health, "/healthz"), "liveness")
			require.NotEqual(t, http.StatusOK, probe(health, "/readyz"), "readiness")
		}
		k.grantRBAC(t)
		poll(t, "readiness", func() bool {
			r.requireRunning(t)
			require.Equal(t, http.StatusOK, probe(health, "/healthz"), "liveness")
			return probe(health, "/readyz") == http.StatusOK
		})
		lines := logs.all()
		ready := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, "Coordinator ready") })
		opened := slices.IndexFunc(lines, func(l string) bool {
			return strings.Contains(l, "Session opened") && strings.Contains(l, string(served.UID))
		})
		require.True(t, opened >= 0 && opened < ready, "the session must open before the coordinator is ready")
	})

	step("no metrics port", func(t *testing.T) {
		// A manager that served metrics would have failed to start on the
		// default metrics address, which holdMetricsPort makes unavailable.
		r.requireRunning(t)
	})

	step("a client limited to TLS 1.2 is refused", func(t *testing.T) {
		nc, err := nats.Connect("tls://"+addr, nats.NoReconnect(), nats.Timeout(time.Second),
			nats.Secure(&tls.Config{RootCAs: roots, ServerName: "localhost", MaxVersion: tls.VersionTLS12}))
		if err == nil {
			nc.Close()
		}
		require.ErrorIs(t, err, nats.ErrTLS)
		assert.ErrorContains(t, err, "protocol version")
	})

	var joined coordinator.Session
	step("an agent joins as the member on its node", func(t *testing.T) {
		s, err := connect(ctx, served.UID, "a")
		require.NoError(t, err)
		assert.Equal(t, 0, s.Rank())
		joined = s
	})

	step("the agent's token is refused for a member on another node", func(t *testing.T) {
		refused, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		_, err := connect(refused, served.UID, "b")
		require.ErrorIs(t, err, session.ErrForbidden)
	})

	step("deleting the resource before the admitted barrier cancels the session", func(t *testing.T) {
		g := k.createCapture(t, true, member{"a", node}, member{"b", otherNode})
		s, err := connect(ctx, g.UID, "a")
		require.NoError(t, err)
		arrived := make(chan error, 1)
		go func() { arrived <- s.Barrier(ctx, session.BarrierAdmitted) }()
		require.NoError(t, k.k8s.Delete(ctx, g))
		select {
		case err := <-arrived:
			require.ErrorIs(t, err, &session.AbortError{Reason: session.ReasonCancelled})
		case <-time.After(timeout):
			t.Fatal("the barrier did not end")
		}
		k.release(t, g)
	})

	step("a rotated certificate is served to new connections", func(t *testing.T) {
		require.Equal(t, cert.Certificate[0], servedCertificate(addr))
		_, rotated := testcert.New(t)
		writeCert(t, dir, rotated)
		poll(t, "the rotated certificate", func() bool {
			return bytes.Equal(servedCertificate(addr), rotated.Certificate[0])
		})
		r.requireRunning(t)
	})

	step("cancelling the context stops the coordinator", func(t *testing.T) {
		require.NoError(t, r.stop(t))
		require.NotNil(t, joined)
		select {
		case <-joined.Done():
			require.ErrorIs(t, joined.Err(), session.ErrCoordinatorLost)
		case <-time.After(timeout):
			t.Fatal("the member did not learn that its coordinator stopped")
		}
		for name, a := range map[string]string{"client": addr, "health": health} {
			conn, err := net.DialTimeout("tcp", a, time.Second)
			if err == nil {
				_ = conn.Close()
			}
			require.Error(t, err, "the %s port is still open", name)
		}
	})
}

// cluster is the test's API server.
type cluster struct {
	cfg   *rest.Config
	admin *kubernetes.Clientset
	k8s   client.Client
}

// startCluster starts an API server with the group resources' CRDs, and stops
// it when the test ends.
func startCluster(t *testing.T) *cluster {
	t.Helper()
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "api", "v1alpha1", "crds")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, env.Stop()) })
	scheme := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	k := &cluster{cfg: cfg}
	k.admin, err = kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	k.k8s, err = client.New(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)
	return k
}

// agentConfig returns a REST config that acts as the agents' ServiceAccount.
func (k *cluster) agentConfig() *rest.Config {
	cfg := rest.CopyConfig(k.cfg)
	cfg.Impersonate = rest.ImpersonationConfig{
		UserName: "system:serviceaccount:" + agents.Namespace + ":" + agents.Name,
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:" + agents.Namespace, "system:authenticated"},
	}
	return cfg
}

// agentToken creates the agents' ServiceAccount and an agent Pod of it on
// node, and returns a token of the ServiceAccount for the audience, bound to
// that Pod, as the agent's projected token is.
func (k *cluster) agentToken(t *testing.T) string {
	t.Helper()
	ctx := t.Context()
	_, err := k.admin.CoreV1().Namespaces().Create(ctx,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: agents.Namespace}}, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = k.admin.CoreV1().ServiceAccounts(agents.Namespace).Create(ctx,
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: agents.Name}}, metav1.CreateOptions{})
	require.NoError(t, err)
	pod, err := k.admin.CoreV1().Pods(agents.Namespace).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "snapshot-agent-0"},
		Spec: corev1.PodSpec{ServiceAccountName: agents.Name, NodeName: node,
			Containers: []corev1.Container{{Name: "agent", Image: "agent"}}},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	r, err := k.admin.CoreV1().ServiceAccounts(agents.Namespace).CreateToken(ctx, agents.Name,
		&authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{
			Audiences:         []string{audience},
			ExpirationSeconds: ptr.To[int64](600),
			BoundObjectRef: &authenticationv1.BoundObjectReference{
				Kind: "Pod", APIVersion: "v1", Name: pod.Name, UID: pod.UID,
			},
		}}, metav1.CreateOptions{})
	require.NoError(t, err)
	return r.Status.Token
}

// grantRBAC binds the ClusterRole the package doc gives to the agents'
// ServiceAccount.
func (k *cluster) grantRBAC(t *testing.T) {
	t.Helper()
	ctx := t.Context()
	_, err := k.admin.RbacV1().ClusterRoles().Create(ctx, &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "snapshot-coordinator"},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{"nvidia.com"}, Resources: []string{"podgroupsnapshots", "podgrouprestores"},
				Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{"authentication.k8s.io"}, Resources: []string{"tokenreviews"},
				Verbs: []string{"create"}},
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = k.admin.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "snapshot-coordinator"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "snapshot-coordinator"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Namespace: agents.Namespace, Name: agents.Name}},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
}

// member is one member of a test's group: its ID and its node.
type member struct{ id, node string }

// createCapture creates a PodGroupSnapshot of members, in rank order, each on
// its node with its member PodSnapshot pinned, as the operator records them.
// With finalizer, deleting it leaves it until release.
func (k *cluster) createCapture(t *testing.T, finalizer bool, members ...member) *v1alpha1.PodGroupSnapshot {
	t.Helper()
	ctx := t.Context()
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
	require.NoError(t, k.k8s.Create(ctx, g))
	for rank, m := range members {
		g.Status.Members = append(g.Status.Members, v1alpha1.PodGroupSnapshotMemberStatus{ID: m.id,
			Rank: int32(rank), NodeName: m.node, Phase: v1alpha1.CheckpointPhaseCheckpointing,
			PodSnapshot: &v1alpha1.ObjectIdentity{Name: g.Name + "-" + m.id,
				UID: types.UID("podsnapshot-uid-" + m.id)}})
	}
	require.NoError(t, k.k8s.Status().Update(ctx, g))
	return g
}

// release removes the in-use finalizer from g, so its deletion completes.
func (k *cluster) release(t *testing.T, g *v1alpha1.PodGroupSnapshot) {
	t.Helper()
	ctx := t.Context()
	require.NoError(t, k.k8s.Get(ctx, client.ObjectKeyFromObject(g), g))
	patch := client.MergeFrom(g.DeepCopy())
	g.Finalizers = nil
	require.NoError(t, k.k8s.Patch(ctx, g, patch))
}

// running is a Run in progress.
type running struct {
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

// start runs Run with cfg until the test ends.
func start(t *testing.T, cfg Config) *running {
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(r.done)
		r.err = Run(ctx, cfg)
	}()
	t.Cleanup(func() {
		cancel()
		// controller-runtime's manager does not return from Start if its
		// context is cancelled before the cache has synced, as when a step
		// fails before the test grants the RBAC.
		select {
		case <-r.done:
		case <-time.After(timeout):
			t.Error("Run did not return")
		}
	})
	return r
}

// requireRunning fails the test if Run has returned.
func (r *running) requireRunning(t *testing.T) {
	t.Helper()
	select {
	case <-r.done:
		t.Fatalf("Run returned: %v", r.err)
	default:
	}
}

// stop cancels Run's context, and returns what Run returns.
func (r *running) stop(t *testing.T) error {
	t.Helper()
	r.cancel()
	select {
	case <-r.done:
		return r.err
	case <-time.After(timeout):
		t.Fatal("Run did not return")
		return nil
	}
}

// poll calls check until it returns true, and fails the test if it has not
// by timeout. Unlike require.Eventually, it calls check on the test's
// goroutine, so check may fail the test.
func poll(t *testing.T, what string, check func() bool) {
	t.Helper()
	for deadline := time.Now().Add(timeout); !check(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("waiting for %s timed out", what)
		}
	}
}

// probe returns the status of a GET of path from the health endpoint at
// addr, or 0 if it does not answer.
func probe(addr, path string) int {
	c := http.Client{Timeout: time.Second}
	resp, err := c.Get("http://" + addr + path)
	if err != nil {
		return 0
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// servedCertificate returns the certificate the client listener at addr
// serves a new connection, in DER, or nil if the handshake did not reach it.
// The connection presents no member, so the coordinator then refuses it.
func servedCertificate(addr string) []byte {
	var leaf []byte
	nc, err := nats.Connect("tls://"+addr, nats.NoReconnect(), nats.Timeout(time.Second),
		nats.Secure(&tls.Config{
			// The test reads the certificate; it does not trust it.
			InsecureSkipVerify: true,
			VerifyConnection: func(cs tls.ConnectionState) error {
				leaf = cs.PeerCertificates[0].Raw
				return nil
			},
		}))
	if err == nil {
		nc.Close()
	}
	return leaf
}

// writeCert writes cert and its key to dir as tls.crt and tls.key, as the
// volume of a kubernetes.io/tls Secret holds them.
func writeCert(t *testing.T, dir string, cert tls.Certificate) {
	t.Helper()
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, corev1.TLSCertKey),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, corev1.TLSPrivateKeyKey),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0o600))
}

// freePort returns a port of 127.0.0.1 that nothing listens on.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// holdMetricsPort makes controller-runtime's default metrics address, :8080,
// unavailable until the test ends: the test listens there, unless another
// process already does.
func holdMetricsPort(t *testing.T) {
	t.Helper()
	l, err := net.Listen("tcp", ":8080")
	if err != nil {
		return
	}
	t.Cleanup(func() { _ = l.Close() })
}

// logLines records every line the coordinator logs, which never holds a key,
// a value, a barrier name, or a token. The test never prints them.
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
