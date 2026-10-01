// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

// Exercise the real worker's phase machine with a mocked driver and transfer
// implementation. No GPU or driver-managed checkpoint storage is needed.
#include "daemon.cpp"
#define main cuda_helper_main
#include "helper.cpp"
#undef main

#include <gtest/gtest.h>
#include <sys/wait.h>
#include <sys/prctl.h>
#include <sys/socket.h>

namespace {
int ring_initializations;
int transfer_calls;
int completion_calls;
int completion_queries;
bool fail_complete;
int context_retains;
bool custom_available;
std::vector<int> driver_calls;
CUcheckpointCustomStorageInfo mock_view;
CUcheckpointCustomStoragePerDeviceData mock_data;
std::atomic<bool> block_transfer;
std::atomic<bool> transfer_started;
bool used_checksum_digest;
size_t allocation_padding;
bool fail_initialization;
int initialization_started_fd = -1;
int initialization_continue_fd = -1;
const std::string mock_digest(64, 'a');
const char* gpu_uuid = "GPU-00000000-0000-0000-0000-000000000001";

struct DaemonProcess {
  pid_t pid;
  FileDescriptor control;
  ~DaemonProcess()
  {
    if (pid > 0) {
      kill(pid, SIGKILL);
      while (waitpid(pid, nullptr, 0) < 0 && errno == EINTR) {
      }
    }
  }
};

std::unique_ptr<DaemonProcess>
StartDaemon(bool custom, bool use_runner = false)
{
  int sockets[2];
  if (socketpair(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0, sockets))
    throw std::runtime_error("test daemon socketpair");
  FileDescriptor control(sockets[0]), peer(sockets[1]);
  const auto pid = fork();
  if (pid < 0)
    throw std::runtime_error("test daemon fork");
  if (!pid) {
    control = FileDescriptor(-1);
    if (use_runner) {
      FileDescriptor runtime_control(fcntl(peer.get(), F_DUPFD_CLOEXEC, 7));
      if (runtime_control.get() < 0)
        std::_Exit(9);
      peer = FileDescriptor(-1);
      checkpoint::RunDaemon(runtime_control.get(), {.custom_storage = custom});
    }
    if (dup2(peer.get(), 3) < 0)
      std::_Exit(9);
    if (peer.get() != 3)
      peer = FileDescriptor(-1);
    char name[] = "cuda-checkpoint-helper";
    char daemon[] = "--daemon";
    char mode[] = "--cuda-storage-mode";
    char driver[] = "driver";
    char custom_mode[] = "custom";
    char* argv[] = {name, daemon, mode, custom ? custom_mode : driver};
    cuda_helper_main(4, argv);
    std::_Exit(9);
  }
  return std::unique_ptr<DaemonProcess>(new DaemonProcess{pid, std::move(control)});
}

bool
Readable(int descriptor, int milliseconds = 5000)
{
  pollfd event{descriptor, POLLIN, 0};
  return poll(&event, 1, milliseconds) == 1;
}

protocol::GPUSessionReply
ReadReply(int descriptor)
{
  if (!Readable(descriptor))
    throw std::runtime_error("test daemon reply timed out");
  protocol::GPUSessionReply reply;
  std::vector<FileDescriptor> rights;
  if (!ReceiveFrame(descriptor, reply, rights) || !rights.empty())
    throw std::runtime_error("test daemon reply missing or carried descriptors");
  return reply;
}

class WorkerModes : public testing::Test {
protected:
  void
  SetUp() override
  {
    ring_initializations = transfer_calls = completion_calls = completion_queries = context_retains = 0;
    custom_available = true;
    fail_complete = false;
    block_transfer = false;
    used_checksum_digest = false;
    allocation_padding = 0;
    fail_initialization = false;
    initialization_started_fd = initialization_continue_fd = -1;
    transfer_started = false;
    driver_calls.clear();
    mock_view = {};
    mock_data = {1, 4096, reinterpret_cast<CUstream>(1)};
    mock_view.deviceCount = 1;
    mock_view.perDeviceData = &mock_data;
    mock_view.handle = reinterpret_cast<CUcheckpointOperationHandle>(1);
    char path[] = "/tmp/pagebroker-worker-XXXXXX";
    directory = mkdtemp(path);
    pid = fork();
    ASSERT_GE(pid, 0);
    if (!pid) {
      for (;;)
        pause();
    }
  }
  void
  TearDown() override
  {
    if (pid > 0) {
      kill(pid, SIGKILL);
      waitpid(pid, nullptr, 0);
    }
    std::filesystem::remove_all(directory);
  }
  protocol::BindGPUSession
  Binding(bool save, bool custom = false)
  {
    protocol::BindGPUSession result;
    result.set_container_pid(getpid());
    result.set_namespace_pid(pid);
    auto* binding = &result;
    binding->set_direction(save ? protocol::BindGPUSession::SAVE : protocol::BindGPUSession::LOAD);
    binding->set_storage_mode(custom ? protocol::BindGPUSession::CUSTOM_STORAGE
                                     : protocol::BindGPUSession::DRIVER_MANAGED);
    binding->add_visible_devices(gpu_uuid);
    return result;
  }
  FileDescriptor
  Directory()
  {
    return FileDescriptor(open(directory.c_str(), O_RDONLY | O_DIRECTORY));
  }
  std::filesystem::path
  Participant() const
  {
    return directory / "native" / std::to_string(pid);
  }
  protocol::GPUSessionReply
  Run(Session& operation, protocol::GPUSessionRequest::Operation phase)
  {
    protocol::GPUSessionRequest request;
    request.set_operation(phase);
    return operation.Execute(request);
  }
  std::filesystem::path directory;
  int pid = -1;
};
} // namespace

