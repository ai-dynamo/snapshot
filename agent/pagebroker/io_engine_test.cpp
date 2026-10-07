// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#include "io_engine.hpp"
#include "cancellation.hpp"
#include "file_descriptor.hpp"

#include <gtest/gtest.h>
#include <nixl.h>
#include <nixl_descriptors.h>
#include <nixl_params.h>
#include <fcntl.h>
#include <unistd.h>

#include <array>
#include <thread>

namespace snapshot::pagebroker {
TEST(NixlTransfer, ReusesRegisteredBufferThroughTransferEngine)
{
  char path[] = "/tmp/pagebroker-nixl-XXXXXX";
  FileDescriptor file(mkstemp(path));
  ASSERT_GE(file.get(), 0);
  ASSERT_EQ(unlink(path), 0);
  constexpr size_t size = 4096;
  ASSERT_EQ(ftruncate(file.get(), size), 0);
  alignas(4096) std::array<unsigned char, size> buffer{};
  std::array<void*, 1> addresses{buffer.data()};
  NixlTransferEngine nixl(addresses, size);
  TransferEngine& engine = nixl;
  ASSERT_EQ(engine.type(), IoEngine::NIXL);
  for (unsigned char value : {0x5a, 0xa5}) {
    buffer.fill(value);
    engine.Open(file.get(), size);
    engine.Submit(0, io::Operation::Write, 0, size);
    engine.Wait(0);
    engine.Close();
    buffer.fill(0);
    engine.Open(file.get(), size);
    engine.Submit(0, io::Operation::Read, 0, size);
    engine.Wait(0);
    engine.Close();
    for (auto actual : buffer) {
      ASSERT_EQ(actual, value);
    }
  }
}

TEST(NixlTransfer, SubmissionFailuresDrainAndDoNotExhaustTheBackendPool)
{
  char path[] = "/tmp/pagebroker-nixl-submit-XXXXXX";
  FileDescriptor writable(mkstemp(path));
  ASSERT_GE(writable.get(), 0);
  FileDescriptor readonly(open(path, O_RDONLY | O_CLOEXEC));
  ASSERT_EQ(unlink(path), 0);
  ASSERT_GE(readonly.get(), 0);
  constexpr size_t size = 4096;
  ASSERT_EQ(ftruncate(writable.get(), size), 0);
  alignas(4096) std::array<unsigned char, size> buffer{};
  buffer.fill(0x5a);
  // Configure the real dependency's minimum pool to expose exhaustion quickly.
  // This avoids adding a test-only pool setting to the production engine.
  nixlAgentConfig config;
  config.useProgThread = true;
  nixlAgent agent("pagebroker-submission-recovery", config);
  nixl_b_params_t parameters{{"use_aio", "true"}, {"ios_pool_size", "64"}};
  nixlBackendH* backend = nullptr;
  ASSERT_EQ(agent.createBackend("POSIX", parameters, backend), NIXL_SUCCESS);
  nixl_reg_dlist_t buffers(DRAM_SEG), files(FILE_SEG);
  buffers.addDesc(nixlBlobDesc(reinterpret_cast<uintptr_t>(buffer.data()), size, 0));
  files.addDesc(nixlBlobDesc(0, size, writable.get()));
  files.addDesc(nixlBlobDesc(0, size, readonly.get()));
  ASSERT_EQ(agent.registerMem(buffers), NIXL_SUCCESS);
  ASSERT_EQ(agent.registerMem(files), NIXL_SUCCESS);
  for (int iteration = 0; iteration < 129; ++iteration) {
    SCOPED_TRACE(iteration);
    const bool recover = iteration == 128;
    nixl_xfer_dlist_t local(DRAM_SEG), remote(FILE_SEG);
    local.addDesc(nixlBlobDesc(reinterpret_cast<uintptr_t>(buffer.data()), size, 0));
    remote.addDesc(nixlBlobDesc(0, size, recover ? writable.get() : readonly.get()));
    nixlXferReqH* request = nullptr;
    ASSERT_EQ(agent.createXferReq(NIXL_WRITE, local, remote, "pagebroker-submission-recovery", request), NIXL_SUCCESS);
    auto status = agent.postXferReq(request);
    const auto deadline = std::chrono::steady_clock::now() + std::chrono::seconds{2};
    while (status == NIXL_IN_PROG && std::chrono::steady_clock::now() < deadline) {
      status = agent.getXferStatus(request);
      std::this_thread::yield();
    }
    ASSERT_NE(status, NIXL_IN_PROG);
    EXPECT_EQ(status, recover ? NIXL_SUCCESS : NIXL_ERR_BACKEND);
    ASSERT_EQ(agent.releaseXferReq(request), NIXL_SUCCESS);
  }
  ASSERT_EQ(agent.deregisterMem(files), NIXL_SUCCESS);
  ASSERT_EQ(agent.deregisterMem(buffers), NIXL_SUCCESS);
  std::array<unsigned char, size> saved{};
  ASSERT_EQ(pread(writable.get(), saved.data(), saved.size(), 0), static_cast<ssize_t>(saved.size()));
  EXPECT_EQ(saved, buffer);
}

void RunFatalNixlCase(NixlTransferEngine::Fault failure, int observed)
{
  // A missing watchdog or unbounded shutdown must fail rather than hang CI.
  alarm(5);
  std::atomic<bool> stopping{false};
  FatalCleanupShutdown shutdown(stopping, std::chrono::seconds{1});
  Cancellation cancellation;
  char path[] = "/tmp/pagebroker-nixl-fatal-XXXXXX";
  FileDescriptor file(mkstemp(path));
  if (file.get() < 0 || unlink(path) || ftruncate(file.get(), 4096)) {
    std::_Exit(2);
  }
  alignas(4096) std::array<unsigned char, 4096> buffer{};
  std::array<void*, 1> addresses{buffer.data()};
  struct Owner {
    ~Owner() { std::_Exit(42); }
  } owner;
  std::jthread other([&] {
    while (!stopping.load() || !cancellation.IsCancelled()) {
      std::this_thread::yield();
    }
    // Observe real cancellation and retained file ownership from another thread.
    const char retained = fcntl(file.get(), F_GETFD) >= 0 ? 1 : 0;
    if (write(observed, &retained, 1) != 1) {
      std::_Exit(4);
    }
  });
  {
    // Force only this failure. Setup, submission, and all other calls remain real.
    NixlTransferEngine engine(addresses, buffer.size(), [failure](NixlTransferEngine::Fault point) {
      return point == failure;
    });
    engine.Open(file.get(), buffer.size());
    engine.Submit(0, io::Operation::Write, 0, buffer.size());
    engine.Wait(0);
    engine.Close();
  }
  std::_Exit(3);  // A fatal path must not return or finish destruction.
}

TEST(NixlTransfer, FatalCleanupRetainsOwnersAndCancelsOtherWorkBeforeExit)
{
  const struct {
    NixlTransferEngine::Fault failure;
    const char* diagnostic;
  } cases[] = {
    {NixlTransferEngine::Fault::DrainTimeout, "NIXL transfer did not drain before its deadline"},
    {NixlTransferEngine::Fault::RequestRelease, "release completed NIXL request failed"},
    {NixlTransferEngine::Fault::FileRelease, "NIXL file deregistration failed"},
    {NixlTransferEngine::Fault::BufferRelease, "NIXL buffer deregistration failed"},
  };
  for (const auto& test : cases) {
    SCOPED_TRACE(test.diagnostic);
    int descriptors[2];
    ASSERT_EQ(pipe2(descriptors, O_CLOEXEC | O_NONBLOCK), 0);
    FileDescriptor reader(descriptors[0]);
    FileDescriptor writer(descriptors[1]);
    ASSERT_EXIT(RunFatalNixlCase(test.failure, writer.get()),
                ::testing::ExitedWithCode(EXIT_FAILURE), test.diagnostic);
    char retained = 0;
    ASSERT_EQ(read(reader.get(), &retained, 1), 1) << "other work did not observe cancellation before exit";
    EXPECT_EQ(retained, 1) << "file ownership was released before exit";
  }
}
}  // namespace snapshot::pagebroker
