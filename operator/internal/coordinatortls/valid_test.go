// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordinatortls

import (
	"crypto/tls"
	"crypto/x509"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

const serverName = "snapshot-coordinator"

func TestValidCA(t *testing.T) {
	ca := mint(t, caTemplate(now.AddDate(10, 0, 0)), nil)
	other := mint(t, caTemplate(now.AddDate(10, 0, 0)), nil)
	notCA := caTemplate(now.AddDate(10, 0, 0))
	notCA.IsCA = false
	noCertSign := caTemplate(now.AddDate(10, 0, 0))
	noCertSign.KeyUsage = x509.KeyUsageDigitalSignature

	for _, tc := range []struct {
		name    string
		pair    pemPair
		wantErr string
	}{
		{"valid", ca, ""},
		{"key of another CA", pemPair{ca.cert, other.key}, "do not load together"},
		{"not a CA", mint(t, notCA, nil), "not a CA"},
		{"no certificate signing", mint(t, noCertSign, nil), "not a CA"},
		{"not valid yet", mint(t, notYetValid(caTemplate(now.AddDate(10, 0, 0))), nil), "ca.crt is not valid until"},
		{"expires inside the window", mint(t, caTemplate(now.Add(renewalWindow-time.Second)), nil), "within the renewal window"},
		{"expires as the window ends", mint(t, caTemplate(now.Add(renewalWindow)), nil), ""},
		{"malformed PEM", pemPair{[]byte("not PEM"), ca.key}, "do not load together"},
		{"absent", pemPair{}, "is missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validCA(tc.pair.cert, tc.pair.key, now, renewalWindow)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				assert.Nil(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, serials(parseOne(t, tc.pair.cert)), serials(got.cert))
		})
	}
}

func TestValidPrevious(t *testing.T) {
	previous := mint(t, caTemplate(now.Add(time.Hour)), nil)
	other := mint(t, caTemplate(now.AddDate(1, 0, 0)), nil)
	expired := mint(t, caTemplate(now.Add(-time.Second)), nil)

	for _, tc := range []struct {
		name    string
		pem     []byte
		want    []string
		wantErr string
	}{
		{"absent", nil, nil, ""},
		{"unexpired", previous.cert, serials(parseOne(t, previous.cert)), ""},
		{"expired", expired.cert, nil, "previous.crt expired at"},
		{"two certificates", bundleOf(previous, other), nil, "previous.crt holds 2 certificates"},
		{"malformed PEM", []byte("not PEM"), nil, "parse previous.crt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validPrevious(tc.pem, now)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				assert.Nil(t, got)
				return
			}
			require.NoError(t, err)
			if tc.want == nil {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, tc.want, serials(got))
		})
	}
}

func TestValidBundle(t *testing.T) {
	current := mint(t, caTemplate(now.AddDate(10, 0, 0)), nil)
	previous := mint(t, caTemplate(now.Add(time.Hour)), nil)
	foreign := mint(t, caTemplate(now.AddDate(5, 0, 0)), nil)
	withPrevious := parseOne(t, previous.cert)

	for _, tc := range []struct {
		name     string
		bundle   *corev1.ConfigMap
		previous *x509.Certificate
		wantErr  string
	}{
		{"the current CA", bundleConfigMap(current), nil, ""},
		{"the current and the previous CA", bundleConfigMap(current, previous), withPrevious, ""},
		{"the previous CA first", bundleConfigMap(previous, current), withPrevious, ""},
		{"without the previous CA", bundleConfigMap(current), withPrevious, "does not hold exactly"},
		{"a previous CA the CA Secret dropped", bundleConfigMap(current, previous), nil, "does not hold exactly"},
		{"a foreign CA added", bundleConfigMap(current, foreign), nil, "does not hold exactly"},
		{"a foreign CA for the previous", bundleConfigMap(current, foreign), withPrevious, "does not hold exactly"},
		{"the current CA twice", bundleConfigMap(current, current), withPrevious, "does not hold exactly"},
		{"without the current CA", bundleConfigMap(previous), withPrevious, "does not hold exactly"},
		{"a private key", &corev1.ConfigMap{Data: map[string]string{bundleKey: string(bundleOf(current)) + string(current.key)}},
			nil, "something other than PEM certificates"},
		{"malformed PEM", &corev1.ConfigMap{Data: map[string]string{bundleKey: "not PEM"}}, nil,
			"something other than PEM certificates"},
		{"another key", &corev1.ConfigMap{Data: map[string]string{bundleKey: string(bundleOf(current)), "other": "x"}},
			nil, "does not hold ca.crt alone"},
		{"ca.crt in binaryData", &corev1.ConfigMap{BinaryData: map[string][]byte{bundleKey: bundleOf(current)}},
			nil, "does not hold ca.crt alone"},
		{"binaryData beside ca.crt", &corev1.ConfigMap{Data: map[string]string{bundleKey: string(bundleOf(current))},
			BinaryData: map[string][]byte{"other": []byte("x")}}, nil, "does not hold ca.crt alone"},
		{"absent", &corev1.ConfigMap{}, nil, "does not hold ca.crt alone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validBundle(tc.bundle, parseOne(t, current.cert), tc.previous)
			if tc.wantErr == "" {
				assert.NoError(t, err)
			} else {
				assert.ErrorContains(t, err, tc.wantErr)
			}
		})
	}
}

