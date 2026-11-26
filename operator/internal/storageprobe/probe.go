// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package storageprobe checks that a snapshot store can be used. It writes a
// test object where snapshots live, reads it back, finds it in a listing and
// deletes it.
package storageprobe

import (
	"context"
	"fmt"
	"path"
	"strings"
)

const (
	StepWrite  = "write"
	StepRead   = "read"
	StepList   = "list"
	StepDelete = "delete"

	artifactsDirectory = "artifacts"
	accessCheckPrefix  = ".access-check-"
)

var accessCheckPayload = []byte("snapshot storage access check\n")

// Failure is a failed access check. Step says where it failed. Credentials says
// the credentials are the cause, not the store or the network.
type Failure struct {
	Step        string
	Credentials bool
	Err         error
}

func (f *Failure) Error() string {
	if f.Step == "" {
		return f.Err.Error()
	}
	return fmt.Sprintf("%s: %v", f.Step, f.Err)
}

func (f *Failure) Unwrap() error { return f.Err }

// Target is one store to check. Exactly one of PVC and S3 is set.
type Target struct {
	// ClusterID names the cluster in the test object, so two clusters that
	// share a store do not collide.
	ClusterID string
	PVC       *PVC
	S3        *S3
}

// PVC is a mounted claim and a directory in it.
type PVC struct {
	MountPath string
	BasePath  string
}

// S3 is a bucket and how to reach it. Credentials is a shared-credentials file;
// empty means the workload identity of the operator. CABundle is PEM
// certificates; empty means system trust.
type S3 struct {
	Endpoint    string
	Region      string
	Bucket      string
	Prefix      string
	PathStyle   bool
	AllowHTTP   bool
	Credentials []byte
	CABundle    []byte
}

// Prober checks a store. A failure is a *Failure.
type Prober interface {
	Check(ctx context.Context, target Target) error
}

// Default checks PVC stores on the local filesystem and S3 stores over the network.
type Default struct{}

func (Default) Check(ctx context.Context, target Target) error {
	switch {
	case target.PVC != nil && target.S3 == nil:
		return checkPVC(ctx, target.ClusterID, *target.PVC)
	case target.S3 != nil && target.PVC == nil:
		return checkS3(ctx, target.ClusterID, *target.S3)
	default:
		return &Failure{Err: fmt.Errorf("exactly one of pvc or s3 must be set")}
	}
}

func accessCheckName(clusterID string) string {
	return accessCheckPrefix + clusterID
}

func accessCheckKey(prefix, clusterID string) string {
	return path.Join(strings.Trim(prefix, "/"), artifactsDirectory, accessCheckName(clusterID))
}
