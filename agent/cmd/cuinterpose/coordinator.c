/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * Placeholder coordinator. The snapshot agent runs the coordinator only when a
 * checkpointed CUDA process shows a cuinterpose control socket, and this
 * build's shim opens none, so this program is never reached in practice. If it
 * is, it refuses to do anything and says so. The real coordinator replaces it
 * in a following change; the agent's argument contract is already final and
 * covered by the Go tests against a fake binary.
 */

#include <stdio.h>

#include "protocol.h"

int
main(int argc, char** argv)
{
  (void)argc;
  (void)argv;
  fprintf(stderr, "cuinterpose-coordinator (protocol %u): this build carries no coordinator; nothing was prepared or restored\n",
          (unsigned)CUINTERPOSE_VERSION);
  return 1;
}
