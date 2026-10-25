#!/bin/sh
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

set -eu
export LC_ALL=C

# The frontend's own references to its wrappers must bind locally, so another preload
# that exports the same names cannot replace them.
library=$1
# Capture each tool's output first so that set -e stops on its failure.
exports=$(nm -D --defined-only --format=posix "$library")
relocations=$(readelf -rW "$library")
{
    printf '%s\n' "$exports" | awk '$2 == "T" { sub(/@.*/, "", $1); print "export", $1 }'
    printf '%s\n' "$relocations" | sed 's/^/relocation /'
} | awk -v library="$library" '
    $1 == "export" { exported[$2] = 1; found = 1; next }
    $1 == "relocation" && NF >= 6 {
        name = $6
        sub(/@.*/, "", name)
        if (name in exported) {
            sub(/^relocation /, "")
            print library ": preemptible reference: " $0 > "/dev/stderr"
            failed = 1
        }
    }
    END {
        if (!found) {
            print library ": no exported functions found" > "/dev/stderr"
            failed = 1
        }
        exit failed
    }
'
