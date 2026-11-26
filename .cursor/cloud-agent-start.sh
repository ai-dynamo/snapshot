#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Start Docker on every agent boot (nested Cloud Agent VM).

set -euo pipefail

if [[ ! -S /var/run/docker.sock ]]; then
  if command -v service >/dev/null 2>&1; then
    sudo service docker start || true
  elif command -v systemctl >/dev/null 2>&1; then
    sudo systemctl start docker || true
  fi
fi

# Allow the ubuntu user to talk to Docker without a login shell group refresh.
if [[ -S /var/run/docker.sock ]] && [[ -w /var/run/docker.sock ]]; then
  exit 0
fi
if [[ -S /var/run/docker.sock ]]; then
  sudo chmod 666 /var/run/docker.sock 2>/dev/null || true
fi

if docker info >/dev/null 2>&1; then
  exit 0
fi

echo "Docker did not become ready; see /tmp/cursor/start-user/start-user.log" >&2
exit 1
