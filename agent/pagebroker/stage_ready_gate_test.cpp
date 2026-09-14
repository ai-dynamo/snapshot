// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include <gtest/gtest.h>

#include <chrono>
#include <cerrno>
#include <fcntl.h>
#include <filesystem>
#include <fstream>
#include <grp.h>
#include <iterator>
#include <string>
#include <sys/stat.h>
#include <sys/wait.h>
#include <unistd.h>
#include <utility>
#include <vector>

#include "stage_ready_gate.hpp"

namespace fs = std::filesystem;
using namespace snapshot::pagebroker;

namespace {

class ScopedUmask {
 public:
  explicit ScopedUmask(mode_t mask) : original_(umask(mask)) {}
  ~ScopedUmask() { umask(original_); }

 private:
  mode_t original_;
};

class StageReadyGateTest : public ::testing::Test {
 protected:
  void SetUp() override
  {
    root_ = fs::temp_directory_path() / "pagebroker-stage-ready-gate" /
            ::testing::UnitTest::GetInstance()->current_test_info()->name();
    fs::remove_all(root_);
    fs::create_directories(root_);
  }

  void TearDown() override { fs::remove_all(root_); }

  StageReadyIdentity Identity(std::string transaction = "transaction") const
  {
    return {
        .node_name = "worker-node-a",
        .pod_uid = "0f78e1b4-7b77-4c63-9836-9b24b47a7861",
        .destination_container = "main",
        .content_uid = "content-uid",
        .source_container = "main",
        .container_id = "containerd://0123456789abcdef",
        .transaction_id = std::move(transaction),
        .stage_request_id = "stage-request-id",
    };
  }

  fs::path MarkerPath(const StageReadyGate& gate,
                      const StageReadyHandle& handle) const
  {
    return root_ / ".pagebroker-stage-ready" / "v1" / "owners" /
           gate.owner_id() / "markers" / (handle.operation_id + ".json");
  }

  fs::path GenerationPath(const StageReadyGate& gate) const
  {
    return root_ / ".pagebroker-stage-ready" / "v1" / "owners" /
           gate.owner_id() / "generation.json";
  }

  std::string Read(const fs::path& path) const
  {
    std::ifstream stream(path);
    return {std::istreambuf_iterator<char>(stream),
            std::istreambuf_iterator<char>()};
  }

  size_t MarkerCount(const StageReadyGate& gate) const
  {
    const auto markers = root_ / ".pagebroker-stage-ready" / "v1" /
                         "owners" / gate.owner_id() / "markers";
    return static_cast<size_t>(std::distance(
        fs::directory_iterator(markers), fs::directory_iterator{}));
  }

