// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include <gtest/gtest.h>
#include <google/protobuf/struct.pb.h>
#include <google/protobuf/text_format.h>
#include <google/protobuf/util/json_util.h>
#include <google/protobuf/util/message_differencer.h>

#include <fstream>
#include <iterator>
#include <memory>
#include <string>

#include "pagebroker_types.hpp"

namespace {

std::unique_ptr<google::protobuf::Message>
WireMessage(const std::string& type)
{
  if (type == "Request")
    return std::make_unique<snapshot::pagebroker::Request>();
  if (type == "Response")
    return std::make_unique<snapshot::pagebroker::Response>();
  return nullptr;
}

std::string
Hex(const std::string& bytes)
{
  constexpr char digits[] = "0123456789abcdef";
  std::string result;
  result.reserve(bytes.size() * 2);
  for (unsigned char byte : bytes) {
    result += digits[byte >> 4];
    result += digits[byte & 15];
  }
  return result;
}

TEST(StorageContract, SharedWireFixtures)
{
  // These are the same golden bytes used by the Go protocol test. An encoder
  // round trip alone would miss a coordinated field-number or enum-value change.
  std::ifstream file("testdata/storage-contract/wire.json");
  ASSERT_TRUE(file.is_open());
  const std::string json((std::istreambuf_iterator<char>(file)), std::istreambuf_iterator<char>());
  google::protobuf::Struct fixtures;
  const auto status = google::protobuf::util::JsonStringToMessage(json, &fixtures);
  ASSERT_TRUE(status.ok()) << status.ToString();
  ASSERT_TRUE(fixtures.fields().contains("cases"));
  const auto& cases = fixtures.fields().at("cases").list_value().values();
  ASSERT_EQ(cases.size(), 21);

  for (const auto& fixture : cases) {
    const auto& fields = fixture.struct_value().fields();
    for (const char* key : {"name", "type", "text", "wire_hex"}) {
      ASSERT_TRUE(fields.contains(key)) << key;
      ASSERT_EQ(fields.at(key).kind_case(), google::protobuf::Value::kStringValue) << key;
    }
    SCOPED_TRACE(fields.at("name").string_value());
    auto expected = WireMessage(fields.at("type").string_value());
    auto decoded = WireMessage(fields.at("type").string_value());
    ASSERT_NE(expected, nullptr);
    ASSERT_NE(decoded, nullptr);
    ASSERT_TRUE(google::protobuf::TextFormat::ParseFromString(fields.at("text").string_value(), expected.get()));

    const auto& hex = fields.at("wire_hex").string_value();
    ASSERT_EQ(hex.size() % 2, 0u);
    std::string wire;
    for (size_t i = 0; i < hex.size(); i += 2) {
      const std::string digits = "0123456789abcdef";
      const auto high = digits.find(hex[i]);
      const auto low = digits.find(hex[i + 1]);
      ASSERT_NE(high, std::string::npos);
      ASSERT_NE(low, std::string::npos);
      wire += static_cast<char>((high << 4) | low);
    }
    ASSERT_TRUE(decoded->ParseFromString(wire));
    EXPECT_TRUE(google::protobuf::util::MessageDifferencer::Equals(*expected, *decoded));
    std::string encoded;
    ASSERT_TRUE(expected->SerializeToString(&encoded));
    EXPECT_EQ(Hex(encoded), hex);
  }
}

}  // namespace
