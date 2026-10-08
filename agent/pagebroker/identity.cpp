// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "identity.hpp"

#include <array>
#include <cstdint>
#include <cstring>
#include <initializer_list>
#include <stdexcept>
#include <vector>

namespace snapshot::pagebroker {
namespace {

// Self-contained SHA-256 (FIPS 180-4). PageBroker's build has no existing
// crypto dependency; adding one for four hashed fields is not worth a new
// base-image package. This never sees untrusted-length input (every field is
// bounded well below uint32 by the public entry points, matching the Go side).
class Sha256 {
 public:
  Sha256() { Reset(); }

  void Update(const unsigned char* data, size_t length)
  {
    for (size_t i = 0; i < length; ++i) {
      buffer_[buffer_length_++] = data[i];
      if (buffer_length_ == 64) {
        Transform(buffer_.data());
        bit_length_ += 512;
        buffer_length_ = 0;
      }
    }
  }

  std::array<unsigned char, 32> Finalize()
  {
    uint64_t total_bits = bit_length_ + static_cast<uint64_t>(buffer_length_) * 8;

    size_t index = buffer_length_;
    buffer_[index++] = 0x80;
    if (index > 56) {
      while (index < 64) buffer_[index++] = 0x00;
      Transform(buffer_.data());
      index = 0;
    }
    while (index < 56) buffer_[index++] = 0x00;
    for (int i = 7; i >= 0; --i) buffer_[index++] = static_cast<unsigned char>(total_bits >> (i * 8));
    Transform(buffer_.data());

    std::array<unsigned char, 32> digest{};
    for (int i = 0; i < 8; ++i) {
      digest[i * 4] = static_cast<unsigned char>(state_[i] >> 24);
      digest[i * 4 + 1] = static_cast<unsigned char>(state_[i] >> 16);
      digest[i * 4 + 2] = static_cast<unsigned char>(state_[i] >> 8);
      digest[i * 4 + 3] = static_cast<unsigned char>(state_[i]);
    }
    return digest;
  }

 private:
  void Reset()
  {
    state_ = {0x6a09e667u, 0xbb67ae85u, 0x3c6ef372u, 0xa54ff53au,
              0x510e527fu, 0x9b05688cu, 0x1f83d9abu, 0x5be0cd19u};
    bit_length_ = 0;
    buffer_length_ = 0;
  }

  static uint32_t RotR(uint32_t value, int bits) { return (value >> bits) | (value << (32 - bits)); }

  void Transform(const unsigned char* block)
  {
    static constexpr uint32_t kRoundConstants[64] = {
        0x428a2f98u, 0x71374491u, 0xb5c0fbcfu, 0xe9b5dba5u, 0x3956c25bu, 0x59f111f1u, 0x923f82a4u, 0xab1c5ed5u,
        0xd807aa98u, 0x12835b01u, 0x243185beu, 0x550c7dc3u, 0x72be5d74u, 0x80deb1feu, 0x9bdc06a7u, 0xc19bf174u,
        0xe49b69c1u, 0xefbe4786u, 0x0fc19dc6u, 0x240ca1ccu, 0x2de92c6fu, 0x4a7484aau, 0x5cb0a9dcu, 0x76f988dau,
        0x983e5152u, 0xa831c66du, 0xb00327c8u, 0xbf597fc7u, 0xc6e00bf3u, 0xd5a79147u, 0x06ca6351u, 0x14292967u,
        0x27b70a85u, 0x2e1b2138u, 0x4d2c6dfcu, 0x53380d13u, 0x650a7354u, 0x766a0abbu, 0x81c2c92eu, 0x92722c85u,
        0xa2bfe8a1u, 0xa81a664bu, 0xc24b8b70u, 0xc76c51a3u, 0xd192e819u, 0xd6990624u, 0xf40e3585u, 0x106aa070u,
        0x19a4c116u, 0x1e376c08u, 0x2748774cu, 0x34b0bcb5u, 0x391c0cb3u, 0x4ed8aa4au, 0x5b9cca4fu, 0x682e6ff3u,
        0x748f82eeu, 0x78a5636fu, 0x84c87814u, 0x8cc70208u, 0x90befffau, 0xa4506cebu, 0xbef9a3f7u, 0xc67178f2u};

    uint32_t w[64];
    for (int i = 0; i < 16; ++i) {
      w[i] = (static_cast<uint32_t>(block[i * 4]) << 24) | (static_cast<uint32_t>(block[i * 4 + 1]) << 16) |
             (static_cast<uint32_t>(block[i * 4 + 2]) << 8) | static_cast<uint32_t>(block[i * 4 + 3]);
    }
    for (int i = 16; i < 64; ++i) {
      const uint32_t s0 = RotR(w[i - 15], 7) ^ RotR(w[i - 15], 18) ^ (w[i - 15] >> 3);
      const uint32_t s1 = RotR(w[i - 2], 17) ^ RotR(w[i - 2], 19) ^ (w[i - 2] >> 10);
      w[i] = w[i - 16] + s0 + w[i - 7] + s1;
    }

    uint32_t a = state_[0], b = state_[1], c = state_[2], d = state_[3];
    uint32_t e = state_[4], f = state_[5], g = state_[6], h = state_[7];

    for (int i = 0; i < 64; ++i) {
      const uint32_t s1 = RotR(e, 6) ^ RotR(e, 11) ^ RotR(e, 25);
      const uint32_t ch = (e & f) ^ (~e & g);
      const uint32_t temp1 = h + s1 + ch + kRoundConstants[i] + w[i];
      const uint32_t s0 = RotR(a, 2) ^ RotR(a, 13) ^ RotR(a, 22);
      const uint32_t maj = (a & b) ^ (a & c) ^ (b & c);
      const uint32_t temp2 = s0 + maj;
      h = g;
      g = f;
      f = e;
      e = d + temp1;
      d = c;
      c = b;
      b = a;
      a = temp1 + temp2;
    }

    state_[0] += a;
    state_[1] += b;
    state_[2] += c;
    state_[3] += d;
    state_[4] += e;
    state_[5] += f;
    state_[6] += g;
    state_[7] += h;
  }

