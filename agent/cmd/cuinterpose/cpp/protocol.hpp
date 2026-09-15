// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <array>
#include <chrono>
#include <cstdint>
#include <optional>
#include <span>
#include <string>
#include <variant>
#include <vector>
#include <nlohmann/json.hpp>

namespace cuinterpose {
using Json = nlohmann::json;
constexpr unsigned protocol_version = 4;
constexpr size_t max_bytes = 32 * 1024 * 1024;
constexpr size_t max_records = 4096;
constexpr size_t max_access = 32;
constexpr size_t max_ticket_bytes = 4096;

struct Id {
    std::array<uint8_t, 16> bytes{};
    auto operator<=>(const Id&) const = default;
    static Id random();
    static Id parse(const std::string&);
};
void to_json(Json&, const Id&);
void from_json(const Json&, Id&);

enum class Operation {
    PrepareMulticast, SaveAllocations, PrepareUnicast, LoadAllocations,
    RestoreUnicast, RestoreMulticastCreators, RestoreMulticastImporters,
    RestoreMulticastDevices, RestoreMulticastBindings
};
enum class ResourceKind { Unicast, Multicast };
enum class BindingVersion { V1, V2 };
void to_json(Json&, Operation);
void from_json(const Json&, Operation&);
void to_json(Json&, ResourceKind);
void from_json(const Json&, ResourceKind&);
void to_json(Json&, BindingVersion);
void from_json(const Json&, BindingVersion&);
std::chrono::seconds timeout(std::optional<Operation> = {});

struct AccessRecord {
    int32_t location_type{}, location_id{};
    uint64_t flags{};
    auto operator<=>(const AccessRecord&) const = default;
};
struct MemberRange {
    Id allocation;
    uint64_t offset{};
    bool operator==(const MemberRange&) const = default;
};
struct AddressBinding {
    uint64_t address{};
    std::optional<MemberRange> tracked_member;
};
using BindingSource = std::variant<MemberRange, AddressBinding>;
struct AllocationRecord {
    Id id;
    bool creator{}, content{};
    uint64_t size{};
    int32_t allocation_type{};
    uint32_t handle_types{};
    int32_t location_type{}, location_id{};
    uint32_t handles{};
};
struct MappingRecord {
    Id id;
    bool creator{};
    uint64_t address{}, size{}, offset{};
    std::vector<AccessRecord> access;
};
struct MulticastRecord {
    Id id, creator;
    bool owned{};
    uint64_t size{};
    uint32_t handles{};
    uint64_t handle_types{}, flags{};
    uint32_t devices{};
};
struct DeviceRecord { Id id; int32_t device{}; };
struct BindingRecord {
    Id id;
    BindingSource source;
    uint64_t size{}, offset{}, flags{};
    BindingVersion version{};
    int32_t device{};
};
struct MulticastMappingRecord {
    Id id;
    uint64_t address{}, size{}, offset{}, flags{};
    std::vector<AccessRecord> access;
};
using Record = std::variant<AllocationRecord, MappingRecord, MulticastRecord,
                            DeviceRecord, BindingRecord, MulticastMappingRecord>;
struct Participant { Id id; std::vector<Record> records; };
struct MulticastResource {
    uint32_t devices{};
    uint64_t size{}, handle_types{}, flags{};
    bool operator==(const MulticastResource&) const = default;
};
using Resource = std::variant<std::monostate, MulticastResource>;
struct Ticket {
    Id creator, allocation;
    std::string endpoint;
    Resource resource;
    bool operator==(const Ticket&) const = default;
    void validate() const;
};

struct Handshake {};
struct Inspect { Id participant; };
struct Execute { Id participant; Operation operation; };
struct Export { Id participant; ResourceKind resource; Id allocation; };
using Request = std::variant<Handshake, Inspect, Execute, Export>;
struct Inspection {
    std::vector<Record> records;
    uint64_t live_raw_imports{}, unsupported_creations{};
};
struct Completed { Operation operation; uint64_t bytes{}; uint32_t copy_us{}; };
struct Exported { ResourceKind resource; Id allocation; };
using Reply = std::variant<Handshake, Inspection, Completed, Exported>;
struct Failure { std::string message; };
struct Response { Id participant; std::variant<Reply, Failure> result; };

NLOHMANN_DEFINE_TYPE_NON_INTRUSIVE(AccessRecord, location_type, location_id, flags)
NLOHMANN_DEFINE_TYPE_NON_INTRUSIVE(MemberRange, allocation, offset)
NLOHMANN_DEFINE_TYPE_NON_INTRUSIVE(AllocationRecord, id, creator, content, size,
    allocation_type, handle_types, location_type, location_id, handles)
NLOHMANN_DEFINE_TYPE_NON_INTRUSIVE(MappingRecord, id, creator, address, size, offset, access)
NLOHMANN_DEFINE_TYPE_NON_INTRUSIVE(MulticastRecord, id, creator, owned, size,
    handles, handle_types, flags, devices)
NLOHMANN_DEFINE_TYPE_NON_INTRUSIVE(DeviceRecord, id, device)
void to_json(Json&, const BindingSource&);
void from_json(const Json&, BindingSource&);
NLOHMANN_DEFINE_TYPE_NON_INTRUSIVE(BindingRecord, id, source, size, offset, flags, version, device)
NLOHMANN_DEFINE_TYPE_NON_INTRUSIVE(MulticastMappingRecord, id, address, size, offset, flags, access)
void to_json(Json&, const Record&);
void from_json(const Json&, Record&);
NLOHMANN_DEFINE_TYPE_NON_INTRUSIVE(Participant, id, records)
void to_json(Json&, const Resource&);
void from_json(const Json&, Resource&);
NLOHMANN_DEFINE_TYPE_NON_INTRUSIVE(Ticket, creator, allocation, endpoint, resource)
void to_json(Json&, const Request&);
void from_json(const Json&, Request&);
NLOHMANN_DEFINE_TYPE_NON_INTRUSIVE(Inspection, records, live_raw_imports, unsupported_creations)
NLOHMANN_DEFINE_TYPE_NON_INTRUSIVE(Completed, operation, bytes, copy_us)
NLOHMANN_DEFINE_TYPE_NON_INTRUSIVE(Exported, resource, allocation)
void to_json(Json&, const Response&);
void from_json(const Json&, Response&);

// Only codec adapters handle the externally tagged v4 representation.
// Domain code works with records and variants, never JSON field lookups.
std::vector<uint8_t> encode(const Json&);
Json decode(std::span<const uint8_t>);

class Fd {
public:
    explicit Fd(int value = -1) noexcept : value_(value) {}
    ~Fd();
    Fd(Fd&&) noexcept;
    Fd& operator=(Fd&&) noexcept;
    Fd(const Fd&) = delete;
    Fd& operator=(const Fd&) = delete;
    int get() const noexcept { return value_; }
    int release() noexcept;
    Fd duplicate() const;
private:
    int value_;
};
struct Received { Json body; Fd descriptor; };
void send(int socket, const Json&, int descriptor = -1);
Received receive(int socket);
Fd connect(const std::string&, std::chrono::seconds);
void write_all(int, std::span<const uint8_t>);
std::vector<uint8_t> read_file(const std::string&);
void write_atomic(const std::string&, std::span<const uint8_t>);
void validate_endpoint(const std::string&);
Fd export_ticket(const Ticket&);
std::optional<Ticket> read_ticket(int);
}
