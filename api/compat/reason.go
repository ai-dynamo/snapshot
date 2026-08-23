// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package compat

import (
	"fmt"
	"strings"

	"github.com/ai-dynamo/snapshot/api/util"
)

// reasonSeparator joins the reasons of a refusal that failed several rules.
const reasonSeparator = "; "

// Reason renders a mismatch as the sentence a user reads. The log field, the pod
// event and the pod annotation all carry this exact string, so an operator can
// match on one and find the others.
func (m Mismatch) Reason() string {
	return fmt.Sprintf("%s: source %s, target %s", m.Check, util.OrUnknown(m.Source), util.OrUnknown(m.Target))
}

// Reasons renders every mismatch of one refusal in report order.
func Reasons(mismatches []Mismatch) string {
	if len(mismatches) == 0 {
		return ""
	}
	reasons := make([]string, 0, len(mismatches))
	for _, mismatch := range mismatches {
		reasons = append(reasons, mismatch.Reason())
	}
	return strings.Join(reasons, reasonSeparator)
}
