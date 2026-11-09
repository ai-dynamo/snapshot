// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordinatortls

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// renewalWindow is how long before it expires a certificate is replaced.
const renewalWindow = 90 * 24 * time.Hour

// The keys of the CA Secret and of the trust bundle ConfigMap. The serving
// Secret's are corev1.TLSCertKey and corev1.TLSPrivateKeyKey.
const (
	caCertKey   = "ca.crt"
	caKeyKey    = "ca.key"
	previousKey = "previous.crt"
	bundleKey   = "ca.crt"
)

// A plan names what ensure must write. Each error is nil when its resource is
// valid, and otherwise says why ensure must write it. A write of the CA Secret
// issues a new CA when newCA is set, and otherwise keeps the CA and drops its
// previous.crt.
type plan struct {
	newCA               bool
	ca, bundle, serving error
}

// errNewCA is why a new CA replaces the bundle and the serving certificate.
var errNewCA = errors.New("the CA is new")

// decide returns what ensure must write, given the three resources as read
// (with no data when absent), the server name and now. A new CA always brings
// a new bundle and a new serving certificate. Otherwise the bundle must be
// exactly what the CA Secret holds: its CA, and its previous CA while that
// has not expired.
func decide(ca *corev1.Secret, bundle *corev1.ConfigMap, serving *corev1.Secret, serverName string, now time.Time) plan {
	current, err := validCA(ca.Data[caCertKey], ca.Data[caKeyKey], now, renewalWindow)
	if err != nil {
		return plan{newCA: true, ca: err, bundle: errNewCA, serving: errNewCA}
	}
	previous, err := validPrevious(ca.Data[previousKey], now)
	return plan{
		ca:     err,
		bundle: validBundle(bundle, current.cert, previous),
		serving: validServing(serving.Data[corev1.TLSCertKey], serving.Data[corev1.TLSPrivateKeyKey],
			current.cert, serverName, now, renewalWindow),
	}
}

// validCA returns the CA that ca.crt and ca.key hold when it may sign: the two
// load together, the certificate is a CA whose key signs certificates, and it
// is valid from now until past now plus window.
func validCA(certPEM, keyPEM []byte, now time.Time, window time.Duration) (*authority, error) {
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		return nil, errors.New("ca.crt or ca.key is missing")
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("ca.crt and ca.key do not load together: %w", err)
	}
	switch {
	case !signsCertificates(pair.Leaf):
		return nil, errors.New("ca.crt is not a CA that signs certificates")
	case now.Before(pair.Leaf.NotBefore):
		return nil, fmt.Errorf("ca.crt is not valid until %s", pair.Leaf.NotBefore.Format(time.RFC3339))
	case now.Add(window).After(pair.Leaf.NotAfter):
		return nil, fmt.Errorf("ca.crt expires at %s, within the renewal window",
			pair.Leaf.NotAfter.Format(time.RFC3339))
	}
	return &authority{cert: pair.Leaf, key: pair.PrivateKey}, nil
}

// validPrevious returns the CA Secret's previous CA: nil when previous.crt is
// absent, and an error, which drops it, when it is not one certificate that
// has not expired at now.
func validPrevious(previousPEM []byte, now time.Time) (*x509.Certificate, error) {
	if len(previousPEM) == 0 {
		return nil, nil
	}
	certs, err := parseCertificates(previousPEM)
	switch {
	case err != nil:
		return nil, fmt.Errorf("parse previous.crt: %w", err)
	case len(certs) != 1:
		return nil, fmt.Errorf("previous.crt holds %d certificates", len(certs))
	case now.After(certs[0].NotAfter):
		return nil, fmt.Errorf("previous.crt expired at %s", certs[0].NotAfter.Format(time.RFC3339))
	}
	return certs[0], nil
}

// validBundle checks that the trust bundle holds exactly what the CA Secret
// dictates: one key, ca.crt, whose certificates are current and, unless it is
// nil, previous, in any order.
func validBundle(bundle *corev1.ConfigMap, current, previous *x509.Certificate) error {
	if len(bundle.Data) != 1 || len(bundle.BinaryData) != 0 {
		return errors.New("the bundle does not hold ca.crt alone")
	}
	certs, err := parseCertificates([]byte(bundle.Data[bundleKey]))
	if err != nil {
		return fmt.Errorf("parse ca.crt: %w", err)
	}
	want := []*x509.Certificate{current}
	if previous != nil {
		want = append(want, previous)
	}
	if len(certs) != len(want) || slices.ContainsFunc(want, func(cert *x509.Certificate) bool {
		return !slices.ContainsFunc(certs, cert.Equal)
	}) {
		return errors.New("ca.crt does not hold exactly the CA Secret's current and previous CA")
	}
	return nil
}

// validServing checks the serving Secret: tls.crt and tls.key load together,
// and the certificate verifies against ca for serverName and server
// authentication both now and at now plus window.
func validServing(certPEM, keyPEM []byte, ca *x509.Certificate, serverName string, now time.Time, window time.Duration) error {
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		return errors.New("tls.crt or tls.key is missing")
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return fmt.Errorf("tls.crt and tls.key do not load together: %w", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	for _, at := range []time.Time{now, now.Add(window)} {
		if _, err := pair.Leaf.Verify(x509.VerifyOptions{
			Roots:       roots,
			DNSName:     serverName,
			KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			CurrentTime: at,
		}); err != nil {
			return fmt.Errorf("tls.crt does not verify at %s: %w", at.Format(time.RFC3339), err)
		}
	}
	return nil
}

func signsCertificates(cert *x509.Certificate) bool {
	return cert.IsCA && cert.BasicConstraintsValid && cert.KeyUsage&x509.KeyUsageCertSign != 0
}
