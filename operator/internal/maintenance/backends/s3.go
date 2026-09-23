// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package backends

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/go-logr/logr"
)

const (
	NameS3 = "S3"

	// deleteObjectsBatchSize is DeleteObjects' per-request object limit.
	deleteObjectsBatchSize = 1000
)

// s3API is the subset of the S3 client S3Backend depends on.
type s3API interface {
	ListObjectsV2(ctx context.Context, params *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	DeleteObjects(ctx context.Context, params *s3.DeleteObjectsInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error)
}

// S3Config carries what S3Backend needs to reach one bucket/prefix.
type S3Config struct {
	Bucket          string
	Prefix          string
	Region          string
	Endpoint        string
	CredentialsPath string
	CABundlePath    string
}

// S3Backend implements maintenance.Backend against one S3-compatible bucket.
// Every artifact for a content lives under <prefix>/artifacts/<contentUID>/.
type S3Backend struct {
	client          s3API
	bucket          string
	artifactsPrefix string
}

// NewS3Backend constructs an S3Backend using a refreshable, shared-credentials-file provider.
func NewS3Backend(ctx context.Context, cfg S3Config) (*S3Backend, error) {
	httpClient, err := s3HTTPClient(cfg.CABundlePath)
	if err != nil {
		return nil, fmt.Errorf("build S3 HTTP client: %w", err)
	}
	awsCfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(cfg.Region),
		config.WithSharedCredentialsFiles([]string{cfg.CredentialsPath}),
		config.WithSharedConfigFiles(nil),
		config.WithHTTPClient(httpClient),
	)
	if err != nil {
		return nil, fmt.Errorf("load S3 client config: %w", err)
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
			o.UsePathStyle = true
		}
	})
	return &S3Backend{
		client:          client,
		bucket:          cfg.Bucket,
		artifactsPrefix: strings.Trim(cfg.Prefix, "/") + "/artifacts/",
	}, nil
}

func s3HTTPClient(caBundlePath string) (*http.Client, error) {
	if caBundlePath == "" {
		return nil, nil
	}
	pem, err := os.ReadFile(caBundlePath)
	if err != nil {
		return nil, fmt.Errorf("read CA bundle %q: %w", caBundlePath, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("CA bundle %q contains no usable certificates", caBundlePath)
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}, nil
}

func (b *S3Backend) Name() string {
	return NameS3
}

// Delete removes every object under the content's artifact prefix; it is a
// no-op when nothing exists for contentUID.
func (b *S3Backend) Delete(ctx context.Context, contentUID string) error {
	prefix := b.artifactsPrefix + contentUID + "/"
	var continuationToken *string
	for {
		page, err := b.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(b.bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: continuationToken,
		})
		if err != nil {
			return fmt.Errorf("list objects under %q: %w", prefix, err)
		}
		if err := b.deleteObjects(ctx, page.Contents); err != nil {
			return err
		}
		if page.IsTruncated == nil || !*page.IsTruncated {
			return nil
		}
		continuationToken = page.NextContinuationToken
	}
}

func (b *S3Backend) deleteObjects(ctx context.Context, objects []s3types.Object) error {
	for start := 0; start < len(objects); start += deleteObjectsBatchSize {
		end := min(start+deleteObjectsBatchSize, len(objects))
		batch := make([]s3types.ObjectIdentifier, 0, end-start)
		for _, object := range objects[start:end] {
			batch = append(batch, s3types.ObjectIdentifier{Key: object.Key})
		}
		out, err := b.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(b.bucket),
			Delete: &s3types.Delete{Objects: batch, Quiet: aws.Bool(true)},
		})
		if err != nil {
			return fmt.Errorf("delete objects: %w", err)
		}
		if len(out.Errors) > 0 {
			return fmt.Errorf("delete objects: %s: %s", aws.ToString(out.Errors[0].Key), aws.ToString(out.Errors[0].Message))
		}
	}
	return nil
}

// Candidates lists content UIDs with artifacts under the store's prefix.
func (b *S3Backend) Candidates(ctx context.Context, _ logr.Logger) (map[string]struct{}, error) {
	candidates := make(map[string]struct{})
	var continuationToken *string
	for {
		page, err := b.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(b.bucket),
			Prefix:            aws.String(b.artifactsPrefix),
			Delimiter:         aws.String("/"),
			ContinuationToken: continuationToken,
		})
		if err != nil {
			return nil, fmt.Errorf("list artifact prefixes under %q: %w", b.artifactsPrefix, err)
		}
		for _, common := range page.CommonPrefixes {
			uid := strings.TrimSuffix(strings.TrimPrefix(aws.ToString(common.Prefix), b.artifactsPrefix), "/")
			if uid != "" {
				candidates[uid] = struct{}{}
			}
		}
		if page.IsTruncated == nil || !*page.IsTruncated {
			return candidates, nil
		}
		continuationToken = page.NextContinuationToken
	}
}