namespace snapshot::pagebroker::cuda {
struct TransferBuffers::Impl {};
TransferBuffers::TransferBuffers(TransferOptions)
{
}
TransferBuffers::~TransferBuffers() = default;
bool
ValidateTransferMemory(const TransferOptions& options, size_t count, size_t limit, size_t* total, std::string* error)
{
  if (!options.buffer_count || !options.chunk_bytes || options.buffer_count > SIZE_MAX / options.chunk_bytes ||
      options.buffer_count * options.chunk_bytes > SIZE_MAX / count) {
    *error = "invalid allocation size";
    return false;
  }
  *total = options.buffer_count * options.chunk_bytes * count;
  if (limit && *total > limit) {
    *error = "allocation exceeds cap";
    return false;
  }
  return true;
}
bool
TransferBuffers::AllocationBytes(CUcontext, const TransferOptions& options, size_t* bytes, std::string*)
{
  *bytes = options.buffer_count * options.chunk_bytes + allocation_padding;
  return true;
}
bool
TransferBuffers::Initialize(CUcontext, std::string* error)
{
  ++ring_initializations;
  if (initialization_started_fd >= 0) {
    char signal = 1;
    if (write(initialization_started_fd, &signal, 1) != 1 || read(initialization_continue_fd, &signal, 1) != 1)
      return false;
  }
  if (fail_initialization) {
    *error = "mock initialization failure";
    return false;
  }
  return true;
}
bool
TransferBuffers::Transfer(int, CUdeviceptr, size_t size, CUstream, TransferOperation, TransferMetrics* metrics,
                          std::string* error, TransferControl control)
{
  ++transfer_calls;
  used_checksum_digest = control.enable_checksum_digest;
  transfer_started = true;
  if (block_transfer) {
    while (!control.cancellation->IsCancelled())
      std::this_thread::sleep_for(std::chrono::milliseconds(1));
    *error = "cancelled mock transfer";
    return false;
  }
  metrics->bytes = size;
  metrics->setup_seconds = 1.0;
  metrics->storage_io_seconds = 2.0;
  metrics->cuda_wait_seconds = 3.0;
  metrics->sha256 = control.enable_checksum_digest ? mock_digest : "";
  return true;
}
} // namespace snapshot::pagebroker::cuda

extern "C" {
CUresult CUDAAPI
cuInit(unsigned int)
{
  return CUDA_SUCCESS;
}
CUresult CUDAAPI
cuGetErrorName(CUresult, const char** name)
{
  *name = "mock error";
  return CUDA_SUCCESS;
}
CUresult CUDAAPI
cuDeviceGetCount(int* count)
{
  *count = 1;
  return CUDA_SUCCESS;
}
CUresult CUDAAPI
cuDeviceGet(CUdevice* device, int)
{
  *device = 0;
  return CUDA_SUCCESS;
}
CUresult CUDAAPI
cuDeviceGetUuid(CUuuid* uuid, CUdevice)
{
  *uuid = {};
  uuid->bytes[15] = 1;
  return CUDA_SUCCESS;
}
CUresult CUDAAPI
cuDevicePrimaryCtxRetain(CUcontext* context, CUdevice)
{
  ++context_retains;
  *context = reinterpret_cast<CUcontext>(1);
  return CUDA_SUCCESS;
}
CUresult CUDAAPI
cuCtxSetCurrent(CUcontext)
{
  return CUDA_SUCCESS;
}
CUresult CUDAAPI
cuStreamGetCtx(CUstream, CUcontext* context)
{
  *context = reinterpret_cast<CUcontext>(1);
  return CUDA_SUCCESS;
}
CUresult CUDAAPI
cuCheckpointOperationComplete(CUcheckpointOperationHandle)
{
  ++completion_calls;
  return fail_complete ? CUDA_ERROR_UNKNOWN : CUDA_SUCCESS;
}
CUresult CUDAAPI
cuGetProcAddress(const char*, void** symbol, int, cuuint64_t, CUdriverProcAddressQueryResult* query)
{
  ++completion_queries;
  if (!custom_available)
    return CUDA_ERROR_NOT_SUPPORTED;
  *symbol = reinterpret_cast<void*>(&cuCheckpointOperationComplete);
  *query = CU_GET_PROC_ADDRESS_SUCCESS;
  return CUDA_SUCCESS;
}
CUresult CUDAAPI
cuDriverGetVersion(int* version)
{
  *version = 13040;
  return CUDA_SUCCESS;
}
CUresult CUDAAPI
cuCheckpointProcessGetState(int, CUprocessState* state)
{
  *state = CU_PROCESS_STATE_RUNNING;
  return CUDA_SUCCESS;
}
CUresult CUDAAPI
cuCheckpointProcessLock(int, CUcheckpointLockArgs*)
{
  driver_calls.push_back(1);
  return CUDA_SUCCESS;
}
CUresult CUDAAPI
cuCheckpointProcessCheckpoint(int, CUcheckpointCheckpointArgs* args)
{
  driver_calls.push_back(2);
  if (args->customStorageInfo_out)
    *args->customStorageInfo_out = &mock_view;
  return CUDA_SUCCESS;
}
CUresult CUDAAPI
cuCheckpointProcessRestore(int, CUcheckpointRestoreArgs* args)
{
  driver_calls.push_back(3);
  if (args->customStorageInfo_out)
    *args->customStorageInfo_out = &mock_view;
  return CUDA_SUCCESS;
}
CUresult CUDAAPI
cuCheckpointProcessUnlock(int, CUcheckpointUnlockArgs*)
{
  driver_calls.push_back(4);
  return CUDA_SUCCESS;
}
}

