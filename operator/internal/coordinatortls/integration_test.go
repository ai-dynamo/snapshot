//go:build integration

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordinatortls

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func TestIssuer(t *testing.T) {
	env := &envtest.Environment{}
	restConfig, err := env.Start()
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, env.Stop()) })
	cl, err := client.New(restConfig, client.Options{})
	require.NoError(t, err)

	t.Run("first run", func(t *testing.T) {
		f := newFixture(t, cl)
		f.requireWrites(f.ensure(), f.cfg.CASecret, f.cfg.CABundle, f.cfg.TLSSecret)

		got := f.read()
		assert.Equal(t, corev1.SecretTypeOpaque, got.ca.Type)
		assert.Equal(t, []string{caCertKey, caKeyKey}, slices.Sorted(maps.Keys(got.ca.Data)))
		assert.Equal(t, corev1.SecretTypeTLS, got.serving.Type)
		assert.Equal(t, []string{corev1.TLSCertKey, corev1.TLSPrivateKeyKey}, slices.Sorted(maps.Keys(got.serving.Data)))
		assert.Equal(t, []string{bundleKey}, slices.Sorted(maps.Keys(got.bundle.Data)))
		assert.Empty(t, got.bundle.BinaryData)

		assert.Equal(t, serials(got.caCert(t)), serials(got.bundleCerts(t)...))
		f.requireServes(got, got.caCert(t))

		caKey := got.ca.Data[caKeyKey]
		assert.False(t, bytes.Contains([]byte(got.bundle.Data[bundleKey]), caKey), "the bundle holds the CA's key")
		assert.False(t, bytes.Contains([]byte(got.bundle.Data[bundleKey]), []byte("PRIVATE KEY")),
			"the bundle holds a private key")
		for key, value := range got.serving.Data {
			assert.False(t, bytes.Contains(value, caKey), "%s holds the CA's key", key)
		}
	})

	t.Run("second run", func(t *testing.T) {
		f := newFixture(t, cl)
		f.ensure()
		before := f.read()

		f.requireWrites(f.ensure())
		assert.Equal(t, before.resourceVersions(), f.read().resourceVersions())
	})

	t.Run("renewal of the serving certificate", func(t *testing.T) {
		f := newFixture(t, cl)
		f.ensure()
		before := f.read()
		renewAt := before.servingCert(t).NotAfter.Add(-renewalWindow)

		f.clock.SetTime(renewAt.Add(-time.Second))
		f.requireWrites(f.ensure())

		f.clock.SetTime(renewAt.Add(time.Second))
		f.requireWrites(f.ensure(), f.cfg.TLSSecret)
		after := f.read()
		f.requireSameCA(before, after)
		assert.NotEqual(t, serials(before.servingCert(t)), serials(after.servingCert(t)))
		assert.Equal(t, f.clock.Now().AddDate(1, 0, 0), after.servingCert(t).NotAfter)
		f.requireServes(after, before.caCert(t))
	})

	t.Run("a changed server name", func(t *testing.T) {
		f := newFixture(t, cl)
		f.ensure()
		before := f.read()

		f.cfg.ServerName = "other-coordinator"
		f.requireWrites(f.ensure(), f.cfg.TLSSecret)
		after := f.read()
		f.requireSameCA(before, after)
		assert.Equal(t, []string{"other-coordinator"}, after.servingCert(t).DNSNames)
		f.requireServes(after, before.caCert(t))
	})

	t.Run("a deleted serving Secret", func(t *testing.T) {
		f := newFixture(t, cl)
		f.ensure()
		before := f.read()

		require.NoError(t, cl.Delete(t.Context(), before.serving))
		f.requireWrites(f.ensure(), f.cfg.TLSSecret)
		after := f.read()
		f.requireSameCA(before, after)
		f.requireServes(after, before.caCert(t))
	})

	t.Run("a mismatched serving Secret", func(t *testing.T) {
		f := newFixture(t, cl)
		f.ensure()
		before := f.read()

		other := mint(t, servingTemplate(f.cfg.ServerName, f.clock.Now().AddDate(1, 0, 0)), testCA(t, f.clock.Now()))
		before.serving.Data[corev1.TLSPrivateKeyKey] = other.key
		require.NoError(t, cl.Update(t.Context(), before.serving))
		f.requireWrites(f.ensure(), f.cfg.TLSSecret)
		after := f.read()
		f.requireSameCA(before, after)
		f.requireServes(after, before.caCert(t))
	})

	t.Run("a deleted bundle", func(t *testing.T) {
		f := newFixture(t, cl)
		f.ensure()
		before := f.read()

		require.NoError(t, cl.Delete(t.Context(), before.bundle))
		f.requireWrites(f.ensure(), f.cfg.CABundle)
		after := f.read()
		assert.Equal(t, before.ca.ResourceVersion, after.ca.ResourceVersion)
		assert.Equal(t, before.serving.ResourceVersion, after.serving.ResourceVersion)
		assert.Equal(t, serials(before.caCert(t)), serials(after.bundleCerts(t)...))
	})

	t.Run("a foreign CA in the bundle", func(t *testing.T) {
		f := newFixture(t, cl)
		f.ensure()
		before := f.read()

		foreign := mint(t, caTemplate(f.clock.Now().AddDate(5, 0, 0)), nil)
		before.bundle.Data[bundleKey] += string(foreign.cert)
		require.NoError(t, cl.Update(t.Context(), before.bundle))
		f.requireWrites(f.ensure(), f.cfg.CABundle)
		after := f.read()
		assert.Equal(t, serials(before.caCert(t)), serials(after.bundleCerts(t)...), "the foreign CA is gone")
		assert.Equal(t, before.ca.ResourceVersion, after.ca.ResourceVersion)
		assert.Equal(t, before.serving.ResourceVersion, after.serving.ResourceVersion)
	})

	t.Run("a bundle whose ca.crt is in binaryData", func(t *testing.T) {
		f := newFixture(t, cl)
		f.ensure()
		before := f.read()

		before.bundle.Data = nil
		before.bundle.BinaryData = map[string][]byte{bundleKey: []byte("not PEM")}
		require.NoError(t, cl.Update(t.Context(), before.bundle))
		f.requireWrites(f.ensure(), f.cfg.CABundle)
		after := f.read()
		assert.Empty(t, after.bundle.BinaryData)
		assert.Equal(t, serials(before.caCert(t)), serials(after.bundleCerts(t)...))
	})

	t.Run("CA replacement when the CA nears expiry", func(t *testing.T) {
		f := newFixture(t, cl)
		f.ensure()
		before := f.read()
		previous := before.caCert(t)
		f.clock.SetTime(previous.NotAfter.Add(-renewalWindow).Add(time.Second))

		f.requireWrites(f.ensure(), f.cfg.CASecret, f.cfg.CABundle, f.cfg.TLSSecret)
		after := f.read()
		current := after.caCert(t)
		assert.NotEqual(t, serials(previous), serials(current))
		assert.Equal(t, serials(previous), serials(after.previousCert(t)), "the CA Secret keeps the CA it replaced")
		assert.Equal(t, serials(current, previous), serials(after.bundleCerts(t)...))
		f.requireServes(after, current)

		// The bundle is rebuilt from the CA Secret, so the previous CA
		// survives the bundle's loss.
		require.NoError(t, cl.Delete(t.Context(), after.bundle))
		f.requireWrites(f.ensure(), f.cfg.CABundle)
		assert.Equal(t, serials(current, previous), serials(f.read().bundleCerts(t)...))

		// Once the previous CA expires, the CA Secret drops it, then the
		// bundle.
		f.clock.SetTime(previous.NotAfter.Add(time.Second))
		f.requireWrites(f.ensure(), f.cfg.CASecret, f.cfg.CABundle)
		expired := f.read()
		assert.Equal(t, []string{caCertKey, caKeyKey}, slices.Sorted(maps.Keys(expired.ca.Data)))
		assert.Equal(t, serials(current), serials(expired.caCert(t)), "the CA is kept")
		assert.Equal(t, serials(current), serials(expired.bundleCerts(t)...))
		assert.Equal(t, after.serving.ResourceVersion, expired.serving.ResourceVersion)
		f.requireServes(expired, current)
	})

	for _, tc := range []struct {
		name   string
		change func(f *fixture, ca *corev1.Secret)
	}{
		{"lost", func(f *fixture, ca *corev1.Secret) {
			require.NoError(f.t, cl.Delete(f.t.Context(), ca))
		}},
		{"damaged", func(f *fixture, ca *corev1.Secret) {
			ca.Data[caKeyKey] = mint(f.t, caTemplate(f.clock.Now().AddDate(10, 0, 0)), nil).key
			require.NoError(f.t, cl.Update(f.t.Context(), ca))
		}},
	} {
		t.Run("CA replacement when the CA Secret is "+tc.name, func(t *testing.T) {
			f := newFixture(t, cl)
			f.ensure()
			before := f.read()

			tc.change(f, before.ca)
			f.requireWrites(f.ensure(), f.cfg.CASecret, f.cfg.CABundle, f.cfg.TLSSecret)
			after := f.read()
			current := after.caCert(t)
			assert.NotEqual(t, serials(before.caCert(t)), serials(current))
			assert.NotContains(t, after.ca.Data, previousKey, "the CA Secret leaves no previous CA")
			assert.Equal(t, serials(current), serials(after.bundleCerts(t)...))
			f.requireServes(after, current)
		})
	}

	t.Run("partial completion", func(t *testing.T) {
		f := newFixture(t, cl)
		f.ensure()
		before := f.read()
		f.clock.SetTime(before.caCert(t).NotAfter.Add(-renewalWindow).Add(time.Second))

		failing := &recordingWriter{Writer: cl, failAfter: 1}
		require.ErrorIs(t, NewIssuer(f.cfg, cl, failing).ensure(t.Context()), errInjected)
		assert.Equal(t, []string{f.cfg.CASecret}, failing.names(), "the CA write succeeds, and the bundle's fails")
		partial := f.read()
		assert.Equal(t, serials(before.caCert(t)), serials(partial.previousCert(t)))
		assert.Equal(t, before.bundle.ResourceVersion, partial.bundle.ResourceVersion)
		assert.Equal(t, before.serving.ResourceVersion, partial.serving.ResourceVersion)

		f.requireWrites(f.ensure(), f.cfg.CABundle, f.cfg.TLSSecret)
		after := f.read()
		assert.Equal(t, partial.ca.ResourceVersion, after.ca.ResourceVersion, "the CA is not replaced again")
		current := partial.caCert(t)
		assert.Equal(t, serials(current, before.caCert(t)), serials(after.bundleCerts(t)...),
			"the bundle is built from the CA Secret")
		f.requireServes(after, current)
	})

	// Two issuers read the same resources and each decides on a new CA: both
	// create one on the first run, and both update the CA when it nears
	// expiry. A writes its CA first, then waits while B's write of its own
	// loses, so B must read again, adopt A's CA, and write the rest. Then A's
	// next write loses too, and A finds nothing left to write.
	for _, tc := range []struct {
		name     string
		previous bool
	}{
		{"a race to create", false},
		{"a race to update", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, cl)
			var previous []*x509.Certificate
			if tc.previous {
				f.ensure()
				previous = []*x509.Certificate{f.read().caCert(t)}
				f.clock.SetTime(previous[0].NotAfter.Add(-renewalWindow).Add(time.Second))
			}

			bRead, aWrote, bDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
			closeBRead, closeAWrote := sync.OnceFunc(func() { close(bRead) }), sync.OnceFunc(func() { close(aWrote) })
			a := &recordingWriter{Writer: cl, hold: func(tried int) {
				switch tried {
				case 0:
					<-bRead
				case 1:
					closeAWrote()
					<-bDone
				}
			}}
			b := &recordingWriter{Writer: cl, hold: func(tried int) {
				if tried == 0 {
					closeBRead()
					<-aWrote
				}
			}}
			var errA, errB error
			var wg sync.WaitGroup
			wg.Go(func() {
				defer closeAWrote()
				errA = NewIssuer(f.cfg, cl, a).ensure(t.Context())
			})
			wg.Go(func() {
				defer close(bDone)
				defer closeBRead()
				errB = NewIssuer(f.cfg, cl, b).ensure(t.Context())
			})
			wg.Wait()
			require.NoError(t, errA)
			require.NoError(t, errB)

			assert.Equal(t, []string{f.cfg.CASecret}, a.names(), "A writes the CA, then its bundle write loses")
			assert.Equal(t, []string{f.cfg.CABundle, f.cfg.TLSSecret}, b.names(), "B's CA write loses")
			got := f.read()
			current := got.caCert(t)
			if tc.previous {
				assert.Equal(t, serials(previous...), serials(got.previousCert(t)))
			}
			assert.Equal(t, serials(append([]*x509.Certificate{current}, previous...)...), serials(got.bundleCerts(t)...))
			f.requireServes(got, current)
		})
	}

	t.Run("start and resync", func(t *testing.T) {
		f := newFixture(t, cl)
		mgr := newManager(t, restConfig, f.cfg.Namespace)
		require.NoError(t, mgr.Add(NewIssuer(f.cfg, mgr.GetAPIReader(), mgr.GetClient())))
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error)
		go func() { done <- mgr.Start(ctx) }()
		defer func() {
			cancel()
			assert.NoError(t, <-done)
		}()

		// At once, on the leader.
		var before state
		require.EventuallyWithT(t, func(c *assert.CollectT) {
			var ok bool
			before, ok = f.tryRead(c)
			require.True(c, ok)
		}, 30*time.Second, 50*time.Millisecond)

		require.NoError(t, cl.Delete(t.Context(), before.serving))
		f.clock.Step(resyncPeriod)
		require.EventuallyWithT(t, func(c *assert.CollectT) {
			after, ok := f.tryRead(c)
			require.True(c, ok)
			assert.NotEqual(c, before.serving.UID, after.serving.UID)
		}, 30*time.Second, 50*time.Millisecond)
		after := f.read()
		f.requireSameCA(before, after)
		f.requireServes(after, before.caCert(t))
	})
}