  fs::path root_;
};

TEST_F(StageReadyGateTest, MatchesConsumerHashAndPublishesExactSchema)
{
  EXPECT_EQ(StageReadyGate::OperationId(
                "0f78e1b4-7b77-4c63-9836-9b24b47a7861", "main"),
            "38362730caf48eb81133e166331e51547700aac12ca266f7b99887dc05fcee50");
  StageReadyGate gate(root_, "release\nworker-node-a", "worker-node-a");
  const auto handle = gate.PublishReady(Identity());
  const std::string payload = Read(MarkerPath(gate, handle));
  EXPECT_EQ(gate.owner_id().size(), 64);
  EXPECT_EQ(gate.generation().size(), 32);
  EXPECT_EQ(
      payload,
      "{\"schema_version\":1,"
      "\"operation_id\":\"38362730caf48eb81133e166331e51547700aac12ca266f7b99887dc05fcee50\"," 
      "\"node_name\":\"worker-node-a\"," 
      "\"pod_uid\":\"0f78e1b4-7b77-4c63-9836-9b24b47a7861\","
      "\"destination_container\":\"main\",\"content_uid\":\"content-uid\","
      "\"source_container\":\"main\","
      "\"container_id\":\"containerd://0123456789abcdef\","
      "\"transaction_id\":\"transaction\","
      "\"stage_request_id\":\"stage-request-id\",\"pagebroker_owner_id\":\"" +
          gate.owner_id() +
          "\",\"pagebroker_generation\":\"" + gate.generation() +
          "\",\"backend\":\"regular\",\"state\":\"ready\"}\n");
  EXPECT_EQ(Read(GenerationPath(gate)),
            "{\"schema_version\":1,\"pagebroker_owner_id\":\"" +
                gate.owner_id() + "\",\"pagebroker_generation\":\"" +
                gate.generation() + "\"}\n");
}

TEST_F(StageReadyGateTest, CollisionNeverReplacesFirstOperation)
{
  StageReadyGate gate(root_, "release\nworker-node-a", "worker-node-a");
  const auto first = gate.PublishReady(Identity("first"));
  EXPECT_THROW(gate.PublishReady(Identity("second")), std::runtime_error);
  EXPECT_NE(Read(MarkerPath(gate, first)).find("\"transaction_id\":\"first\""),
            std::string::npos);
}

TEST_F(StageReadyGateTest, NewContainerAtomicallySupersedesCommittedOperation)
{
  StageReadyGate gate(root_, "release\nworker-node-a", "worker-node-a");
  const auto first = gate.PublishReady(Identity("first"));
  gate.Transition(first, StageReadyState::COMMITTED);

  auto replacement = Identity("second");
  replacement.container_id = "containerd://new-container";
  replacement.stage_request_id = "new-stage-request";
  const auto second = gate.PublishReady(replacement);

  EXPECT_EQ(second.operation_id, first.operation_id);
  EXPECT_EQ(MarkerCount(gate), 1u);
  const auto payload = Read(MarkerPath(gate, second));
  EXPECT_NE(payload.find("\"container_id\":\"containerd://new-container\""),
            std::string::npos);
  EXPECT_NE(payload.find("\"transaction_id\":\"second\""),
            std::string::npos);
  EXPECT_NE(payload.find("\"stage_request_id\":\"new-stage-request\""),
            std::string::npos);
  EXPECT_NE(payload.find("\"state\":\"ready\""), std::string::npos);
}

TEST_F(StageReadyGateTest, NewContainerCanSupersedeCommittedPriorGeneration)
{
  StageReadyHandle first;
  {
    StageReadyGate gate(root_, "release\nworker-node-a", "worker-node-a");
    first = gate.PublishReady(Identity("first"));
    gate.Transition(first, StageReadyState::COMMITTED);
  }
  StageReadyGate replacement_gate(
      root_, "release\nworker-node-a", "worker-node-a");
  auto replacement = Identity("second");
  replacement.container_id = "containerd://new-container";
  replacement.stage_request_id = "new-stage-request";
  const auto second = replacement_gate.PublishReady(replacement);

  EXPECT_EQ(second.operation_id, first.operation_id);
  EXPECT_NE(second.pagebroker_generation, first.pagebroker_generation);
  EXPECT_EQ(MarkerCount(replacement_gate), 1u);
  EXPECT_NE(Read(MarkerPath(replacement_gate, second))
                .find("\"state\":\"ready\""),
            std::string::npos);
}

TEST_F(StageReadyGateTest, NewContainerCanSupersedeCoordinatorLostPriorGeneration)
{
  StageReadyHandle first;
  {
    StageReadyGate gate(root_, "release\nworker-node-a", "worker-node-a");
    first = gate.PublishReady(Identity("first"));
  }
  StageReadyGate replacement_gate(
      root_, "release\nworker-node-a", "worker-node-a");
  auto replacement = Identity("second");
  replacement.container_id = "containerd://new-container";
  replacement.stage_request_id = "new-stage-request";
  const auto second = replacement_gate.PublishReady(replacement);

  EXPECT_EQ(second.operation_id, first.operation_id);
  EXPECT_NE(second.pagebroker_generation, first.pagebroker_generation);
  EXPECT_EQ(MarkerCount(replacement_gate), 1u);
  const auto payload = Read(MarkerPath(replacement_gate, second));
  EXPECT_NE(payload.find("\"operation_id\":\"" + second.operation_id + "\""),
            std::string::npos);
  EXPECT_NE(payload.find("\"state\":\"ready\""), std::string::npos);
}

TEST_F(StageReadyGateTest, SupersessionRejectsEveryNonReplaceableState)
{
  for (const auto state : {StageReadyState::READY, StageReadyState::ABORTED,
                           StageReadyState::EXPIRED}) {
    SCOPED_TRACE(static_cast<int>(state));
    const auto iteration_root = root_ / std::to_string(static_cast<int>(state));
    fs::create_directories(iteration_root);
    StageReadyGate gate(
        iteration_root, "release\nworker-node-a", "worker-node-a");
    const auto first = gate.PublishReady(Identity("first"));
    if (state != StageReadyState::READY)
      gate.Transition(first, state);
    auto replacement = Identity("second");
    replacement.container_id = "containerd://new-container";
    replacement.stage_request_id = "new-stage-request";

    EXPECT_THROW(gate.PublishReady(replacement), std::runtime_error);
    const auto payload = Read(
        iteration_root / ".pagebroker-stage-ready" / "v1" / "owners" /
        gate.owner_id() / "markers" / (first.operation_id + ".json"));
    EXPECT_NE(payload.find("\"container_id\":\"containerd://0123456789abcdef\""),
              std::string::npos);
    EXPECT_EQ(payload.find("containerd://new-container"), std::string::npos);
  }
}

TEST_F(StageReadyGateTest, SupersessionRejectsSameContainerOrReusedProof)
{
  for (const int scenario : {0, 1, 2}) {
    SCOPED_TRACE(scenario);
    const auto iteration_root = root_ / std::to_string(scenario);
    fs::create_directories(iteration_root);
    StageReadyGate gate(
        iteration_root, "release\nworker-node-a", "worker-node-a");
    const auto first = gate.PublishReady(Identity("first"));
    gate.Transition(first, StageReadyState::COMMITTED);
    auto replacement = Identity(scenario == 1 ? "first" : "second");
    if (scenario != 0)
      replacement.container_id = "containerd://new-container";
    replacement.stage_request_id =
        scenario == 2 ? "stage-request-id" : "new-stage-request";

    EXPECT_THROW(gate.PublishReady(replacement), std::runtime_error);
    EXPECT_NE(Read(
                  iteration_root / ".pagebroker-stage-ready" / "v1" /
                  "owners" / gate.owner_id() / "markers" /
                  (first.operation_id + ".json"))
                  .find("\"state\":\"committed\""),
              std::string::npos);
  }
}

TEST_F(StageReadyGateTest, SupersessionFailuresBeforeRenamePreserveCommitted)
{
  for (const std::string failure : {"supersede_marker_write",
                                    "supersede_marker_file_fsync",
                                    "supersede_marker_rename"}) {
    SCOPED_TRACE(failure);
    const auto iteration_root = root_ / failure;
    fs::create_directories(iteration_root);
    bool inject = false;
    StageReadyGate gate(
        iteration_root, "release\nworker-node-a", "worker-node-a",
        [&](const std::string& operation) {
          return inject && operation == failure ? EIO : 0;
        });
    const auto first = gate.PublishReady(Identity("first"));
    gate.Transition(first, StageReadyState::COMMITTED);
    auto replacement = Identity("second");
    replacement.container_id = "containerd://new-container";
    replacement.stage_request_id = "new-stage-request";
    inject = true;

    EXPECT_THROW(gate.PublishReady(replacement), std::system_error);
    const auto marker = iteration_root / ".pagebroker-stage-ready" / "v1" /
                        "owners" / gate.owner_id() / "markers" /
                        (first.operation_id + ".json");
    const auto payload = Read(marker);
    EXPECT_NE(payload.find("\"state\":\"committed\""), std::string::npos);
    EXPECT_NE(payload.find("containerd://0123456789abcdef"),
              std::string::npos);
    EXPECT_EQ(payload.find("containerd://new-container"), std::string::npos);
  }
}

TEST_F(StageReadyGateTest, SupersessionDirectorySyncFailureOwnsVisibleReady)
{
  bool inject = false;
  StageReadyGate gate(
      root_, "release\nworker-node-a", "worker-node-a",
      [&](const std::string& operation) {
        if (inject && operation == "supersede_marker_directory_fsync") {
          inject = false;
          return EIO;
        }
        return 0;
      });
  const auto first = gate.PublishReady(Identity("first"));
  gate.Transition(first, StageReadyState::COMMITTED);
  auto replacement = Identity("second");
  replacement.container_id = "containerd://new-container";
  replacement.stage_request_id = "new-stage-request";
  inject = true;

  StageReadyHandle uncertain;
  try {
    (void)gate.PublishReady(replacement);
    FAIL() << "supersession should report uncertain durability";
  }
  catch (const StageReadyDurabilityUncertain& error) {
    uncertain = error.handle();
    EXPECT_EQ(error.state(), StageReadyState::READY);
    EXPECT_EQ(uncertain.transaction_id, "second");
    EXPECT_EQ(uncertain.stage_request_id, "new-stage-request");
  }
  EXPECT_EQ(MarkerCount(gate), 1u);
  const auto payload = Read(MarkerPath(gate, uncertain));
  EXPECT_NE(payload.find("containerd://new-container"), std::string::npos);
  EXPECT_NE(payload.find("\"state\":\"ready\""), std::string::npos);
  EXPECT_NO_THROW(gate.PublishReady(replacement));
}

TEST_F(StageReadyGateTest, PublicationFailuresBeforeRenameLeaveNoMarker)
{
  for (const std::string failure : {"publish_marker_write",
                                    "publish_marker_file_fsync",
                                    "publish_marker_rename"}) {
    SCOPED_TRACE(failure);
    const auto iteration_root = root_ / failure;
    fs::create_directories(iteration_root);
    StageReadyGate gate(
        iteration_root, "release\nworker-node-a", "worker-node-a",
        [&](const std::string& operation) {
          return operation == failure ? EIO : 0;
        });
    EXPECT_THROW(gate.PublishReady(Identity()), std::system_error);
    const auto operation_id = StageReadyGate::OperationId(
        Identity().pod_uid, Identity().destination_container);
    const auto marker = iteration_root / ".pagebroker-stage-ready" / "v1" /
                        "owners" / gate.owner_id() / "markers" /
                        (operation_id + ".json");
    EXPECT_FALSE(fs::exists(marker));
  }
}

TEST_F(StageReadyGateTest,
       LostRenameResponseIsReconciledAndCanBeTerminalized)
{
  bool observed_private_marker = false;
  bool inject = true;
  StageReadyGate gate(
      root_, "release\nworker-node-a", "worker-node-a",
      [&](const std::string& operation) {
        if (!inject || operation != "publish_marker_renamed")
          return 0;
        inject = false;
        const auto markers = root_ / ".pagebroker-stage-ready" / "v1" /
                             "owners" / gate.owner_id() / "markers";
        std::vector<struct stat> records;
        for (const auto& entry : fs::directory_iterator(markers)) {
          struct stat status{};
          EXPECT_EQ(lstat(entry.path().c_str(), &status), 0);
          records.push_back(status);
        }
        EXPECT_EQ(records.size(), 1u);
        if (records.size() == 1u) {
          EXPECT_EQ(records[0].st_nlink, 1);
          EXPECT_EQ(records[0].st_mode & 0777, 0600);
          observed_private_marker = true;
        }
        return EIO;
      });

  const auto handle = gate.PublishReady(Identity());
  EXPECT_TRUE(observed_private_marker);
  EXPECT_EQ(MarkerCount(gate), 1u);
  struct stat marker_status{};
  ASSERT_EQ(stat(MarkerPath(gate, handle).c_str(), &marker_status), 0);
  EXPECT_EQ(marker_status.st_nlink, 1);
  EXPECT_EQ(marker_status.st_mode & 0777, 0644);
  EXPECT_NE(Read(MarkerPath(gate, handle)).find("\"state\":\"ready\""),
            std::string::npos);
  gate.Transition(handle, StageReadyState::ABORTED);
  EXPECT_NE(Read(MarkerPath(gate, handle)).find("\"state\":\"aborted\""),
            std::string::npos);
}

TEST_F(StageReadyGateTest, OwnerLockSerializesNoReplacePublication)
{
  StageReadyGate gate(root_, "release\nworker-node-a", "worker-node-a");
  EXPECT_THROW(StageReadyGate(root_, "release\nworker-node-a",
                              "worker-node-a"),
               fs::filesystem_error);
}

TEST_F(StageReadyGateTest,
       RenameReconciliationLookupFailureHasUncertainOutcome)
{
  StageReadyGate gate(
      root_, "release\nworker-node-a", "worker-node-a",
      [](const std::string& operation) {
        if (operation == "publish_marker_renamed")
          return EIO;
        if (operation == "publish_marker_reconcile_published_stat")
          return EACCES;
        return 0;
      });
  StageReadyHandle handle;
  try {
    (void)gate.PublishReady(Identity());
    FAIL() << "publication should report uncertain namespace state";
  }
  catch (const StageReadyDurabilityUncertain& error) {
    handle = error.handle();
    EXPECT_EQ(error.state(), StageReadyState::READY);
  }
  EXPECT_NO_THROW(gate.Transition(handle, StageReadyState::ABORTED));
  EXPECT_NE(Read(MarkerPath(gate, handle)).find("\"state\":\"aborted\""),
            std::string::npos);
}

TEST_F(StageReadyGateTest, ModeSyncFailureHasUncertainOutcomeAndCanAbort)
{
  bool fail = true;
  StageReadyGate gate(
      root_, "release\nworker-node-a", "worker-node-a",
      [&](const std::string& operation) {
        if (fail && operation == "publish_marker_mode_fsync") {
          fail = false;
          return EIO;
        }
        return 0;
      });
  StageReadyHandle handle;
  try {
    (void)gate.PublishReady(Identity());
    FAIL() << "publication should report uncertain mode durability";
  }
  catch (const StageReadyDurabilityUncertain& error) {
    handle = error.handle();
    EXPECT_EQ(error.state(), StageReadyState::READY);
  }
  EXPECT_NO_THROW(gate.Transition(handle, StageReadyState::ABORTED));
  EXPECT_NE(Read(MarkerPath(gate, handle)).find("\"state\":\"aborted\""),
            std::string::npos);
}

TEST_F(StageReadyGateTest, PublicationDirectorySyncFailureLeavesCompleteMarker)
{
  bool fail = true;
  StageReadyGate gate(
      root_, "release\nworker-node-a", "worker-node-a",
      [&](const std::string& operation) {
        if (operation == "publish_marker_directory_fsync" && fail) {
          fail = false;
          return EIO;
        }
        return 0;
      });
  StageReadyHandle handle;
  try {
    (void)gate.PublishReady(Identity());
    FAIL() << "publication should report uncertain durability";
  }
  catch (const StageReadyDurabilityUncertain& error) {
    handle = error.handle();
    EXPECT_EQ(error.state(), StageReadyState::READY);
    EXPECT_EQ(handle.transaction_id, "transaction");
    EXPECT_EQ(handle.stage_request_id, "stage-request-id");
    EXPECT_EQ(handle.pagebroker_generation, gate.generation());
  }
  EXPECT_NE(Read(MarkerPath(gate, handle)).find("\"state\":\"ready\""),
            std::string::npos);
  const auto repaired = gate.PublishReady(Identity());
  EXPECT_EQ(repaired.operation_id, handle.operation_id);
  EXPECT_EQ(repaired.transaction_id, handle.transaction_id);
  EXPECT_EQ(repaired.stage_request_id, handle.stage_request_id);
  EXPECT_EQ(repaired.pagebroker_generation, handle.pagebroker_generation);
}

TEST_F(StageReadyGateTest, TransitionFailuresBeforeRenamePreserveReady)
{
  for (const std::string failure : {"transition_marker_write",
                                    "transition_marker_file_fsync",
                                    "transition_marker_rename"}) {
    SCOPED_TRACE(failure);
    const auto iteration_root = root_ / failure;
    fs::create_directories(iteration_root);
    bool fail = false;
    StageReadyGate gate(
        iteration_root, "release\nworker-node-a", "worker-node-a",
        [&](const std::string& operation) {
          return fail && operation == failure ? EIO : 0;
        });
    const auto handle = gate.PublishReady(Identity());
    fail = true;
    EXPECT_THROW(gate.Transition(handle, StageReadyState::COMMITTED),
                 std::system_error);
    const auto marker = iteration_root / ".pagebroker-stage-ready" / "v1" /
                        "owners" / gate.owner_id() / "markers" /
                        (handle.operation_id + ".json");
    EXPECT_NE(Read(marker).find("\"state\":\"ready\""), std::string::npos);
  }
}

TEST_F(StageReadyGateTest, IdempotentTransitionRepairsDirectoryDurability)
{
  bool fail = false;
  int directory_sync_attempts = 0;
  StageReadyGate gate(
      root_, "release\nworker-node-a", "worker-node-a",
      [&](const std::string& operation) {
        if (operation != "transition_marker_directory_fsync")
          return 0;
        ++directory_sync_attempts;
        if (fail) {
          fail = false;
          return EIO;
        }
        return 0;
      });
  const auto handle = gate.PublishReady(Identity());
  fail = true;
  try {
    gate.Transition(handle, StageReadyState::COMMITTED);
    FAIL() << "transition should report uncertain durability";
  }
  catch (const StageReadyDurabilityUncertain& error) {
    EXPECT_EQ(error.handle().operation_id, handle.operation_id);
    EXPECT_EQ(error.handle().stage_request_id, handle.stage_request_id);
    EXPECT_EQ(error.state(), StageReadyState::COMMITTED);
  }
  EXPECT_NE(Read(MarkerPath(gate, handle)).find("\"state\":\"committed\""),
            std::string::npos);
  EXPECT_NO_THROW(gate.Transition(handle, StageReadyState::COMMITTED));
  EXPECT_EQ(directory_sync_attempts, 2);
}

TEST_F(StageReadyGateTest, StartupChangesReadyToCoordinatorLostBeforeNewGeneration)
{
  StageReadyHandle handle;
  std::string first_generation;
  std::string owner_id;
  {
    StageReadyGate first(root_, "release\nworker-node-a", "worker-node-a");
    handle = first.PublishReady(Identity());
    first_generation = first.generation();
    owner_id = first.owner_id();
  }
  StageReadyGate replacement(root_, "release\nworker-node-a", "worker-node-a");
  EXPECT_NE(first_generation, replacement.generation());
  const auto path = root_ / ".pagebroker-stage-ready" / "v1" / "owners" /
                    owner_id / "markers" / (handle.operation_id + ".json");
  EXPECT_NE(Read(path).find("\"state\":\"coordinator_lost\""),
            std::string::npos);
  EXPECT_NE(Read(path).find("\"pagebroker_generation\":\"" +
                            first_generation + "\""),
            std::string::npos);
}

TEST_F(StageReadyGateTest, StartupRejectsReadyMarkerOutsideRecordedGeneration)
{
  StageReadyHandle handle;
  fs::path marker;
  {
    StageReadyGate first(root_, "release\nworker-node-a", "worker-node-a");
    handle = first.PublishReady(Identity());
    marker = MarkerPath(first, handle);
  }
  auto payload = Read(marker);
  const auto generation = payload.find(handle.pagebroker_generation);
  ASSERT_NE(generation, std::string::npos);
  payload.replace(generation, handle.pagebroker_generation.size(),
                  std::string(32, 'c'));
  std::ofstream(marker, std::ios::trunc) << payload;
  EXPECT_THROW(
      StageReadyGate(root_, "release\nworker-node-a", "worker-node-a"),
      std::runtime_error);
}

TEST_F(StageReadyGateTest, RecoveryRenameFailureIsSafelyReconstructed)
{
  StageReadyHandle handle;
  std::string owner_id;
  std::string old_generation;
  {
    StageReadyGate first(root_, "release\nworker-node-a", "worker-node-a");
    handle = first.PublishReady(Identity());
    owner_id = first.owner_id();
    old_generation = first.generation();
  }
  EXPECT_THROW(
      StageReadyGate(root_, "release\nworker-node-a", "worker-node-a",
                     [](const std::string& operation) {
                       return operation == "recover_marker_rename" ? EIO : 0;
                     }),
      std::runtime_error);
  const auto marker = root_ / ".pagebroker-stage-ready" / "v1" / "owners" /
                      owner_id / "markers" / (handle.operation_id + ".json");
  EXPECT_NE(Read(marker).find("\"state\":\"ready\""), std::string::npos);
  EXPECT_NE(Read(marker).find(old_generation), std::string::npos);

  StageReadyGate recovered(root_, "release\nworker-node-a", "worker-node-a");
  EXPECT_NE(recovered.generation(), old_generation);
  EXPECT_NE(Read(marker).find("\"state\":\"coordinator_lost\""),
            std::string::npos);
}

TEST_F(StageReadyGateTest, RecoveryDirectorySyncFailureIsSafelyReconstructed)
{
  StageReadyHandle handle;
  std::string owner_id;
  std::string old_generation;
  {
    StageReadyGate first(root_, "release\nworker-node-a", "worker-node-a");
    handle = first.PublishReady(Identity());
    owner_id = first.owner_id();
    old_generation = first.generation();
  }
  EXPECT_THROW(
      StageReadyGate(root_, "release\nworker-node-a", "worker-node-a",
                     [](const std::string& operation) {
                       return operation == "recover_marker_directory_fsync"
                                  ? EIO
                                  : 0;
                     }),
      std::runtime_error);
  const auto marker = root_ / ".pagebroker-stage-ready" / "v1" / "owners" /
                      owner_id / "markers" / (handle.operation_id + ".json");
  EXPECT_NE(Read(marker).find("\"state\":\"coordinator_lost\""),
            std::string::npos);

  StageReadyGate recovered(root_, "release\nworker-node-a", "worker-node-a");
  EXPECT_NE(recovered.generation(), old_generation);
  EXPECT_NE(Read(marker).find("\"state\":\"coordinator_lost\""),
            std::string::npos);
}

TEST_F(StageReadyGateTest, GenerationPublicationFailuresAreSafelyReconstructed)
{
  for (const std::string failure : {"publish_generation_rename",
                                    "publish_generation_directory_fsync"}) {
    SCOPED_TRACE(failure);
    const auto iteration_root = root_ / failure;
    fs::create_directories(iteration_root);
    EXPECT_THROW(
        StageReadyGate(iteration_root, "release\nworker-node-a", "worker-node-a",
                       [&](const std::string& operation) {
                         return operation == failure ? EIO : 0;
                       }),
        std::runtime_error);
    EXPECT_NO_THROW(StageReadyGate(iteration_root, "release\nworker-node-a",
                                   "worker-node-a"));
  }
}

TEST_F(StageReadyGateTest, StartupCleansOnlyExactGenerationTemporaryLeaves)
{
  fs::path owner_root;
  {
    StageReadyGate first(root_, "release\nworker-node-a", "worker-node-a");
    owner_root = root_ / ".pagebroker-stage-ready" / "v1" / "owners" /
                 first.owner_id();
  }
  const auto exact = owner_root /
                     (".generation.json.tmp." + std::string(32, 'a'));
  const auto unrelated = owner_root / ".generation.json.tmp.not-exact";
  std::ofstream(exact) << "partial";
  std::ofstream(unrelated) << "keep";

  EXPECT_NO_THROW(
      StageReadyGate(root_, "release\nworker-node-a", "worker-node-a"));
  EXPECT_FALSE(fs::exists(exact));
  EXPECT_TRUE(fs::exists(unrelated));
}

TEST_F(StageReadyGateTest, RestrictiveUmaskStillPublishesReadablePrivateTree)
{
  ScopedUmask restrictive(0077);
  StageReadyGate gate(root_, "release\nworker-node-a", "worker-node-a");
  const auto handle = gate.PublishReady(Identity());
  const auto stage = root_ / ".pagebroker-stage-ready";
  const auto version = stage / "v1";
  const auto owners = version / "owners";
  const auto owner = owners / gate.owner_id();
  const auto markers = owner / "markers";
  for (const auto& directory : {stage, version, owners, owner, markers}) {
    struct stat status{};
    ASSERT_EQ(stat(directory.c_str(), &status), 0);
    EXPECT_EQ(status.st_mode & 0777, 0755) << directory;
  }
  for (const auto& record : {GenerationPath(gate), MarkerPath(gate, handle)}) {
    struct stat status{};
    ASSERT_EQ(stat(record.c_str(), &status), 0);
    EXPECT_EQ(status.st_mode & 0777, 0644) << record;
  }
  struct stat lock_status{};
  ASSERT_EQ(stat((owner / "owner.lock").c_str(), &lock_status), 0);
  EXPECT_EQ(lock_status.st_mode & 0777, 0600);
}

TEST_F(StageReadyGateTest, StartupNormalizesExistingRecordAndDirectoryModes)
{
  StageReadyHandle handle;
  fs::path stage;
  fs::path version;
  fs::path owners;
  fs::path owner;
  fs::path markers;
  fs::path generation;
  fs::path marker;
  {
    StageReadyGate first(root_, "release\nworker-node-a", "worker-node-a");
    handle = first.PublishReady(Identity());
    first.Transition(handle, StageReadyState::COMMITTED);
    stage = root_ / ".pagebroker-stage-ready";
    version = stage / "v1";
    owners = version / "owners";
    owner = owners / first.owner_id();
    markers = owner / "markers";
    generation = GenerationPath(first);
    marker = MarkerPath(first, handle);
  }
  for (const auto& directory : {stage, version, owners, owner, markers})
    ASSERT_EQ(chmod(directory.c_str(), 0700), 0);
  ASSERT_EQ(chmod(generation.c_str(), 0600), 0);
  ASSERT_EQ(chmod(marker.c_str(), 0600), 0);
  ASSERT_EQ(chmod((owner / "owner.lock").c_str(), 0666), 0);

  StageReadyGate replacement(root_, "release\nworker-node-a", "worker-node-a");
  for (const auto& directory : {stage, version, owners, owner, markers}) {
    struct stat status{};
    ASSERT_EQ(stat(directory.c_str(), &status), 0);
    EXPECT_EQ(status.st_mode & 0777, 0755) << directory;
  }
  for (const auto& record : {generation, marker}) {
    struct stat status{};
    ASSERT_EQ(stat(record.c_str(), &status), 0);
    EXPECT_EQ(status.st_mode & 0777, 0644) << record;
  }
  struct stat lock_status{};
  ASSERT_EQ(stat((owner / "owner.lock").c_str(), &lock_status), 0);
  EXPECT_EQ(lock_status.st_mode & 0777, 0600);
}

TEST_F(StageReadyGateTest,
       StartupRejectsForeignOwnedPredictableDirectoriesBeforeChangingMode)
{
  if (geteuid() != 0)
    GTEST_SKIP() << "foreign-ownership check requires the root test image";
  for (size_t index = 0; index < 5; ++index) {
    fs::remove_all(root_);
    fs::create_directories(root_);
    std::string owner_id;
    {
      StageReadyGate gate(root_, "release\nworker-node-a", "worker-node-a");
      owner_id = gate.owner_id();
    }
    const std::vector<fs::path> paths = {
        root_ / ".pagebroker-stage-ready",
        root_ / ".pagebroker-stage-ready" / "v1",
        root_ / ".pagebroker-stage-ready" / "v1" / "owners",
        root_ / ".pagebroker-stage-ready" / "v1" / "owners" / owner_id,
        root_ / ".pagebroker-stage-ready" / "v1" / "owners" / owner_id /
            "markers",
    };
    ASSERT_EQ(chmod(paths[index].c_str(), 0700), 0);
    ASSERT_EQ(chown(paths[index].c_str(), 65534, 65534), 0);
    EXPECT_THROW(
        StageReadyGate(root_, "release\nworker-node-a", "worker-node-a"),
        fs::filesystem_error)
        << paths[index];
    struct stat status{};
    ASSERT_EQ(lstat(paths[index].c_str(), &status), 0);
    EXPECT_EQ(status.st_mode & 0777, 0700) << paths[index];
  }
}

TEST_F(StageReadyGateTest,
       StartupRejectsForeignOwnedPredictableRecordsBeforeChangingMode)
{
  if (geteuid() != 0)
    GTEST_SKIP() << "foreign-ownership check requires the root test image";
  for (size_t index = 0; index < 3; ++index) {
    fs::remove_all(root_);
    fs::create_directories(root_);
    StageReadyHandle handle;
    fs::path owner;
    {
      StageReadyGate gate(root_, "release\nworker-node-a", "worker-node-a");
      handle = gate.PublishReady(Identity());
      owner = root_ / ".pagebroker-stage-ready" / "v1" / "owners" /
              gate.owner_id();
    }
    const std::vector<fs::path> paths = {
        owner / "owner.lock",
        owner / "generation.json",
        owner / "markers" / (handle.operation_id + ".json"),
    };
    ASSERT_EQ(chmod(paths[index].c_str(), 0600), 0);
    ASSERT_EQ(chown(paths[index].c_str(), 65534, 65534), 0);
    EXPECT_THROW(
        StageReadyGate(root_, "release\nworker-node-a", "worker-node-a"),
        fs::filesystem_error)
        << paths[index];
    struct stat status{};
    ASSERT_EQ(lstat(paths[index].c_str(), &status), 0);
    EXPECT_EQ(status.st_mode & 0777, 0600) << paths[index];
  }
}

TEST_F(StageReadyGateTest, StartupRejectsHardlinkedPredictableRecords)
{
  for (size_t index = 0; index < 3; ++index) {
    fs::remove_all(root_);
    fs::create_directories(root_);
    StageReadyHandle handle;
    fs::path owner;
    {
      StageReadyGate gate(root_, "release\nworker-node-a", "worker-node-a");
      handle = gate.PublishReady(Identity());
      owner = root_ / ".pagebroker-stage-ready" / "v1" / "owners" /
              gate.owner_id();
    }
    const std::vector<fs::path> paths = {
        owner / "owner.lock",
        owner / "generation.json",
        owner / "markers" / (handle.operation_id + ".json"),
    };
    fs::create_hard_link(
        paths[index], root_ / ("record-alias-" + std::to_string(index)));
    EXPECT_THROW(
        StageReadyGate(root_, "release\nworker-node-a", "worker-node-a"),
        std::runtime_error)
        << paths[index];
  }
}

TEST_F(StageReadyGateTest, DifferentUidCanReadGenerationAndMarker)
{
  if (geteuid() != 0)
    GTEST_SKIP() << "different-UID check requires the root PageBroker test image";
  StageReadyGate gate(root_, "release\nworker-node-a", "worker-node-a");
  const auto handle = gate.PublishReady(Identity());
  ASSERT_EQ(chmod(root_.c_str(), 0755), 0);
  const std::string generation = GenerationPath(gate).string();
  const std::string marker = MarkerPath(gate, handle).string();
  const pid_t child = fork();
  ASSERT_GE(child, 0);
  if (child == 0) {
    if (setgroups(0, nullptr) != 0 || setgid(65534) != 0 ||
        setuid(65534) != 0)
      _exit(10);
    for (const auto& path : {generation, marker}) {
      const int descriptor = open(path.c_str(), O_RDONLY | O_CLOEXEC);
      if (descriptor < 0)
        _exit(11);
      char byte = 0;
      if (read(descriptor, &byte, 1) != 1)
        _exit(12);
      close(descriptor);
    }
    _exit(0);
  }
  int status = 0;
  ASSERT_EQ(waitpid(child, &status, 0), child);
  ASSERT_TRUE(WIFEXITED(status));
  EXPECT_EQ(WEXITSTATUS(status), 0);
}

TEST_F(StageReadyGateTest,
       TransitionRequiresExactTransactionStageRequestAndGeneration)
{
  StageReadyGate gate(root_, "release\nworker-node-a", "worker-node-a");
  const auto handle = gate.PublishReady(Identity());
  auto wrong_transaction = handle;
  wrong_transaction.transaction_id = "other";
  EXPECT_THROW(gate.Transition(wrong_transaction, StageReadyState::COMMITTED),
               std::runtime_error);
  auto wrong_stage_request = handle;
  wrong_stage_request.stage_request_id = "other";
  EXPECT_THROW(
      gate.Transition(wrong_stage_request, StageReadyState::COMMITTED),
      std::runtime_error);
  auto wrong_generation = handle;
  wrong_generation.pagebroker_generation = std::string(32, 'c');
  EXPECT_THROW(gate.Transition(wrong_generation, StageReadyState::ABORTED),
               std::runtime_error);
  gate.Transition(handle, StageReadyState::COMMITTED);
  gate.Transition(handle, StageReadyState::COMMITTED);
  EXPECT_THROW(gate.Transition(handle, StageReadyState::ABORTED),
               std::runtime_error);
}

TEST_F(StageReadyGateTest, RetentionNeverUnlinksReadyAndReapsOldTerminal)
{
  StageReadyGate gate(root_, "release\nworker-node-a", "worker-node-a");
  const auto ready = gate.PublishReady(Identity("ready"));
  auto other = Identity("terminal");
  other.destination_container = "engine";
  const auto terminal = gate.PublishReady(other);
  gate.Transition(terminal, StageReadyState::EXPIRED);

  const auto old = std::chrono::system_clock::now() - std::chrono::hours(2);
  const auto old_time = std::chrono::system_clock::to_time_t(old);
  const timespec times[2] = {{old_time, 0}, {old_time, 0}};
  ASSERT_EQ(utimensat(AT_FDCWD, MarkerPath(gate, terminal).c_str(), times, 0), 0);
  EXPECT_EQ(gate.ReapRetainedMarkers(std::chrono::system_clock::now(),
                                     std::chrono::minutes(65)),
            1);
  EXPECT_TRUE(fs::exists(MarkerPath(gate, ready)));
  EXPECT_FALSE(fs::exists(MarkerPath(gate, terminal)));
}

TEST_F(StageReadyGateTest, RetentionRetriesFailedUnlinkWithoutTouchingReady)
{
  bool fail_unlink = true;
  StageReadyGate gate(
      root_, "release\nworker-node-a", "worker-node-a",
      [&](const std::string& operation) {
        if (operation == "reap_marker_unlink" && fail_unlink) {
          fail_unlink = false;
          return EIO;
        }
        return 0;
      });
  const auto ready = gate.PublishReady(Identity("ready"));
  auto terminal_identity = Identity("terminal");
  terminal_identity.destination_container = "engine";
  const auto terminal = gate.PublishReady(terminal_identity);
  gate.Transition(terminal, StageReadyState::ABORTED);
  const auto old = std::chrono::system_clock::now() - std::chrono::hours(2);
  const auto old_time = std::chrono::system_clock::to_time_t(old);
  const timespec times[2] = {{old_time, 0}, {old_time, 0}};
  ASSERT_EQ(utimensat(AT_FDCWD, MarkerPath(gate, terminal).c_str(), times, 0), 0);

  EXPECT_THROW(gate.ReapRetainedMarkers(std::chrono::system_clock::now(),
                                        std::chrono::hours(1)),
               std::system_error);
  EXPECT_TRUE(fs::exists(MarkerPath(gate, terminal)));
  EXPECT_EQ(gate.ReapRetainedMarkers(std::chrono::system_clock::now(),
                                     std::chrono::hours(1)),
            1);
  EXPECT_TRUE(fs::exists(MarkerPath(gate, ready)));
  EXPECT_FALSE(fs::exists(MarkerPath(gate, terminal)));
}

TEST_F(StageReadyGateTest,
       RetentionRetriesDirectorySyncAfterMarkerWasAlreadyUnlinked)
{
  size_t sync_attempts = 0;
  StageReadyGate gate(
      root_, "release\nworker-node-a", "worker-node-a",
      [&](const std::string& operation) {
        if (operation != "reap_marker_directory_fsync")
          return 0;
        ++sync_attempts;
        return sync_attempts == 1 ? EIO : 0;
      });
  const auto terminal = gate.PublishReady(Identity());
  gate.Transition(terminal, StageReadyState::COMMITTED);
  const auto old = std::chrono::system_clock::now() - std::chrono::hours(2);
  const auto old_time = std::chrono::system_clock::to_time_t(old);
  const timespec times[2] = {{old_time, 0}, {old_time, 0}};
  ASSERT_EQ(utimensat(AT_FDCWD, MarkerPath(gate, terminal).c_str(), times, 0), 0);

  EXPECT_THROW(gate.ReapRetainedMarkers(std::chrono::system_clock::now(),
                                        std::chrono::hours(1)),
               std::system_error);
  EXPECT_FALSE(fs::exists(MarkerPath(gate, terminal)));
  EXPECT_EQ(sync_attempts, 1u);
  EXPECT_EQ(gate.ReapRetainedMarkers(std::chrono::system_clock::now(),
                                     std::chrono::hours(1)),
            0);
  EXPECT_EQ(sync_attempts, 2u);
  EXPECT_EQ(gate.ReapRetainedMarkers(std::chrono::system_clock::now(),
                                     std::chrono::hours(1)),
            0);
  EXPECT_EQ(sync_attempts, 2u);
}

TEST_F(StageReadyGateTest, ShutdownClosesEveryReadyMarkerAndRejectsActivation)
{
  StageReadyGate gate(root_, "release\nworker-node-a", "worker-node-a");
  const auto ready = gate.PublishReady(Identity("ready"));
  auto committed_identity = Identity("committed");
  committed_identity.destination_container = "engine";
  const auto committed = gate.PublishReady(committed_identity);
  gate.Transition(committed, StageReadyState::COMMITTED);

  gate.BeginShutdown();
  EXPECT_NE(Read(MarkerPath(gate, ready)).find(
                "\"state\":\"coordinator_lost\""),
            std::string::npos);
  EXPECT_NE(Read(MarkerPath(gate, committed)).find(
                "\"state\":\"committed\""),
            std::string::npos);
  auto later = Identity("later");
  later.destination_container = "later";
  EXPECT_THROW(gate.PublishReady(later), std::runtime_error);
  EXPECT_NO_THROW(gate.BeginShutdown());
}

TEST_F(StageReadyGateTest, RestartRejectsCorruptMarker)
{
  StageReadyHandle handle;
  std::string owner_id;
  {
    StageReadyGate gate(root_, "release\nworker-node-a", "worker-node-a");
    handle = gate.PublishReady(Identity());
    owner_id = gate.owner_id();
  }
  const auto path = root_ / ".pagebroker-stage-ready" / "v1" / "owners" /
                    owner_id / "markers" / (handle.operation_id + ".json");
  std::ofstream(path, std::ios::trunc) << "{not-json";
  EXPECT_THROW(
      StageReadyGate(root_, "release\nworker-node-a", "worker-node-a"),
      std::runtime_error);
}

TEST_F(StageReadyGateTest, RestartRejectsNonIntegerAndDuplicateSchema)
{
  for (const std::string schema : {"1.0", "1e0", "1,\"schema_version\":1"}) {
    SCOPED_TRACE(schema);
    const auto iteration_root = root_ / ("schema-" + schema);
    fs::create_directories(iteration_root);
    StageReadyHandle handle;
    fs::path marker;
    {
      StageReadyGate gate(iteration_root, "release\nworker-node-a",
                          "worker-node-a");
      handle = gate.PublishReady(Identity());
      marker = iteration_root / ".pagebroker-stage-ready" / "v1" / "owners" /
               gate.owner_id() / "markers" / (handle.operation_id + ".json");
    }
    auto payload = Read(marker);
    const auto value = payload.find("\"schema_version\":1");
    ASSERT_NE(value, std::string::npos);
    payload.replace(value, std::string("\"schema_version\":1").size(),
                    "\"schema_version\":" + schema);
    std::ofstream(marker, std::ios::trunc) << payload;
    EXPECT_THROW(StageReadyGate(iteration_root, "release\nworker-node-a",
                                "worker-node-a"),
                 std::runtime_error);
  }
}

TEST_F(StageReadyGateTest, RejectsSymlinkedIntermediateAndMarker)
{
  const auto redirect = root_ / "redirect";
  fs::create_directories(redirect);
  fs::create_directory_symlink(
      redirect, root_ / ".pagebroker-stage-ready");
  EXPECT_THROW(
      StageReadyGate(root_, "release\nworker-node-a", "worker-node-a"),
      fs::filesystem_error);
  EXPECT_TRUE(fs::is_empty(redirect));

  fs::remove(root_ / ".pagebroker-stage-ready");
  StageReadyHandle handle;
  std::string owner_id;
  {
    StageReadyGate gate(root_, "release\nworker-node-a", "worker-node-a");
    handle = gate.PublishReady(Identity());
    owner_id = gate.owner_id();
  }
  const auto marker = root_ / ".pagebroker-stage-ready" / "v1" / "owners" /
                      owner_id / "markers" /
                      (handle.operation_id + ".json");
  fs::remove(marker);
  fs::create_symlink(root_ / "outside", marker);
  EXPECT_THROW(
      StageReadyGate(root_, "release\nworker-node-a", "worker-node-a"),
      fs::filesystem_error);
}

TEST_F(StageReadyGateTest, Enforces253ByteIdentityContractForEveryField)
{
  StageReadyGate gate(root_, "release\nworker-node-a", "worker-node-a");
  const std::string accepted(253, 'a');
  const std::string too_long(254, 'a');
  auto identity = Identity();
  identity.pod_uid = accepted;
  identity.destination_container = accepted;
  identity.content_uid = accepted;
  identity.source_container = accepted;
  identity.container_id = accepted;
  identity.transaction_id = accepted;
  identity.stage_request_id = accepted;
  EXPECT_NO_THROW(gate.PublishReady(identity));

  const auto rejects = [&](auto member) {
    auto invalid = Identity();
    invalid.*member = too_long;
    EXPECT_THROW(gate.PublishReady(invalid), std::invalid_argument);
    invalid = Identity();
    invalid.*member = "bad\nvalue";
    EXPECT_THROW(gate.PublishReady(invalid), std::invalid_argument);
    invalid = Identity();
    invalid.*member = std::string("\xc3\x28", 2);
    EXPECT_THROW(gate.PublishReady(invalid), std::invalid_argument);
  };
  rejects(&StageReadyIdentity::node_name);
  rejects(&StageReadyIdentity::pod_uid);
  rejects(&StageReadyIdentity::destination_container);
  rejects(&StageReadyIdentity::content_uid);
  rejects(&StageReadyIdentity::source_container);
  rejects(&StageReadyIdentity::container_id);
  rejects(&StageReadyIdentity::transaction_id);
  rejects(&StageReadyIdentity::stage_request_id);
}

}  // namespace
