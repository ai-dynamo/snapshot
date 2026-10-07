#!/bin/sh
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
# SPDX-License-Identifier: Apache-2.0

set -eu
export LC_ALL=C

# The preload libraries run against the workload's glibc, not the builder's.
for library do
    versions=$(readelf --version-info --wide "$library")
    printf '%s\n' "$versions" | awk -v library="$library" '
        /^Version needs section/ { needed = 1; next }
        /^Version .* section/ { needed = 0 }
        needed {
            for (i = 1; i < NF; i++) {
                if ($i != "Name:" || $(i + 1) !~ /^GLIBC_/) continue
                version = $(i + 1)
                found = 1
                count = split(substr(version, 7), parts, ".")
                if (version !~ /^GLIBC_[0-9]+\.[0-9]+(\.[0-9]+)?$/ ||
                    parts[1] > 2 || (parts[1] == 2 &&
                    (parts[2] > 34 || (parts[2] == 34 && count == 3 && parts[3] > 0)))) {
                    print library ": unsupported requirement " version " (maximum GLIBC_2.34)" > "/dev/stderr"
                    failed = 1
                }
            }
        }
        END {
            if (!found) {
                print library ": no imported GLIBC versions found" > "/dev/stderr"
                failed = 1
            }
            exit failed
        }
    '
done