// start is the fake clock's first time, a whole second as certificate times
// are.
var start = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

// fixture is an Issuer's configuration in a namespace of its own.
type fixture struct {
	t     *testing.T
	cl    client.Client
	clock *clocktesting.FakeClock
	cfg   Config
}

func newFixture(t *testing.T, cl client.Client) *fixture {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "coordinator-tls-"}}
	require.NoError(t, cl.Create(t.Context(), ns))
	clock := clocktesting.NewFakeClock(start)
	return &fixture{t: t, cl: cl, clock: clock, cfg: Config{
		Namespace:  ns.Name,
		CASecret:   "snapshot-coordinator-ca",
		TLSSecret:  "snapshot-coordinator-tls",
		CABundle:   "snapshot-coordinator-ca-bundle",
		ServerName: serverName,
		Clock:      clock,
		Log:        testr.New(t),
	}}
}

// ensure runs ensure once, through a writer that records the writes, and
// returns it.
func (f *fixture) ensure() *recordingWriter {
	f.t.Helper()
	w := &recordingWriter{Writer: f.cl}
	require.NoError(f.t, NewIssuer(f.cfg, f.cl, w).ensure(f.t.Context()))
	return w
}

// requireWrites requires w to have written the resources named, in order.
func (f *fixture) requireWrites(w *recordingWriter, names ...string) {
	f.t.Helper()
	require.Equal(f.t, names, w.names())
}

