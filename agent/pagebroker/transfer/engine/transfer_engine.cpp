// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "transfer_engine.hpp"

#include "transfer/filesystem/filesystem_storage.hpp"

namespace snapshot::pagebroker {
TransferEngine::~TransferEngine() = default;

RestorePlan
TransferEngine::InspectCheckpoint(const Path& source, const PublishedArtifact* artifact, TransferControl control) const
{
  if (artifact)
    throw std::invalid_argument("transfer engine does not support artifacts");
  return filesystem_storage::BuildRestorePlan(source, control);
}

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
