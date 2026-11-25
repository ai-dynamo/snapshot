// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storageprobe

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSharedCredentials(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		key   string
		token string
		valid bool
	}{
		{"default profile", "[default]\naws_access_key_id = AKIDEXAMPLE\naws_secret_access_key = secret\n", "AKIDEXAMPLE", "", true},
		{"session token", "[default]\naws_access_key_id=AKIDEXAMPLE\naws_secret_access_key=secret\naws_session_token=token\n", "AKIDEXAMPLE", "token", true},
		{"comments and blanks", "# note\n\n; note\n[default]\n  aws_access_key_id = AKIDEXAMPLE\n  aws_secret_access_key = secret\n", "AKIDEXAMPLE", "", true},
		{"other profile only", "[other]\naws_access_key_id = AKIDEXAMPLE\naws_secret_access_key = secret\n", "", "", false},
		{"default after another", "[other]\naws_access_key_id = WRONG\naws_secret_access_key = wrong\n[default]\naws_access_key_id = AKIDEXAMPLE\naws_secret_access_key = secret\n", "AKIDEXAMPLE", "", true},
		{"no secret", "[default]\naws_access_key_id = AKIDEXAMPLE\n", "", "", false},
		{"empty", "", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			credentials, err := ParseSharedCredentials([]byte(tc.input))
			if !tc.valid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.key, credentials.AccessKeyID)
			assert.Equal(t, tc.token, credentials.SessionToken)
		})
	}
}

func TestParseSharedCredentialsErrorNeverEchoesTheFile(t *testing.T) {
	_, err := ParseSharedCredentials([]byte("[default]\naws_access_key_id = AKIDSECRETVALUE\n"))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "AKIDSECRETVALUE")
}
