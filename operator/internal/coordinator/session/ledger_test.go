// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sentinels is every sentinel errors.go declares, by name, including the ones
// it re-exports from kv. TestSentinelsListsEveryOneErrorsGoDeclares keeps it
// complete, so a sentinel added later cannot escape the identity test.
var sentinels = map[string]error{
	"ErrNotFound":       ErrNotFound,
	"ErrExists":         ErrExists,
	"ErrMismatch":       ErrMismatch,
	"ErrNotCounter":     ErrNotCounter,
	"ErrBarrierCount":   ErrBarrierCount,
	"ErrNotYet":         ErrNotYet,
	"ErrAborted":        ErrAborted,
	"ErrClosed":         ErrClosed,
	"ErrUnknownSession": ErrUnknownSession,
	"ErrNotMember":      ErrNotMember,
	"ErrForbidden":      ErrForbidden,
	"ErrTooLarge":       ErrTooLarge,
	"ErrTooMany":        ErrTooMany,
	"ErrInvalid":        ErrInvalid,
}

func TestSentinelsListsEveryOneErrorsGoDeclares(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "errors.go", nil, 0)
	require.NoError(t, err)
	var declared []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			for _, name := range spec.(*ast.ValueSpec).Names {
				if strings.HasPrefix(name.Name, "Err") {
					declared = append(declared, name.Name)
				}
			}
		}
	}

	assert.ElementsMatch(t, declared, slices.Collect(maps.Keys(sentinels)))
}

func TestARecordKeepsAnErrorsIdentityAndNeverItsMessage(t *testing.T) {
	const named = "the-key-it-named"
	type identity struct {
		err  error
		want error
		kept bool
	}
	tests := map[string]identity{
		"an abort, with its reason": {
			err:  fmt.Errorf("key %q: %w", named, &AbortError{Reason: "NodeLost"}),
			want: &AbortError{Reason: "NodeLost"},
			kept: true,
		},
		"no error":                          {kept: true},
		"a backend failure, which has none": {err: fmt.Errorf("key %q: %w", named, errors.New("connection reset"))},
	}
	for name, sentinel := range sentinels {
		tests[name] = identity{err: fmt.Errorf("key %q: %w", named, sentinel), want: sentinel, kept: true}
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			kept, ok := identityOf(tt.err)

			require.Equal(t, tt.kept, ok)
			assert.Equal(t, tt.want, kept)
			if kept != nil {
				assert.NotContains(t, kept.Error(), named)
			}
		})
	}
}