TEST_F(WorkerModes, OlderDriverWarmsContextsWithoutAllocatingCustomStorageBuffers)
{
  custom_available = false;
  Engine engine({.custom_storage = false});
  for (bool save : {true, false}) {
    Session operation(engine, Binding(save), Directory());
    EXPECT_THROW(Run(operation, protocol::GPUSessionRequest::COMPLETE), std::runtime_error);
    if (save)
      Run(operation, protocol::GPUSessionRequest::LOCK);
    Run(operation, protocol::GPUSessionRequest::PREPARE);
    EXPECT_THROW(Run(operation, protocol::GPUSessionRequest::COMPLETE), std::runtime_error);
    EXPECT_EQ(Run(operation, protocol::GPUSessionRequest::TRANSFER).metrics().bytes(), 0);
    Run(operation, protocol::GPUSessionRequest::COMPLETE);
    operation.Drain();
  }
  EXPECT_EQ(driver_calls, (std::vector<int>{1, 2, 3, 4}));
  EXPECT_EQ(context_retains, 1);
  EXPECT_EQ(ring_initializations, 0);
  EXPECT_EQ(transfer_calls, 0);
  EXPECT_EQ(completion_queries, 1);
  EXPECT_EQ(completion_calls, 0);
  EXPECT_TRUE(std::filesystem::is_empty(directory));
  EXPECT_EQ(kill(pid, 0), 0);
}

TEST_F(WorkerModes, DriverModeWarmsResourcesOnceAndReusesThemAcrossRestores)
{
  Engine engine({.custom_storage = false});
  ASSERT_EQ(ring_initializations, 1);
  ASSERT_EQ(context_retains, 1);
  ASSERT_EQ(completion_queries, 1);
  {
    Session custom(engine, Binding(true, true), Directory());
    Run(custom, protocol::GPUSessionRequest::LOCK);
    Run(custom, protocol::GPUSessionRequest::PREPARE);
    Run(custom, protocol::GPUSessionRequest::TRANSFER);
    Run(custom, protocol::GPUSessionRequest::COMPLETE);
    custom.Drain();
  }
  std::vector<storage::ManifestExtent> manifest;
  std::string error;
  ASSERT_TRUE(storage::ReadManifest(Participant(), &manifest, &error)) << error;
  for (int i = 0; i < 2; ++i) {
    Session custom(engine, Binding(false, true), Directory());
    Run(custom, protocol::GPUSessionRequest::PREPARE);
    Run(custom, protocol::GPUSessionRequest::TRANSFER);
    Run(custom, protocol::GPUSessionRequest::COMPLETE);
    custom.Drain();
  }
  Session ordinary(engine, Binding(false), Directory());
  Run(ordinary, protocol::GPUSessionRequest::PREPARE);
  Run(ordinary, protocol::GPUSessionRequest::TRANSFER);
  Run(ordinary, protocol::GPUSessionRequest::COMPLETE);
  ordinary.Drain();
  EXPECT_EQ(transfer_calls, 3);
  EXPECT_EQ(completion_calls, 3);
  EXPECT_EQ(ring_initializations, 1);
  EXPECT_EQ(context_retains, 1);
  EXPECT_EQ(completion_queries, 1);
}

TEST_F(WorkerModes, UnknownModeIsRejectedBeforeDriverOperation)
{
  Engine engine({.custom_storage = false});
  auto binding = Binding(false);
  binding.set_storage_mode(static_cast<protocol::BindGPUSession::StorageMode>(99));
  EXPECT_THROW(Session(engine, binding, Directory()), std::runtime_error);
  EXPECT_TRUE(driver_calls.empty());
  EXPECT_EQ(ring_initializations, 1);
}

TEST_F(WorkerModes, InvalidDeviceMapIsRejectedAtAdmissionBeforeAnyCUDAOperation)
{
  Engine engine;
  auto binding = Binding(true, true);
  binding.set_device_map("not-a-GPU=also-not-a-GPU");
  EXPECT_THROW(Session(engine, binding, Directory()), std::runtime_error);
  EXPECT_TRUE(driver_calls.empty());
  EXPECT_TRUE(std::filesystem::is_empty(directory));
}

