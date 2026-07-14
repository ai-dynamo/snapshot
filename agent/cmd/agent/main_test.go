// Copyright 2026 NVIDIA Corporation
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestServerEndpoints verifies the health and version endpoints respond.
func TestServerEndpoints(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{path: "/healthz", want: "ok\n"},
		{path: "/version", want: "dev\n"},
	}

	srv := newServer()
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			rec := httptest.NewRecorder()

			srv.Handler.ServeHTTP(rec, req)

			require.Equal(t, http.StatusOK, rec.Code)
			body, err := io.ReadAll(rec.Result().Body)
			require.NoError(t, err)
			assert.Equal(t, tc.want, string(body))
		})
	}
}
