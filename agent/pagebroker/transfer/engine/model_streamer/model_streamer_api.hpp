// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <streamer/streamer.h>

// Compile against the public headers shipped with the pinned native library.
// Keep only convenience aliases here; the upstream header owns the C ABI.
namespace snapshot::pagebroker::model_streamer_api {
using SubmissionId = RunaiFileStreamerSubmissionId;
inline constexpr int kTimedOutStatusCode = RUNAI_FILE_STREAMER_RESPONSE_TIMED_OUT;
using ::runai_file_streamer_start;
using ::runai_file_streamer_end;
using ::runai_file_streamer_request;
using ::runai_file_streamer_response;
using ::runai_file_streamer_response_str;
using ::runai_file_streamer_set_credentials;
}  // namespace snapshot::pagebroker::model_streamer_api
