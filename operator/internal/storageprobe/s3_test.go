// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storageprobe

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeObjects struct {
	objects map[string][]byte
	failOn  string
	err     error
	calls   []string
	corrupt bool
	hide    bool
}

func (f *fakeObjects) fail(step string) error {
	f.calls = append(f.calls, step)
	if f.failOn == step {
		return f.err
	}
	return nil
}

func (f *fakeObjects) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	if err := f.fail(StepWrite); err != nil {
		return nil, err
	}
	data, _ := io.ReadAll(in.Body)
	if f.objects == nil {
		f.objects = map[string][]byte{}
	}
	f.objects[aws.ToString(in.Key)] = data
	return &s3.PutObjectOutput{}, nil
}

func (f *fakeObjects) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	if err := f.fail(StepRead); err != nil {
		return nil, err
	}
	data := f.objects[aws.ToString(in.Key)]
	if f.corrupt {
		data = []byte("other")
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(data))}, nil
}

func (f *fakeObjects) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	if err := f.fail(StepList); err != nil {
		return nil, err
	}
	out := &s3.ListObjectsV2Output{}
	if f.hide {
		return out, nil
	}
	for key := range f.objects {
		out.Contents = append(out.Contents, s3types.Object{Key: aws.String(key)})
	}
	return out, nil
}

func (f *fakeObjects) DeleteObject(_ context.Context, in *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	if err := f.fail(StepDelete); err != nil {
		return nil, err
	}
	delete(f.objects, aws.ToString(in.Key))
	return &s3.DeleteObjectOutput{}, nil
}

func TestS3CheckRunsTheFourStepsAndLeavesNothing(t *testing.T) {
	api := &fakeObjects{}
	require.NoError(t, runS3Check(t.Context(), api, "bucket", "team-a/artifacts/.access-check-c"))
	assert.Equal(t, []string{StepWrite, StepRead, StepList, StepDelete}, api.calls)
	assert.Empty(t, api.objects)
}

func TestS3CheckNamesTheStepThatFailed(t *testing.T) {
	denied := &smithy.GenericAPIError{Code: "AccessDenied", Message: "denied"}
	for _, step := range []string{StepWrite, StepRead, StepList, StepDelete} {
		t.Run(step, func(t *testing.T) {
			err := runS3Check(t.Context(), &fakeObjects{failOn: step, err: denied}, "bucket", "k")
			var failure *Failure
			require.ErrorAs(t, err, &failure)
			assert.Equal(t, step, failure.Step)
			assert.False(t, failure.Credentials, "a policy that denies access is not a credentials problem")
		})
	}
}

func TestS3CheckFailsWhenTheObjectReadBackDiffers(t *testing.T) {
	err := runS3Check(t.Context(), &fakeObjects{corrupt: true}, "bucket", "k")
	var failure *Failure
	require.ErrorAs(t, err, &failure)
	assert.Equal(t, StepRead, failure.Step)
}

func TestS3CheckFailsWhenTheObjectIsNotListed(t *testing.T) {
	err := runS3Check(t.Context(), &fakeObjects{hide: true}, "bucket", "k")
	var failure *Failure
	require.ErrorAs(t, err, &failure)
	assert.Equal(t, StepList, failure.Step)
}

func TestCredentialsCause(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"signing failed", &v4.SigningError{Err: errors.New("failed to retrieve credentials")}, true},
		{"unknown key", &smithy.GenericAPIError{Code: "InvalidAccessKeyId"}, true},
		{"bad signature", &smithy.GenericAPIError{Code: "SignatureDoesNotMatch"}, true},
		{"expired token", &smithy.GenericAPIError{Code: "ExpiredToken"}, true},
		{"provider message", errors.New("operation error S3: PutObject, failed to retrieve credentials: no provider"), true},
		{"identity did not resolve", errors.New("operation error S3: PutObject, get identity: get credentials: failed to refresh cached credentials, no EC2 IMDS role found"), true},
		{"access denied", &smithy.GenericAPIError{Code: "AccessDenied"}, false},
		{"no such bucket", &smithy.GenericAPIError{Code: "NoSuchBucket"}, false},
		{"network", errors.New("dial tcp: connection refused"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, credentialsCause(tc.err))
		})
	}
}

func clearAWSEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_PROFILE",
		"AWS_ROLE_ARN", "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_CONTAINER_CREDENTIALS_FULL_URI",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE",
		"AWS_SHARED_CREDENTIALS_FILE", "AWS_CONFIG_FILE",
	} {
		t.Setenv(name, "")
	}
}

func TestDefaultCheckWithoutCredentialsFailsAsACredentialsProblem(t *testing.T) {
	clearAWSEnvironment(t)
	err := Default{}.Check(t.Context(), Target{ClusterID: "c", S3: &S3{
		Endpoint: "https://127.0.0.1:1", Region: "region-1", Bucket: "bucket",
	}})

	var failure *Failure
	require.ErrorAs(t, err, &failure)
	assert.True(t, failure.Credentials)
	assert.Equal(t, StepWrite, failure.Step)
}

func TestDefaultCheckRejectsAnUnreadableCredentialsFileWithoutEchoingIt(t *testing.T) {
	err := Default{}.Check(t.Context(), Target{ClusterID: "c", S3: &S3{
		Endpoint: "https://127.0.0.1:1", Region: "region-1", Bucket: "bucket",
		Credentials: []byte("[default]\naws_access_key_id = AKIDSECRETVALUE\n"),
	}})

	var failure *Failure
	require.ErrorAs(t, err, &failure)
	assert.True(t, failure.Credentials)
	assert.NotContains(t, err.Error(), "AKIDSECRETVALUE")
}

func TestDefaultCheckRefusesPlainHTTPUnlessAllowed(t *testing.T) {
	err := Default{}.Check(t.Context(), Target{ClusterID: "c", S3: &S3{
		Endpoint: "http://127.0.0.1:1", Region: "region-1", Bucket: "bucket",
		Credentials: []byte("[default]\naws_access_key_id = AKIDEXAMPLE\naws_secret_access_key = secret\n"),
	}})

	var failure *Failure
	require.ErrorAs(t, err, &failure)
	assert.Empty(t, failure.Step)
	assert.Contains(t, failure.Error(), "allowHttp")
}

func TestDefaultCheckRejectsACABundleWithNoCertificates(t *testing.T) {
	err := Default{}.Check(t.Context(), Target{ClusterID: "c", S3: &S3{
		Endpoint: "https://127.0.0.1:1", Region: "region-1", Bucket: "bucket",
		Credentials: []byte("[default]\naws_access_key_id = AKIDEXAMPLE\naws_secret_access_key = secret\n"),
		CABundle:    []byte("not a certificate"),
	}})

	var failure *Failure
	require.ErrorAs(t, err, &failure)
	assert.False(t, failure.Credentials)
	assert.Contains(t, failure.Error(), "no usable certificates")
}

func TestAccessCheckKeyIsInsideTheArtifactsDirectory(t *testing.T) {
	assert.Equal(t, "team-a/artifacts/.access-check-c", accessCheckKey("/team-a/", "c"))
	assert.Equal(t, "artifacts/.access-check-c", accessCheckKey("", "c"))
}
