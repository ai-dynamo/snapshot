// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <cstddef>
#include <cstdint>

// Snapshot-owned declarations for the libstreamer.so C ABI. These signatures
// match run-ai/runai-model-streamer commit bc21fd4182cc06ce9475452d16697d50ce3588c4
// (multi-submission, per-range destinations). The public 0.16.1 ABI is older
// and is not compatible with these declarations.
namespace snapshot::pagebroker::model_streamer_api {
using SubmissionId = std::uint64_t;

// ResponseCode::TimedOut in the pinned Model Streamer ABI. A finite response
// timeout lets the single API thread notice and submit newly queued work.
inline constexpr int kTimedOut = 16;

extern "C" {
int runai_start(void** streamer);
void runai_end(void* streamer);

int runai_request(
    void* streamer,
    SubmissionId* out_submission_id,
    unsigned num_files,
    const char** paths,
    unsigned* num_ranges,
    std::size_t* range_offsets,
    std::size_t* range_sizes,
    void** range_destinations);

int runai_response(
    void* streamer,
    SubmissionId* out_submission_id,
    unsigned* file_index,
    unsigned* range_index,
    int* submission_done,
    unsigned timeout_ms);

const char* runai_response_str(int response_code);
}
}  // namespace snapshot::pagebroker::model_streamer_api
