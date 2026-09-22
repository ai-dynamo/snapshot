// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "checkpoint_archive.hpp"

#include <zlib.h>

#include <cstdio>
#include <cstring>
#include <fstream>
#include <stdexcept>
#include <vector>

namespace snapshot::pagebroker {
namespace {

constexpr size_t kBlockSize = 512;
constexpr size_t kReadChunk = 1 << 22;  // 4 MiB: fewer syscalls per GB moved than a smaller chunk.

// POSIX ustar header (see POSIX.1-2001 / GNU tar's tar.h). Numeric fields
// are NUL-terminated octal ASCII strings; string fields are NUL-padded and
// need not be NUL-terminated if they fill the field exactly.
struct TarHeader {
  char name[100];
  char mode[8];
  char uid[8];
  char gid[8];
  char size[12];
  char mtime[12];
  char chksum[8];
  char typeflag[1];
  char linkname[100];
  char magic[6];
  char version[2];
  char uname[32];
  char gname[32];
  char devmajor[8];
  char devminor[8];
  char prefix[155];
  char pad[12];
};
static_assert(sizeof(TarHeader) == kBlockSize, "ustar header must be exactly one block");

void
WriteOctal(char* field, size_t width, uintmax_t value)
{
  // width includes the trailing NUL: width-1 octal digits, zero-padded.
  const int written = std::snprintf(field, width, "%0*llo", static_cast<int>(width - 1), static_cast<unsigned long long>(value));
  if (written < 0 || static_cast<size_t>(written) >= width)
    throw std::runtime_error("checkpoint archive: value does not fit in tar header field");
}

uintmax_t
ParseOctal(const char* field, size_t width)
{
  uintmax_t value = 0;
  for (size_t i = 0; i < width && field[i] != '\0' && field[i] != ' '; ++i) {
    if (field[i] < '0' || field[i] > '7')
      throw std::runtime_error("checkpoint archive: malformed tar header field");
    value = value * 8 + static_cast<uintmax_t>(field[i] - '0');
  }
  return value;
}

// Plain ustar's 12-byte octal `size` field tops out around 8 GiB, well
// within what a single CRIU pages-*.img file can reach for a GPU-memory
// checkpoint. Fall back to the GNU/POSIX base-256 extension above that: the
// field's leading bit marks binary encoding, with the value as big-endian
// binary across the remaining bytes.
void
WriteSize(char* field, size_t width, uintmax_t value)
{
  constexpr uintmax_t kMaxOctal = (uintmax_t(1) << ((11) * 3)) - 1;  // 8^11 - 1, for an 11-digit field.
  if (value <= kMaxOctal) {
    WriteOctal(field, width, value);
    return;
  }
  auto* bytes = reinterpret_cast<unsigned char*>(field);
  for (size_t i = width; i-- > 1;) {
    bytes[i] = static_cast<unsigned char>(value & 0xFF);
    value >>= 8;
  }
  if (value != 0)
    throw std::runtime_error("checkpoint archive: value too large even for base-256 encoding");
  bytes[0] = 0x80;
}

uintmax_t
ParseSize(const char* field, size_t width)
{
  const auto* bytes = reinterpret_cast<const unsigned char*>(field);
  if ((bytes[0] & 0x80) != 0) {
    uintmax_t value = bytes[0] & 0x7F;
    for (size_t i = 1; i < width; ++i)
      value = (value << 8) | bytes[i];
    return value;
  }
  return ParseOctal(field, width);
}

unsigned
ComputeChecksum(const TarHeader& header)
{
  const auto* bytes = reinterpret_cast<const unsigned char*>(&header);
  const size_t chksum_begin = offsetof(TarHeader, chksum);
  const size_t chksum_end = chksum_begin + sizeof(header.chksum);
  unsigned sum = 0;
  for (size_t i = 0; i < sizeof(TarHeader); ++i)
    sum += (i >= chksum_begin && i < chksum_end) ? ' ' : bytes[i];
  return sum;
}

void
SetChecksum(TarHeader& header)
{
  const unsigned sum = ComputeChecksum(header);
  // Standard layout: 6 octal digits, NUL, trailing space.
  std::snprintf(header.chksum, 7, "%06o", sum);
  header.chksum[7] = ' ';
}

class GzFile {
 public:
  GzFile(const Path& path, const char* mode) : handle_(gzopen(path.c_str(), mode))
  {
    if (handle_ == nullptr)
      throw std::runtime_error("checkpoint archive: failed to open " + path.string());
  }