TEST_F(WorkerModes, CustomStorageWarmsTransfersBeforeReady)
{
  Engine engine;
  EXPECT_EQ(ring_initializations, 1);
  EXPECT_EQ(context_retains, 1);
  EXPECT_EQ(completion_queries, 1);
  Session operation(engine, Binding(true, true), Directory());
  EXPECT_EQ(ring_initializations, 1);
}

TEST_F(WorkerModes, AllocationCapRejectsBeforeAllocating)
{
  for (const bool custom : {true, false})
    EXPECT_THROW(Engine({.custom_storage = custom, .max_pinned_bytes = 1}), std::runtime_error);
  EXPECT_EQ(context_retains, 0);
  EXPECT_EQ(ring_initializations, 0);
}

TEST_F(WorkerModes, DriverManagedChecksumDigestIsRejectedWithoutTouchingTarget)
{
  Engine engine({.custom_storage = false});
  auto binding = Binding(true);
  binding.set_enable_checksum_digest(true);
  EXPECT_THROW(Session(engine, binding, Directory()), std::runtime_error);
  EXPECT_TRUE(driver_calls.empty());
  EXPECT_EQ(ring_initializations, 1);
}

TEST_F(WorkerModes, ChecksumDigestWritesSidecarAndRejectsMismatchBeforeCompletion)
{
  Engine engine;
  auto save = Binding(true, true);
  save.set_enable_checksum_digest(true);
  {
    Session operation(engine, save, Directory());
    Run(operation, protocol::GPUSessionRequest::LOCK);
    Run(operation, protocol::GPUSessionRequest::PREPARE);
    Run(operation, protocol::GPUSessionRequest::TRANSFER);
    Run(operation, protocol::GPUSessionRequest::COMPLETE);
    operation.Drain();
  }
  EXPECT_TRUE(used_checksum_digest);
  std::vector<storage::ManifestExtent> manifest;
  std::vector<std::string> digests;
  std::string error;
  ASSERT_TRUE(storage::ReadManifest(Participant(), &manifest, &error)) << error;
  ASSERT_TRUE(storage::ReadExtentDigests(Participant(), manifest, &digests, &error)) << error;
  ASSERT_EQ(digests, (std::vector<std::string>{mock_digest}));
  ASSERT_TRUE(std::filesystem::remove(Participant() / storage::kExtentDigestsName));
  ASSERT_TRUE(storage::WriteExtentDigests(Participant(), manifest, {std::string(64, 'b')}, &error)) << error;
  auto load = Binding(false, true);
  load.set_enable_checksum_digest(true);
  Session operation(engine, load, Directory());
  Run(operation, protocol::GPUSessionRequest::PREPARE);
  EXPECT_THROW(Run(operation, protocol::GPUSessionRequest::TRANSFER), std::runtime_error);
  EXPECT_THROW(Run(operation, protocol::GPUSessionRequest::COMPLETE), std::runtime_error);
  EXPECT_EQ(completion_calls, 1); // Only the successful capture completed.
  EXPECT_EQ(std::count(driver_calls.begin(), driver_calls.end(), 4), 0);
  operation.Drain();
}

TEST_F(WorkerModes, DisabledChecksumDigestWritesNoSidecar)
{
  Engine engine;
  Session operation(engine, Binding(true, true), Directory());
  Run(operation, protocol::GPUSessionRequest::LOCK);
  Run(operation, protocol::GPUSessionRequest::PREPARE);
  Run(operation, protocol::GPUSessionRequest::TRANSFER);
  Run(operation, protocol::GPUSessionRequest::COMPLETE);
  operation.Drain();
  EXPECT_FALSE(used_checksum_digest);
  EXPECT_FALSE(std::filesystem::exists(Participant() / storage::kExtentDigestsName));
}

TEST_F(WorkerModes, HalfCloseCancelsRunningTransferAndAcknowledgesDrain)
{
  Engine engine;
  int sockets[2];
  ASSERT_EQ(socketpair(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0, sockets), 0);
  FileDescriptor client(sockets[0]);
  auto serving = std::async(std::launch::async, ServeSession, std::ref(engine), Binding(true, true),
                            FileDescriptor(sockets[1]), Directory());
  protocol::GPUSessionReply reply;
  std::vector<FileDescriptor> descriptors;
  ASSERT_TRUE(ReceiveFrame(client.get(), reply, descriptors));
  for (const auto phase : {protocol::GPUSessionRequest::LOCK, protocol::GPUSessionRequest::PREPARE}) {
    protocol::GPUSessionRequest command;
    command.set_operation(phase);
    SendFrame(client.get(), command);
    ASSERT_TRUE(ReceiveFrame(client.get(), reply, descriptors));
    ASSERT_FALSE(reply.has_failure());
  }
  block_transfer = true;
  protocol::GPUSessionRequest command;
  command.set_operation(protocol::GPUSessionRequest::TRANSFER);
  SendFrame(client.get(), command);
  const auto deadline = Clock::now() + std::chrono::seconds(5);
  while (!transfer_started && Clock::now() < deadline)
    std::this_thread::yield();
  EXPECT_TRUE(transfer_started);
  ASSERT_EQ(shutdown(client.get(), SHUT_WR), 0);
  ASSERT_TRUE(ReceiveFrame(client.get(), reply, descriptors));
  EXPECT_TRUE(reply.has_failure());
  ASSERT_TRUE(ReceiveFrame(client.get(), reply, descriptors));
  EXPECT_EQ(reply.status(), protocol::GPUSessionReply::DRAINED);
  EXPECT_EQ(serving.wait_for(std::chrono::seconds(5)), std::future_status::ready);
  serving.get();
}

