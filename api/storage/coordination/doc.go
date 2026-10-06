// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package coordination is the shared contract that any maintenance backend
// (PVC today, S3 later) and the agent's checkpoint/restore callers enforce
// before mutating a published artifact. It performs no storage I/O itself:
// identity validation, keyed locking, publication-evidence matching, and the
// SNEP Stage 1 deletion window are defined here so every caller gets the
// same enforcement, not reimplemented per backend.
package coordination
