// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0
#pragma once
#include <functional>

#include "driver.hpp"
#include <atomic>
#include <condition_variable>
#include <deque>
#include <map>
#include <memory>
#include <mutex>
#include <set>
#include <thread>

namespace cuinterpose {
constexpr uint64_t handle_tag = 0xd94d000000000000ULL, handle_mask = 0xffff000000000000ULL;
struct Allocation {
    Id id;
    Ticket ticket;
    std::optional<uint64_t> driver;
    size_t size{};
    AllocationProp properties{};
    bool creator{}, shared{}, checkpointed{}, host_checkpointed{};
    void* context{};
    size_t pins{};
};
struct Mapping {
    Id id;
    uint64_t address{};
    size_t size{}, offset{};
    std::vector<::Access> access;
    bool unknown{}, checkpointed{};
    uint64_t flags{};
};
struct Multicast {
    Id id;
    Ticket ticket;
    std::optional<uint64_t> driver;
    MulticastProp properties{};
    bool creator{}, shared{}, checkpointed{};
    void* context{};
    size_t extent{};
    size_t inflight{};
    std::set<int32_t> devices;
    std::vector<BindingRecord> bindings;
};
enum class Phase {
    Active, MulticastPrepared, AllocationsSaved, UnicastPrepared,
    AllocationsLoaded, UnicastRestored, MulticastCreatorsRestored,
    MulticastImportersRestored, MulticastDevicesRestored, ReconstructingMulticast
};
struct Transfer { uint64_t bytes{}; uint32_t copy_us{}; };
struct Content {
    Id id;
    std::optional<uint64_t> driver;
    size_t size{};
    AllocationProp properties{};
    void* context{};
};
class Context {
    void* previous_{};
    std::optional<int32_t> primary_;
    bool changed_{};
public:
    Context(void*, int32_t);
    int leave() noexcept;
    template<class F> static void run(void* context, int32_t device, F&& body) {
        Context scope(context, device);
        try { body(); }
        catch (...) { scope.leave(); throw; }
        check(scope.leave());
    }
};
struct Arena {
    void* base{};
    size_t size{};
    void* context{};
    int32_t device{};
    std::map<Id, size_t> offsets;
    static std::pair<std::unique_ptr<Arena>, uint32_t> save(const std::vector<Content>&);
    uint32_t load(std::vector<Content>&) const;
    uint32_t copy(const std::vector<Content>&, bool) const;
    int release() noexcept;
};
class ExportCache {
public:
    using Key = std::pair<ResourceKind, Id>;
    struct Entry { Fd descriptor; size_t transfers{}; bool retiring{}; };
    std::mutex mutex;
    std::condition_variable drained;
    std::map<Key, Entry> entries;
    size_t transfers{};
    bool draining{};
    struct Lease {
        ExportCache* cache;
        Key key;
        Fd descriptor;
        ~Lease();
        Lease(ExportCache*, Key, Fd);
        Lease(const Lease&) = delete;
    };
    std::unique_ptr<Lease> acquire(Key);
    void replace(Key, Fd = Fd{});
    void clear();
private:
    std::mutex mutations_;
};
struct State {
    Id identity;
    std::string endpoint;
    std::map<Id, Allocation> allocations;
    std::map<Id, Multicast> multicasts;
    std::map<uint64_t, Id> handles;
    std::map<uint64_t, Mapping> mappings;
    std::map<uint64_t, uint32_t> raw;
    std::vector<uint64_t> unreleased_handles;
    uint64_t unsupported{};
    Phase phase{};
    std::unique_ptr<Arena> arena;
    size_t inflight{};
    std::vector<std::pair<uint64_t, size_t>> pending_maps;
    uint64_t next{1};
    uint64_t mint(Id);
    void settle(Id);
    std::vector<uint64_t> covered(uint64_t, size_t) const;
    std::vector<Record> inspect() const;
    DebugStats stats() const;
    Phase next_phase(Operation) const;
    void validate_lifecycle(Operation) const;
    Transfer lifecycle(Operation);
    void remap(bool);
};
struct Generation {
    State state;
    std::mutex mutex;
    ExportCache cache;
};
extern std::atomic<bool> failed;
extern std::atomic<Generation*> generation;
int initialize() noexcept;
std::unique_lock<std::mutex> lock_state(bool active = true);
void* current_context();
void start_control(Generation&);
void fork_prepare() noexcept;
void fork_parent() noexcept;
void fork_child() noexcept;

// Socket registration is separate from CUDA state so peer exports never wait
// behind a driver call. Registry mutation and descriptor close are one action.
class Socket {
    Fd fd_;
public:
    explicit Socket(Fd);
    ~Socket();
    Socket(Socket&&) = delete;
    int get() const noexcept { return fd_.get(); }
};
std::unique_ptr<Socket> open_socket(const std::function<Fd()>&);
Fd request_export(const Ticket&);

namespace backend {
#define DECLARE(name, parameters, arguments) int name parameters;
CUINTERPOSE_MEMORY_API(DECLARE)
#undef DECLARE
}
void prepare_multicast(State&);
void restore_multicast(Operation, std::unique_lock<std::mutex>&);
int map_multicast(Id, uint64_t, size_t, size_t, uint64_t, std::unique_lock<std::mutex>&);
int import_multicast(uint64_t*, Ticket, std::unique_lock<std::mutex>&);
}
