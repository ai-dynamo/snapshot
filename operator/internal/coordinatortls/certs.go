// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package coordinatortls

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"time"
)

// The lifetimes of the certificates the issuer creates.
const (
	caYears      = 10
	servingYears = 1
	// clockSkew backdates NotBefore, so a peer whose clock is behind the
	// operator's accepts a certificate as soon as it is issued.
	clockSkew = time.Hour
)

// authority is a CA that may sign: its certificate and its private key.
type authority struct {
	cert *x509.Certificate
	key  crypto.PrivateKey
}

// newCA creates a CA, valid for ten years from now, and returns its
// certificate and private key as PEM.
func newCA(now time.Time) (certPEM, keyPEM []byte, err error) {
	return issue(&x509.Certificate{
		Subject:               pkix.Name{CommonName: "snapshot-coordinator-ca"},
		NotBefore:             now.Add(-clockSkew),
		NotAfter:              now.AddDate(caYears, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign,
	}, nil)
}

// signServing signs a serving certificate for serverName with ca, valid for
// one year from now, and returns it and its private key as PEM.
func signServing(ca *authority, serverName string, now time.Time) (certPEM, keyPEM []byte, err error) {
	return issue(&x509.Certificate{
		Subject:     pkix.Name{CommonName: serverName},
		DNSNames:    []string{serverName},
		NotBefore:   now.Add(-clockSkew),
		NotAfter:    now.AddDate(servingYears, 0, 0),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, ca)
}

// buildBundle returns the trust bundle of current and, unless it is nil,
// previous, as PEM.
func buildBundle(current, previous *x509.Certificate) []byte {
	bundle := encodeCertificate(current.Raw)
	if previous != nil {
		bundle = append(bundle, encodeCertificate(previous.Raw)...)
	}
	return bundle
}

// issue creates an ECDSA P-256 key and a certificate for it from template,
// with a random 128-bit serial number, signed by parent, or self-signed when
// parent is nil.
func issue(template *x509.Certificate, parent *authority) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate a key: %w", err)
	}
	template.SerialNumber, err = rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("generate a serial number: %w", err)
	}
	signer := &authority{cert: template, key: key}
	if parent != nil {
		signer = parent
	}
	der, err := x509.CreateCertificate(rand.Reader, template, signer.cert, key.Public(), signer.key)
	if err != nil {
		return nil, nil, fmt.Errorf("create the certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("encode the key: %w", err)
	}
	return encodeCertificate(der), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
}

func encodeCertificate(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// parseCertificates parses data as one or more PEM certificates and nothing
// else.
func parseCertificates(data []byte) ([]*x509.Certificate, error) {
	var certs []*x509.Certificate
	for rest := bytes.TrimSpace(data); len(rest) > 0; rest = bytes.TrimSpace(rest) {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil || block.Type != "CERTIFICATE" {
			return nil, errors.New("holds something other than PEM certificates")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		certs = append(certs, cert)
	}
	if len(certs) == 0 {
		return nil, errors.New("holds no certificate")
	}
	return certs, nil
}
