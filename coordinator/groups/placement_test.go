// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package groups_test

import (
	"github.com/ai-dynamo/snapshot/coordinator/agentauth"
	"github.com/ai-dynamo/snapshot/coordinator/groups"
)

// The attempts are the Placement of the Authorizer a host builds before its
// server, although package groups does not import agentauth.
var _ agentauth.Placement = (*groups.Attempts)(nil)
