/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved. SPDX-License-Identifier: Apache-2.0
 */

#pragma once

#include <cstdint>
#include <string>

namespace cuda_checkpoint_server {

int RunDaemon(const std::string &socket_path, uint64_t max_operation_seconds);
int RunHealthClient(const std::string &socket_path);

} // namespace cuda_checkpoint_server
