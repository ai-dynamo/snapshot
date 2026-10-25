// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package coordination is the shared contract maintenance backends and
// checkpoint/restore callers enforce before mutating a published artifact.
// No storage I/O here, just identity validation and locking.
package coordination