func TestValidServing(t *testing.T) {
	ca := mint(t, caTemplate(now.AddDate(10, 0, 0)), nil).authority(t)
	other := mint(t, caTemplate(now.AddDate(10, 0, 0)), nil).authority(t)
	valid := mint(t, servingTemplate(serverName, now.AddDate(1, 0, 0)), ca)
	clientAuth := servingTemplate(serverName, now.AddDate(1, 0, 0))
	clientAuth.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}

	for _, tc := range []struct {
		name    string
		pair    pemPair
		wantErr string
	}{
		{"valid", valid, ""},
		{"key of another certificate", pemPair{valid.cert, mint(t, servingTemplate(serverName, now.AddDate(1, 0, 0)), ca).key},
			"do not load together"},
		{"signed by another CA", mint(t, servingTemplate(serverName, now.AddDate(1, 0, 0)), other),
			"certificate signed by unknown authority"},
		{"another server name", mint(t, servingTemplate("other-coordinator", now.AddDate(1, 0, 0)), ca),
			"certificate is valid for other-coordinator, not snapshot-coordinator"},
		// A certificate without any extended key usage is valid for every
		// usage, to Verify and to the agents' TLS client alike, so this one
		// names another.
		{"client authentication only", mint(t, clientAuth, ca), "incompatible key usage"},
		// Valid through the window, but not now.
		{"not valid yet", mint(t, notYetValid(servingTemplate(serverName, now.AddDate(1, 0, 0))), ca), "is before"},
		{"expires inside the window", mint(t, servingTemplate(serverName, now.Add(renewalWindow-time.Second)), ca),
			"certificate has expired"},
		{"expires as the window ends", mint(t, servingTemplate(serverName, now.Add(renewalWindow)), ca), ""},
		{"malformed PEM", pemPair{[]byte("not PEM"), valid.key}, "do not load together"},
		{"absent", pemPair{}, "is missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validServing(tc.pair.cert, tc.pair.key, ca.cert, serverName, now, renewalWindow)
			if tc.wantErr == "" {
				assert.NoError(t, err)
			} else {
				assert.ErrorContains(t, err, tc.wantErr)
			}
		})
	}
}

