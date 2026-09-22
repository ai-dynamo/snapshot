// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <cstdint>
#include <filesystem>

namespace snapshot::pagebroker {

using Path = std::filesystem::path;

// Recursively archives every regular file under `source` (except `exclude`,
// matched by filename) into a gzip-compressed POSIX ustar archive at
// `destination`. Rejects symlinks, matching the rest of this engine's
// checkpoint handling. `destination`'s parent directory must already exist.
void ArchiveDirectory(const Path& source, const Path& destination, const Path& exclude);

// Extracts a gzip-compressed ustar archive at `source` into `destination`,
// recreating the directory structure its entries were written with.
// `destination` must already exist.
void ExtractArchive(const Path& source, const Path& destination);

// Sums the *uncompressed* size of every entry in a gzip-compressed ustar
// archive by reading only headers, not entry bodies -- the size that will
// actually be written once extracted, not the smaller compressed size on
// disk.
uintmax_t ArchiveUncompressedSize(const Path& archive);

// Writes a single tar header at `archive` claiming `declared_size` for an
// entry, without writing that many actual bytes. Exposed only so tests can
// exercise capacity-checking logic (ArchiveUncompressedSize) against an
// oversized checkpoint without materializing gigabytes of real data; not
// something PublishCheckpoint ever calls.
void WriteOversizedEntryForTesting(const Path& archive, uintmax_t declared_size);

}  // namespace snapshot::pagebroker
