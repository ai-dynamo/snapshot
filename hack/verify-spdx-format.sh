#!/bin/sh
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Checks the shape of SPDX headers, which addlicense does not.
#
# addlicense -check only asks whether license text is present, so a header that
# folds the identifier into another line:
#
#     * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
#     * All rights reserved. SPDX-License-Identifier: Apache-2.0
#
# passes it while still being non-conforming: the identifier has to start a line
# of its own. This catches that.

set -eu

# This script's own pattern would match, so exclude it.
offenders=$(
    git grep -nI 'SPDX-License-Identifier:' -- . ':!hack/verify-spdx-format.sh' \
        | grep -vE ':[0-9]+:[[:space:]]*(#|//|\*|/\*|--|;|<!--)?[[:space:]]*SPDX-License-Identifier:' \
        || true
)

if [ -n "$offenders" ]; then
    echo "ERROR: SPDX-License-Identifier must start its own line, after at most a comment leader:"
    echo "$offenders" | sed 's/^/  /'
    echo
    echo "Expected two separate lines:"
    echo "  SPDX-FileCopyrightText: Copyright (c) <year> NVIDIA CORPORATION & AFFILIATES. All rights reserved."
    echo "  SPDX-License-Identifier: Apache-2.0"
    exit 1
fi
