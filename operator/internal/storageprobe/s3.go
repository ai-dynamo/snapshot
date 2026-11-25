// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storageprobe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	"github.com/ai-dynamo/snapshot/operator/internal/maintenance/backends"
)

// objectAPI is the subset of the S3 client the access check uses.
type objectAPI interface {
	PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	ListObjectsV2(ctx context.Context, params *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	DeleteObject(ctx context.Context, params *s3.DeleteObjectInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
}

var credentialErrorCodes = map[string]bool{
	"InvalidAccessKeyId":    true,
	"SignatureDoesNotMatch": true,
	"ExpiredToken":          true,
	"InvalidToken":          true,
	"TokenRefreshRequired":  true,
}

func checkS3(ctx context.Context, clusterID string, store S3) error {
	endpoint, err := url.Parse(store.Endpoint)
	if err != nil || endpoint.Host == "" {
		return &Failure{Err: fmt.Errorf("the endpoint is not a URL")}
	}
	if endpoint.Scheme == "http" && !store.AllowHTTP {
		return &Failure{Err: fmt.Errorf("the endpoint is plain http; set allowHttp to use it")}
	}
	config := backends.S3ClientConfig{
		Region:      store.Region,
		Endpoint:    store.Endpoint,
		PathStyle:   store.PathStyle,
		CABundlePEM: store.CABundle,
	}
	if len(store.Credentials) > 0 {
		credentials, err := ParseSharedCredentials(store.Credentials)
		if err != nil {
			return &Failure{Credentials: true, Err: err}
		}
		config.Credentials = aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return credentials, nil
		})
	}
	client, err := backends.NewS3Client(ctx, config)
	if err != nil {
		return &Failure{Err: err}
	}
	return runS3Check(ctx, client, store.Bucket, accessCheckKey(store.Prefix, clusterID))
}

func runS3Check(ctx context.Context, api objectAPI, bucket, key string) error {
	if _, err := api.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(accessCheckPayload),
	}); err != nil {
		return s3Failure(StepWrite, err)
	}
	object, err := api.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return s3Failure(StepRead, err)
	}
	got, err := io.ReadAll(io.LimitReader(object.Body, int64(len(accessCheckPayload))+1))
	_ = object.Body.Close()
	if err != nil {
		return s3Failure(StepRead, err)
	}
	if !bytes.Equal(got, accessCheckPayload) {
		return &Failure{Step: StepRead, Err: fmt.Errorf("the object read back differs from the one written")}
	}
	listing, err := api.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(key)})
	if err != nil {
		return s3Failure(StepList, err)
	}
	found := false
	for _, object := range listing.Contents {
		if aws.ToString(object.Key) == key {
			found = true
			break
		}
	}
	if !found {
		return &Failure{Step: StepList, Err: fmt.Errorf("the object is not in the listing")}
	}
	if _, err := api.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}); err != nil {
		return s3Failure(StepDelete, err)
	}
	return nil
}

func s3Failure(step string, err error) *Failure {
	return &Failure{Step: step, Credentials: credentialsCause(err), Err: err}
}

func credentialsCause(err error) bool {
	var signing *v4.SigningError
	if errors.As(err, &signing) {
		return true
	}
	var api smithy.APIError
	if errors.As(err, &api) && credentialErrorCodes[api.ErrorCode()] {
		return true
	}
	message := err.Error()
	return strings.Contains(message, "failed to retrieve credentials") ||
		strings.Contains(message, "failed to refresh cached credentials") ||
		strings.Contains(message, "get identity:")
}
