// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include <gtest/gtest.h>

#include <filesystem>
#include <fstream>
#include <system_error>

#include "tests/temporary_directory.hpp"
#include "transfer/engine/transfer_engine.hpp"
#include "transfer/filesystem/filesystem_storage.hpp"

namespace snapshot::pagebroker {
namespace fs = std::filesystem;

TEST(FilesystemStorageTest, ExclusiveCreationPreservesEveryExistingEntry)
{
  test::TemporaryDirectory root;
  fs::create_directory(root.path() / "directory");
  std::ofstream(root.path() / "directory/marker") << "keep";
  std::ofstream(root.path() / "file") << "keep";
  fs::create_directory_symlink(root.path() / "directory", root.path() / "live");
  fs::create_directory_symlink(root.path() / "missing", root.path() / "dangling");
  for (const auto* name : {"directory", "file", "live", "dangling"}) {
    SCOPED_TRACE(name);
    const auto path = root.path() / name;
    const auto before = fs::symlink_status(path);
    try {
      filesystem_storage::CreateDirectoryExclusive(path, fs::perms::owner_all);
      FAIL() << "existing entry was accepted";
    }
    catch (const TransferError& error) {
      EXPECT_EQ(error.code, Failure::TRANSACTION_CONFLICT);
    }
    EXPECT_EQ(fs::symlink_status(path).type(), before.type());
    EXPECT_EQ(fs::symlink_status(path).permissions(), before.permissions());
  }
  EXPECT_EQ(fs::file_size(root.path() / "directory/marker"), 4u);
  EXPECT_EQ(fs::file_size(root.path() / "file"), 4u);
  EXPECT_EQ(fs::read_symlink(root.path() / "live"), root.path() / "directory");
  EXPECT_EQ(fs::read_symlink(root.path() / "dangling"), root.path() / "missing");
  EXPECT_FALSE(fs::exists(root.path() / "missing"));
}

TEST(FilesystemStorageTest, ExclusiveCreationRespectsRequestedPermissionsAndUmask)
{
  for (const auto permissions : {fs::perms::all, fs::perms::owner_all}) {
    SCOPED_TRACE(static_cast<unsigned>(permissions));
    test::TemporaryDirectory root;
    fs::create_directory(root.path() / "reference");
    const auto expected = fs::status(root.path() / "reference").permissions() & permissions;
    filesystem_storage::CreateDirectoryExclusive(root.path() / "created", permissions);
    EXPECT_TRUE(fs::is_directory(root.path() / "created"));
    EXPECT_EQ(fs::status(root.path() / "created").permissions() & fs::perms::all, expected);
  }
}

TEST(FilesystemStorageTest, ExclusiveCreationPreservesNonConflictErrors)
{
  test::TemporaryDirectory root;
  try {
    filesystem_storage::CreateDirectoryExclusive(root.path() / "missing/child", fs::perms::all);
    FAIL() << "missing parent was accepted";
  }
  catch (const std::system_error& error) {
    EXPECT_EQ(error.code(), std::errc::no_such_file_or_directory);
    EXPECT_EQ(TransferFailureCode(error), Failure::STORAGE_ERROR);
  }
  EXPECT_FALSE(fs::exists(root.path() / "missing"));
}

TEST(FilesystemStorageTest, AppliesOnlyKnownRestorePermissions)
{
  for (bool known : {false, true}) {
    SCOPED_TRACE(known);
    test::TemporaryDirectory root;
    const auto source = root.path() / "source";
    const auto destination = root.path() / "restored";
    std::ofstream(source) << "image";
    const auto file_mode = fs::perms::owner_read | fs::perms::owner_write;
    fs::permissions(source, file_mode);
    fs::create_directory(destination);
    fs::permissions(destination, fs::perms::owner_all);
    fs::create_directory(root.path() / "reference");
    const auto created_mode = fs::status(root.path() / "reference").permissions();

    CheckpointInventory inventory;
    inventory.directories.push_back(CheckpointDirectory{"nested"});
    inventory.files.push_back(CheckpointFile{source.string(), "nested/image", 5});
    const auto root_mode = fs::perms::owner_all | fs::perms::group_read | fs::perms::group_exec;
    if (known) {
      inventory.root_permissions = root_mode;
      inventory.directories.front().permissions = fs::perms::owner_all;
      inventory.files.front().permissions = fs::perms::owner_read;
    }

    filesystem_storage::StageRestore(inventory, destination, {});
    EXPECT_EQ(fs::status(destination).permissions(), known ? root_mode : fs::perms::owner_all);
    EXPECT_EQ(fs::status(destination / "nested").permissions(), known ? fs::perms::owner_all : created_mode);
    EXPECT_EQ(fs::status(destination / "nested/image").permissions(), known ? fs::perms::owner_read : file_mode);
    EXPECT_EQ(fs::file_size(destination / "nested/image"), 5u);
  }
}
}  // namespace snapshot::pagebroker
