// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package backends

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeS3API struct {
	listPages     []*s3.ListObjectsV2Output
	deleteInputs  []*s3.DeleteObjectsInput
	deleteOutputs []*s3.DeleteObjectsOutput
	deleteErr     error
	listErr       error
}

func (f *fakeS3API) ListObjectsV2(_ context.Context, _ *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	page := f.listPages[0]
	f.listPages = f.listPages[1:]
	return page, nil
}

func (f *fakeS3API) DeleteObjects(_ context.Context, params *s3.DeleteObjectsInput, _ ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error) {
	f.deleteInputs = append(f.deleteInputs, params)
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	if len(f.deleteOutputs) > 0 {
		out := f.deleteOutputs[0]
		f.deleteOutputs = f.deleteOutputs[1:]
		return out, nil
	}
	return &s3.DeleteObjectsOutput{}, nil
}

func newTestS3Backend(api s3API) *S3Backend {
	return &S3Backend{client: api, bucket: "checkpoints", artifactsPrefix: "snapshots/artifacts/"}
}

func TestS3BackendName(t *testing.T) {
	assert.Equal(t, "S3", newTestS3Backend(nil).Name())
}

func TestS3BackendDeleteRemovesEveryObjectUnderTheContentPrefix(t *testing.T) {
	api := &fakeS3API{listPages: []*s3.ListObjectsV2Output{{
		Contents: []s3types.Object{
			{Key: aws.String("snapshots/artifacts/uid-1/containers/main/index")},
			{Key: aws.String("snapshots/artifacts/uid-1/containers/main/data")},
		},
		IsTruncated: aws.Bool(false),
	}}}
	b := newTestS3Backend(api)

	require.NoError(t, b.Delete(context.Background(), "uid-1"))

	require.Len(t, api.deleteInputs, 1)
	assert.Equal(t, "checkpoints", aws.ToString(api.deleteInputs[0].Bucket))
	assert.Len(t, api.deleteInputs[0].Delete.Objects, 2)
}

func TestS3BackendDeleteIsANoopWhenNothingExists(t *testing.T) {
	api := &fakeS3API{listPages: []*s3.ListObjectsV2Output{{IsTruncated: aws.Bool(false)}}}
	b := newTestS3Backend(api)

	require.NoError(t, b.Delete(context.Background(), "uid-absent"))
	assert.Empty(t, api.deleteInputs)
}

func TestS3BackendDeletePaginatesListing(t *testing.T) {
	api := &fakeS3API{listPages: []*s3.ListObjectsV2Output{
		{Contents: []s3types.Object{{Key: aws.String("snapshots/artifacts/uid-1/a")}}, IsTruncated: aws.Bool(true), NextContinuationToken: aws.String("next")},
		{Contents: []s3types.Object{{Key: aws.String("snapshots/artifacts/uid-1/b")}}, IsTruncated: aws.Bool(false)},
	}}
	b := newTestS3Backend(api)

	require.NoError(t, b.Delete(context.Background(), "uid-1"))
	require.Len(t, api.deleteInputs, 2)
}

func TestS3BackendDeleteBatchesOverOneThousandObjects(t *testing.T) {
	objects := make([]s3types.Object, deleteObjectsBatchSize+1)
	for i := range objects {
		objects[i] = s3types.Object{Key: aws.String("snapshots/artifacts/uid-1/o")}
	}
	api := &fakeS3API{listPages: []*s3.ListObjectsV2Output{{Contents: objects, IsTruncated: aws.Bool(false)}}}
	b := newTestS3Backend(api)

	require.NoError(t, b.Delete(context.Background(), "uid-1"))

	require.Len(t, api.deleteInputs, 2)
	assert.Len(t, api.deleteInputs[0].Delete.Objects, deleteObjectsBatchSize)
	assert.Len(t, api.deleteInputs[1].Delete.Objects, 1)
}

func TestS3BackendDeleteFailsOnPartialDeleteErrors(t *testing.T) {
	api := &fakeS3API{
		listPages: []*s3.ListObjectsV2Output{{
			Contents:    []s3types.Object{{Key: aws.String("snapshots/artifacts/uid-1/a")}},
			IsTruncated: aws.Bool(false),
		}},
		deleteOutputs: []*s3.DeleteObjectsOutput{{
			Errors: []s3types.Error{{Key: aws.String("snapshots/artifacts/uid-1/a"), Message: aws.String("access denied")}},
		}},
	}
	b := newTestS3Backend(api)

	err := b.Delete(context.Background(), "uid-1")
	require.ErrorContains(t, err, "access denied")
}

func TestS3BackendCandidatesListsUIDsFromCommonPrefixes(t *testing.T) {
	api := &fakeS3API{listPages: []*s3.ListObjectsV2Output{{
		CommonPrefixes: []s3types.CommonPrefix{
			{Prefix: aws.String("snapshots/artifacts/uid-1/")},
			{Prefix: aws.String("snapshots/artifacts/uid-2/")},
		},
		IsTruncated: aws.Bool(false),
	}}}
	b := newTestS3Backend(api)

	candidates, err := b.Candidates(context.Background(), logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, map[string]struct{}{"uid-1": {}, "uid-2": {}}, candidates)
}

func TestS3BackendCandidatesPaginates(t *testing.T) {
	api := &fakeS3API{listPages: []*s3.ListObjectsV2Output{
		{CommonPrefixes: []s3types.CommonPrefix{{Prefix: aws.String("snapshots/artifacts/uid-1/")}}, IsTruncated: aws.Bool(true), NextContinuationToken: aws.String("next")},
		{CommonPrefixes: []s3types.CommonPrefix{{Prefix: aws.String("snapshots/artifacts/uid-2/")}}, IsTruncated: aws.Bool(false)},
	}}
	b := newTestS3Backend(api)

	candidates, err := b.Candidates(context.Background(), logr.Discard())
	require.NoError(t, err)
	assert.Equal(t, map[string]struct{}{"uid-1": {}, "uid-2": {}}, candidates)
}

func TestS3BackendCandidatesReturnsErrorOnListFailure(t *testing.T) {
	api := &fakeS3API{listErr: assert.AnError}
	b := newTestS3Backend(api)

	_, err := b.Candidates(context.Background(), logr.Discard())
	require.Error(t, err)
}
