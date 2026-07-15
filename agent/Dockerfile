# Copyright 2026 NVIDIA Corporation
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Build context is the repository root so the api/ module (referenced via the
# replace directive in agent/go.mod) is available inside the build.
FROM golang:1.26-alpine AS builder
WORKDIR /workspace
COPY api/ api/
COPY agent/ agent/
WORKDIR /workspace/agent
RUN go build -o bin/agent ./cmd/agent

FROM ubuntu:22.04
RUN apt-get update && apt-get install -y criu && rm -rf /var/lib/apt/lists/*
COPY --from=builder /workspace/agent/bin/agent /agent
ENTRYPOINT ["/agent"]
