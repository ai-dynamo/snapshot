// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <string>

namespace snapshot::pagebroker {

// ComputeStoreID matches api/storage/identity.go's PVC.StoreID() exactly: the
// v1 preimage is five length-prefixed UTF-8 fields ("snapshot.store/v1",
// "pvc", namespace, claimName, normalizedBasePath), each preceded by an
// unsigned 32-bit big-endian byte length, hashed with SHA-256. basePath is
// normalized (lexically cleaned) the same way Go's NormalizedBasePath is,
// rejecting traversal rather than silently identifying a different store.
// Throws std::invalid_argument on inputs Go's validation would also reject.
std::string ComputeStoreID(const std::string& ns, const std::string& claim_name, const std::string& base_path);

// ValidateArtifactIdentity matches api/storage/identity.go's
// ValidateArtifactIdentity exactly: store_id is store-v1- plus 64 lowercase
// hex characters, artifact_uid is an opaque Kubernetes UID (nonempty, at most
// 256 bytes, valid UTF-8, no whitespace, control characters or path
// separators) and container_name is a DNS1123 label. Throws
// std::invalid_argument on the first violation.
void ValidateArtifactIdentity(
    const std::string& store_id, const std::string& artifact_uid, const std::string& container_name);

// Sha256Hex is exposed for tests that want to check the preimage encoding
// directly rather than only the final store ID.
std::string Sha256Hex(const std::string& data);

}  // namespace snapshot::pagebroker
