// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import "fmt"

// CheckpointCommitError means the dump finished but publication was not confirmed by its Commit reply.
// The controller must inspect durable publication evidence before deciding the capture failed.
type CheckpointCommitError struct {
	Err error
}

func (e *CheckpointCommitError) Error() string {
	return fmt.Sprintf("commit PageBroker checkpoint: %v", e.Err)
}
func (e *CheckpointCommitError) Unwrap() error { return e.Err }
