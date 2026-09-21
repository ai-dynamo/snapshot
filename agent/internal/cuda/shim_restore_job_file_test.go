// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cuda

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr"

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

func TestRunActionInheritsJobFileEnvironment(t *testing.T) {
	trace := filepath.Join(t.TempDir(), "trace")
	installFakeCUDAHelper(t, "printf '%s' \"$CUDA_CHECKPOINT_JOB_FILE\" > \""+trace+"\"\n")
	t.Setenv(JobFileEnv, podcontract.CUDAJobFilePath)

	if err := runAction(context.Background(), 11, actionRestore, "", cudaCheckpointHelperBinary, logr.Discard()); err != nil {
		t.Fatalf("runAction() error = %v", err)
	}
	content, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(content), podcontract.CUDAJobFilePath; got != want {
		t.Fatalf("helper environment = %q, want %q", got, want)
	}
}