// state is the three resources as read.
type state struct {
	ca      *corev1.Secret
	bundle  *corev1.ConfigMap
	serving *corev1.Secret
}

func (f *fixture) read() state {
	f.t.Helper()
	s, ok := f.tryRead(f.t)
	require.True(f.t, ok)
	return s
}

func (f *fixture) tryRead(t require.TestingT) (state, bool) {
	s := state{ca: &corev1.Secret{}, bundle: &corev1.ConfigMap{}, serving: &corev1.Secret{}}
	for name, obj := range map[string]client.Object{
		f.cfg.CASecret: s.ca, f.cfg.CABundle: s.bundle, f.cfg.TLSSecret: s.serving,
	} {
		if !assert.NoError(t, f.cl.Get(f.t.Context(), client.ObjectKey{Namespace: f.cfg.Namespace, Name: name}, obj)) {
			return s, false
		}
	}
	return s, true
}

func (s state) resourceVersions() []string {
	return []string{s.ca.ResourceVersion, s.bundle.ResourceVersion, s.serving.ResourceVersion}
}

func (s state) caCert(t *testing.T) *x509.Certificate {
	return parseOne(t, s.ca.Data[caCertKey])
}

func (s state) previousCert(t *testing.T) *x509.Certificate {
	return parseOne(t, s.ca.Data[previousKey])
}

