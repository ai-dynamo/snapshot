# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

FROM golang:1.27.2@sha256:5bc7f572bbaa98885a3a1fd9c0aa76b59e3e14e8628bfc316bbfd0c701e4818c AS build
ENV GOTOOLCHAIN=local GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH=amd64
WORKDIR /src/agent
COPY api/go.mod api/go.sum /src/api/
COPY agent/go.mod agent/go.sum ./
COPY agent/internal/pagebroker/ internal/pagebroker/
RUN test "$(go env GOVERSION)" = "go$(awk '/^go / { print $2 }' go.mod)" \
    && go test ./internal/pagebroker -run '^TestSmoke' \
    && go test -c ./internal/pagebroker -o /pagebroker-smoke.test

FROM python:3.12-slim@sha256:a6e34c598f2467ed0e9a8d349809fcd8b5c603269512df273a0bb1784edc11b1 AS runtime
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1
COPY --from=build /pagebroker-smoke.test /usr/local/bin/pagebroker-smoke.test
COPY agent/pagebroker/tests/k3s/run.py agent/pagebroker/tests/k3s/monitor.py /opt/smoke/
USER 65532:65532
ENTRYPOINT ["python3", "/opt/smoke/run.py"]
