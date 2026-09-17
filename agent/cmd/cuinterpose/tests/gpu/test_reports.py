# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

"""The JSONL parser also runs on headless hosts without importing CUDA."""

from reports import parse_phase_lines


def test_reports():
    assert parse_phase_lines(
        'noise\n{"phase":"load_allocations","status":"ok","allocation_bytes":123}'
    ) == {"load_allocations": {"status": "ok", "allocation_bytes": 123}}
    for invalid in ('null', '[]', '{}', '{"phase":"inspect"}',
                    '{"phase":"inspect","status":"ok"} trailing'):
        assert parse_phase_lines(invalid) == {}


if __name__ == "__main__":
    test_reports()
