// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0
#include "client.hpp"

#include <fcntl.h>
#include <signal.h>
#include <sys/socket.h>
#include <google/protobuf/util/json_util.h>

#include <atomic>
#include <charconv>
#include <exception>
#include <iostream>
#include <limits>
#include <mutex>
#include <set>
#include <stdexcept>
#include <string>
#include <string_view>
#include <syncstream>
#include <thread>
#include <vector>

#include "fd_transport.hpp"
#include "helper.pb.h"

namespace snapshot::cuda_checkpoint {
namespace {
namespace protocol = internal;
using snapshot::pagebroker::ReceiveFrame;
using snapshot::pagebroker::SendFrame;
using Request = protocol::GPUSessionRequest;
using Reply = protocol::GPUSessionReply;

void Require(bool condition, const char* message)
{
  if (!condition) throw std::runtime_error(message);
}

uint64_t Number(std::string_view value, uint64_t maximum, bool zero = false)
{
  uint64_t number = 0;
  const auto [end, error] = std::from_chars(value.data(), value.data() + value.size(), number);
  Require(error == std::errc{} && end == value.data() + value.size() &&
              (zero || number > 0) && number <= maximum, "invalid numeric helper argument");
  return number;
}

int Descriptor(std::string_view value)
{
  const auto descriptor = static_cast<int>(Number(value, std::numeric_limits<int>::max(), true));
  Require(fcntl(descriptor, F_GETFD) >= 0, "helper argument is not an open descriptor");
  return descriptor;
}

std::vector<std::string_view> Fields(std::string_view value, size_t count)
{
  std::vector<std::string_view> fields;
  while (true) {
    const auto colon = value.find(':');
    fields.push_back(value.substr(0, colon));
    if (colon == std::string_view::npos) break;
    value.remove_prefix(colon + 1);
  }
  Require(fields.size() == count, "invalid helper session argument");
  return fields;
}

struct Session {
  uint64_t id = 0;
  uint32_t saved_pid = 0;
  int client = -1;
  int peer = -1;
  uint32_t observed_pid = 0;
};
struct Options {
  int control = -1;
  int directory = -1;
  protocol::BindGPUSession binding;
  std::vector<Session> sessions;
};

Options Parse(int argc, char** argv, std::string_view mode)
{
  Options options;
  std::set<std::string_view> seen;
  std::set<uint64_t> ids;
  std::set<uint32_t> pids;
  std::set<int> descriptors;
  for (int i = 2; i < argc; ++i) {
    const std::string_view name(argv[i]);
    Require(name == "--session" || name == "--visible-device" || seen.insert(name).second,
            "duplicate helper option");
    if (name == "--checksum") {
      Require(mode == "--bind-batch", "checksum is only a bind option");
      options.binding.set_enable_checksum_digest(true);
      continue;
    }
    Require(++i < argc, "helper option requires a value");
    const std::string_view value(argv[i]);
    if (name == "--control-fd") {
      Require(mode != "--run-batch", "batch execution does not receive the control socket");
      options.control = Descriptor(value);
    } else if (name == "--directory-fd") {
      Require(mode == "--bind-batch", "directory is only a bind option");
      options.directory = Descriptor(value);
    } else if (name == "--container-pid") {
      Require(mode == "--bind-batch", "container PID is only a bind option");
      options.binding.set_container_pid(Number(value, std::numeric_limits<int>::max()));
    } else if (name == "--direction") {
      Require(mode == "--bind-batch" || mode == "--run-batch", "direction is only a batch option");
      Require(value == "save" || value == "load", "invalid helper direction");
      options.binding.set_direction(value == "save" ? protocol::BindGPUSession::SAVE : protocol::BindGPUSession::LOAD);
    } else if (name == "--storage-mode") {
      Require(mode == "--bind-batch", "storage mode is only a bind option");
      Require(value == "driver" || value == "custom", "invalid helper storage mode");
      options.binding.set_storage_mode(value == "driver" ? protocol::BindGPUSession::DRIVER_MANAGED
                                                          : protocol::BindGPUSession::CUSTOM_STORAGE);
    } else if (name == "--visible-device") {
      Require(mode == "--bind-batch" && !value.empty(), "visible device is only a bind option");
      options.binding.add_visible_devices(std::string(value));
    } else if (name == "--device-map") {
      Require(mode == "--bind-batch", "device map is only a bind option");
      options.binding.set_device_map(std::string(value));
    } else if (name == "--session") {
      Session session;
      if (mode == "--bind-batch") {
        const auto fields = Fields(value, 4);
        session.id = Number(fields[0], std::numeric_limits<uint64_t>::max());
        session.saved_pid = Number(fields[1], std::numeric_limits<int>::max());
        session.client = Descriptor(fields[2]);
        session.peer = Descriptor(fields[3]);
        Require(ids.insert(session.id).second && pids.insert(session.saved_pid).second &&
                    descriptors.insert(session.client).second && descriptors.insert(session.peer).second,
                "duplicate helper session");
      } else if (mode == "--run-batch") {
        const auto fields = Fields(value, 3);
        session.saved_pid = Number(fields[0], std::numeric_limits<int>::max());
        session.client = Descriptor(fields[1]);
        session.observed_pid = Number(fields[2], std::numeric_limits<int>::max(), true);
        Require(pids.insert(session.saved_pid).second && descriptors.insert(session.client).second,
                "duplicate helper session");
      } else {
        Require(mode == "--drain-batch", "wait-ready does not take sessions");
        session.id = Number(value, std::numeric_limits<uint64_t>::max());
        Require(ids.insert(session.id).second, "duplicate helper session");
      }
      options.sessions.push_back(session);
    } else {
      throw std::runtime_error("unknown helper client option: " + std::string(name));
    }
  }
  if (mode != "--run-batch")
    Require(options.control >= 0, "helper control descriptor is required");
  if (mode != "--wait-ready")
    Require(!options.sessions.empty(), "helper batch requires sessions");
  if (mode == "--bind-batch" || mode == "--run-batch")
    Require(options.binding.direction() != protocol::BindGPUSession::UNSPECIFIED, "helper direction is required");
  if (mode == "--bind-batch") {
    Require(options.directory >= 0 && options.binding.container_pid() > 1 &&
                options.binding.visible_devices_size() > 0 && seen.contains("--storage-mode"),
            "incomplete helper binding");
    Require(options.control != options.directory && !descriptors.contains(options.control) &&
                !descriptors.contains(options.directory), "helper descriptors alias different roles");
  }
  return options;
}

Reply ReceiveReply(int descriptor, Reply::Status expected)
{
  Reply reply;
  std::vector<FileDescriptor> descriptors;
  Require(ReceiveFrame(descriptor, reply, descriptors), "CUDA helper disconnected");
  Require(descriptors.empty(), "CUDA helper reply has unexpected descriptors");
  if (reply.has_failure()) throw std::runtime_error(reply.failure().message());
  Require(reply.status() == expected, "unexpected CUDA helper reply status");
  return reply;
}

// Logging must not change a completed CUDA operation into an operation failure.
bool PrintJSON(const google::protobuf::Message& message) noexcept
{
  try {
    std::string json;
    google::protobuf::util::JsonPrintOptions options;
    options.preserve_proto_field_names = true;
    options.always_print_primitive_fields = true;
    const auto status = google::protobuf::util::MessageToJsonString(message, &json, options);
    if (!status.ok()) return false;
    static std::mutex output_mutex;
    std::lock_guard lock(output_mutex);
    std::cout << json << '\n';
    std::cout.flush();
    return static_cast<bool>(std::cout);
  } catch (...) {
    return false;
  }
}

void PrintPhase(uint32_t pid, Request::Operation operation, const Reply& reply) noexcept
{
  try {
    protocol::CLIPhaseResult result;
    result.set_pid(pid);
    result.set_operation(operation);
    *result.mutable_reply() = reply;
    if (PrintJSON(result)) return;
  } catch (...) {
  }
  // Stream failures are diagnostics only; the verified CUDA reply is final.
  try {
    std::osyncstream(std::cerr) << "CUDA helper phase telemetry could not be written\n";
  } catch (...) {
  }
}

void Cancel(const std::vector<Session>& sessions) noexcept
{
  for (const auto& session : sessions)
    if (session.client >= 0) shutdown(session.client, SHUT_WR);
}

void Bind(const Options& options)
{
  try {
    for (const auto& session : options.sessions) {
      protocol::HelperRequest request;
      request.set_session_id(session.id);
      *request.mutable_bind() = options.binding;
      request.mutable_bind()->set_namespace_pid(session.saved_pid);
      SendFrame(options.control, request, {session.peer, options.directory});
      ReceiveReply(session.client, Reply::READY);
    }
  } catch (...) {
    Cancel(options.sessions);
    throw;
  }
}

void RunBatch(const Options& options)
{
  std::atomic<bool> cancelled = false;
  std::mutex error_mutex;
  std::exception_ptr failure;
  std::vector<std::thread> transfers;
  transfers.reserve(options.sessions.size());
  auto fail = [&] {
    {
      std::lock_guard lock(error_mutex);
      if (!failure) failure = std::current_exception();
    }
    cancelled = true;
    Cancel(options.sessions);
  };
  auto run = [&](size_t index, Request::Operation operation, Reply::Status expected) {
    Require(!cancelled, "CUDA sibling operation failed");
    const auto& session = options.sessions[index];
    Request request;
    request.set_operation(operation);
    request.set_target_pid(session.observed_pid);
    SendFrame(session.client, request);
    auto reply = ReceiveReply(session.client, expected);
    PrintPhase(session.saved_pid, operation, reply);
  };
  try {
    const bool save = options.binding.direction() == protocol::BindGPUSession::SAVE;
    if (save)
      for (size_t i = 0; i < options.sessions.size(); ++i) run(i, Request::LOCK, Reply::LOCKED);
    for (size_t i = 0; i < options.sessions.size(); ++i) {
      run(i, Request::PREPARE, Reply::PREPARED);
      if (!save)
        transfers.emplace_back([&, i] {
          try { run(i, Request::TRANSFER, Reply::TRANSFERRED); } catch (...) { fail(); }
        });
    }
    if (save)
      for (size_t i = 0; i < options.sessions.size(); ++i)
        transfers.emplace_back([&, i] {
          try { run(i, Request::TRANSFER, Reply::TRANSFERRED); } catch (...) { fail(); }
        });
  } catch (...) {
    fail();
  }
  for (auto& transfer : transfers) transfer.join();
  if (failure) std::rethrow_exception(failure);
  try {
    for (size_t i = options.sessions.size(); i > 0; --i) run(i - 1, Request::COMPLETE, Reply::COMPLETE);
  } catch (...) {
    Cancel(options.sessions);
    throw;
  }
}
} // namespace

std::optional<int> RunClient(int argc, char** argv)
{
  if (argc < 2) return std::nullopt;
  const std::string_view mode(argv[1]);
  if (mode != "--wait-ready" && mode != "--bind-batch" && mode != "--run-batch" && mode != "--drain-batch")
    return std::nullopt;
  // An abandoned logging pipe must not interrupt CUDA lifecycle handling.
  signal(SIGPIPE, SIG_IGN);
  try {
    const auto options = Parse(argc, argv, mode);
    if (mode == "--wait-ready") {
      auto ready = ReceiveReply(options.control, Reply::READY);
      Require(PrintJSON(ready), "could not print CUDA helper readiness");
    } else if (mode == "--bind-batch") {
      Bind(options);
    } else if (mode == "--run-batch") {
      RunBatch(options);
    } else {
      for (const auto& session : options.sessions) {
        protocol::HelperRequest request;
        request.set_session_id(session.id);
        request.set_drain(true);
        SendFrame(options.control, request);
        const auto reply = ReceiveReply(options.control, Reply::DRAINED);
        Require(reply.session_id() == session.id, "CUDA helper drain acknowledged another session");
      }
    }
    return 0;
  } catch (const std::exception& error) {
    std::osyncstream(std::cerr) << "CUDA helper client: " << error.what() << '\n';
    return 1;
  }
}
} // namespace snapshot::cuda_checkpoint
