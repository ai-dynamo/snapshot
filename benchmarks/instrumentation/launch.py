# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Install timers before starting the measured Python interpreter."""

import os
import subprocess
import sys

env = dict(os.environ)
env.pop("PYTHONPROFILEIMPORTTIME", None)
subprocess.run([sys.executable, "/snapshot-app/patch_sources.py"], env=env, check=True)
os.execv(sys.executable, [sys.executable, "/snapshot-app/bench.py", *sys.argv[1:]])
