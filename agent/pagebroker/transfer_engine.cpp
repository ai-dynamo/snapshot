// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "transfer_engine.hpp"

#include <stdexcept>

namespace snapshot::pagebroker {
TransferEngine::~TransferEngine() = default;
namespace {
[[noreturn]] void UnsupportedOperation()
{
  throw std::logic_error("operation is not supported by the selected transfer engine");
}
}  // namespace

void TransferEngine::Open(int, size_t) { UnsupportedOperation(); }
void TransferEngine::Submit(size_t, io::Operation, size_t, size_t) { UnsupportedOperation(); }
void TransferEngine::Wait(size_t) { UnsupportedOperation(); }
void TransferEngine::Close() { UnsupportedOperation(); }
Path TransferEngine::DestinationDirectory(const StorageBackend&) const { UnsupportedOperation(); }
Path TransferEngine::SourceDirectory(const StorageBackend&) const { UnsupportedOperation(); }
uintmax_t TransferEngine::RestoreSize(const StorageBackend&) const { UnsupportedOperation(); }
void TransferEngine::StageRestore(const StorageBackend&, const Path&) const { UnsupportedOperation(); }
void TransferEngine::ValidateCheckpointDestination(const StorageBackend&) const { UnsupportedOperation(); }
bool TransferEngine::CheckpointDestinationConflicts(const StorageBackend&) const { UnsupportedOperation(); }
void TransferEngine::PublishCheckpoint(const Path&, const StorageBackend&) const { UnsupportedOperation(); }
void TransferEngine::CopyDirectory(const Path&, const Path&) const { UnsupportedOperation(); }
}  // namespace snapshot::pagebroker