TEST_F(WorkerModes, FailedGPUCompletionKillsTargetAndExitsEngine)
{
  int sockets[2];
  ASSERT_EQ(socketpair(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0, sockets), 0);
  FileDescriptor client(sockets[0]), server(sockets[1]);
  const auto worker = fork();
  ASSERT_GE(worker, 0);
  if (!worker) {
    client = FileDescriptor(-1);
    fail_complete = true;
    Engine engine;
    ServeSession(engine, Binding(true, true), std::move(server), Directory());
    std::_Exit(9); // An uncertain engine must never return to serving requests.
  }
  server = FileDescriptor(-1);
  protocol::GPUSessionReply reply;
  std::vector<FileDescriptor> descriptors;
  ASSERT_TRUE(ReceiveFrame(client.get(), reply, descriptors));
  for (const auto phase : {protocol::GPUSessionRequest::LOCK, protocol::GPUSessionRequest::PREPARE,
                           protocol::GPUSessionRequest::TRANSFER, protocol::GPUSessionRequest::COMPLETE}) {
    protocol::GPUSessionRequest command;
    command.set_operation(phase);
    SendFrame(client.get(), command);
    ASSERT_TRUE(ReceiveFrame(client.get(), reply, descriptors));
    EXPECT_EQ(reply.has_failure(), phase == protocol::GPUSessionRequest::COMPLETE);
  }
  EXPECT_FALSE(ReceiveFrame(client.get(), reply, descriptors));
  int status = 0;
  ASSERT_EQ(waitpid(worker, &status, 0), worker);
  ASSERT_TRUE(WIFEXITED(status));
  EXPECT_EQ(WEXITSTATUS(status), 1);
  ASSERT_EQ(waitpid(pid, &status, 0), pid);
  ASSERT_TRUE(WIFSIGNALED(status));
  EXPECT_EQ(WTERMSIG(status), SIGKILL);
  pid = -1;
}

TEST_F(WorkerModes, RepliesCarryTypedStatusAndNumericMetrics)
{
  Engine engine;
  EXPECT_EQ(engine.ready_metrics.visible_devices(), 1);
  EXPECT_GE(engine.ready_metrics.initialization_seconds(), 0);
  Session operation(engine, Binding(true, true), Directory());
  EXPECT_EQ(Run(operation, protocol::GPUSessionRequest::LOCK).status(), protocol::GPUSessionReply::LOCKED);
  const auto prepared = Run(operation, protocol::GPUSessionRequest::PREPARE);
  EXPECT_EQ(prepared.status(), protocol::GPUSessionReply::PREPARED);
  EXPECT_GE(prepared.metrics().prepare_end_ns(), prepared.metrics().prepare_start_ns());
  const auto transferred = Run(operation, protocol::GPUSessionRequest::TRANSFER);
  EXPECT_EQ(transferred.status(), protocol::GPUSessionReply::TRANSFERRED);
  EXPECT_EQ(transferred.metrics().bytes(), mock_data.size);
  EXPECT_EQ(transferred.metrics().transfer_setup_seconds(), 1.0);
  EXPECT_EQ(transferred.metrics().storage_request_service_seconds(), 2.0);
  EXPECT_EQ(transferred.metrics().cuda_wait_seconds(), 3.0);
  EXPECT_GE(transferred.metrics().transfer_end_ns(), transferred.metrics().transfer_start_ns());
  const auto completed = Run(operation, protocol::GPUSessionRequest::COMPLETE);
  EXPECT_EQ(completed.status(), protocol::GPUSessionReply::COMPLETE);
  EXPECT_EQ(completed.metrics().bytes(), mock_data.size);
  EXPECT_EQ(completed.metrics().prepare_seconds(), prepared.metrics().prepare_seconds());
  EXPECT_EQ(completed.metrics().transfer_seconds(), transferred.metrics().transfer_seconds());
  EXPECT_EQ(completed.metrics().transfer_setup_seconds(), 1.0);
  EXPECT_EQ(completed.metrics().storage_request_service_seconds(), 2.0);
  EXPECT_EQ(completed.metrics().cuda_wait_seconds(), 3.0);
  operation.Drain();
  EXPECT_EQ(completion_calls, 1);
  EXPECT_EQ(kill(pid, 0), 0);
}

extern "C" int
cuda_checkpoint_cli_main(int, char**)
{
  return 23;
}