  ~GzFile()
  {
    if (handle_ != nullptr)
      gzclose(handle_);
  }

  GzFile(const GzFile&) = delete;
  GzFile& operator=(const GzFile&) = delete;

  void Write(const void* data, unsigned size)
  {
    if (size == 0)
      return;
    if (gzwrite(handle_, data, size) != static_cast<int>(size))
      throw std::runtime_error("checkpoint archive: write failed");
  }

  // Reads exactly `size` bytes, or throws on a short read (a truncated
  // archive is corrupt, not a valid empty tail).
  void ReadFull(void* data, unsigned size)
  {
    if (size == 0)
      return;
    if (gzread(handle_, data, size) != static_cast<int>(size))
      throw std::runtime_error("checkpoint archive: unexpected end of archive");
  }

  // Reads up to `size` bytes, returning however many were actually read
  // (possibly 0 at genuine end of stream). Used only where a short read is
  // a legitimate outcome, not corruption -- see ArchiveUncompressedSize.
  unsigned ReadPartial(void* data, unsigned size)
  {
    if (size == 0)
      return 0;
    const int read = gzread(handle_, data, size);
    if (read < 0)
      throw std::runtime_error("checkpoint archive: read failed");
    return static_cast<unsigned>(read);
  }

 private:
  gzFile handle_;
};

void
WritePadding(GzFile& out, uintmax_t size)
{
  const size_t remainder = size % kBlockSize;
  if (remainder == 0)
    return;
  static const char zeros[kBlockSize] = {};
  out.Write(zeros, static_cast<unsigned>(kBlockSize - remainder));
}

void
WriteEntryHeader(GzFile& out, const Path& relative_name, uintmax_t size)
{
  TarHeader header{};
  const std::string name = relative_name.generic_string();
  if (name.empty() || name.size() >= sizeof(header.name))
    throw std::runtime_error("checkpoint archive: entry name too long or empty: " + name);
  std::memcpy(header.name, name.data(), name.size());
  WriteOctal(header.mode, sizeof(header.mode), 0644);
  WriteOctal(header.uid, sizeof(header.uid), 0);
  WriteOctal(header.gid, sizeof(header.gid), 0);
  WriteSize(header.size, sizeof(header.size), size);
  WriteOctal(header.mtime, sizeof(header.mtime), 0);
  header.typeflag[0] = '0';
  std::memcpy(header.magic, "ustar", 5);
  header.version[0] = '0';
  header.version[1] = '0';
  SetChecksum(header);
  out.Write(&header, sizeof(header));
}

void
WriteEntry(GzFile& out, const Path& relative_name, const Path& file_path, uintmax_t size)
{
  WriteEntryHeader(out, relative_name, size);

  std::ifstream in(file_path, std::ios::binary);
  if (!in)
    throw std::runtime_error("checkpoint archive: failed to open " + file_path.string());
  std::vector<char> buffer(kReadChunk);
  uintmax_t remaining = size;
  while (remaining > 0) {
    const auto chunk = static_cast<std::streamsize>(std::min<uintmax_t>(remaining, buffer.size()));
    in.read(buffer.data(), chunk);
    if (in.gcount() != chunk)
      throw std::runtime_error("checkpoint archive: short read from " + file_path.string());
    out.Write(buffer.data(), static_cast<unsigned>(chunk));
    remaining -= static_cast<uintmax_t>(chunk);
  }
  WritePadding(out, size);
}

// Reads one header block. Returns false at a well-formed end-of-archive
// (an all-zero block), throws on anything else malformed.
bool
ReadHeader(GzFile& in, TarHeader& header)
{
  in.ReadFull(&header, sizeof(header));
  static const TarHeader zero{};
  if (std::memcmp(&header, &zero, sizeof(header)) == 0)
    return false;
  if (std::memcmp(header.magic, "ustar", 5) != 0)
    throw std::runtime_error("checkpoint archive: not a ustar archive");
  if (header.typeflag[0] != '0' && header.typeflag[0] != '\0')
    throw std::runtime_error("checkpoint archive: unsupported entry type");
  return true;
}

Path
EntryName(const TarHeader& header)
{
  // name is NUL-padded but not guaranteed NUL-terminated if it fills the
  // field exactly.
  const size_t length = strnlen(header.name, sizeof(header.name));
  return Path(std::string(header.name, length));
}

}  // namespace

void
ArchiveDirectory(const Path& source, const Path& destination, const Path& exclude)
{
  GzFile out(destination, "wb");
  for (const auto& entry : std::filesystem::recursive_directory_iterator(source)) {
    if (entry.is_symlink())
      throw std::runtime_error("checkpoint contains symlink");
    if (!entry.is_regular_file())
      continue;
    const Path relative = std::filesystem::relative(entry.path(), source);
    if (relative == exclude)
      continue;
    WriteEntry(out, relative, entry.path(), entry.file_size());
  }
  static const char end_marker[kBlockSize * 2] = {};
  out.Write(end_marker, sizeof(end_marker));
}

void
ExtractArchive(const Path& source, const Path& destination)
{
  GzFile in(source, "rb");
  TarHeader header;
  std::vector<char> buffer(kReadChunk);
  while (ReadHeader(in, header)) {
    const Path name = EntryName(header);
    const uintmax_t size = ParseSize(header.size, sizeof(header.size));
    const Path target = destination / name;
    std::filesystem::create_directories(target.parent_path());

    std::ofstream out(target, std::ios::binary | std::ios::trunc);
    if (!out)
      throw std::runtime_error("checkpoint archive: failed to create " + target.string());
    uintmax_t remaining = size;
    while (remaining > 0) {
      const auto chunk = static_cast<unsigned>(std::min<uintmax_t>(remaining, buffer.size()));
      in.ReadFull(buffer.data(), chunk);
      out.write(buffer.data(), static_cast<std::streamsize>(chunk));
      remaining -= chunk;
    }

    const size_t remainder = size % kBlockSize;
    if (remainder != 0) {
      char pad[kBlockSize];
      in.ReadFull(pad, static_cast<unsigned>(kBlockSize - remainder));
    }
  }
}

uintmax_t
ArchiveUncompressedSize(const Path& archive)
{
  GzFile in(archive, "rb");
  TarHeader header;
  std::vector<char> discard(kReadChunk);
  uintmax_t total = 0;
  while (ReadHeader(in, header)) {
    const uintmax_t size = ParseSize(header.size, sizeof(header.size));
    total += size;
    // A short read here only means "nothing more to sum" -- a genuine
    // archive we wrote ourselves never truncates mid-body, so this only
    // matters for the synthetic single-header archives capacity checks use
    // to represent an oversized checkpoint without materializing one.
    uintmax_t remaining = size + (size % kBlockSize == 0 ? 0 : kBlockSize - size % kBlockSize);
    while (remaining > 0) {
      const auto chunk = static_cast<unsigned>(std::min<uintmax_t>(remaining, discard.size()));
      const unsigned read = in.ReadPartial(discard.data(), chunk);
      if (read == 0)
        return total;
      remaining -= read;
    }
  }
  return total;
}

void
WriteOversizedEntryForTesting(const Path& archive, uintmax_t declared_size)
{
  GzFile out(archive, "wb");
  WriteEntryHeader(out, "oversized", declared_size);
}

}  // namespace snapshot::pagebroker
