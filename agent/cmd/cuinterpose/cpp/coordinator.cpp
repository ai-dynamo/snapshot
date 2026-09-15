// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#include "protocol.hpp"
#include <CLI/CLI.hpp>
#include <algorithm>
#include <future>
#include <iostream>
#include <map>
#include <set>

namespace cuinterpose {
namespace {
using Clock = std::chrono::steady_clock;
struct CanonicalAllocation { Id creator; uint64_t size; bool content, anchor; };
struct Group {
    Id creator;
    uint64_t size, handle_types, flags;
    uint32_t count, creators{};
    std::map<int32_t, bool> devices;
};
void require(bool condition, const char* message) {
    if (!condition) throw std::runtime_error(message);
}
bool fits(uint64_t offset, uint64_t size, uint64_t extent) {
    return size && offset <= extent && size <= extent-offset;
}
std::vector<CanonicalAllocation> validate(const std::vector<Participant>& participants) {
    require(!participants.empty(), "topology has no participants");
    std::set<Id> identities;
    std::map<Id, CanonicalAllocation> allocations;
    std::map<Id, Group> groups;
    for (auto& participant : participants) {
        require(identities.insert(participant.id).second, "duplicate participant identity");
        require(participant.records.size() <= max_records, "too many records");
        for (auto& record : participant.records) {
            if (auto allocation = std::get_if<AllocationRecord>(&record)) {
                if (allocation->creator) {
                    require(allocation->handle_types == 1 && allocation->size, "invalid allocation creator");
                    require(allocations.emplace(allocation->id, CanonicalAllocation{
                        participant.id, allocation->size, allocation->content, allocation->handles != 0}).second,
                        "duplicate allocation creator");
                } else require(!allocation->content, "allocation content flag on importer");
            } else if (auto object = std::get_if<MulticastRecord>(&record)) {
                require(object->handle_types == 1 && object->size && object->devices, "invalid multicast properties");
                auto [found, inserted] = groups.emplace(object->id, Group{
                    object->creator, object->size, object->handle_types, object->flags, object->devices, 0, {}});
                auto& group = found->second;
                require(inserted || (group.creator == object->creator && group.handle_types == object->handle_types &&
                    group.flags == object->flags && group.count == object->devices), "inconsistent multicast properties");
                group.size = std::max(group.size, object->size);
                if (object->owned) {
                    require(participant.id == object->creator, "invalid multicast creator");
                    ++group.creators;
                }
            }
        }
    }
    for (auto& participant : participants)
        for (auto& record : participant.records)
            if (auto device = std::get_if<DeviceRecord>(&record))
                require(groups.at(device->id).devices.emplace(device->device, false).second, "duplicate multicast device");
    for (auto& participant : participants) {
        for (auto& record : participant.records) {
            std::visit([&](auto& item) {
                using T = std::decay_t<decltype(item)>;
                if constexpr (std::is_same_v<T, AllocationRecord>) {
                    require(allocations.contains(item.id), "missing creator");
                } else if constexpr (std::is_same_v<T, MappingRecord>) {
                    auto& allocation = allocations.at(item.id);
                    require(item.address && item.access.size() <= max_access &&
                        fits(item.offset, item.size, allocation.size), "invalid mapping or mapping out of bounds");
                    allocation.anchor |= item.creator && allocation.creator == participant.id;
                } else if constexpr (std::is_same_v<T, BindingRecord>) {
                    auto& group = groups.at(item.id);
                    require(fits(item.offset, item.size, group.size), "invalid multicast binding");
                    std::optional<MemberRange> member;
                    if (auto memory = std::get_if<MemberRange>(&item.source)) member = *memory;
                    else {
                        auto& address = std::get<AddressBinding>(item.source);
                        require(address.address != 0, "invalid multicast member address");
                        member = address.tracked_member;
                    }
                    if (member) require(fits(member->offset, item.size, allocations.at(member->allocation).size),
                        "multicast binding out of member bounds");
                    group.devices.at(item.device) = true;
                } else if constexpr (std::is_same_v<T, MulticastMappingRecord>) {
                    require(item.address && item.access.size() <= max_access &&
                        fits(item.offset, item.size, groups.at(item.id).size), "invalid multicast mapping");
                }
            }, record);
        }
    }
    std::vector<CanonicalAllocation> result;
    for (auto& [id, allocation] : allocations) {
        (void)id; require(allocation.anchor, "missing creator anchor"); result.push_back(allocation);
    }
    for (auto& [id, group] : groups) {
        (void)id;
        require(group.creators == 1, "multicast group must have exactly one creator");
        require(group.devices.size() == group.count, "incomplete multicast device group");
        require(std::ranges::all_of(group.devices, [](auto& item) { return item.second; }), "incomplete multicast binding group");
    }
    return result;
}
struct Peer {
    std::string endpoint;
    Id id;
    Response exchange(const Request& request) const {
        std::optional<Operation> operation;
        if (auto execute = std::get_if<Execute>(&request)) operation = execute->operation;
        try {
            auto socket = connect(endpoint, timeout(operation));
            send(socket.get(), request);
            auto received = receive(socket.get());
            require(received.descriptor.get() < 0, "unexpected coordinator descriptor");
            return received.body.get<Response>();
        } catch (const std::exception& error) {
            throw std::runtime_error(endpoint + ": " + Json(request).dump() + ": " + error.what());
        }
    }
    void identify() {
        auto response = exchange(Handshake{});
        if (auto failure = std::get_if<Failure>(&response.result)) throw std::runtime_error(failure->message);
        require(std::holds_alternative<Handshake>(std::get<Reply>(response.result)), "unexpected handshake response");
        id = response.participant;
    }
    Reply request(const Request& request) const {
        auto response = exchange(request);
        require(response.participant == id, "participant changed");
        if (auto failure = std::get_if<Failure>(&response.result)) throw std::runtime_error(failure->message);
        return std::get<Reply>(std::move(response.result));
    }
};
struct InspectResult {
    std::vector<Participant> participants;
    uint64_t raw{}, unsupported{};
    size_t records{};
};
InspectResult inspect(const std::vector<Peer>& peers) {
    InspectResult result;
    for (auto& peer : peers) {
        auto inspection = std::get<Inspection>(peer.request(Inspect{peer.id}));
        result.raw += inspection.live_raw_imports;
        result.unsupported += inspection.unsupported_creations;
        result.records += inspection.records.size();
        result.participants.push_back({peer.id, std::move(inspection.records)});
    }
    return result;
}
void report(const std::string& phase, Clock::time_point start, size_t participants, Json fields = Json::object()) {
    fields["phase"] = phase;
    fields["status"] = "ok";
    fields["elapsed_ms"] = std::chrono::duration<double, std::milli>(Clock::now()-start).count();
    fields["participants"] = participants;
    std::cout << fields.dump() << '\n' << std::flush;
    if (!std::cout) throw std::runtime_error("report output failed");
}
uint32_t command_all(const std::vector<Peer>& peers, Operation operation, const std::vector<CanonicalAllocation>& allocations = {}) {
    std::vector<std::future<uint32_t>> jobs;
    jobs.reserve(peers.size());
    // One concurrent exchange per participant is mandatory for collectives.
    // Every future is joined before advancing, including after a peer fails.
    for (auto& peer : peers) {
        uint64_t bytes = 0;
        for (auto& allocation : allocations) if (allocation.content && allocation.creator == peer.id) {
            require(allocation.size <= UINT64_MAX-bytes, "allocation size overflow");
            bytes += allocation.size;
        }
        jobs.push_back(std::async(std::launch::async, [&peer, operation, bytes] {
            auto result = std::get<Completed>(peer.request(Execute{peer.id, operation}));
            require(result.operation == operation && result.bytes == bytes, "unexpected lifecycle result or transfer size");
            return result.copy_us;
        }));
    }
    uint32_t longest = 0;
    std::exception_ptr error;
    for (auto& job : jobs) {
        try { longest = std::max(longest, job.get()); }
        catch (...) { if (!error) error = std::current_exception(); }
    }
    if (error) std::rethrow_exception(error);
    return longest;
}
void transfer(const std::vector<Peer>& peers, Operation operation, const std::vector<CanonicalAllocation>& allocations) {
    auto start = Clock::now();
    uint32_t copy_us = command_all(peers, operation, allocations);
    size_t count = 0;
    uint64_t bytes = 0;
    for (auto& allocation : allocations) if (allocation.content) {
        require(allocation.size <= UINT64_MAX-bytes, "allocation size overflow");
        ++count; bytes += allocation.size;
    }
    report(Json(operation).get<std::string>(), start, peers.size(), {
        {"allocation_count", count}, {"allocation_bytes", bytes},
        {"gb_per_s", bytes/1e9/std::chrono::duration<double>(Clock::now()-start).count()},
        {"copy_gb_per_s", copy_us ? bytes/1000.0/copy_us : 0.0}});
}
void normalize(std::vector<Participant>& participants) {
    std::ranges::sort(participants, {}, &Participant::id);
    for (auto& participant : participants)
        std::ranges::sort(participant.records, [](const Record& a, const Record& b) {
            return Json(a) < Json(b);
        });
}
int run(int argc, char** argv) {
    CLI::App arguments{"Reconstruct shared CUDA memory"};
    arguments.name("cuinterpose-coordinator");
    bool prepare = false, restore = false;
    std::string proc_root, checkpoint_dir, control_dir;
    std::vector<int> processes;
    auto prepare_option = arguments.add_flag("--prepare", prepare);
    auto restore_option = arguments.add_flag("--restore", restore);
    prepare_option->excludes(restore_option);
    arguments.add_option("--proc-root", proc_root)->required();
    arguments.add_option("--checkpoint-dir", checkpoint_dir)->required();
    arguments.add_option("--control-dir", control_dir)->required();
    arguments.add_option("--process", processes)->required()->type_size(2)->expected(1, -1);
    try { arguments.parse(argc, argv); }
    catch (const CLI::ParseError& error) { return arguments.exit(error); }
    require(prepare != restore, "exactly one of --prepare and --restore is required");
    validate_endpoint(control_dir + "/cuinterpose-1.sock");
    std::string path = checkpoint_dir + "/cuinterpose.state";
    std::vector<Participant> expected;
    if (restore) expected = decode(read_file(path)).get<std::vector<Participant>>();
    auto start = Clock::now();
    std::vector<Peer> peers;
    require(processes.size() % 2 == 0, "--process requires host and namespace PIDs");
    for (size_t index = 0; index < processes.size(); index += 2) {
        int observed = processes[index], pid = processes[index + 1];
        require(observed > 0 && pid > 0, "process IDs must be positive");
        auto endpoint = (proc_root.empty() ? "" : proc_root + "/" + std::to_string(observed) + "/root") +
            control_dir + "/cuinterpose-" + std::to_string(pid) + ".sock";
        validate_endpoint(endpoint);
        Peer peer{std::move(endpoint), {}};
        peer.identify();
        peers.push_back(std::move(peer));
    }
    if (prepare) {
        auto inspection = inspect(peers);
        report("inspect", start, peers.size(), {{"records", inspection.records},
            {"live_raw_imports", inspection.raw}, {"unsupported_exportable_creations", inspection.unsupported}});
        require(!inspection.raw, "prepare refused: live raw imports");
        require(!inspection.unsupported, "prepare refused: unsupported exportable handle types");
        start = Clock::now();
        auto allocations = validate(inspection.participants);
        report("validate", start, peers.size());
        start = Clock::now();
        command_all(peers, Operation::PrepareMulticast);
        report("prepare_multicast", start, peers.size());
        transfer(peers, Operation::SaveAllocations, allocations);
        start = Clock::now();
        command_all(peers, Operation::PrepareUnicast);
        report("prepare_unicast", start, peers.size());
        start = Clock::now();
        normalize(inspection.participants);
        write_atomic(path, encode(inspection.participants));
        report("state_write", start, peers.size());
    } else {
        std::ranges::sort(peers, {}, &Peer::id);
        normalize(expected);
        require(peers.size() == expected.size(), "restored processes do not match participants");
        for (size_t i = 0; i < peers.size(); ++i) require(peers[i].id == expected[i].id, "restored participant mismatch");
        auto allocations = validate(expected);
        report("handshake", start, peers.size());
        transfer(peers, Operation::LoadAllocations, allocations);
        start = Clock::now();
        command_all(peers, Operation::RestoreUnicast);
        report("restore_unicast", start, peers.size());
        start = Clock::now();
        for (auto operation : {Operation::RestoreMulticastCreators, Operation::RestoreMulticastImporters,
             Operation::RestoreMulticastDevices, Operation::RestoreMulticastBindings}) {
            command_all(peers, operation);
        }
        report("restore_multicast", start, peers.size());
        start = Clock::now();
        for (auto& peer : peers) {
            auto id = peer.id;
            peer.identify();
            require(peer.id == id, "restored participant changed");
        }
        auto actual = inspect(peers);
        require(!actual.raw && !actual.unsupported, "restored unsupported CUDA state");
        validate(actual.participants);
        normalize(actual.participants);
        require(Json(actual.participants) == Json(expected), "restored topology does not match checkpoint");
        report("validate", start, peers.size());
    }
    return 0;
}
}
}
int main(int argc, char** argv) {
    try { return cuinterpose::run(argc, argv); }
    catch (const std::exception& error) {
        std::cerr << "cuinterpose-coordinator: " << error.what() << '\n';
        return 1;
    }
    catch (...) {
        std::cerr << "cuinterpose-coordinator: unexpected exception\n";
        return 1;
    }
}