TEST_F(WorkerModes, UnsupportedCustomStorageFailsAtStartupOrAdmission)
{
  custom_available = false;
  EXPECT_THROW(Engine(), std::runtime_error);
  Engine ordinary({.custom_storage = false});
  EXPECT_FALSE(ordinary.checkpoint.SupportsCustomStorage());
  EXPECT_THROW(Session(ordinary, Binding(false, true), Directory()), std::runtime_error);
  EXPECT_EQ(ring_initializations, 0);
  EXPECT_EQ(context_retains, 1);
  EXPECT_TRUE(driver_calls.empty());
}

TEST_F(WorkerModes, LegacyArgumentsStillDispatchToTheCLI)
{
  char name[] = "cuda-checkpoint-helper";
  char argument[] = "--get-state";
  char* argv[] = {name, argument};
  EXPECT_EQ(cuda_helper_main(2, argv), 23);
  EXPECT_EQ(completion_queries, 0);
}

TEST_F(WorkerModes, RoundedAllocationCapIsCheckedBeforeAnyRingAllocation)
{
  allocation_padding = 4096;
  for (const bool custom : {true, false}) {
    EXPECT_THROW(Engine({.custom_storage = custom, .buffer_count = 1, .chunk_bytes = 4096, .max_pinned_bytes = 4096}),
                 std::runtime_error);
  }
  EXPECT_EQ(context_retains, 2);
  EXPECT_EQ(ring_initializations, 0);
}

TEST_F(WorkerModes, InitializationFailureDoesNotFallBackToLazyTransfers)
{
  fail_initialization = true;
  for (const bool custom : {true, false})
    EXPECT_THROW(Engine({.custom_storage = custom}), std::runtime_error);
  EXPECT_TRUE(driver_calls.empty());
}

TEST_F(WorkerModes, StartupDoesNotReportReadyUntilAllTransferResourcesAreWarm)
{
  for (const bool custom : {true, false}) {
    int started[2], proceed[2];
    ASSERT_EQ(pipe2(started, O_CLOEXEC), 0);
    ASSERT_EQ(pipe2(proceed, O_CLOEXEC), 0);
    FileDescriptor started_read(started[0]), started_write(started[1]);
    FileDescriptor proceed_read(proceed[0]), proceed_write(proceed[1]);
    initialization_started_fd = started_write.get();
    initialization_continue_fd = proceed_read.get();
    auto daemon = StartDaemon(custom);
    ASSERT_TRUE(Readable(started_read.get()));
    char signal = 0;
    ASSERT_EQ(read(started_read.get(), &signal, 1), 1);
    EXPECT_FALSE(Readable(daemon->control.get(), 20));
    ASSERT_EQ(write(proceed_write.get(), &signal, 1), 1);
    const auto reply = ReadReply(daemon->control.get());
    EXPECT_EQ(reply.status(), protocol::GPUSessionReply::READY);
    EXPECT_TRUE(reply.custom_storage_available());
    EXPECT_EQ(reply.metrics().visible_devices(), 1);
  }
}

TEST_F(WorkerModes, UnsupportedDriverModeReportsReadyWithoutCustomStorage)
{
  custom_available = false;
  auto daemon = StartDaemon(false);
  const auto reply = ReadReply(daemon->control.get());
  EXPECT_EQ(reply.status(), protocol::GPUSessionReply::READY);
  EXPECT_FALSE(reply.custom_storage_available());
}

TEST_F(WorkerModes, RunnerAcceptsControlSocketOutsideTheCLIConvention)
{
  auto daemon = StartDaemon(true, true);
  const auto ready = ReadReply(daemon->control.get());
  EXPECT_EQ(ready.status(), protocol::GPUSessionReply::READY);
  EXPECT_TRUE(ready.custom_storage_available());
  protocol::HelperRequest drain;
  drain.set_session_id(99);
  drain.set_drain(true);
  SendFrame(daemon->control.get(), drain);
  const auto reply = ReadReply(daemon->control.get());
  EXPECT_EQ(reply.status(), protocol::GPUSessionReply::DRAINED);
  EXPECT_EQ(reply.session_id(), 99);
}

TEST_F(WorkerModes, LoadAdmissionValidatesManifestAndFilesBeforeAnyCUDAOperation)
{
  Engine engine;
  const auto participant = directory / "native" / std::to_string(pid);
  std::filesystem::create_directories(participant);
  std::filesystem::permissions(participant, std::filesystem::perms::owner_all);
  const std::vector<storage::ManifestExtent> manifest{{gpu_uuid, 4096, "device-0000.bin"}};
  std::string error;
  ASSERT_TRUE(storage::WriteManifest(participant, manifest, &error)) << error;
  EXPECT_THROW(Session(engine, Binding(false, true), Directory()), std::runtime_error);
  FileDescriptor extent(open((participant / manifest[0].filename).c_str(), O_CREAT | O_RDWR, 0600));
  ASSERT_GE(extent.get(), 0);
  ASSERT_EQ(ftruncate(extent.get(), 4096), 0);
  EXPECT_NO_THROW(Session(engine, Binding(false, true), Directory()));
  auto checksummed = Binding(false, true);
  checksummed.set_enable_checksum_digest(true);
  EXPECT_THROW(Session(engine, checksummed, Directory()), std::runtime_error);
  EXPECT_TRUE(driver_calls.empty());
}

