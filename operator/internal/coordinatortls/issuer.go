// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordinatortls

import (
	"context"
	"crypto/x509"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// resyncPeriod is how often the Issuer checks the three resources, which also
// repairs one that was deleted or edited.
const resyncPeriod = time.Minute

// Config configures an Issuer.
type Config struct {
	// Namespace holds the three resources.
	Namespace string
	// CASecret names the Opaque Secret that holds the CA: ca.crt and ca.key.
	CASecret string
	// TLSSecret names the kubernetes.io/tls Secret that holds the
	// coordinator's serving certificate: tls.crt and tls.key.
	TLSSecret string
	// CABundle names the ConfigMap that holds the trust bundle: ca.crt.
	CABundle string
	// ServerName is the serving certificate's only DNS name, the name the
	// agents verify.
	ServerName string
	// Clock gives the time and ticks the resync.
	Clock clock.WithTicker
	// Log receives each certificate issued, and each failure. It is never
	// given a key.
	Log logr.Logger
}

// Issuer keeps the three resources valid. It is a manager.Runnable that runs
// on the leader only.
type Issuer struct {
	cfg    Config
	reader client.Reader
	writer client.Writer
}

// NewIssuer returns an Issuer that reads through reader, the manager's API
// reader, so that it needs no cache and no watch, and writes through writer.
func NewIssuer(cfg Config, reader client.Reader, writer client.Writer) *Issuer {
	return &Issuer{cfg: cfg, reader: reader, writer: writer}
}

// NeedLeaderElection implements manager.LeaderElectionRunnable: only the
// leader writes the resources.
func (i *Issuer) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable. It ensures the resources at once, then
// every resyncPeriod until ctx is done. A failure is logged, and the next
// tick retries it.
func (i *Issuer) Start(ctx context.Context) error {
	ticker := i.cfg.Clock.NewTicker(resyncPeriod)
	defer ticker.Stop()
	for {
		if err := i.ensure(ctx); err != nil {
			i.cfg.Log.Error(err, "Failed to issue the coordinator's TLS certificate; retrying at the next resync")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C():
		}
	}
}

// ensure reads the three resources, decides, and writes what the decision
// names, in this order: the CA Secret; then the bundle, built from the CA
// Secret as persisted; then the serving certificate, signed by the CA as
// persisted. So the bundle that trusts a new CA is published before the
// serving certificate it signs, although each node's kubelet refreshes the
// mounted files on its own schedule. Each write is conditional on the
// resourceVersion read, or is a create when the resource was absent. When one
// loses a race, ensure discards what it generated, then reads and decides
// again, a bounded number of times.
func (i *Issuer) ensure(ctx context.Context) error {
	return retry.OnError(retry.DefaultRetry, lostRace, func() error {
		ca := &corev1.Secret{ObjectMeta: i.meta(i.cfg.CASecret)}
		bundle := &corev1.ConfigMap{ObjectMeta: i.meta(i.cfg.CABundle)}
		serving := &corev1.Secret{ObjectMeta: i.meta(i.cfg.TLSSecret)}
		for _, obj := range []client.Object{ca, bundle, serving} {
			if err := i.reader.Get(ctx, client.ObjectKeyFromObject(obj), obj); client.IgnoreNotFound(err) != nil {
				return err
			}
		}
		now := i.cfg.Clock.Now()
		p := decide(ca, bundle, serving, i.cfg.ServerName, now)

		if p.ca != nil {
			if p.newCA {
				certPEM, keyPEM, err := newCA(now)
				if err != nil {
					return fmt.Errorf("create a CA: %w", err)
				}
				data := map[string][]byte{caCertKey: certPEM, caKeyKey: keyPEM}
				// The CA it replaces stays trusted until it expires, unless the
				// CA Secret was lost or damaged.
				if replaced, err := validCA(ca.Data[caCertKey], ca.Data[caKeyKey], now, 0); err == nil {
					data[previousKey] = encodeCertificate(replaced.cert.Raw)
				}
				ca.Type = corev1.SecretTypeOpaque
				ca.Data = data
			} else {
				delete(ca.Data, previousKey)
			}
			if err := i.write(ctx, ca); err != nil {
				return err
			}
		}
		current, err := validCA(ca.Data[caCertKey], ca.Data[caKeyKey], now, renewalWindow)
		if err != nil {
			return fmt.Errorf("load the CA as persisted: %w", err)
		}
		previous, err := validPrevious(ca.Data[previousKey], now)
		if err != nil {
			return fmt.Errorf("load the previous CA as persisted: %w", err)
		}
		switch {
		case p.newCA:
			i.cfg.Log.Info("Issued the coordinator's CA", append([]any{"secret", ca.Name, "serial", serial(current.cert),
				"notAfter", current.cert.NotAfter, "reason", p.ca.Error()}, previousValues(previous)...)...)
		case p.ca != nil:
			i.cfg.Log.Info("Dropped the coordinator's previous CA", "secret", ca.Name, "reason", p.ca.Error())
		}

		if p.bundle != nil {
			bundle.Data = map[string]string{bundleKey: string(buildBundle(current.cert, previous))}
			bundle.BinaryData = nil
			if err := i.write(ctx, bundle); err != nil {
				return err
			}
			i.cfg.Log.Info("Wrote the coordinator's CA bundle", append([]any{"configMap", bundle.Name,
				"serial", serial(current.cert), "reason", p.bundle.Error()}, previousValues(previous)...)...)
		}

		if p.serving != nil {
			certPEM, keyPEM, err := signServing(current, i.cfg.ServerName, now)
			if err != nil {
				return fmt.Errorf("sign a serving certificate: %w", err)
			}
			serving.Type = corev1.SecretTypeTLS
			serving.Data = map[string][]byte{corev1.TLSCertKey: certPEM, corev1.TLSPrivateKeyKey: keyPEM}
			if err := i.write(ctx, serving); err != nil {
				return err
			}
			certs, err := parseCertificates(serving.Data[corev1.TLSCertKey])
			if err != nil {
				return fmt.Errorf("parse the serving certificate as persisted: %w", err)
			}
			i.cfg.Log.Info("Issued the coordinator's serving certificate", "secret", serving.Name,
				"serial", serial(certs[0]), "notAfter", certs[0].NotAfter, "reason", p.serving.Error())
		}
		return nil
	})
}

// write creates obj when ensure found it absent, and otherwise updates it on
// the resourceVersion ensure read, so that a concurrent write makes it fail.
func (i *Issuer) write(ctx context.Context, obj client.Object) error {
	if obj.GetResourceVersion() == "" {
		return i.writer.Create(ctx, obj)
	}
	return i.writer.Update(ctx, obj)
}

func (i *Issuer) meta(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Namespace: i.cfg.Namespace, Name: name}
}

// previousValues names the previous CA, if any, in a log line.
func previousValues(previous *x509.Certificate) []any {
	if previous == nil {
		return nil
	}
	return []any{"previousSerial", serial(previous), "previousNotAfter", previous.NotAfter}
}

// lostRace reports whether a write lost to a concurrent one: an update on a
// stale resourceVersion, or a create of a resource that now exists.
func lostRace(err error) bool {
	return apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err)
}

func serial(cert *x509.Certificate) string {
	return cert.SerialNumber.Text(16)
}
