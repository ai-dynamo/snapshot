// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Drives the cuinterpose-coordinator binary against fake participants: small
// in-process servers that speak the shim's control protocol and record what
// they were asked to do. No CUDA, no shim.

#include <fcntl.h>
#include <gtest/gtest.h>
#include <pthread.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/un.h>
#include <sys/wait.h>
#include <unistd.h>

#include <atomic>
#include <chrono>
#include <condition_variable>
#include <cstring>
#include <fstream>
#include <functional>
#include <mutex>
#include <sstream>
#include <filesystem>
#include <string>
#include <thread>
#include <vector>

extern "C" {
#include "../protocol.h"
#include "../util.h"
}

namespace {

std::string coordinator_path() {
  const char* path = getenv("CUINTERPOSE_COORDINATOR");
  return path != nullptr ? path : "./cuinterpose-coordinator";
}

// A fake shim endpoint. It answers every operation with success unless a
// handler says otherwise, and remembers the order of operations it served.
class Participant {
 public:
  Participant(const std::string& dir, int namespace_pid, const std::string& id)
      : id_(id), path_(dir + "/" + CUINTERPOSE_SOCKET_PREFIX + std::to_string(namespace_pid) + ".sock") {
    listener_ = socket(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0);
    sockaddr_un address{};
    address.sun_family = AF_UNIX;
    std::strncpy(address.sun_path, path_.c_str(), sizeof(address.sun_path) - 1);
    if (bind(listener_, reinterpret_cast<sockaddr*>(&address), sizeof(address)) != 0 || listen(listener_, 16) != 0) {
      perror("participant listen");
      abort();
    }
    thread_ = std::thread([this] { serve(); });
  }
  ~Participant() {
    stop();
    unlink(path_.c_str());
  }
  void stop() {
    if (listener_ >= 0) {
      shutdown(listener_, SHUT_RDWR);
      close(listener_);
      listener_ = -1;
    }
    if (thread_.joinable()) thread_.join();
  }

  std::vector<struct cuinterpose_record> records;  // INSPECT payload
  uint32_t live_raw_imports = 0;
  uint8_t phase = CUINTERPOSE_PHASE_ACTIVE;
  // Operations to fail, and an optional hook that runs before answering.
  std::vector<uint16_t> fail;
  std::function<void(uint16_t)> before_reply;

  std::vector<uint16_t> operations() {
    std::lock_guard<std::mutex> lock(mutex_);
    return operations_;
  }

 private:
  void serve() {
    for (;;) {
      int client = accept4(listener_, nullptr, nullptr, SOCK_CLOEXEC);
      if (client < 0) return;
      std::thread([this, client] { handle(client); }).detach();
    }
  }
  void handle(int client) {
    struct cuinterpose_header request{};
    int passed = -1;
    if (cuinterpose_receive_header(client, &request, &passed) == 0) {
      if (passed >= 0) close(passed);
      {
        std::lock_guard<std::mutex> lock(mutex_);
        operations_.push_back(request.operation);
      }
      if (before_reply) before_reply(request.operation);
      struct cuinterpose_header reply{};
      reply.magic = CUINTERPOSE_MAGIC;
      reply.version = CUINTERPOSE_VERSION;
      reply.operation = request.operation;
      std::strncpy(reply.participant_id, id_.c_str(), sizeof(reply.participant_id) - 1);
      reply.live_raw_imports = live_raw_imports;
      reply.phase = phase;
      bool failing = false;
      for (uint16_t op : fail) failing = failing || op == request.operation;
      if (failing) {
        cuinterpose_header_error(&reply, "fake participant failure");
        cuinterpose_send_header(client, &reply, -1);
      } else if (request.operation == CUINTERPOSE_INSPECT) {
        reply.count = static_cast<uint32_t>(records.size());
        reply.payload_size = records.size() * sizeof(struct cuinterpose_record);
        if (cuinterpose_send_header(client, &reply, -1) == 0 && !records.empty())
          cuinterpose_send_all(client, records.data(), reply.payload_size);
      } else {
        cuinterpose_send_header(client, &reply, -1);
      }
    }
    close(client);
  }