func (s state) servingCert(t *testing.T) *x509.Certificate {
	return parseOne(t, s.serving.Data[corev1.TLSCertKey])
}

func (s state) bundleCerts(t *testing.T) []*x509.Certificate {
	t.Helper()
	certs, err := parseCertificates([]byte(s.bundle.Data[bundleKey]))
	require.NoError(t, err)
	return certs
}

// requireServes requires the serving Secret to hold a certificate signed by
// ca that verifies against the bundle for the server name now, and its key.
func (f *fixture) requireServes(s state, ca *x509.Certificate) {
	t := f.t
	t.Helper()
	cert := s.servingCert(t)
	require.NoError(t, cert.CheckSignatureFrom(ca))
	roots := x509.NewCertPool()
	for _, root := range s.bundleCerts(t) {
		roots.AddCert(root)
	}
	_, err := cert.Verify(x509.VerifyOptions{
		Roots:       roots,
		DNSName:     f.cfg.ServerName,
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		CurrentTime: f.clock.Now(),
	})
	require.NoError(t, err)
	require.NoError(t, validServing(s.serving.Data[corev1.TLSCertKey], s.serving.Data[corev1.TLSPrivateKeyKey],
		ca, f.cfg.ServerName, f.clock.Now(), 0))
}

// requireSameCA requires the CA and the bundle to be unchanged.
func (f *fixture) requireSameCA(before, after state) {
	f.t.Helper()
	assert.Equal(f.t, before.ca.ResourceVersion, after.ca.ResourceVersion)
	assert.Equal(f.t, before.bundle.ResourceVersion, after.bundle.ResourceVersion)
}