TEST_F(WorkerModes, CompletedSessionWaitsForDrainAndControlAcknowledgesReleasedResources)
{
  auto daemon = StartDaemon(false);
  ASSERT_EQ(ReadReply(daemon->control.get()).status(), protocol::GPUSessionReply::READY);
  int sockets[2];
  ASSERT_EQ(socketpair(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0, sockets), 0);
  FileDescriptor client(sockets[0]), peer(sockets[1]);
  protocol::HelperRequest admission;
  admission.set_session_id(41);
  *admission.mutable_bind() = Binding(false);
  auto root = Directory();
  SendFrame(daemon->control.get(), admission, {peer.get(), root.get()});
  peer = FileDescriptor(-1);
  ASSERT_EQ(ReadReply(client.get()).status(), protocol::GPUSessionReply::READY);
  for (const auto phase : {protocol::GPUSessionRequest::PREPARE, protocol::GPUSessionRequest::TRANSFER,
                           protocol::GPUSessionRequest::COMPLETE}) {
    protocol::GPUSessionRequest request;
    request.set_operation(phase);
    SendFrame(client.get(), request);
    ASSERT_FALSE(ReadReply(client.get()).has_failure());
  }
  EXPECT_FALSE(Readable(client.get(), 20)); // COMPLETE does not release the session.
  ASSERT_EQ(shutdown(client.get(), SHUT_WR), 0);
  protocol::HelperRequest drain;
  drain.set_session_id(41);
  drain.set_drain(true);
  SendFrame(daemon->control.get(), drain);
  const auto drained = ReadReply(daemon->control.get());
  EXPECT_EQ(drained.status(), protocol::GPUSessionReply::DRAINED);
  EXPECT_EQ(drained.session_id(), 41);
  // The control reply follows session destruction, including directory closure.
  for (const auto& entry : std::filesystem::directory_iterator("/proc/" + std::to_string(daemon->pid) + "/fd")) {
    std::error_code error;
    EXPECT_NE(std::filesystem::read_symlink(entry.path(), error), directory);
  }
  EXPECT_EQ(kill(pid, 0), 0);
}

TEST_F(WorkerModes, DrainAcknowledgesAnUnadmittedOrAlreadyDrainedBatchID)
{
  auto daemon = StartDaemon(false);
  ASSERT_EQ(ReadReply(daemon->control.get()).status(), protocol::GPUSessionReply::READY);
  protocol::HelperRequest drain;
  drain.set_session_id(99);
  drain.set_drain(true);
  for (int i = 0; i < 2; ++i) {
    SendFrame(daemon->control.get(), drain);
    const auto reply = ReadReply(daemon->control.get());
    EXPECT_EQ(reply.status(), protocol::GPUSessionReply::DRAINED);
    EXPECT_EQ(reply.session_id(), 99);
  }
}

TEST_F(WorkerModes, SharedNamespaceCacheRejectsAnExitedTarget)
{
  Engine engine;
  auto first = engine.PinNamespace(getpid());
  auto second = engine.PinNamespace(getpid());
  ASSERT_EQ(first, second);
  EXPECT_EQ(first->Resolve(pid), pid);
  ASSERT_EQ(kill(pid, SIGKILL), 0);
  ASSERT_EQ(waitpid(pid, nullptr, 0), pid);
  const auto exited = pid;
  pid = -1;
  EXPECT_THROW(second->Resolve(exited), std::runtime_error);
}

TEST_F(WorkerModes, AdmissionPinsNamespaceBeforeRestoredTargetPIDIsKnown)
{
  Engine engine;
  auto binding = Binding(false);
  binding.set_namespace_pid(INT_MAX); // Captured PID need not exist after restore.
  Session session(engine, binding, Directory());
  EXPECT_TRUE(driver_calls.empty());
  protocol::GPUSessionRequest request;
  request.set_target_pid(pid);
  for (const auto phase : {protocol::GPUSessionRequest::PREPARE, protocol::GPUSessionRequest::TRANSFER,
                           protocol::GPUSessionRequest::COMPLETE}) {
    request.set_operation(phase);
    EXPECT_FALSE(session.Execute(request).has_failure());
  }
  session.Drain();
  EXPECT_EQ(driver_calls, (std::vector<int>{3, 4}));
  EXPECT_EQ(kill(pid, 0), 0);
}

TEST_F(WorkerModes, MalformedControlExitsWithoutWaitingForIdleSessions)
{
  auto daemon = StartDaemon(false);
  ASSERT_EQ(ReadReply(daemon->control.get()).status(), protocol::GPUSessionReply::READY);
  int sockets[2];
  ASSERT_EQ(socketpair(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0, sockets), 0);
  FileDescriptor client(sockets[0]), peer(sockets[1]);
  protocol::HelperRequest admission;
  admission.set_session_id(7);
  *admission.mutable_bind() = Binding(false);
  auto root = Directory();
  SendFrame(daemon->control.get(), admission, {peer.get(), root.get()});
  peer = FileDescriptor(-1);
  ASSERT_EQ(ReadReply(client.get()).status(), protocol::GPUSessionReply::READY);
  protocol::HelperRequest malformed;
  SendFrame(daemon->control.get(), malformed);
  ASSERT_TRUE(Readable(daemon->control.get()));
  protocol::GPUSessionReply reply;
  std::vector<FileDescriptor> rights;
  EXPECT_FALSE(ReceiveFrame(daemon->control.get(), reply, rights));
  int status = 0;
  ASSERT_EQ(waitpid(daemon->pid, &status, 0), daemon->pid);
  daemon->pid = -1;
  ASSERT_TRUE(WIFEXITED(status));
  EXPECT_EQ(WEXITSTATUS(status), 1);
  EXPECT_EQ(kill(pid, 0), 0); // Idle admission never touched the target.
}

