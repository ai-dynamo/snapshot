#!/bin/sh
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Emits the Go-module section of a THIRD-PARTY attribution file: the module set
# the build actually used, followed by each module's upstream license text.
#
# Shared by the agent and operator attribution generators, which differ only in
# the sections around this one.

set -eu

VENDOR=${1:?usage: go-licenses.sh <vendor-dir>}

[ -d "$VENDOR" ] || exit 0

echo
echo "================================================================================"
echo "GO MODULES (STATICALLY LINKED)"
echo "--------------------------------------------------------------------------------"
echo

# vendor/modules.txt records the exact module set and versions the build used.
if [ -f "$VENDOR/modules.txt" ]; then
    grep '^# ' "$VENDOR/modules.txt" | sed 's/^# /  /'
fi

echo
echo "--------------------------------------------------------------------------------"
echo "FULL LICENSE TEXT — GO MODULES"
echo "--------------------------------------------------------------------------------"
find "$VENDOR" -type f \
    \( -iname 'LICENSE*' -o -iname 'LICENCE*' -o -iname 'COPYING*' -o -iname 'NOTICE*' \) \
    | LC_ALL=C sort | while read -r lf; do
    mod=$(dirname "${lf#"$VENDOR"/}")
    echo
    echo "================================================================================"
    echo "MODULE: $mod  ($(basename "$lf"))"
    echo "================================================================================"
    cat "$lf"
done
