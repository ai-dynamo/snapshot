// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "transfer/engine/transfer_engine.hpp"


namespace snapshot::pagebroker {
Failure::Code
TransferFailureCode(const std::exception& error)
{
  if (const auto* transfer = dynamic_cast<const TransferError*>(&error))
    return transfer->code;
  // The wire protocol has no cancellation code; an explicit stop is not an I/O failure.
  if (const auto* interrupted = dynamic_cast<const CancellationError*>(&error))
    return interrupted->reason == CancellationError::Reason::DEADLINE_EXCEEDED
        ? Failure::TRANSACTION_EXPIRED : Failure::INTERNAL_ERROR;
  return Failure::STORAGE_ERROR;
}

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
PreparedRestore TransferEngine::PrepareRestore(const StorageBackend&, Cancellation, const PublishedArtifact*, bool, TransferMode) const { UnsupportedOperation(); }
RestoreAccess TransferEngine::StageRestore(PreparedRestore, const Path&, Cancellation) const { UnsupportedOperation(); }
Path TransferEngine::PrepareCheckpoint(const StorageBackend&, const Path&, const std::string&, TransferMode, const PublishedArtifact*, Cancellation) const { UnsupportedOperation(); }
void TransferEngine::ValidateCheckpointDestination(const StorageBackend&, const PublishedArtifact*, Cancellation) const { UnsupportedOperation(); }
CheckpointInventory TransferEngine::InspectCheckpoint(const Path&, const PublishedArtifact*, Cancellation) const { UnsupportedOperation(); }
void TransferEngine::PublishCheckpoint(const Path&, const StorageBackend&, CheckpointInventory, CheckpointPublication*, Cancellation, TransferMode) const { UnsupportedOperation(); }


PublishedArtifact
TransferEngine::ResolveArtifactTarget(const ArtifactTarget&) const
{
  throw std::invalid_argument("transfer engine does not support artifacts");
}

void
TransferEngine::ValidateArtifact(const PublishedArtifact&) const
{
  throw std::invalid_argument("transfer engine does not support artifacts");
}
}  // namespace snapshot::pagebroker