TEST_F(WorkerModes, ControlEOFExitsEvenWithAnIdleAdmittedSession)
{
  auto daemon = StartDaemon(false);
  ASSERT_EQ(ReadReply(daemon->control.get()).status(), protocol::GPUSessionReply::READY);
  int sockets[2];
  ASSERT_EQ(socketpair(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0, sockets), 0);
  FileDescriptor client(sockets[0]), peer(sockets[1]);
  protocol::HelperRequest admission;
  admission.set_session_id(7);
  *admission.mutable_bind() = Binding(false);
  auto root = Directory();
  SendFrame(daemon->control.get(), admission, {peer.get(), root.get()});
  peer = FileDescriptor(-1);
  ASSERT_EQ(ReadReply(client.get()).status(), protocol::GPUSessionReply::READY);
  daemon->control = FileDescriptor(-1);
  ASSERT_TRUE(Readable(client.get()));
  protocol::GPUSessionReply reply;
  std::vector<FileDescriptor> rights;
  EXPECT_FALSE(ReceiveFrame(client.get(), reply, rights));
  int status = 0;
  ASSERT_EQ(waitpid(daemon->pid, &status, 0), daemon->pid);
  daemon->pid = -1;
  ASSERT_TRUE(WIFEXITED(status));
  EXPECT_EQ(WEXITSTATUS(status), 0);
  EXPECT_EQ(kill(pid, 0), 0);
}

TEST_F(WorkerModes, OwnerDeathExitsEvenWhenASeparateCLIProcessKeepsControlOpen)
{
  // Adopt the owner's children when it exits so this regression leaves no
  // orphan helper/CLI processes or zombies behind in the test container.
  struct Children {
    int was_subreaper = 0;
    pid_t owner = -1;
    pid_t helper = -1;
    pid_t cli = -1;
    ~Children()
    {
      for (const auto child : {owner, helper, cli}) {
        if (child <= 0) continue;
        kill(child, SIGKILL);
        while (waitpid(child, nullptr, 0) < 0 && errno == EINTR) {}
      }
      prctl(PR_SET_CHILD_SUBREAPER, was_subreaper);
    }
  } children;
  ASSERT_EQ(prctl(PR_GET_CHILD_SUBREAPER, &children.was_subreaper), 0);
  ASSERT_EQ(prctl(PR_SET_CHILD_SUBREAPER, 1), 0);
  int reports[2];
  ASSERT_EQ(pipe2(reports, O_CLOEXEC), 0);
  FileDescriptor report_read(reports[0]), report_write(reports[1]);
  children.owner = fork();
  ASSERT_GE(children.owner, 0);
  if (!children.owner) {
    report_read = FileDescriptor(-1);
    auto daemon = StartDaemon(false); // The owner itself creates the socketpair.
    const auto ready = ReadReply(daemon->control.get());
    if (ready.status() != protocol::GPUSessionReply::READY) std::_Exit(8);
    const auto cli = fork();
    if (cli < 0) std::_Exit(8);
    if (!cli) {
      report_write = FileDescriptor(-1);
      // Simulate a stuck inherited --drain-batch process. Its copy prevents
      // control EOF when the agent is killed, but cannot keep the agent alive.
      while (true) pause();
    }
    const pid_t ids[]{daemon->pid, cli};
    if (write(report_write.get(), ids, sizeof(ids)) != sizeof(ids)) std::_Exit(8);
    while (true) pause();
  }
  report_write = FileDescriptor(-1);
  ASSERT_TRUE(Readable(report_read.get()));
  pid_t ids[2];
  ASSERT_EQ(read(report_read.get(), ids, sizeof(ids)), sizeof(ids));
  children.helper = ids[0];
  children.cli = ids[1];
  FileDescriptor helper_pidfd(static_cast<int>(syscall(SYS_pidfd_open, children.helper, 0)));
  ASSERT_GE(helper_pidfd.get(), 0);
  EXPECT_FALSE(Readable(helper_pidfd.get(), 20));
  ASSERT_EQ(kill(children.owner, SIGKILL), 0);
  ASSERT_EQ(waitpid(children.owner, nullptr, 0), children.owner);
  children.owner = -1;
  ASSERT_TRUE(Readable(helper_pidfd.get()));
  int status = 0;
  ASSERT_EQ(waitpid(children.helper, &status, 0), children.helper);
  children.helper = -1;
  ASSERT_TRUE(WIFEXITED(status));
  EXPECT_EQ(WEXITSTATUS(status), 1);
  EXPECT_EQ(kill(children.cli, 0), 0); // It still holds the control endpoint.
}
