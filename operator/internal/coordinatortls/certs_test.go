// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordinatortls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// now is a whole second, as certificate times are.
var now = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

func TestNewCA(t *testing.T) {
	ca := testCA(t, now)

	cert := ca.cert
	assert.True(t, cert.IsCA)
	assert.True(t, cert.BasicConstraintsValid)
	assert.True(t, cert.MaxPathLenZero)
	assert.Zero(t, cert.MaxPathLen)
	assert.Equal(t, x509.KeyUsageCertSign, cert.KeyUsage)
	assert.Empty(t, cert.ExtKeyUsage)
	assert.Equal(t, now.Add(-time.Hour), cert.NotBefore)
	assert.Equal(t, now.AddDate(10, 0, 0), cert.NotAfter)
	assert.NoError(t, cert.CheckSignatureFrom(cert), "self-signed")
	requireP256(t, cert, ca.key)
	assertSerial(t, cert)
	assert.NotEqual(t, 0, testCA(t, now).cert.SerialNumber.Cmp(cert.SerialNumber), "serial numbers are random")
}

func TestSignServing(t *testing.T) {
	ca := testCA(t, now)
	certPEM, keyPEM, err := signServing(ca, "snapshot-coordinator", now)
	require.NoError(t, err)

	cert := parseOne(t, certPEM)
	assert.False(t, cert.IsCA)
	assert.Equal(t, x509.KeyUsageDigitalSignature, cert.KeyUsage)
	assert.Equal(t, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, cert.ExtKeyUsage)
	assert.Equal(t, []string{"snapshot-coordinator"}, cert.DNSNames)
	assert.Empty(t, cert.IPAddresses)
	assert.Equal(t, now.Add(-time.Hour), cert.NotBefore)
	assert.Equal(t, now.AddDate(1, 0, 0), cert.NotAfter)
	assert.NoError(t, cert.CheckSignatureFrom(ca.cert))
	assertSerial(t, cert)
	assert.NoError(t, validServing(certPEM, keyPEM, ca.cert, "snapshot-coordinator", now, renewalWindow))
}

func TestBuildBundle(t *testing.T) {
	current, previous := testCA(t, now).cert, testCA(t, now).cert

	certs, err := parseCertificates(buildBundle(current, nil))
	require.NoError(t, err)
	assert.Equal(t, serials(current), serials(certs...))

	certs, err = parseCertificates(buildBundle(current, previous))
	require.NoError(t, err)
	assert.Equal(t, serials(current, previous), serials(certs...), "the current CA first")
}

// testCA creates a CA issued at issuedAt.
func testCA(t *testing.T, issuedAt time.Time) *authority {
	t.Helper()
	ca, _, _ := testCAPEM(t, issuedAt)
	return ca
}

// testCAPEM creates a CA issued at issuedAt, and returns it and its PEM.
func testCAPEM(t *testing.T, issuedAt time.Time) (ca *authority, certPEM, keyPEM []byte) {
	t.Helper()
	certPEM, keyPEM, err := newCA(issuedAt)
	require.NoError(t, err)
	ca, err = validCA(certPEM, keyPEM, issuedAt, 0)
	require.NoError(t, err)
	return ca, certPEM, keyPEM
}

func parseOne(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	certs, err := parseCertificates(certPEM)
	require.NoError(t, err)
	require.Len(t, certs, 1)
	return certs[0]
}

// serials identifies certificates in a failure message, which never shows PEM.
func serials(certs ...*x509.Certificate) []string {
	out := make([]string, len(certs))
	for i, cert := range certs {
		out[i] = serial(cert)
	}
	return out
}

func requireP256(t *testing.T, cert *x509.Certificate, key any) {
	t.Helper()
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	require.True(t, ok, "an ECDSA key")
	assert.Equal(t, elliptic.P256(), pub.Curve)
	priv, ok := key.(*ecdsa.PrivateKey)
	require.True(t, ok, "an ECDSA private key")
	assert.True(t, priv.PublicKey.Equal(pub))
}

func assertSerial(t *testing.T, cert *x509.Certificate) {
	t.Helper()
	assert.Positive(t, cert.SerialNumber.Sign())
	assert.LessOrEqual(t, cert.SerialNumber.BitLen(), 128)
}