  std::string id_;
  std::string path_;
  int listener_ = -1;
  std::thread thread_;
  std::mutex mutex_;
  std::vector<uint16_t> operations_;
};

struct Outcome {
  int status;
  std::string out;
  std::string err;
};

Outcome run_coordinator(const std::vector<std::string>& args) {
  int out_pipe[2], err_pipe[2];
  if (pipe(out_pipe) != 0 || pipe(err_pipe) != 0) abort();
  pid_t child = fork();
  if (child == 0) {
    dup2(out_pipe[1], STDOUT_FILENO);
    dup2(err_pipe[1], STDERR_FILENO);
    close(out_pipe[0]);
    close(err_pipe[0]);
    std::vector<char*> argv;
    std::string binary = coordinator_path();
    argv.push_back(binary.data());
    for (auto& a : args) argv.push_back(const_cast<char*>(a.c_str()));
    argv.push_back(nullptr);
    setenv("SNAPSHOT_CONTROL_TIMEOUT_SECONDS", "5", 1);
    execv(binary.c_str(), argv.data());
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
  Outcome run;
  run.out = drain(out_pipe[0]);
  run.err = drain(err_pipe[0]);
  int status = 0;
  waitpid(child, &status, 0);
  run.status = WIFEXITED(status) ? WEXITSTATUS(status) : -1;
  return run;
}

class Coordinator : public ::testing::Test {
 protected:
  void SetUp() override {
    char tmpl[] = "/tmp/cuinterpose-coord-XXXXXX";
    ASSERT_NE(mkdtemp(tmpl), nullptr);
    dir = tmpl;
    control = dir + "/control";
    checkpoint = dir + "/checkpoint";
    ASSERT_EQ(mkdir(control.c_str(), 0700), 0);
    ASSERT_EQ(mkdir(checkpoint.c_str(), 0700), 0);
  }
  void TearDown() override {
    std::error_code ignored;
    std::filesystem::remove_all(dir, ignored);
  }

  std::vector<std::string> args(const char* mode, std::initializer_list<int> pids) {
    std::vector<std::string> a = {mode, "--proc-root", "", "--checkpoint-dir", checkpoint, "--control-dir", control};
    for (int pid : pids) {
      a.push_back("--process");
      a.push_back(std::to_string(pid));
      a.push_back(std::to_string(pid));
    }
    return a;
  }
  std::string state_path() const { return checkpoint + "/" + CUINTERPOSE_STATE_FILENAME; }
  bool state_exists() const {
    struct stat st{};
    return stat(state_path().c_str(), &st) == 0;
  }

  static struct cuinterpose_record creator_allocation(uint8_t id_byte, uint64_t size) {
    struct cuinterpose_record r{};
    r.kind = CUINTERPOSE_ALLOCATION;
    r.flags = CUINTERPOSE_CREATOR | CUINTERPOSE_APPLICATION_HANDLE_LIVE;
    std::memset(r.allocation_id, id_byte, sizeof(r.allocation_id));
    r.allocation_size = size;
    r.allocation_type = 1;
    r.requested_handle_types = CUINTERPOSE_POSIX_HANDLE_TYPE;
    r.allocation_location_type = 1;
    r.application_handle_count = 1;
    return r;
  }
  static struct cuinterpose_record mapping(uint8_t id_byte, uint64_t address, uint64_t size, uint64_t offset, bool creator) {
    struct cuinterpose_record r{};
    r.kind = CUINTERPOSE_MAPPING;
    r.flags = creator ? CUINTERPOSE_CREATOR : 0;
    std::memset(r.allocation_id, id_byte, sizeof(r.allocation_id));
    r.address = address;
    r.size = size;
    r.offset = offset;
    return r;
  }
  static struct cuinterpose_record importer_allocation(uint8_t id_byte) {
    struct cuinterpose_record r{};
    r.kind = CUINTERPOSE_ALLOCATION;
    r.flags = CUINTERPOSE_APPLICATION_HANDLE_LIVE;
    std::memset(r.allocation_id, id_byte, sizeof(r.allocation_id));
    r.application_handle_count = 1;
    return r;
  }

  std::string dir, control, checkpoint;
};

const char* kIdA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
const char* kIdB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb";

TEST_F(Coordinator, UsageErrors) {
  EXPECT_NE(run_coordinator({}).status, 0);
  EXPECT_NE(run_coordinator({"--prepare", "--proc-root", "", "--checkpoint-dir", checkpoint}).status, 0)
      << "no --control-dir and no participants";
  EXPECT_NE(run_coordinator({"--prepare", "--proc-root", "", "--checkpoint-dir", checkpoint, "--control-dir",
                             "relative", "--process", "1", "1"})
                .status,
            0)
      << "control dir must be absolute";
}

TEST_F(Coordinator, PrepareDrivesEveryPhaseInOrderAndWritesState) {
  Participant a(control, 1, kIdA), b(control, 2, kIdB);
  a.records = {creator_allocation(0x11, 1 << 20), mapping(0x11, 0x10000, 1 << 20, 0, true)};
  b.records = {importer_allocation(0x11), mapping(0x11, 0x20000, 1 << 20, 0, false)};

  Outcome run = run_coordinator(args("--prepare", {1, 2}));
  EXPECT_EQ(run.status, 0) << run.err;
  EXPECT_TRUE(state_exists());
  for (auto* p : {&a, &b}) {
    std::vector<uint16_t> want = {CUINTERPOSE_IDENTIFY, CUINTERPOSE_INSPECT, CUINTERPOSE_PREPARE_MULTICAST,
                                  CUINTERPOSE_PREPARE};
    EXPECT_EQ(p->operations(), want);
  }
  // Progress lines, one per phase, in order.
  for (const char* phase : {"inspect", "validate", "prepare_multicast", "save_host_carrier", "prepare", "state_write"}) {
    EXPECT_NE(run.out.find(std::string("cuinterpose-coordinator phase=") + phase + " status=ok"), std::string::npos)
        << run.out;
  }
  EXPECT_NE(run.out.find("records=4"), std::string::npos) << run.out;
  EXPECT_NE(run.out.find("carrier_count=0 carrier_bytes=0"), std::string::npos) << run.out;

  // The state file is text: a header, then per participant a line and one hex
  // record per line, sorted, so the same topology always produces the same file.
  std::ifstream in(state_path());
  std::string line;
  ASSERT_TRUE(std::getline(in, line));
  EXPECT_EQ(line, CUINTERPOSE_STATE_HEADER);
  ASSERT_TRUE(std::getline(in, line));
  EXPECT_EQ(line, std::string("participant ") + kIdA + " 2");
  for (int i = 0; i < 2; i++) {
    ASSERT_TRUE(std::getline(in, line));
    EXPECT_EQ(line.size(), sizeof(struct cuinterpose_record) * 2);
  }
  ASSERT_TRUE(std::getline(in, line));
  EXPECT_EQ(line, std::string("participant ") + kIdB + " 2");
}

TEST_F(Coordinator, PrepareRefusesLiveRawImports) {
  Participant a(control, 1, kIdA), b(control, 2, kIdB);
  b.live_raw_imports = 3;
  Outcome run = run_coordinator(args("--prepare", {1, 2}));
  EXPECT_NE(run.status, 0);
  EXPECT_NE(run.err.find("holds 3 live raw imports"), std::string::npos) << run.err;
  EXPECT_FALSE(state_exists());
  // Nothing destructive happened: no PREPARE_MULTICAST or PREPARE was sent.
  for (auto* p : {&a, &b}) {
    for (uint16_t op : p->operations()) {
      EXPECT_NE(op, CUINTERPOSE_PREPARE_MULTICAST);
      EXPECT_NE(op, CUINTERPOSE_PREPARE);
    }
  }
}

TEST_F(Coordinator, PrepareRejectsTopologyWithoutCreator) {
  Participant a(control, 1, kIdA);
  a.records = {importer_allocation(0x11), mapping(0x11, 0x20000, 4096, 0, false)};
  Outcome run = run_coordinator(args("--prepare", {1}));
  EXPECT_NE(run.status, 0);
  EXPECT_NE(run.err.find("missing creator"), std::string::npos) << run.err;
  EXPECT_FALSE(state_exists());
}

TEST_F(Coordinator, PrepareRejectsMappingBeyondAllocation) {
  Participant a(control, 1, kIdA);
  a.records = {creator_allocation(0x11, 4096), mapping(0x11, 0x10000, 8192, 0, true)};
  Outcome run = run_coordinator(args("--prepare", {1}));
  EXPECT_NE(run.status, 0);
  EXPECT_NE(run.err.find("mapping out of bounds"), std::string::npos) << run.err;
}

TEST_F(Coordinator, PrepareRejectsMulticastBindingBeyondMember) {
  Participant a(control, 1, kIdA);
  struct cuinterpose_record mc{};
  mc.kind = CUINTERPOSE_MULTICAST;
  mc.flags = CUINTERPOSE_CREATOR;
  std::memset(mc.allocation_id, 0x22, sizeof(mc.allocation_id));
  mc.allocation_size = 1 << 20;
  mc.handle_types = CUINTERPOSE_POSIX_HANDLE_TYPE;
  mc.num_devices = 1;
  std::strcpy(mc.creator_participant, kIdA);
  struct cuinterpose_record dev{};
  dev.kind = CUINTERPOSE_MULTICAST_DEVICE;
  std::memset(dev.allocation_id, 0x22, sizeof(dev.allocation_id));
  dev.device = 0;
  struct cuinterpose_record bind{};
  bind.kind = CUINTERPOSE_MULTICAST_BINDING;
  std::memset(bind.allocation_id, 0x22, sizeof(bind.allocation_id));
  std::memset(bind.member_id, 0x11, sizeof(bind.member_id));
  bind.binding_kind = CUINTERPOSE_MULTICAST_BIND_MEM;
  bind.api_version = 1;
  bind.size = 8192;         // member allocation is only 4096
  bind.member_offset = 0;
  bind.device = 0;
  a.records = {creator_allocation(0x11, 4096), mapping(0x11, 0x10000, 4096, 0, true), mc, dev, bind};
  Outcome run = run_coordinator(args("--prepare", {1}));
  EXPECT_NE(run.status, 0);
  EXPECT_NE(run.err.find("multicast binding out of member bounds"), std::string::npos) << run.err;
}

TEST_F(Coordinator, PrepareStopsAtTheFirstFailingParticipant) {
  Participant a(control, 1, kIdA), b(control, 2, kIdB);
  b.fail = {CUINTERPOSE_PREPARE_MULTICAST};
  Outcome run = run_coordinator(args("--prepare", {1, 2}));
  EXPECT_NE(run.status, 0);
  EXPECT_NE(run.err.find("multicast teardown"), std::string::npos) << run.err;
  EXPECT_FALSE(state_exists());
  for (uint16_t op : a.operations()) EXPECT_NE(op, CUINTERPOSE_PREPARE) << "PREPARE must not run after a failure";
}

// PREPARE_MULTICAST tears down collective objects, and a participant's reply
// can depend on the other ranks having reached the same point (the driver's
// multicast teardown waits for the team). The coordinator must therefore ask
// every participant before waiting on any reply; a serial dispatch deadlocks a
// real workload.
TEST_F(Coordinator, PrepareAsksEveryParticipantToTearDownMulticastAtOnce) {
  Participant a(control, 1, kIdA), b(control, 2, kIdB);
  a.records = {creator_allocation(0x11, 1 << 20), mapping(0x11, 0x10000, 1 << 20, 0, true)};
  b.records = {importer_allocation(0x11), mapping(0x11, 0x20000, 1 << 20, 0, false)};

  std::mutex m;
  std::condition_variable cv;
  int arrived = 0;
  std::atomic<bool> waited_alone{false};
  auto rendezvous = [&](uint16_t op) {
    if (op != CUINTERPOSE_PREPARE_MULTICAST) return;
    std::unique_lock<std::mutex> lock(m);
    arrived++;
    cv.notify_all();
    if (!cv.wait_for(lock, std::chrono::seconds(2), [&] { return arrived == 2; })) waited_alone = true;
  };
  a.before_reply = rendezvous;
  b.before_reply = rendezvous;

  Outcome run = run_coordinator(args("--prepare", {1, 2}));
  EXPECT_EQ(run.status, 0) << run.err;
  EXPECT_FALSE(waited_alone) << "PREPARE_MULTICAST was dispatched one participant at a time";
  EXPECT_TRUE(state_exists());
}

TEST_F(Coordinator, RestoreRequiresTheStateFile) {
  Participant a(control, 1, kIdA);
  Outcome run = run_coordinator(args("--restore", {1}));
  EXPECT_NE(run.status, 0) << "a missing state file is a damaged artifact, not a no-op";
  EXPECT_NE(run.err.find("missing or unreadable"), std::string::npos) << run.err;
  EXPECT_TRUE(a.operations().empty());
}

TEST_F(Coordinator, RestoreRejectsAParticipantSetThatChanged) {
  {
    Participant a(control, 1, kIdA), b(control, 2, kIdB);
    ASSERT_EQ(run_coordinator(args("--prepare", {1, 2})).status, 0);
  }
  Participant a(control, 1, kIdA), c(control, 2, "cccccccccccccccccccccccccccccccc");
  Outcome run = run_coordinator(args("--restore", {1, 2}));
  EXPECT_NE(run.status, 0);
  EXPECT_NE(run.err.find("do not match the checkpointed participants"), std::string::npos) << run.err;
}

TEST_F(Coordinator, RestoreRunsPhasesWithBarriers) {
  {
    Participant a(control, 1, kIdA), b(control, 2, kIdB);
    a.records = {creator_allocation(0x11, 1 << 20), mapping(0x11, 0x10000, 1 << 20, 0, true)};
    b.records = {importer_allocation(0x11), mapping(0x11, 0x20000, 1 << 20, 0, false)};
    ASSERT_EQ(run_coordinator(args("--prepare", {1, 2})).status, 0);
  }
  Participant a(control, 1, kIdA), b(control, 2, kIdB);
  a.records = {creator_allocation(0x11, 1 << 20), mapping(0x11, 0x10000, 1 << 20, 0, true)};
  b.records = {importer_allocation(0x11), mapping(0x11, 0x20000, 1 << 20, 0, false)};

  // b holds up its RESTORE_MULTICAST_DEVICES reply. While it is held, a must
  // not receive the final RESTORE_MULTICAST: binding before every device is
  // attached would spin forever in the real driver.
  std::mutex m;
  std::condition_variable cv;
  bool release_b = false;
  std::atomic<bool> a_got_final_while_b_held{false};
  b.before_reply = [&](uint16_t op) {
    if (op == CUINTERPOSE_RESTORE_MULTICAST_DEVICES) {
      std::unique_lock<std::mutex> lock(m);
      cv.wait_for(lock, std::chrono::seconds(2), [&] { return release_b; });
    }
  };
  a.before_reply = [&](uint16_t op) {
    if (op == CUINTERPOSE_RESTORE_MULTICAST) {
      std::lock_guard<std::mutex> lock(m);
      if (!release_b) a_got_final_while_b_held = true;
    }
  };
  std::thread releaser([&] {
    std::this_thread::sleep_for(std::chrono::milliseconds(500));
    std::lock_guard<std::mutex> lock(m);
    release_b = true;
    cv.notify_all();
  });
  Outcome run = run_coordinator(args("--restore", {1, 2}));
  releaser.join();
  EXPECT_EQ(run.status, 0) << run.err;
  EXPECT_FALSE(a_got_final_while_b_held) << "RESTORE_MULTICAST was dispatched before every DEVICES reply";
  std::vector<uint16_t> want = {CUINTERPOSE_IDENTIFY,
                                CUINTERPOSE_RESTORE_CREATORS,
                                CUINTERPOSE_RESTORE_IMPORTERS,
                                CUINTERPOSE_RESTORE_MULTICAST_CREATORS,
                                CUINTERPOSE_RESTORE_MULTICAST_IMPORTERS,
                                CUINTERPOSE_RESTORE_MULTICAST_DEVICES,
                                CUINTERPOSE_RESTORE_MULTICAST,
                                CUINTERPOSE_IDENTIFY,
                                CUINTERPOSE_INSPECT};
  EXPECT_EQ(a.operations(), want);
  EXPECT_EQ(b.operations(), want);
  for (const char* phase : {"identify", "restore_host_carrier", "restore_unicast", "restore_multicast", "validate"}) {
    EXPECT_NE(run.out.find(std::string("phase=") + phase + " status=ok"), std::string::npos) << run.out;
  }
}

TEST_F(Coordinator, RestoreRejectsATopologyThatCameBackDifferent) {
  {
    Participant a(control, 1, kIdA);
    a.records = {creator_allocation(0x11, 1 << 20), mapping(0x11, 0x10000, 1 << 20, 0, true)};
    ASSERT_EQ(run_coordinator(args("--prepare", {1})).status, 0);
  }
  Participant a(control, 1, kIdA);
  a.records = {creator_allocation(0x11, 1 << 20), mapping(0x11, 0x30000, 1 << 20, 0, true)};  // moved
  Outcome run = run_coordinator(args("--restore", {1}));
  EXPECT_NE(run.status, 0);
  EXPECT_NE(run.err.find("does not match the checkpoint"), std::string::npos) << run.err;
}

TEST_F(Coordinator, StateFileIsRejectedWhenCorrupt) {
  {
    Participant a(control, 1, kIdA);
    ASSERT_EQ(run_coordinator(args("--prepare", {1})).status, 0);
  }
  {
    std::ofstream out(state_path(), std::ios::trunc);
    out << "cuinterpose-state-v1\n";
  }
  Participant a(control, 1, kIdA);
  Outcome run = run_coordinator(args("--restore", {1}));
  EXPECT_NE(run.status, 0);
  EXPECT_NE(run.err.find("cannot parse"), std::string::npos) << run.err;
}

}  // namespace
