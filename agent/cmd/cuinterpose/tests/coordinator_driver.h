/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
 * All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

// Runs the real coordinator binary against this test process (and any forked
// children) the way the snapshot agent would, for the lifecycle tests.

#ifndef CUINTERPOSE_TESTS_COORDINATOR_DRIVER_H
#define CUINTERPOSE_TESTS_COORDINATOR_DRIVER_H

#include <sys/types.h>
#include <sys/wait.h>
#include <unistd.h>

#include <cstdlib>
#include <string>
#include <vector>

struct Outcome {
  int status;
  std::string out;
  std::string err;
};

// Runs `cuinterpose-coordinator MODE` (CUINTERPOSE_COORDINATOR in the
// environment) against the given PIDs, which double as namespace PIDs because
// the tests run without a PID namespace. Returns its exit status and output.
inline Outcome coordinate(const char* mode, const std::string& checkpoint, const std::vector<pid_t>& pids) {
  const char* binary = getenv("CUINTERPOSE_COORDINATOR");
  const char* control = getenv("SNAPSHOT_CONTROL_DIR");
  int out_pipe[2], err_pipe[2];
  if (pipe(out_pipe) != 0 || pipe(err_pipe) != 0) abort();
  pid_t child = fork();
  if (child == 0) {
    dup2(out_pipe[1], STDOUT_FILENO);
    dup2(err_pipe[1], STDERR_FILENO);
    std::vector<std::string> args = {binary, mode, "--proc-root", "", "--checkpoint-dir", checkpoint,
                                     "--control-dir", control};
    for (pid_t pid : pids) {
      args.push_back("--process");
      args.push_back(std::to_string(pid));
      args.push_back(std::to_string(pid));
    }
    std::vector<char*> argv;
    for (auto& a : args) argv.push_back(const_cast<char*>(a.c_str()));
    argv.push_back(nullptr);
    // The coordinator must not itself be interposed.
    unsetenv("LD_PRELOAD");
    execv(binary, argv.data());
    perror("execv cuinterpose-coordinator");
    _exit(127);
  }
  close(out_pipe[1]);
  close(err_pipe[1]);
  auto drain = [](int fd) {
    std::string s;
    char buffer[4096];
    ssize_t n;
    while ((n = read(fd, buffer, sizeof(buffer))) > 0) s.append(buffer, n);
    close(fd);
    return s;
  };
  Outcome outcome;
  outcome.out = drain(out_pipe[0]);
  outcome.err = drain(err_pipe[0]);
  int status = 0;
  waitpid(child, &status, 0);
  outcome.status = WIFEXITED(status) ? WEXITSTATUS(status) : -1;
  return outcome;
}

#endif
