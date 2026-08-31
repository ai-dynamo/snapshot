// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

type fakeContainerStatusService struct {
	response *runtimeapi.ContainerStatusResponse
	err      error
	verbose  bool
}

func (f *fakeContainerStatusService) ContainerStatus(_ context.Context, _ string, verbose bool) (*runtimeapi.ContainerStatusResponse, error) {
	f.verbose = verbose
	return f.response, f.err
}

func TestResolveContainerImageID(t *testing.T) {
	tests := []struct {
		name      string
		service   *fakeContainerStatusService
		want      string
		wantError string
	}{
		{
			name: "returns the runtime image ID",
			service: &fakeContainerStatusService{response: &runtimeapi.ContainerStatusResponse{
				Status: &runtimeapi.ContainerStatus{ImageId: "sha256:config"},
			}},
			want: "sha256:config",
		},
		{
			name:      "rejects a missing status",
			service:   &fakeContainerStatusService{response: &runtimeapi.ContainerStatusResponse{}},
			wantError: "has no image ID",
		},
		{
			name:      "reports the runtime error",
			service:   &fakeContainerStatusService{err: errors.New("unavailable")},
			wantError: "unavailable",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveContainerImageID(context.Background(), tc.service, "container-id")
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("error = %v, want containing %q", err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveContainerImageID: %v", err)
			}
			if got != tc.want {
				t.Fatalf("image ID = %q, want %q", got, tc.want)
			}
			if tc.service.verbose {
				t.Fatal("ContainerStatus requested verbose output")
			}
		})
	}
}