func TestDecide(t *testing.T) {
	ca := mint(t, caTemplate(now.AddDate(10, 0, 0)), nil)
	other := mint(t, caTemplate(now.AddDate(10, 0, 0)), nil)
	nearExpiry := mint(t, caTemplate(now.Add(renewalWindow-time.Second)), nil)
	future := mint(t, notYetValid(caTemplate(now.AddDate(10, 0, 0))), nil)
	notCA := caTemplate(now.AddDate(10, 0, 0))
	notCA.IsCA = false
	expired := mint(t, caTemplate(now.Add(-time.Second)), nil)
	previous := mint(t, caTemplate(now.Add(time.Hour)), nil)
	serving := mint(t, servingTemplate(serverName, now.AddDate(1, 0, 0)), ca.authority(t))
	all := want{newCA: true, ca: true, bundle: true, serving: true}

	for _, tc := range []struct {
		name       string
		ca         *corev1.Secret
		bundle     *corev1.ConfigMap
		serving    *corev1.Secret
		serverName string
		want       want
	}{
		{name: "valid", want: want{}},
		{name: "valid with a previous CA", ca: caSecret(ca, previous), bundle: bundleConfigMap(ca, previous), want: want{}},
		{name: "nothing exists", ca: &corev1.Secret{}, bundle: &corev1.ConfigMap{}, serving: &corev1.Secret{}, want: all},
		{name: "the CA Secret is lost", ca: &corev1.Secret{}, want: all},
		{name: "the CA expires inside the window", ca: caSecret(nearExpiry), bundle: bundleConfigMap(nearExpiry), want: all},
		{name: "the CA is not valid yet", ca: caSecret(future), bundle: bundleConfigMap(future), want: all},
		{name: "ca.key of another CA", ca: caSecret(pemPair{ca.cert, other.key}), want: all},
		{name: "the CA is not a CA", ca: caSecret(mint(t, notCA, nil)), want: all},
		{name: "the previous CA has expired", ca: caSecret(ca, expired), bundle: bundleConfigMap(ca, expired),
			want: want{ca: true, bundle: true}},
		{name: "the previous CA has expired, and the bundle dropped it", ca: caSecret(ca, expired),
			want: want{ca: true}},
		{name: "the bundle lacks the previous CA", ca: caSecret(ca, previous), want: want{bundle: true}},
		{name: "the bundle is absent", bundle: &corev1.ConfigMap{}, want: want{bundle: true}},
		{name: "the bundle lacks the current CA", bundle: bundleConfigMap(other), want: want{bundle: true}},
		{name: "the bundle holds a foreign CA", bundle: bundleConfigMap(ca, other), want: want{bundle: true}},
		{name: "the bundle's ca.crt is in binaryData",
			bundle: &corev1.ConfigMap{BinaryData: map[string][]byte{bundleKey: bundleOf(ca)}}, want: want{bundle: true}},
		{name: "the serving Secret is absent", serving: &corev1.Secret{}, want: want{serving: true}},
		{name: "the serving certificate is not valid yet",
			serving: tlsSecret(mint(t, notYetValid(servingTemplate(serverName, now.AddDate(1, 0, 0))), ca.authority(t))),
			want:    want{serving: true}},
		{name: "the serving certificate expires inside the window",
			serving: tlsSecret(mint(t, servingTemplate(serverName, now.Add(renewalWindow-time.Second)), ca.authority(t))),
			want:    want{serving: true}},
		{name: "another server name", serverName: "other-coordinator", want: want{serving: true}},
		{name: "the serving certificate is signed by another CA",
			serving: tlsSecret(mint(t, servingTemplate(serverName, now.AddDate(1, 0, 0)), other.authority(t))),
			want:    want{serving: true}},
		{name: "tls.key of another certificate", serving: tlsSecret(pemPair{serving.cert, other.key}),
			want: want{serving: true}},
		{name: "the bundle and the serving Secret are absent", bundle: &corev1.ConfigMap{}, serving: &corev1.Secret{},
			want: want{bundle: true, serving: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.ca == nil {
				tc.ca = caSecret(ca)
			}
			if tc.bundle == nil {
				tc.bundle = bundleConfigMap(ca)
			}
			if tc.serving == nil {
				tc.serving = tlsSecret(serving)
			}
			if tc.serverName == "" {
				tc.serverName = serverName
			}
			p := decide(tc.ca, tc.bundle, tc.serving, tc.serverName, now)
			assert.Equal(t, tc.want, want{p.newCA, p.ca != nil, p.bundle != nil, p.serving != nil})
			if p.newCA {
				assert.ErrorIs(t, p.bundle, errNewCA)
				assert.ErrorIs(t, p.serving, errNewCA)
			}
		})
	}
}

// want is what a plan writes.
type want struct{ newCA, ca, bundle, serving bool }

// pemPair is a certificate and its private key, as PEM.
type pemPair struct{ cert, key []byte }

// mint issues template for a new key, signed by parent, or self-signed when
// parent is nil.
func mint(t *testing.T, template *x509.Certificate, parent *authority) pemPair {
	t.Helper()
	certPEM, keyPEM, err := issue(template, parent)
	require.NoError(t, err)
	return pemPair{certPEM, keyPEM}
}

// authority loads p as a CA that signs, whether or not it is a valid one.
func (p pemPair) authority(t *testing.T) *authority {
	t.Helper()
	pair, err := tls.X509KeyPair(p.cert, p.key)
	require.NoError(t, err)
	return &authority{cert: pair.Leaf, key: pair.PrivateKey}
}

func caTemplate(notAfter time.Time) *x509.Certificate {
	return &x509.Certificate{
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              notAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
}

func servingTemplate(name string, notAfter time.Time) *x509.Certificate {
	return &x509.Certificate{
		DNSNames:    []string{name},
		NotBefore:   now.Add(-time.Hour),
		NotAfter:    notAfter,
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
}

// notYetValid makes template valid from an hour after now.
func notYetValid(template *x509.Certificate) *x509.Certificate {
	template.NotBefore = now.Add(time.Hour)
	return template
}

func bundleOf(pairs ...pemPair) []byte {
	var bundle []byte
	for _, p := range pairs {
		bundle = append(bundle, p.cert...)
	}
	return bundle
}

// caSecret holds p as the CA and, when given, previous as the previous CA.
func caSecret(p pemPair, previous ...pemPair) *corev1.Secret {
	s := &corev1.Secret{Data: map[string][]byte{caCertKey: p.cert, caKeyKey: p.key}}
	if len(previous) > 0 {
		s.Data[previousKey] = bundleOf(previous...)
	}
	return s
}

func bundleConfigMap(pairs ...pemPair) *corev1.ConfigMap {
	return &corev1.ConfigMap{Data: map[string]string{bundleKey: string(bundleOf(pairs...))}}
}

func tlsSecret(p pemPair) *corev1.Secret {
	return &corev1.Secret{Data: map[string][]byte{corev1.TLSCertKey: p.cert, corev1.TLSPrivateKeyKey: p.key}}
}
