// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0
#pragma once

#include <google/protobuf/message_lite.h>
#include <vector>

#include "file_descriptor.hpp"

namespace snapshot::pagebroker {
constexpr size_t kAllocationBatchLimit = 32;
constexpr size_t kFrameDescriptorLimit = 2 * kAllocationBatchLimit;
constexpr size_t kFrameSizeLimit = 64 << 10;

// All rights accompany the first bytes of the network-order length prefix.
// Received rights are close-on-exec and owned even on malformed/truncated input.
// EOF before a frame returns false; partial frames and invalid input throw.
bool ReceiveFrame(int socket, google::protobuf::MessageLite& message, std::vector<FileDescriptor>& descriptors);
void SendFrame(int socket, const google::protobuf::MessageLite& message, const std::vector<int>& descriptors = {});
void SetAllocationTimeout(int socket);
}  // namespace snapshot::pagebroker
