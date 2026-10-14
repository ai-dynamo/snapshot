// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package backends

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	operatortypes "github.com/ai-dynamo/snapshot/operator/internal/types"
)

func TestNewS3ConfigCopiesEveryField(t *testing.T) {
	cfg := operatortypes.S3Config{
		Bucket: "checkpoints", Prefix: "snapshots", Region: "us-east-1",
		Endpoint: "https://s3.example.com", CredentialsPath: "/etc/creds", CABundlePath: "/etc/ca.pem",
	}
	assert.Equal(t, S3Config{
		Bucket: "checkpoints", Prefix: "snapshots", Region: "us-east-1",
		Endpoint: "https://s3.example.com", CredentialsPath: "/etc/creds", CABundlePath: "/etc/ca.pem",
	}, NewS3Config(cfg))
}

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

func TestS3ArtifactsPrefix(t *testing.T) {
	for prefix, want := range map[string]string{
		"":                "artifacts/",
		"/":               "artifacts/",
		"snapshots":       "snapshots/artifacts/",
		"/snapshots/":     "snapshots/artifacts/",
		"team/snapshots/": "team/snapshots/artifacts/",
	} {
		assert.Equal(t, want, s3ArtifactsPrefix(prefix), "prefix %q", prefix)
	}
}

func writeS3Credentials(t *testing.T, path, accessKeyID string) {
	t.Helper()
	content := "[default]\naws_access_key_id = " + accessKeyID + "\naws_secret_access_key = fake-secret\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

func newListObjectsServer(t *testing.T, newServer func(http.Handler) *httptest.Server) (*httptest.Server, *[]*http.Request) {
	t.Helper()
	var requests []*http.Request
	server := newServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Clone(context.Background()))
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>`))
	}))
	t.Cleanup(server.Close)
	return server, &requests
}

func TestNewS3BackendReachesEndpointWithoutCABundle(t *testing.T) {
	server, requests := newListObjectsServer(t, httptest.NewServer)
	credentialsPath := filepath.Join(t.TempDir(), "credentials")
	writeS3Credentials(t, credentialsPath, "FAKEACCESSKEY")

	b, err := NewS3Backend(context.Background(), S3Config{
		Bucket: "checkpoints", Region: "us-east-1", Endpoint: server.URL, CredentialsPath: credentialsPath,
	})
	require.NoError(t, err)

	_, err = b.Candidates(context.Background(), logr.Discard())
	require.NoError(t, err)
	require.Len(t, *requests, 1)
	assert.Equal(t, "artifacts/", (*requests)[0].URL.Query().Get("prefix"))
	assert.Contains(t, (*requests)[0].Header.Get("Authorization"), "Credential=FAKEACCESSKEY/")
}

func TestNewS3BackendTrustsCustomCABundle(t *testing.T) {
	server, requests := newListObjectsServer(t, httptest.NewTLSServer)
	dir := t.TempDir()
	caBundlePath := filepath.Join(dir, "ca.pem")
	require.NoError(t, os.WriteFile(caBundlePath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600))
	credentialsPath := filepath.Join(dir, "credentials")
	writeS3Credentials(t, credentialsPath, "FAKEACCESSKEY")

	b, err := NewS3Backend(context.Background(), S3Config{
		Bucket: "checkpoints", Region: "us-east-1", Endpoint: server.URL, CredentialsPath: credentialsPath, CABundlePath: caBundlePath,
	})
	require.NoError(t, err)

	_, err = b.Candidates(context.Background(), logr.Discard())
	require.NoError(t, err)
	assert.Len(t, *requests, 1)
}

func TestNewS3BackendRejectsCABundleWithoutCertificates(t *testing.T) {
	caBundlePath := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(caBundlePath, []byte("not a certificate"), 0o600))

	_, err := NewS3Backend(context.Background(), S3Config{
		Bucket: "checkpoints", Region: "us-east-1", CredentialsPath: "unused", CABundlePath: caBundlePath,
	})
	require.ErrorContains(t, err, "contains no usable certificates")
}

func TestSharedCredentialsFileProviderRereadsRotatedCredentials(t *testing.T) {
	credentialsPath := filepath.Join(t.TempDir(), "credentials")
	provider := sharedCredentialsFileProvider(credentialsPath)

	writeS3Credentials(t, credentialsPath, "FIRSTKEY")
	first, err := provider.Retrieve(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "FIRSTKEY", first.AccessKeyID)
	assert.True(t, first.CanExpire)

	writeS3Credentials(t, credentialsPath, "ROTATEDKEY")
	rotated, err := provider.Retrieve(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "ROTATEDKEY", rotated.AccessKeyID)
}

func TestSharedCredentialsFileProviderFailsWithoutKeys(t *testing.T) {
	credentialsPath := filepath.Join(t.TempDir(), "credentials")
	require.NoError(t, os.WriteFile(credentialsPath, []byte("[default]\nregion = us-east-1\n"), 0o600))

	_, err := sharedCredentialsFileProvider(credentialsPath).Retrieve(context.Background())
	require.Error(t, err)
}