var errInjected = errors.New("injected write failure")

// recordingWriter records the names of the resources it writes, in order. When
// set, hold is called before each write with the number of writes tried
// before it, and every write after failAfter of them fails.
type recordingWriter struct {
	client.Writer
	failAfter int
	hold      func(tried int)

	mu      sync.Mutex
	tried   int
	written []string
}

func (w *recordingWriter) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	return w.write(obj, func() error { return w.Writer.Create(ctx, obj, opts...) })
}

func (w *recordingWriter) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	return w.write(obj, func() error { return w.Writer.Update(ctx, obj, opts...) })
}

func (w *recordingWriter) write(obj client.Object, write func() error) error {
	w.mu.Lock()
	tried := w.tried
	w.tried++
	w.mu.Unlock()
	if w.hold != nil {
		w.hold(tried)
	}
	if w.failAfter > 0 && len(w.names()) >= w.failAfter {
		return errInjected
	}
	if err := write(); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.written = append(w.written, obj.GetName())
	return nil
}

func (w *recordingWriter) names() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.written)
}

// newManager returns a manager that elects its leader in namespace, and
// serves neither metrics nor health probes.
func newManager(t *testing.T, restConfig *rest.Config, namespace string) manager.Manager {
	mgr, err := manager.New(restConfig, manager.Options{
		LeaderElection:                true,
		LeaderElectionID:              "coordinator-tls-test",
		LeaderElectionNamespace:       namespace,
		LeaderElectionReleaseOnCancel: true,
		Metrics:                       metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress:        "0",
	})
	require.NoError(t, err)
	return mgr
}
