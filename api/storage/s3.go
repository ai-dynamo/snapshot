// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"fmt"
	"net/url"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

// S3 identifies a logical store inside an S3-compatible bucket. Prefix is a
// relative key prefix inside the bucket, not an absolute filesystem path.
// Credentials, part sizes and transfer limits are not identity inputs: they
// can change without moving the store to a new identity.
type S3 struct {
	Endpoint string `json:"endpoint" yaml:"endpoint"`
	Region   string `json:"region" yaml:"region"`
	Bucket   string `json:"bucket" yaml:"bucket"`
	Prefix   string `json:"prefix,omitempty" yaml:"prefix,omitempty"`
}

// StoreID hashes the canonical S3 location. No network request is performed;
// credentials, addressing mode and transfer limits are not inputs.
// The exact v1 preimage is six length-prefixed UTF-8 fields, in order:
// "snapshot.store/v1", "s3", normalizedEndpoint, region, bucket, normalizedPrefix.
// Each length is an unsigned 32-bit big-endian byte count (no terminator).
// Shared vectors in testdata/identity.json pin this encoding for other languages.
func (s S3) StoreID() (string, error) {
	endpoint, err := s.NormalizedEndpoint()
	if err != nil {
		return "", err
	}
	if s.Region == "" || len(s.Region) > 255 || !isPrintableASCII(s.Region) {
		return "", fmt.Errorf("S3 region must be a nonempty printable ASCII string of at most 255 bytes")
	}
	if err := validateBucket(s.Bucket); err != nil {
		return "", err
	}
	prefix, err := s.NormalizedPrefix()
	if err != nil {
		return "", err
	}
	return "store-v1-" + hashFields("snapshot.store/v1", "s3", endpoint, s.Region, s.Bucket, prefix), nil
}

// NormalizedEndpoint validates the endpoint as an absolute http(s) URL and
// lowercases its scheme and host, so differently-cased but equivalent
// endpoints resolve to the same store identity. The path, query and fragment
// are rejected: the endpoint identifies a service, not a location in it. A
// scheme's default port is dropped so an explicit ":443"/":80" hashes the
// same as its implicit form.
func (s S3) NormalizedEndpoint() (string, error) {
	if !utf8.ValidString(s.Endpoint) || len(s.Endpoint) > 2048 {
		return "", fmt.Errorf("S3 endpoint must be a valid UTF-8 string of at most 2048 bytes")
	}
	parsed, err := url.Parse(s.Endpoint)
	if err != nil {
		return "", fmt.Errorf("S3 endpoint: %w", err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	hostname := strings.ToLower(parsed.Hostname())
	if (scheme != "http" && scheme != "https") || hostname == "" ||
		parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return "", fmt.Errorf("S3 endpoint must be an absolute http(s) URL with no path, query, fragment or userinfo")
	}
	if strings.Contains(hostname, ":") {
		hostname = "[" + hostname + "]"
	}
	port := parsed.Port()
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	host := hostname
	if port != "" {
		host = hostname + ":" + port
	}
	return scheme + "://" + host, nil
}

// NormalizedPrefix cleans redundant separators and dot components inside a
// relative bucket-key prefix, but rejects traversal and absolute paths
// instead of silently identifying a different location. An empty prefix
// identifies the bucket root.
func (s S3) NormalizedPrefix() (string, error) {
	prefix := s.Prefix
	if !utf8.ValidString(prefix) || len(prefix) > 1024 || path.IsAbs(prefix) ||
		strings.TrimSpace(prefix) != prefix || strings.Contains(prefix, `\`) ||
		strings.ContainsFunc(prefix, unicode.IsControl) {
		return "", fmt.Errorf("S3 prefix must be a relative key prefix, at most 1024 bytes, " +
			"without control characters or backslashes")
	}
	if prefix == "" {
		return "", nil
	}
	for component := range strings.SplitSeq(prefix, "/") {
		if component == ".." {
			return "", fmt.Errorf("S3 prefix must not contain parent traversal")
		}
	}
	cleaned := path.Clean(prefix)
	if cleaned == "." {
		return "", nil
	}
	return cleaned, nil
}

func validateBucket(bucket string) error {
	// RFC-compatible subset: DNS-style bucket naming, which also satisfies
	// path- and virtual-hosted-style addressing against any S3-compatible
	// endpoint, not only AWS.
	if len(bucket) < 3 || len(bucket) > 63 {
		return fmt.Errorf("S3 bucket must be between 3 and 63 characters")
	}
	for i := range bucket {
		c := bucket[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '-') {
			return fmt.Errorf("S3 bucket must contain only lowercase letters, digits, dots and hyphens")
		}
	}
	if bucket[0] == '.' || bucket[0] == '-' || bucket[len(bucket)-1] == '.' || bucket[len(bucket)-1] == '-' {
		return fmt.Errorf("S3 bucket must not start or end with a dot or hyphen")
	}
	return nil
}

func isPrintableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}
