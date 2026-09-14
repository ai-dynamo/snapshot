// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "stage_ready_gate.hpp"

#include "../cmd/cuda-checkpoint-helper/content_digest.h"

#include <google/protobuf/struct.pb.h>
#include <google/protobuf/util/json_util.h>

#include <array>
#include <cerrno>
#include <chrono>
#include <dirent.h>
#include <exception>
#include <fcntl.h>
#include <memory>
#include <stdexcept>
#include <string_view>
#include <system_error>
#include <sys/file.h>
#include <sys/random.h>
#include <sys/stat.h>
#include <unistd.h>
#include <vector>

namespace snapshot::pagebroker {
namespace {

constexpr int kSchemaVersion = 1;
constexpr size_t kMaximumIdentityBytes = 253;
constexpr size_t kMaximumJsonBytes = 16 * 1024;
constexpr std::string_view kStageRoot = ".pagebroker-stage-ready";
constexpr std::string_view kVersionRoot = "v1";
constexpr std::string_view kOwnersRoot = "owners";
constexpr std::string_view kMarkersRoot = "markers";
constexpr std::string_view kGenerationFile = "generation.json";
constexpr std::string_view kOwnerLockFile = "owner.lock";

using Directory = std::unique_ptr<DIR, int (*)(DIR*)>;

class VisibleDurabilityUncertain : public std::runtime_error {
 public:
  using std::runtime_error::runtime_error;
};

[[noreturn]] void ThrowFileError(const std::string& operation,
                                 const std::filesystem::path& path,
                                 int error)
{
  throw std::filesystem::filesystem_error(
      operation, path, std::error_code(error, std::generic_category()));
}

std::string Digest(std::string_view first,
                   std::string_view second = {},
                   bool nul_separator = false)
{
  cuda_checkpoint_storage::ContentDigest digest;
  std::string error;
  if (!digest.Update(first.data(), first.size(), &error))
    throw std::runtime_error("cannot hash PageBroker stage identity: " + error);
  if (nul_separator) {
    constexpr char separator = '\0';
    if (!digest.Update(&separator, 1, &error))
      throw std::runtime_error("cannot hash PageBroker stage identity: " + error);
  }
  if (!second.empty() &&
      !digest.Update(second.data(), second.size(), &error))
    throw std::runtime_error("cannot hash PageBroker stage identity: " + error);
  std::string result;
  if (!digest.Finalize(&result, &error))
    throw std::runtime_error("cannot hash PageBroker stage identity: " + error);
  return result;
}

std::string RandomHex()
{
  std::array<unsigned char, 16> bytes{};
  size_t offset = 0;
  while (offset < bytes.size()) {
    const ssize_t count = getrandom(bytes.data() + offset,
                                    bytes.size() - offset, 0);
    if (count < 0 && errno == EINTR)
      continue;
    if (count <= 0)
      throw std::system_error(errno == 0 ? EIO : errno,
                              std::generic_category(),
                              "generate PageBroker stage generation");
    offset += static_cast<size_t>(count);
  }
  constexpr char hex[] = "0123456789abcdef";
  std::string result;
  result.reserve(bytes.size() * 2);
  for (const unsigned char byte : bytes) {
    result.push_back(hex[byte >> 4]);
    result.push_back(hex[byte & 0x0f]);
  }
  return result;
}

bool IsLowerHex(std::string_view value, size_t size)
{
  if (value.size() != size)
    return false;
  for (const char character : value) {
    if (!((character >= '0' && character <= '9') ||
          (character >= 'a' && character <= 'f')))
      return false;
  }
  return true;
}

bool IsValidUtf8(std::string_view value)
{
  const auto continuation = [](unsigned char byte) {
    return byte >= 0x80 && byte <= 0xbf;
  };
  for (size_t index = 0; index < value.size();) {
    const auto byte = static_cast<unsigned char>(value[index]);
    if (byte <= 0x7f) {
      ++index;
      continue;
    }
    if (byte >= 0xc2 && byte <= 0xdf && index + 1 < value.size() &&
        continuation(static_cast<unsigned char>(value[index + 1]))) {
      index += 2;
      continue;
    }
    if (index + 2 < value.size()) {
      const auto second = static_cast<unsigned char>(value[index + 1]);
      const auto third = static_cast<unsigned char>(value[index + 2]);
      if (((byte == 0xe0 && second >= 0xa0 && second <= 0xbf) ||
           ((byte >= 0xe1 && byte <= 0xec) && continuation(second)) ||
           (byte == 0xed && second >= 0x80 && second <= 0x9f) ||
           ((byte >= 0xee && byte <= 0xef) && continuation(second))) &&
          continuation(third)) {
        index += 3;
        continue;
      }
    }
    if (index + 3 < value.size()) {
      const auto second = static_cast<unsigned char>(value[index + 1]);
      const auto third = static_cast<unsigned char>(value[index + 2]);
      const auto fourth = static_cast<unsigned char>(value[index + 3]);
      if (((byte == 0xf0 && second >= 0x90 && second <= 0xbf) ||
           ((byte >= 0xf1 && byte <= 0xf3) && continuation(second)) ||
           (byte == 0xf4 && second >= 0x80 && second <= 0x8f)) &&
          continuation(third) && continuation(fourth)) {
        index += 4;
        continue;
      }
    }
    return false;
  }
  return true;
}

void ValidateText(std::string_view value,
                  size_t maximum_bytes,
                  const char* label)
{
  if (value.empty() || value.size() > maximum_bytes || !IsValidUtf8(value))
    throw std::invalid_argument(std::string("invalid PageBroker stage ") + label);
  for (const unsigned char byte : value) {
    if (byte < 0x20 || byte == 0x7f)
      throw std::invalid_argument(std::string("invalid PageBroker stage ") + label);
  }
}

std::string JsonEscape(std::string_view value)
{
  std::string result;
  constexpr char hex[] = "0123456789abcdef";
  for (const unsigned char byte : value) {
    switch (byte) {
      case '"': result += "\\\""; break;
      case '\\': result += "\\\\"; break;
      case '\b': result += "\\b"; break;
      case '\f': result += "\\f"; break;
      case '\n': result += "\\n"; break;
      case '\r': result += "\\r"; break;
      case '\t': result += "\\t"; break;
      default:
        if (byte < 0x20) {
          result += "\\u00";
          result.push_back(hex[byte >> 4]);
          result.push_back(hex[byte & 0x0f]);
        }
        else {
          result.push_back(static_cast<char>(byte));
        }
    }
  }
  return result;
}

std::string StateName(StageReadyState state)
{
  switch (state) {
    case StageReadyState::READY: return "ready";
    case StageReadyState::COMMITTED: return "committed";
    case StageReadyState::ABORTED: return "aborted";
    case StageReadyState::EXPIRED: return "expired";
    case StageReadyState::COORDINATOR_LOST: return "coordinator_lost";
  }
  throw std::invalid_argument("invalid PageBroker stage state");
}

std::string BackendName(StageReadyBackend backend)
{
  switch (backend) {
    case StageReadyBackend::REGULAR: return "regular";
    case StageReadyBackend::POSIX_CUSTOM_STORAGE:
      return "posix-custom-storage";
  }
  throw std::invalid_argument("invalid PageBroker stage backend");
}

StageReadyBackend ParseBackend(const std::string& value)
{
  if (value == "regular") return StageReadyBackend::REGULAR;
  if (value == "posix-custom-storage")
    return StageReadyBackend::POSIX_CUSTOM_STORAGE;
  throw std::runtime_error("invalid PageBroker stage marker backend");
}

StageReadyState ParseState(const std::string& value)
{
  if (value == "ready") return StageReadyState::READY;
  if (value == "committed") return StageReadyState::COMMITTED;
  if (value == "aborted") return StageReadyState::ABORTED;
  if (value == "expired") return StageReadyState::EXPIRED;
  if (value == "coordinator_lost") return StageReadyState::COORDINATOR_LOST;
  throw std::runtime_error("invalid PageBroker stage marker state");
}

FileDescriptor OpenDirectoryAt(int parent_fd,
                               const std::string& leaf,
                               const std::filesystem::path& display,
                               bool create)
{
  if (create && mkdirat(parent_fd, leaf.c_str(), 0755) != 0 && errno != EEXIST)
    ThrowFileError("create PageBroker stage directory", display, errno);
  const int fd = openat(parent_fd, leaf.c_str(),
                        O_RDONLY | O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW);
  if (fd < 0)
    ThrowFileError("open PageBroker stage directory", display, errno);
  FileDescriptor descriptor(fd);
  struct stat status{};
  if (fstat(descriptor.get(), &status) != 0)
    ThrowFileError("stat PageBroker stage directory", display, errno);
  // Every component below the administrator-selected storage root is a
  // predictable path. Never normalize or use an object planted by another
  // identity; in production PageBroker runs as the chart's effective uid/gid
  // and creates this complete tree itself.
  if (!S_ISDIR(status.st_mode) || status.st_uid != geteuid() ||
      status.st_gid != getegid())
    ThrowFileError("validate PageBroker stage directory ownership", display,
                   EPERM);
  if (create && fchmod(descriptor.get(), 0755) != 0)
    ThrowFileError("set PageBroker stage directory mode", display, errno);
  if (create && fsync(descriptor.get()) != 0)
    ThrowFileError("sync PageBroker stage directory mode", display, errno);
  if (create && fsync(parent_fd) != 0)
    ThrowFileError("sync PageBroker stage directory parent",
                   display.parent_path(), errno);
  return descriptor;
}

Directory OpenDirectoryStream(int directory_fd,
                              const std::filesystem::path& display)
{
  const int duplicate = openat(directory_fd, ".",
                               O_RDONLY | O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW);
  if (duplicate < 0)
    ThrowFileError("duplicate PageBroker stage directory", display, errno);
  DIR* directory = fdopendir(duplicate);
  if (directory == nullptr) {
    const int error = errno;
    close(duplicate);
    ThrowFileError("scan PageBroker stage directory", display, error);
  }
  return Directory(directory, closedir);
}

bool IsExactTemporary(std::string_view leaf,
                      std::string_view prefix,
                      size_t random_hex_bytes)
{
  return leaf.size() == prefix.size() + random_hex_bytes &&
         leaf.starts_with(prefix) &&
         IsLowerHex(leaf.substr(prefix.size()), random_hex_bytes);
}

bool IsMarkerTemporary(std::string_view leaf)
{
  constexpr size_t operation_bytes = 64;
  constexpr std::string_view middle = ".json.tmp.";
  constexpr size_t random_bytes = 32;
  return leaf.size() == 1 + operation_bytes + middle.size() + random_bytes &&
         leaf.front() == '.' &&
         IsLowerHex(leaf.substr(1, operation_bytes), operation_bytes) &&
         leaf.substr(1 + operation_bytes, middle.size()) == middle &&
         IsLowerHex(leaf.substr(1 + operation_bytes + middle.size()),
                    random_bytes);
}

void CleanupGenerationTemporaries(int owner_fd,
                                  const std::filesystem::path& display)
{
  constexpr std::string_view prefix = ".generation.json.tmp.";
  bool removed = false;
  auto directory = OpenDirectoryStream(owner_fd, display);
  for (;;) {
    errno = 0;
    const dirent* entry = readdir(directory.get());
    if (entry == nullptr) {
      if (errno != 0)
        ThrowFileError("scan PageBroker stage owner", display, errno);
      break;
    }
    const std::string_view leaf(entry->d_name);
    if (!IsExactTemporary(leaf, prefix, 32))
      continue;
    if (unlinkat(owner_fd, entry->d_name, 0) != 0 && errno != ENOENT)
      ThrowFileError("remove PageBroker generation temporary",
                     display / entry->d_name, errno);
    removed = true;
  }
  if (removed && fsync(owner_fd) != 0)
    ThrowFileError("sync PageBroker generation temporary cleanup", display,
                   errno);
}

std::vector<std::string> MarkerLeaves(
    int markers_fd, const std::filesystem::path& display)
{
  std::vector<std::string> leaves;
  bool removed_temporary = false;
  auto directory = OpenDirectoryStream(markers_fd, display);
  for (;;) {
    errno = 0;
    const dirent* entry = readdir(directory.get());
    if (entry == nullptr) {
      if (errno != 0)
        ThrowFileError("scan PageBroker stage markers", display, errno);
      break;
    }
    const std::string leaf(entry->d_name);
    if (leaf == "." || leaf == "..")
      continue;
    if (IsMarkerTemporary(leaf)) {
      if (unlinkat(markers_fd, leaf.c_str(), 0) != 0 && errno != ENOENT)
        ThrowFileError("remove PageBroker stage temporary", display / leaf,
                       errno);
      removed_temporary = true;
      continue;
    }
    if (leaf.size() != 69 || !leaf.ends_with(".json") ||
        !IsLowerHex(std::string_view(leaf).substr(0, 64), 64))
      throw std::runtime_error("invalid PageBroker stage marker leaf: " + leaf);
    leaves.push_back(leaf.substr(0, 64));
  }
  if (removed_temporary && fsync(markers_fd) != 0)
    ThrowFileError("sync PageBroker stage temporary cleanup", display, errno);
  return leaves;
}

std::string ReadFileAt(int parent_fd,
                       const std::string& leaf,
                       const std::filesystem::path& display)
{
  const int value = openat(parent_fd, leaf.c_str(),
                           O_RDONLY | O_CLOEXEC | O_NOFOLLOW);
  if (value < 0)
    ThrowFileError("open PageBroker stage record", display, errno);
  const FileDescriptor fd(value);
  struct stat status{};
  if (fstat(fd.get(), &status) != 0)
    ThrowFileError("stat PageBroker stage record", display, errno);
  if (status.st_uid != geteuid() || status.st_gid != getegid())
    ThrowFileError("validate PageBroker stage record ownership", display,
                   EPERM);
  if (!S_ISREG(status.st_mode) || status.st_nlink != 1 || status.st_size <= 0 ||
      static_cast<uintmax_t>(status.st_size) > kMaximumJsonBytes)
    throw std::runtime_error("invalid PageBroker stage record: " +
                             display.string());
  if (fchmod(fd.get(), 0644) != 0)
    ThrowFileError("set PageBroker stage record mode", display, errno);
  if (fsync(fd.get()) != 0)
    ThrowFileError("sync PageBroker stage record mode", display, errno);
  std::string result(static_cast<size_t>(status.st_size), '\0');
  size_t offset = 0;
  while (offset < result.size()) {
    const ssize_t count = read(fd.get(), result.data() + offset,
                               result.size() - offset);
    if (count < 0 && errno == EINTR)
      continue;
    if (count <= 0)
      ThrowFileError("read PageBroker stage record", display,
                     count == 0 ? EIO : errno);
    offset += static_cast<size_t>(count);
  }
  return result;
}

void WriteAll(int fd,
              std::string_view value,
              const std::filesystem::path& display)
{
  size_t offset = 0;
  while (offset < value.size()) {
    const ssize_t count = write(fd, value.data() + offset,
                                value.size() - offset);
    if (count < 0 && errno == EINTR)
      continue;
    if (count <= 0)
      ThrowFileError("write PageBroker stage record", display,
                     count == 0 ? EIO : errno);
    offset += static_cast<size_t>(count);
  }
}

google::protobuf::Struct ParseObject(std::string_view serialized,
                                     const std::filesystem::path& display)
{
  // These records are written only by PageBroker. Requiring the writer's
  // canonical integer spelling preserves Dynamo's stricter JSON `int` schema
  // contract even though protobuf Struct represents every number as double.
  constexpr std::string_view schema_prefix = "{\"schema_version\":1,";
  if (!serialized.starts_with(schema_prefix))
    throw std::runtime_error("invalid PageBroker stage schema encoding: " +
                             display.string());
  google::protobuf::Struct object;
  const auto status = google::protobuf::util::JsonStringToMessage(
      std::string(serialized), &object);
  if (!status.ok())
    throw std::runtime_error("invalid PageBroker stage JSON: " +
                             display.string());
  return object;
}

const google::protobuf::Value& Field(const google::protobuf::Struct& object,
                                     const char* name,
                                     const std::filesystem::path& display)
{
  const auto iterator = object.fields().find(name);
  if (iterator == object.fields().end())
    throw std::runtime_error("missing PageBroker stage field " +
                             std::string(name) + ": " + display.string());
  return iterator->second;
}

std::string StringField(const google::protobuf::Struct& object,
                        const char* name,
                        const std::filesystem::path& display)
{
  const auto& field = Field(object, name, display);
  if (field.kind_case() != google::protobuf::Value::kStringValue)
    throw std::runtime_error("invalid PageBroker stage string field " +
                             std::string(name) + ": " + display.string());
  return field.string_value();
}

void ValidateSchema(const google::protobuf::Struct& object,
                    size_t expected_fields,
                    const std::filesystem::path& display)
{
  if (object.fields_size() != static_cast<int>(expected_fields))
    throw std::runtime_error("invalid PageBroker stage field set: " +
                             display.string());
  const auto& schema = Field(object, "schema_version", display);
  if (schema.kind_case() != google::protobuf::Value::kNumberValue ||
      schema.number_value() != kSchemaVersion)
    throw std::runtime_error("invalid PageBroker stage schema: " +
                             display.string());
}

std::string GenerationJson(const std::string& owner_id,
                           const std::string& generation)
{
  return "{\"schema_version\":1,\"pagebroker_owner_id\":\"" + owner_id +
         "\",\"pagebroker_generation\":\"" + generation + "\"}\n";
}

void AtomicWriteAt(int parent_fd,
                   const std::filesystem::path& parent_display,
                   const std::string& leaf,
                   std::string_view contents,
                   bool no_replace,
                   const std::function<void(const char*)>& maybe_fail,
                   const char* operation)
{
  const std::string temporary = "." + leaf + ".tmp." + RandomHex();
  const std::filesystem::path temporary_display = parent_display / temporary;
  const int value = openat(parent_fd, temporary.c_str(),
                           O_WRONLY | O_CREAT | O_EXCL | O_CLOEXEC | O_NOFOLLOW,
                           0600);
  if (value < 0)
    ThrowFileError(std::string(operation) + " create temporary",
                   temporary_display, errno);
  bool visible = false;
  struct stat temporary_status{};
  try {
    {
      const FileDescriptor fd(value);
      maybe_fail((std::string(operation) + "_write").c_str());
      WriteAll(fd.get(), contents, temporary_display);
      maybe_fail((std::string(operation) + "_file_fsync").c_str());
      if (fsync(fd.get()) != 0)
        ThrowFileError(std::string(operation) + " sync temporary",
                       temporary_display, errno);
      if (fstat(fd.get(), &temporary_status) != 0)
        ThrowFileError(std::string(operation) + " stat temporary",
                       temporary_display, errno);
    }
    maybe_fail((std::string(operation) + "_rename").c_str());
    if (no_replace) {
      // The owner lock excludes another PageBroker generation and callers hold
      // the gate mutex. The directory is not writable by the cross-UID reader,
      // and same-UID writers are explicitly part of the trusted computing
      // base. Under those invariants, an existence check followed by portable
      // same-directory rename preserves no-replace semantics without the
      // transient second hard link that NFS cannot always remove cleanly.
      struct stat existing_status{};
      if (fstatat(parent_fd, leaf.c_str(), &existing_status,
                  AT_SYMLINK_NOFOLLOW) == 0)
        ThrowFileError(std::string(operation) + " publish",
                       parent_display / leaf, EEXIST);
      if (errno != ENOENT)
        ThrowFileError(std::string(operation) + " inspect destination",
                       parent_display / leaf, errno);
    }
    std::exception_ptr rename_error;
    try {
      if (renameat(parent_fd, temporary.c_str(), parent_fd, leaf.c_str()) != 0)
        ThrowFileError(std::string(operation) +
                           (no_replace ? " publish" : " replace"),
                       parent_display / leaf, errno);
      maybe_fail((std::string(operation) + "_renamed").c_str());
    }
    catch (...) {
      rename_error = std::current_exception();
    }
    if (rename_error) {
      struct stat published_status{};
      struct stat remaining_status{};
      try {
        maybe_fail(
            (std::string(operation) + "_reconcile_published_stat").c_str());
      }
      catch (...) {
        throw VisibleDurabilityUncertain(
            std::string(operation) +
            " rename result cannot be reconciled: destination lookup failed");
      }
      const bool published_exists =
          fstatat(parent_fd, leaf.c_str(), &published_status,
                  AT_SYMLINK_NOFOLLOW) == 0;
      const int published_error = published_exists ? 0 : errno;
      try {
        maybe_fail(
            (std::string(operation) + "_reconcile_temporary_stat").c_str());
      }
      catch (...) {
        throw VisibleDurabilityUncertain(
            std::string(operation) +
            " rename result cannot be reconciled: temporary lookup failed");
      }
      const bool temporary_exists =
          fstatat(parent_fd, temporary.c_str(), &remaining_status,
                  AT_SYMLINK_NOFOLLOW) == 0;
      const int temporary_error = temporary_exists ? 0 : errno;
      const bool published_matches =
          published_exists && S_ISREG(published_status.st_mode) &&
          published_status.st_dev == temporary_status.st_dev &&
          published_status.st_ino == temporary_status.st_ino;
      const bool temporary_matches =
          temporary_exists && S_ISREG(remaining_status.st_mode) &&
          remaining_status.st_dev == temporary_status.st_dev &&
          remaining_status.st_ino == temporary_status.st_ino;
      if (published_matches && !temporary_exists &&
          temporary_error == ENOENT) {
        // The server applied rename but its reply (or our injected response
        // boundary) was lost. Finish the durability protocol exactly once.
        visible = true;
      }
      else if (temporary_matches && !published_exists &&
               published_error == ENOENT) {
        std::rethrow_exception(rename_error);
      }
      else {
        throw VisibleDurabilityUncertain(
            std::string(operation) +
            " rename result cannot be reconciled from the namespace");
      }
    }
    else {
      visible = true;
    }
    const int published_value = openat(
        parent_fd, leaf.c_str(), O_RDONLY | O_CLOEXEC | O_NOFOLLOW);
    if (published_value < 0)
      ThrowFileError(std::string(operation) + " open published record",
                     parent_display / leaf, errno);
    {
      const FileDescriptor published_fd(published_value);
      struct stat published_status{};
      if (fstat(published_fd.get(), &published_status) != 0)
        ThrowFileError(std::string(operation) + " stat published record",
                       parent_display / leaf, errno);
      if (published_status.st_uid != geteuid() ||
          published_status.st_gid != getegid() ||
          !S_ISREG(published_status.st_mode) ||
          published_status.st_nlink != 1 ||
          published_status.st_dev != temporary_status.st_dev ||
          published_status.st_ino != temporary_status.st_ino)
        ThrowFileError(std::string(operation) + " validate published record",
                       parent_display / leaf, EINVAL);
      if (fchmod(published_fd.get(), 0644) != 0)
        ThrowFileError(std::string(operation) + " set record mode",
                       parent_display / leaf, errno);
      maybe_fail((std::string(operation) + "_mode_fsync").c_str());
      if (fsync(published_fd.get()) != 0)
        ThrowFileError(std::string(operation) + " sync record mode",
                       parent_display / leaf, errno);
    }
    maybe_fail((std::string(operation) + "_directory_fsync").c_str());
    if (fsync(parent_fd) != 0)
      ThrowFileError(std::string(operation) + " sync directory",
                     parent_display, errno);
  }
  catch (...) {
    if (!visible)
      (void)unlinkat(parent_fd, temporary.c_str(), 0);
    if (visible)
      throw VisibleDurabilityUncertain(
          std::string(operation) +
          " is visible but directory durability is uncertain");
    throw;
  }
}

}  // namespace

struct StageReadyGate::Marker {
  StageReadyIdentity identity;
  std::string owner_id;
  std::string generation;
  StageReadyState state;
};

StageReadyDurabilityUncertain::StageReadyDurabilityUncertain(
    StageReadyHandle handle, StageReadyState state, std::string message)
    : std::runtime_error(std::move(message)),
      handle_(std::move(handle)),
      state_(state)
{}

StageReadyGate::StageReadyGate(std::filesystem::path storage_root,
                               std::string owner_identity,
                               std::string node_name,
                               FailureForTesting failure_for_testing)
    : storage_root_(std::move(storage_root)),
      storage_root_fd_(open(storage_root_.c_str(),
                            O_RDONLY | O_DIRECTORY | O_CLOEXEC | O_NOFOLLOW)),
      owner_id_(Digest(owner_identity)),
      node_name_(std::move(node_name)),
      failure_for_testing_(std::move(failure_for_testing))
{
  if (!storage_root_.is_absolute() || storage_root_ == "/" ||
      storage_root_.lexically_normal() != storage_root_)
    throw std::invalid_argument("PageBroker stage storage root must be a clean absolute path");
  if (storage_root_fd_.get() < 0)
    ThrowFileError("open PageBroker stage storage root", storage_root_, errno);
  if (owner_identity.empty())
    throw std::invalid_argument("PageBroker stage owner identity is required");
  ValidateText(node_name_, kMaximumIdentityBytes, "node name");

  auto stage = OpenDirectoryAt(storage_root_fd_.get(), std::string(kStageRoot),
                               storage_root_ / kStageRoot, true);
  auto version = OpenDirectoryAt(stage.get(), std::string(kVersionRoot),
                                 storage_root_ / kStageRoot / kVersionRoot,
                                 true);
  owners_root_fd_ = OpenDirectoryAt(
      version.get(), std::string(kOwnersRoot),
      storage_root_ / kStageRoot / kVersionRoot / kOwnersRoot, true);
  owner_root_ = storage_root_ / kStageRoot / kVersionRoot / kOwnersRoot /
                owner_id_;
  owner_root_fd_ = OpenDirectoryAt(owners_root_fd_.get(), owner_id_,
                                   owner_root_, true);

  const int lock_value = openat(owner_root_fd_.get(), kOwnerLockFile.data(),
                                O_RDWR | O_CREAT | O_CLOEXEC | O_NOFOLLOW, 0600);
  if (lock_value < 0)
    ThrowFileError("open PageBroker stage owner lock",
                   owner_root_ / kOwnerLockFile, errno);
  owner_lock_fd_ = FileDescriptor(lock_value);
  struct stat lock_status{};
  if (fstat(owner_lock_fd_.get(), &lock_status) != 0)
    ThrowFileError("validate PageBroker stage owner lock",
                   owner_root_ / kOwnerLockFile, errno);
  if (lock_status.st_uid != geteuid() || lock_status.st_gid != getegid())
    ThrowFileError("validate PageBroker stage owner lock ownership",
                   owner_root_ / kOwnerLockFile, EPERM);
  if (!S_ISREG(lock_status.st_mode) || lock_status.st_nlink != 1)
    ThrowFileError("validate PageBroker stage owner lock",
                   owner_root_ / kOwnerLockFile, EINVAL);
  if (fchmod(owner_lock_fd_.get(), 0600) != 0)
    ThrowFileError("set PageBroker stage owner lock mode",
                   owner_root_ / kOwnerLockFile, errno);
  if (fsync(owner_lock_fd_.get()) != 0)
    ThrowFileError("sync PageBroker stage owner lock mode",
                   owner_root_ / kOwnerLockFile, errno);
  if (flock(owner_lock_fd_.get(), LOCK_EX | LOCK_NB) != 0)
    ThrowFileError("lock PageBroker stage owner", owner_root_, errno);

  CleanupGenerationTemporaries(owner_root_fd_.get(), owner_root_);
  markers_root_fd_ = OpenDirectoryAt(owner_root_fd_.get(),
                                     std::string(kMarkersRoot),
                                     owner_root_ / kMarkersRoot, true);
  RecoverPriorGeneration();
  generation_ = RandomHex();
  AtomicWriteAt(
      owner_root_fd_.get(), owner_root_, std::string(kGenerationFile),
      GenerationJson(owner_id_, generation_), false,
      [this](const char* operation) { MaybeFail(operation); },
      "publish_generation");
}

std::string StageReadyGate::OperationId(
    std::string_view pod_uid, std::string_view destination_container)
{
  ValidateText(pod_uid, kMaximumIdentityBytes, "Pod UID");
  ValidateText(destination_container, kMaximumIdentityBytes,
               "destination container");
  return Digest(pod_uid, destination_container, true);
}

void StageReadyGate::MaybeFail(const char* operation) const
{
  if (!failure_for_testing_)
    return;
  const int error = failure_for_testing_(operation);
  if (error != 0)
    throw std::system_error(error, std::generic_category(), operation);
}

StageReadyHandle StageReadyGate::PublishReady(
    const StageReadyIdentity& identity)
{
  ValidateText(identity.node_name, kMaximumIdentityBytes, "node name");
  if (identity.node_name != node_name_)
    throw std::invalid_argument("PageBroker stage node name does not match owner");
  ValidateText(identity.pod_uid, kMaximumIdentityBytes, "Pod UID");
  ValidateText(identity.destination_container, kMaximumIdentityBytes,
               "destination container");
  ValidateText(identity.content_uid, kMaximumIdentityBytes, "content UID");
  ValidateText(identity.source_container, kMaximumIdentityBytes,
               "source container");
  ValidateText(identity.container_id, kMaximumIdentityBytes, "container ID");
  ValidateText(identity.transaction_id, kMaximumIdentityBytes,
               "transaction ID");
  ValidateText(identity.stage_request_id, kMaximumIdentityBytes,
               "stage request ID");
  const std::string operation_id =
      OperationId(identity.pod_uid, identity.destination_container);
  const StageReadyHandle handle{operation_id, identity.transaction_id,
                                identity.stage_request_id, generation_};
  std::lock_guard lock(mutex_);
  if (shutting_down_)
    throw std::runtime_error("PageBroker stage activation is shutting down");
  try {
    WriteMarker(
        operation_id,
        Marker{identity, owner_id_, generation_, StageReadyState::READY}, true,
        "publish_marker");
  }
  catch (const VisibleDurabilityUncertain& error) {
    throw StageReadyDurabilityUncertain(handle, StageReadyState::READY,
                                        error.what());
  }
  catch (const std::filesystem::filesystem_error& error) {
    if (error.code().value() != EEXIST)
      throw;
    const auto marker = ReadMarker(operation_id);
    const auto& existing = marker.identity;
    const bool exact_identity =
        existing.node_name == identity.node_name &&
        existing.pod_uid == identity.pod_uid &&
        existing.destination_container == identity.destination_container &&
        existing.content_uid == identity.content_uid &&
        existing.source_container == identity.source_container &&
        existing.container_id == identity.container_id &&
        existing.transaction_id == identity.transaction_id &&
        existing.stage_request_id == identity.stage_request_id &&
        existing.backend == identity.backend &&
        marker.owner_id == owner_id_ && marker.generation == generation_ &&
        (marker.state == StageReadyState::READY ||
         marker.state == StageReadyState::COMMITTED);
    if (!exact_identity) {
      const bool supersedes_terminal_container =
          marker.owner_id == owner_id_ &&
          existing.node_name == identity.node_name &&
          (marker.state == StageReadyState::COMMITTED ||
           marker.state == StageReadyState::COORDINATOR_LOST) &&
          existing.container_id != identity.container_id &&
          existing.transaction_id != identity.transaction_id &&
          existing.stage_request_id != identity.stage_request_id;
      if (!supersedes_terminal_container)
        throw std::runtime_error(
            "PageBroker stage operation collides with a different identity");
      try {
        WriteMarker(
            operation_id,
            Marker{identity, owner_id_, generation_, StageReadyState::READY},
            false, "supersede_marker");
      }
      catch (const VisibleDurabilityUncertain& replacement_error) {
        throw StageReadyDurabilityUncertain(
            handle, StageReadyState::READY, replacement_error.what());
      }
      return handle;
    }
    try {
      MaybeFail("publish_marker_repair_directory_fsync");
      if (fsync(markers_root_fd_.get()) != 0)
        ThrowFileError("repair PageBroker stage publication durability",
                       owner_root_ / kMarkersRoot, errno);
    }
    catch (const std::exception& repair_error) {
      throw StageReadyDurabilityUncertain(handle, marker.state,
                                          repair_error.what());
    }
  }
  return handle;
}

void StageReadyGate::BeginShutdown()
{
  std::lock_guard lock(mutex_);
  shutting_down_ = true;
  for (const auto& operation_id :
       MarkerLeaves(markers_root_fd_.get(), owner_root_ / kMarkersRoot)) {
    auto marker = ReadMarker(operation_id);
    if (marker.state != StageReadyState::READY)
      continue;
    marker.state = StageReadyState::COORDINATOR_LOST;
    WriteMarker(operation_id, marker, false, "shutdown_marker");
  }
  MaybeFail("shutdown_markers_directory_fsync");
  if (fsync(markers_root_fd_.get()) != 0)
    ThrowFileError("sync shutdown PageBroker stage markers",
                   owner_root_ / kMarkersRoot, errno);
}

void StageReadyGate::Transition(const StageReadyHandle& handle,
                                StageReadyState state)
{
  if (!IsLowerHex(handle.operation_id, 64) ||
      !IsLowerHex(handle.pagebroker_generation, 32) ||
      handle.transaction_id.empty() || handle.stage_request_id.empty())
    throw std::invalid_argument("invalid PageBroker stage handle");
  ValidateText(handle.transaction_id, kMaximumIdentityBytes, "transaction ID");
  ValidateText(handle.stage_request_id, kMaximumIdentityBytes,
               "stage request ID");
  if (state == StageReadyState::READY)
    throw std::invalid_argument("PageBroker stage transition must be terminal");
  std::lock_guard lock(mutex_);
  auto marker = ReadMarker(handle.operation_id);
  if (marker.owner_id != owner_id_ ||
      marker.generation != handle.pagebroker_generation ||
      marker.identity.transaction_id != handle.transaction_id ||
      marker.identity.stage_request_id != handle.stage_request_id)
    throw std::runtime_error("PageBroker stage transition ownership conflict");
  if (marker.state == state) {
    // A prior attempt may have completed rename but lost the directory fsync.
    // Re-establish the durability barrier before acknowledging idempotence.
    try {
      MaybeFail("transition_marker_directory_fsync");
      if (fsync(markers_root_fd_.get()) != 0)
        ThrowFileError("sync idempotent PageBroker stage transition",
                       owner_root_ / kMarkersRoot, errno);
    }
    catch (const std::exception& error) {
      throw StageReadyDurabilityUncertain(handle, state, error.what());
    }
    return;
  }
  if (marker.state != StageReadyState::READY)
    throw std::runtime_error("PageBroker stage marker is already terminal");
  marker.state = state;
  try {
    WriteMarker(handle.operation_id, marker, false, "transition_marker");
  }
  catch (const VisibleDurabilityUncertain& error) {
    throw StageReadyDurabilityUncertain(handle, state, error.what());
  }
}

size_t StageReadyGate::ReapRetainedMarkers(
    std::chrono::system_clock::time_point now,
    std::chrono::system_clock::duration retention)
{
  if (retention < std::chrono::system_clock::duration::zero())
    throw std::invalid_argument("PageBroker stage retention must be nonnegative");
  std::lock_guard lock(mutex_);
  size_t removed = 0;
  for (const auto& operation_id :
       MarkerLeaves(markers_root_fd_.get(), owner_root_ / kMarkersRoot)) {
    const auto marker = ReadMarker(operation_id);
    if (marker.state == StageReadyState::READY)
      continue;
    struct stat status{};
    const std::string leaf = operation_id + ".json";
    if (fstatat(markers_root_fd_.get(), leaf.c_str(), &status,
                AT_SYMLINK_NOFOLLOW) != 0)
      ThrowFileError("stat retained PageBroker stage marker",
                     owner_root_ / kMarkersRoot / leaf, errno);
    const auto modified = std::chrono::system_clock::time_point(
        std::chrono::seconds(status.st_mtim.tv_sec) +
        std::chrono::nanoseconds(status.st_mtim.tv_nsec));
    if (now - modified < retention)
      continue;
    MaybeFail("reap_marker_unlink");
    if (unlinkat(markers_root_fd_.get(), leaf.c_str(), 0) != 0)
      ThrowFileError("reap retained PageBroker stage marker",
                     owner_root_ / kMarkersRoot / leaf, errno);
    ++removed;
    reap_sync_pending_ = true;
  }
  if (reap_sync_pending_) {
    MaybeFail("reap_marker_directory_fsync");
    if (fsync(markers_root_fd_.get()) != 0)
      ThrowFileError("sync reaped PageBroker stage markers",
                     owner_root_ / kMarkersRoot, errno);
    reap_sync_pending_ = false;
  }
  return removed;
}

void StageReadyGate::RecoverPriorGeneration()
{
  std::string prior_generation;
  struct stat generation_status{};
  if (fstatat(owner_root_fd_.get(), kGenerationFile.data(), &generation_status,
              AT_SYMLINK_NOFOLLOW) == 0) {
    const std::filesystem::path path = owner_root_ / kGenerationFile;
    const auto object = ParseObject(
        ReadFileAt(owner_root_fd_.get(), std::string(kGenerationFile), path),
        path);
    ValidateSchema(object, 3, path);
    if (StringField(object, "pagebroker_owner_id", path) != owner_id_)
      throw std::runtime_error("PageBroker stage generation owner mismatch");
    prior_generation =
        StringField(object, "pagebroker_generation", path);
    if (!IsLowerHex(prior_generation, 32))
      throw std::runtime_error("invalid PageBroker stage generation");
  }
  else if (errno != ENOENT) {
    ThrowFileError("inspect PageBroker stage generation",
                   owner_root_ / kGenerationFile, errno);
  }

  const auto operations =
      MarkerLeaves(markers_root_fd_.get(), owner_root_ / kMarkersRoot);
  if (prior_generation.empty() && !operations.empty())
    throw std::runtime_error("PageBroker stage markers exist without a generation");
  for (const auto& operation_id : operations) {
    auto marker = ReadMarker(operation_id);
    if (marker.owner_id != owner_id_ || marker.identity.node_name != node_name_)
      throw std::runtime_error("PageBroker stage marker owner mismatch");
    if (marker.state != StageReadyState::READY)
      continue;
    if (marker.generation != prior_generation)
      throw std::runtime_error("PageBroker ready stage marker generation mismatch");
    marker.state = StageReadyState::COORDINATOR_LOST;
    WriteMarker(operation_id, marker, false, "recover_marker");
  }
  // Also establishes durability for terminal renames that were visible after
  // a previous process failed between rename and directory fsync.
  MaybeFail("recover_markers_directory_fsync");
  if (fsync(markers_root_fd_.get()) != 0)
    ThrowFileError("sync recovered PageBroker stage markers",
                   owner_root_ / kMarkersRoot, errno);
}

StageReadyGate::Marker StageReadyGate::ReadMarker(
    const std::string& operation_id) const
{
  if (!IsLowerHex(operation_id, 64))
    throw std::invalid_argument("invalid PageBroker stage operation ID");
  const std::string leaf = operation_id + ".json";
  const std::filesystem::path path = owner_root_ / kMarkersRoot / leaf;
  const auto object = ParseObject(
      ReadFileAt(markers_root_fd_.get(), leaf, path), path);
  ValidateSchema(object, 14, path);
  if (StringField(object, "operation_id", path) != operation_id)
    throw std::runtime_error("invalid PageBroker stage marker operation ID");
  Marker marker{
      .identity = {
          .node_name = StringField(object, "node_name", path),
          .pod_uid = StringField(object, "pod_uid", path),
          .destination_container =
              StringField(object, "destination_container", path),
          .content_uid = StringField(object, "content_uid", path),
          .source_container = StringField(object, "source_container", path),
          .container_id = StringField(object, "container_id", path),
          .transaction_id = StringField(object, "transaction_id", path),
          .stage_request_id = StringField(object, "stage_request_id", path),
      },
      .owner_id = StringField(object, "pagebroker_owner_id", path),
      .generation = StringField(object, "pagebroker_generation", path),
      .state = ParseState(StringField(object, "state", path)),
  };
  marker.identity.backend =
      ParseBackend(StringField(object, "backend", path));
  if (
      !IsLowerHex(marker.owner_id, 64) ||
      !IsLowerHex(marker.generation, 32) ||
      OperationId(marker.identity.pod_uid,
                  marker.identity.destination_container) != operation_id)
    throw std::runtime_error("invalid PageBroker stage marker binding");
  ValidateText(marker.identity.node_name, kMaximumIdentityBytes, "node name");
  ValidateText(marker.identity.content_uid, kMaximumIdentityBytes,
               "content UID");
  ValidateText(marker.identity.source_container, kMaximumIdentityBytes,
               "source container");
  ValidateText(marker.identity.container_id, kMaximumIdentityBytes,
               "container ID");
  ValidateText(marker.identity.transaction_id, kMaximumIdentityBytes,
               "transaction ID");
  ValidateText(marker.identity.stage_request_id, kMaximumIdentityBytes,
               "stage request ID");
  return marker;
}

void StageReadyGate::WriteMarker(const std::string& operation_id,
                                 const Marker& marker,
                                 bool no_replace,
                                 const char* operation)
{
  const auto& identity = marker.identity;
  const std::string payload =
      "{\"schema_version\":1,\"operation_id\":\"" + operation_id +
      "\",\"node_name\":\"" +
      JsonEscape(identity.node_name) + "\",\"pod_uid\":\"" +
      JsonEscape(identity.pod_uid) +
      "\",\"destination_container\":\"" +
      JsonEscape(identity.destination_container) +
      "\",\"content_uid\":\"" + JsonEscape(identity.content_uid) +
      "\",\"source_container\":\"" +
      JsonEscape(identity.source_container) +
      "\",\"container_id\":\"" + JsonEscape(identity.container_id) +
      "\",\"transaction_id\":\"" + JsonEscape(identity.transaction_id) +
      "\",\"stage_request_id\":\"" +
      JsonEscape(identity.stage_request_id) +
      "\",\"pagebroker_owner_id\":\"" + marker.owner_id +
      "\",\"pagebroker_generation\":\"" + marker.generation +
      "\",\"backend\":\"" + BackendName(identity.backend) +
      "\",\"state\":\"" +
      StateName(marker.state) + "\"}\n";
  if (payload.size() > kMaximumJsonBytes)
    throw std::invalid_argument("PageBroker stage marker exceeds size limit");
  AtomicWriteAt(
      markers_root_fd_.get(), owner_root_ / kMarkersRoot,
      operation_id + ".json", payload, no_replace,
      [this](const char* step) { MaybeFail(step); }, operation);
}

}  // namespace snapshot::pagebroker
