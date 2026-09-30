/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#include "extent_digests.hpp"
#include "content_digest.hpp"
#include "file_descriptor.hpp"

#include <fcntl.h>
#include <sys/stat.h>
#include <unistd.h>
#include <cerrno>
#include <charconv>
#include <cstring>
#include <sstream>
#include <unordered_set>

namespace cuda_checkpoint_storage {

namespace {
constexpr size_t kMaximumExtents = 1024;
constexpr size_t kMaximumSidecarBytes = 256 * 1024;
constexpr const char* kDigestVersion = "PAGEBROKER_SHA256_V1";

bool ValidateManifest(const std::vector<ManifestExtent>& manifest, std::string* error)
{
  if (manifest.size() > kMaximumExtents) {
    *error = "too many extents in SHA-256 metadata";
    return false;
  }
  std::unordered_set<std::string> uuids;
  for (size_t i = 0; i < manifest.size(); ++i) {
    const auto& extent = manifest[i];
    std::string canonical;
    if (!CanonicalizeGPUUUID(extent.source_uuid, &canonical) || canonical != extent.source_uuid ||
        !uuids.insert(canonical).second || !extent.size || extent.filename != DeviceFilename(i)) {
      *error = "invalid manifest identity for SHA-256 metadata";
      return false;
    }
  }
  return true;
}

bool ParseSize(const std::string& text, size_t* value)
{
  const auto [end, status] = std::from_chars(text.data(), text.data() + text.size(), *value);
  return status == std::errc{} && end == text.data() + text.size();
}

bool IOError(const char* operation, std::string* error)
{
  *error = std::string(operation) + " SHA-256 metadata: " + std::strerror(errno);
  return false;
}
}  // namespace

bool WriteExtentDigests(const std::filesystem::path& directory,
                        const std::vector<ManifestExtent>& manifest,
                        const std::vector<std::string>& digests,
                        std::string* error)
{
  if (!error) return false;
  if (!ValidateManifest(manifest, error)) return false;
  if (digests.size() != manifest.size()) {
    *error = "SHA-256 metadata coverage mismatch";
    return false;
  }
  std::ostringstream output;
  output << kDigestVersion << '\n' << manifest.size() << '\n';
  for (size_t i = 0; i < manifest.size(); ++i) {
    if (!IsSHA256Hex(digests[i])) {
      *error = "invalid SHA-256 digest";
      return false;
    }
    const auto& extent = manifest[i];
    output << extent.source_uuid << ' ' << extent.size << ' ' << extent.filename
           << ' ' << digests[i] << '\n';
  }
  FileDescriptor dir(open(directory.c_str(), O_RDONLY | O_CLOEXEC | O_DIRECTORY));
  if (dir.get() < 0) return IOError("open directory for", error);
  FileDescriptor fd(openat(dir.get(), kExtentDigestsName,
                          O_WRONLY | O_CLOEXEC | O_NOFOLLOW | O_CREAT | O_EXCL, 0600));
  if (fd.get() < 0) return IOError("create", error);
  const auto contents = output.str();
  for (size_t offset = 0; offset < contents.size();) {
    const ssize_t count = write(fd.get(), contents.data() + offset, contents.size() - offset);
    if (count < 0 && errno == EINTR) continue;
    if (count <= 0) return IOError("write", error);
    offset += static_cast<size_t>(count);
  }
  if (fsync(fd.get()) || fsync(dir.get())) return IOError("sync", error);
  return true;
}

bool ReadExtentDigests(const std::filesystem::path& directory,
                       const std::vector<ManifestExtent>& manifest,
                       std::vector<std::string>* digests, std::string* error)
{
  if (!digests || !error) return false;
  if (!ValidateManifest(manifest, error)) return false;
  FileDescriptor fd(open((directory / kExtentDigestsName).c_str(),
                         O_RDONLY | O_CLOEXEC | O_NOFOLLOW | O_NONBLOCK));
  if (fd.get() < 0) return IOError("open required", error);
  struct stat statbuf{};
  if (fstat(fd.get(), &statbuf) || !S_ISREG(statbuf.st_mode) || statbuf.st_size < 0 ||
      static_cast<uintmax_t>(statbuf.st_size) > kMaximumSidecarBytes) {
    *error = "SHA-256 metadata is not a bounded regular file";
    return false;
  }
  std::string contents(static_cast<size_t>(statbuf.st_size), '\0');
  for (size_t offset = 0; offset < contents.size();) {
    const ssize_t count = read(fd.get(), contents.data() + offset, contents.size() - offset);
    if (count < 0 && errno == EINTR) continue;
    if (count <= 0) {
      *error = "incomplete SHA-256 metadata read";
      return false;
    }
    offset += static_cast<size_t>(count);
  }
  char trailing;
  ssize_t extra;
  do { extra = read(fd.get(), &trailing, 1); } while (extra < 0 && errno == EINTR);
  if (extra != 0) {
    *error = "SHA-256 metadata changed while reading";
    return false;
  }
  std::istringstream input(contents);
  std::string version, count_text;
  size_t count = 0;
  if (!(input >> version >> count_text) || version != kDigestVersion ||
      !ParseSize(count_text, &count) || count != manifest.size()) {
    *error = "SHA-256 metadata version or coverage mismatch";
    return false;
  }
  std::vector<std::string> result;
  result.reserve(count);
  for (size_t i = 0; i < count; ++i) {
    std::string uuid, size_text, filename, digest;
    size_t bytes = 0;
    if (!(input >> uuid >> size_text >> filename >> digest) ||
        !ParseSize(size_text, &bytes) || uuid != manifest[i].source_uuid ||
        bytes != manifest[i].size || filename != manifest[i].filename || !IsSHA256Hex(digest)) {
      *error = "SHA-256 metadata extent identity or digest is invalid";
      return false;
    }
    result.push_back(std::move(digest));
  }
  std::string extra_field;
  if (input >> extra_field) {
    *error = "trailing SHA-256 metadata";
    return false;
  }
  *digests = std::move(result);
  return true;
}

bool ApplyOrVerifyExtentDigests(bool checkpoint,
                                const std::vector<TransferJob> &jobs,
                                const std::vector<std::string> &digests,
                                std::vector<std::string> *extent_digests,
                                std::string *error) {
  if (extent_digests == nullptr || error == nullptr) {
    return false;
  }
  if (jobs.size() != digests.size()) {
    *error = "custom storage digest coverage mismatch";
    return false;
  }
  std::vector<unsigned char> covered(extent_digests->size(), 0);
  for (size_t job_index = 0; job_index < jobs.size(); ++job_index) {
    const size_t extent_index = jobs[job_index].extent_index;
    if (extent_index >= extent_digests->size() || covered[extent_index] != 0 ||
        !IsSHA256Hex(digests[job_index])) {
      *error = "custom storage digest metadata is invalid";
      return false;
    }
    covered[extent_index] = 1;
    auto &expected = (*extent_digests)[extent_index];
    if (checkpoint) {
      expected = digests[job_index];
      continue;
    }
    if (expected != digests[job_index]) {
      *error = "custom storage extent SHA-256 mismatch for " + std::to_string(extent_index);
      return false;
    }
  }
  for (const unsigned char value : covered) {
    if (value == 0) {
      *error = "one or more custom storage extents lack digest coverage";
      return false;
    }
  }
  return true;
}

} // namespace cuda_checkpoint_storage
