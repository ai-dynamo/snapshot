// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package backends

import (
	"bytes"
	"context"
	"crypto/x509"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/go-logr/logr"

	operatortypes "github.com/ai-dynamo/snapshot/operator/internal/types"
)

const (
	NameS3 = "S3"

	// deleteObjectsBatchSize is DeleteObjects' per-request object limit.
	deleteObjectsBatchSize = 1000

	s3CredentialsRefreshInterval = 5 * time.Minute
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

// NewS3Config copies an operator-facing S3Config into a backends.S3Config.
func NewS3Config(cfg operatortypes.S3Config) S3Config {
	return S3Config{
		Bucket:          cfg.Bucket,
		Prefix:          cfg.Prefix,
		Region:          cfg.Region,
		Endpoint:        cfg.Endpoint,
		CredentialsPath: cfg.CredentialsPath,
		CABundlePath:    cfg.CABundlePath,
	}
}

// S3Backend implements maintenance.Backend against one S3-compatible bucket.
// Every artifact for a content lives under <prefix>/artifacts/<contentUID>/.
type S3Backend struct {
	client          s3API
	bucket          string
	artifactsPrefix string
}

// NewS3Backend constructs an S3Backend whose credentials are re-read from
// cfg.CredentialsPath every s3CredentialsRefreshInterval.
func NewS3Backend(ctx context.Context, cfg S3Config) (*S3Backend, error) {
	loadOptions := []func(*config.LoadOptions) error{
		config.WithRegion(cfg.Region),
		config.WithSharedConfigFiles([]string{}),
		config.WithCredentialsProvider(aws.NewCredentialsCache(sharedCredentialsFileProvider(cfg.CredentialsPath))),
	}
	if cfg.CABundlePath != "" {
		caBundle, err := readCABundle(cfg.CABundlePath)
		if err != nil {
			return nil, err
		}
		loadOptions = append(loadOptions, config.WithCustomCABundle(bytes.NewReader(caBundle)))
	}
	awsCfg, err := config.LoadDefaultConfig(ctx, loadOptions...)
	if err != nil {
		return nil, fmt.Errorf("load S3 client config: %w", err)
	}
	b := &S3Backend{}
	b.client = s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
			o.UsePathStyle = true
		}
	})
	b.bucket = cfg.Bucket
	b.artifactsPrefix = s3ArtifactsPrefix(cfg.Prefix)
	return b, nil
}

// s3ArtifactsPrefix mirrors PageBroker's key layout (see SNEP-237); do not change independently.
func s3ArtifactsPrefix(prefix string) string {
	return path.Join(strings.Trim(prefix, "/"), "artifacts") + "/"
}

func sharedCredentialsFileProvider(credentialsPath string) aws.CredentialsProviderFunc {
	return func(ctx context.Context) (aws.Credentials, error) {
		shared, err := config.LoadSharedConfigProfile(ctx, config.DefaultSharedConfigProfile, func(o *config.LoadSharedConfigOptions) {
			o.CredentialsFiles = []string{credentialsPath}
			o.ConfigFiles = []string{}
		})
		if err != nil {
			return aws.Credentials{}, fmt.Errorf("load S3 credentials from %q: %w", credentialsPath, err)
		}
		if !shared.Credentials.HasKeys() {
			return aws.Credentials{}, fmt.Errorf("S3 credentials file %q has no access key", credentialsPath)
		}
		credentials := shared.Credentials
		credentials.CanExpire = true
		credentials.Expires = time.Now().Add(s3CredentialsRefreshInterval)
		return credentials, nil
	}
}

func readCABundle(caBundlePath string) ([]byte, error) {
	pem, err := os.ReadFile(caBundlePath)
	if err != nil {
		return nil, fmt.Errorf("read CA bundle %q: %w", caBundlePath, err)
	}
	if !x509.NewCertPool().AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("CA bundle %q contains no usable certificates", caBundlePath)
	}
	return pem, nil
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
