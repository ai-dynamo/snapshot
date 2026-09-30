// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0
#pragma once

#include <optional>

namespace snapshot::cuda_checkpoint {
// nullopt leaves ordinary one-shot CUDA commands to the existing C CLI.
std::optional<int> RunClient(int argc, char** argv);
} // namespace snapshot::cuda_checkpoint
