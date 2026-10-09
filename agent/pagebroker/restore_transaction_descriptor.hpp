// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include "transfer_engine.hpp"
#include "file_descriptor.hpp"
#include "gpu/engine.hpp"

namespace snapshot::pagebroker {
class RestoreTransactionDescriptor {
 public:
  explicit RestoreTransactionDescriptor(Path staging_directory, FileDescriptor source = FileDescriptor(-1),
                                        std::shared_ptr<gpu::RestorePreparation> preparation = {});

  const Path& staging_directory() const;
  const auto& preparation() const { return preparation_; }
  int source_fd() const
  {
    return source_.get();
  }

 private:
  std::shared_ptr<gpu::RestorePreparation> preparation_;
  Path staging_directory_;
  FileDescriptor source_;
};
}  // namespace snapshot::pagebroker
