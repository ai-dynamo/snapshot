# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Decode span records, preserving evidence of MPI-forwarder split writes."""

import json
import re


def decode_spans(lines):
    records = []
    repairs = []
    pending = None
    for number, line in enumerate(lines, 1):
        if "STARTUP_SPAN " in line:
            pieces = line.split("STARTUP_SPAN ")[1:]
            for piece in pieces:
                piece = re.split(r"import time:\s+\d+\s*\|", piece, maxsplit=1)[0]
                try:
                    record = json.loads(piece)
                except json.JSONDecodeError:
                    if pending is not None:
                        raise ValueError(
                            f"Multiple incomplete span records at line {number}"
                        )
                    pending = (number, piece)
                    continue
                records.append(record)
        elif pending is not None:
            # MPI forwarding can insert complete records between a JSON key and
            # its value. Rejoin original bytes only; never infer missing fields.
            try:
                record = json.loads(pending[1] + line)
            except json.JSONDecodeError:
                continue
            records.append(record)
            repairs.append(
                dict(
                    prefix_line=pending[0],
                    continuation_line=number,
                    span=record["span"],
                )
            )
            pending = None
    if pending is not None:
        raise ValueError(f"Unrecoverable partial span at line {pending[0]}")
    return records, repairs