  std::array<uint32_t, 8> state_{};
  uint64_t bit_length_ = 0;
  std::array<unsigned char, 64> buffer_{};
  size_t buffer_length_ = 0;
};

std::string
HexEncode(const std::array<unsigned char, 32>& digest)
{
  static constexpr char kDigits[] = "0123456789abcdef";
  std::string hex;
  hex.reserve(64);
  for (unsigned char byte : digest) {
    hex += kDigits[byte >> 4];
    hex += kDigits[byte & 0x0f];
  }
  return hex;
}

// AppendLengthPrefixed matches Go's hashFields: a big-endian uint32 byte
// count, no terminator, followed by the field bytes.
void
AppendLengthPrefixed(Sha256& hash, const std::string& field)
{
  unsigned char length[4];
  const uint32_t size = static_cast<uint32_t>(field.size());
  length[0] = static_cast<unsigned char>(size >> 24);
  length[1] = static_cast<unsigned char>(size >> 16);
  length[2] = static_cast<unsigned char>(size >> 8);
  length[3] = static_cast<unsigned char>(size);
  hash.Update(length, 4);
  hash.Update(reinterpret_cast<const unsigned char*>(field.data()), field.size());
}

std::string
HashFields(std::initializer_list<std::string> fields)
{
  Sha256 hash;
  for (const auto& field : fields) AppendLengthPrefixed(hash, field);
  return HexEncode(hash.Finalize());
}

bool
IsDNS1123Label(const std::string& value)
{
  // RFC 1123 label: lowercase alphanumeric or '-', <= 63 chars, must start
  // and end with an alphanumeric character. Matches
  // k8s.io/apimachinery/pkg/util/validation.IsDNS1123Label's shape.
  if (value.empty() || value.size() > 63)
    return false;
  for (size_t i = 0; i < value.size(); ++i) {
    const char c = value[i];
    const bool alnum = (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9');
    if (alnum)
      continue;
    if (c == '-' && i != 0 && i != value.size() - 1)
      continue;
    return false;
  }
  return true;
}

bool
IsDNS1123Subdomain(const std::string& value)
{
  // Dot-separated DNS1123 labels, <= 253 chars total.
  if (value.empty() || value.size() > 253)
    return false;
  size_t start = 0;
  while (start <= value.size()) {
    const size_t dot = value.find('.', start);
    const std::string label = value.substr(start, dot == std::string::npos ? std::string::npos : dot - start);
    if (!IsDNS1123Label(label))
      return false;
    if (dot == std::string::npos)
      break;
    start = dot + 1;
  }
  return true;
}

bool
IsControl(unsigned char c)
{
  return c < 0x20 || c == 0x7f;
}

// NormalizeBasePath mirrors Go's PVC.NormalizedBasePath: absolute, bounded,
// no backslashes or control characters, no ".." traversal component, then
// lexically cleaned (collapsing "//", "/./" and a trailing slash) the same
// way Go's path.Clean does for an already-traversal-free absolute path.
std::string
NormalizeBasePath(const std::string& base_path)
{
  if (base_path.empty() || base_path.size() > 4096 || base_path.front() != '/')
    throw std::invalid_argument(
        "PVC base path must be an absolute path within the claim, at most 4096 bytes, "
        "without control characters or backslashes");
  if (base_path.find('\\') != std::string::npos)
    throw std::invalid_argument("PVC base path must not contain backslashes");
  for (unsigned char c : base_path) {
    if (IsControl(c))
      throw std::invalid_argument("PVC base path must not contain control characters");
  }
  if (base_path.front() == ' ' || base_path.front() == '\t' || base_path.back() == ' ' || base_path.back() == '\t' ||
      base_path.back() == '\n' || base_path.back() == '\r')
    throw std::invalid_argument("PVC base path must not have leading or trailing whitespace");

  std::vector<std::string> components;
  size_t start = 0;
  while (start <= base_path.size()) {
    const size_t slash = base_path.find('/', start);
    const std::string part = base_path.substr(start, slash == std::string::npos ? std::string::npos : slash - start);
    if (part == "..")
      throw std::invalid_argument("PVC base path must not contain parent traversal");
    if (!part.empty() && part != ".")
      components.push_back(part);
    if (slash == std::string::npos)
      break;
    start = slash + 1;
  }

  if (components.empty())
    return "/";
  std::string cleaned;
  for (const auto& component : components) {
    cleaned += '/';
    cleaned += component;
  }
  return cleaned;
}

}  // namespace

std::string
Sha256Hex(const std::string& data)
{
  Sha256 hash;
  hash.Update(reinterpret_cast<const unsigned char*>(data.data()), data.size());
  return HexEncode(hash.Finalize());
}

std::string
ComputeStoreID(const std::string& ns, const std::string& claim_name, const std::string& base_path)
{
  if (!IsDNS1123Label(ns))
    throw std::invalid_argument("PVC namespace must be a valid DNS1123 label");
  if (!IsDNS1123Subdomain(claim_name))
    throw std::invalid_argument("PVC claim name must be a valid DNS1123 subdomain");
  const std::string normalized = NormalizeBasePath(base_path);
  return "store-v1-" + HashFields({"snapshot.store/v1", "pvc", ns, claim_name, normalized});
}

std::string
ComputeCommitID(const std::string& store_id, const std::string& artifact_uid, const std::string& container_name)
{
  // Mirrors the Go side's storeIDPattern: exactly "store-v1-" + 64 lowercase
  // hex characters.
  static constexpr char kPrefix[] = "store-v1-";
  constexpr size_t kPrefixLength = sizeof(kPrefix) - 1;
  constexpr size_t kHexLength = 64;
  if (store_id.size() != kPrefixLength + kHexLength || store_id.compare(0, kPrefixLength, kPrefix) != 0)
    throw std::invalid_argument("store ID must be store-v1- followed by 64 lowercase hexadecimal characters");
  for (size_t i = kPrefixLength; i < store_id.size(); ++i) {
    const char c = store_id[i];
    if (!((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')))
      throw std::invalid_argument("store ID must be store-v1- followed by 64 lowercase hexadecimal characters");
  }
  if (artifact_uid.empty() || artifact_uid.size() > 256)
    throw std::invalid_argument(
        "artifact UID must be nonempty, at most 256 bytes, without whitespace, control characters or path "
        "separators");
  for (unsigned char c : artifact_uid) {
    if (c == '/' || c == '\\' || IsControl(c) || c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' ||
        c == '\f')
      throw std::invalid_argument(
          "artifact UID must be nonempty, at most 256 bytes, without whitespace, control characters or path "
          "separators");
  }
  if (!IsDNS1123Label(container_name))
    throw std::invalid_argument("container name must be a valid DNS1123 label");
  return "commit-v1-" + HashFields({"snapshot.commit/v1", store_id, artifact_uid, container_name});
}

}  // namespace snapshot::pagebroker
