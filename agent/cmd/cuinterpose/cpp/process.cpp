// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#include "state.hpp"
#include <algorithm>
#include <sys/mman.h>
#include <unistd.h>

namespace cuinterpose {
Host host{};
std::atomic<bool> failed{};
std::atomic<Generation*> generation{};
namespace {
std::mutex initialization, sockets_mutex;
std::vector<int> sockets;
bool child_generation{};
struct ForkSnapshot {
    std::unique_lock<std::mutex> initialization, state, cache, sockets;
    std::vector<int> descriptors;
};
ForkSnapshot* fork_snapshot{};
}
Socket::Socket(Fd fd) : fd_(std::move(fd)) { sockets.push_back(fd_.get()); }
Socket::~Socket() {
    std::lock_guard lock(sockets_mutex);
    std::erase(sockets, fd_.get());
    fd_ = Fd{};
}
std::unique_ptr<Socket> open_socket(const std::function<Fd()>& open) {
    std::lock_guard lock(sockets_mutex);
    return std::make_unique<Socket>(open());
}
int initialize() noexcept {
    try {
        if (generation.load(std::memory_order_acquire)) return success;
        if (failed.load()) return unknown;
        std::unique_lock lock(initialization, std::try_to_lock);
        if (!lock) return not_initialized;
        if (generation.load()) return success;
        if (failed.load()) return unknown;
        auto next = std::make_unique<Generation>();
        auto& state = next->state;
        auto configured = std::getenv("CUINTERPOSE_PARTICIPANT_ID");
        state.identity = configured && !child_generation && host.origin_pid == ::getpid()
            ? Id::parse(configured) : Id::random();
        auto directory = std::getenv("SNAPSHOT_CONTROL_DIR");
        state.endpoint = std::string(directory ? directory : "/snapshot-control") +
            "/cuinterpose-" + std::to_string(::getpid()) + ".sock";
        validate_endpoint(state.endpoint);
        start_control(*next);
        generation.store(next.release(), std::memory_order_release);
        return success;
    } catch (const std::bad_alloc&) { failed.store(true); return out_of_memory; }
    catch (...) { failed.store(true); return not_initialized; }
}
void fork_prepare() noexcept {
    try {
        auto snapshot = std::make_unique<ForkSnapshot>();
        snapshot->initialization = std::unique_lock(initialization);
        if (auto current = generation.load()) {
            snapshot->state = std::unique_lock(current->mutex);
            snapshot->cache = std::unique_lock(current->cache.mutex);
            current->cache.drained.wait(snapshot->cache, [&] { return current->cache.transfers == 0; });
            for (auto& [key, entry] : current->cache.entries) {
                (void)key; snapshot->descriptors.push_back(entry.descriptor.get());
            }
        }
        snapshot->sockets = std::unique_lock(sockets_mutex);
        snapshot->descriptors.insert(snapshot->descriptors.end(), sockets.begin(), sockets.end());
        fork_snapshot = snapshot.release();
    } catch (...) { ::_exit(127); }
}
void fork_parent() noexcept {
    try { delete std::exchange(fork_snapshot, nullptr); }
    catch (...) { ::_exit(127); }
}
void fork_child() noexcept {
    try {
        auto snapshot = std::exchange(fork_snapshot, nullptr);
        if (!snapshot) return;
        for (int fd : snapshot->descriptors) ::close(fd);
        sockets.clear();
        snapshot->sockets.unlock();
        if (auto current = generation.load()) {
            if (auto& arena = current->state.arena) ::munmap(arena->base, arena->size);
        }
        // The parent's generation, mutexes, and CUDA resources are abandoned,
        // not destroyed or unlocked in the child. Only this fork thread's
        // process-global admission and FD-registry locks may be released.
        snapshot->state.release(); snapshot->cache.release();
        generation.store(nullptr);
        child_generation = true;
        failed.store(false);
        snapshot->initialization.unlock();
    } catch (...) { ::_exit(127); }
}
namespace {
template<class F> int boundary(F&& body) noexcept {
    try {
        if (failed.load()) return not_ready;
        check(initialize());
        return body();
    } catch (const CudaError& error) { return error.code; }
    catch (const std::bad_alloc&) { failed.store(true); return out_of_memory; }
    catch (...) { failed.store(true); return unknown; }
}
#define CALLBACK(name, parameters, arguments) \
 int name parameters noexcept { return boundary([&] { return backend::name arguments; }); }
CUINTERPOSE_MEMORY_API(CALLBACK)
#undef CALLBACK
int ensure_ready() noexcept {
    // initialize() also serves diagnostics and may find an existing generation.
    // CUDA resolver readiness must additionally refuse a poisoned generation.
    return boundary([] { return success; });
}
void debug_stats(DebugStats* out) noexcept {
    try {
        if (!out) return;
        if (failed.load()) { *out = {}; out->phase = 5; return; }
        check(initialize());
        auto lock = lock_state(false);
        *out = generation.load()->state.stats();
    } catch (...) {}
}
const Core api{
    ABI_VERSION, sizeof(Core), debug_stats, fork_prepare, fork_parent, fork_child, ensure_ready,
#define POINTER(name, parameters, arguments) name,
    CUINTERPOSE_MEMORY_API(POINTER)
#undef POINTER
};
}
extern "C" __attribute__((visibility("default")))
int cuinterpose_core_init(const Host* input, const Core** output) noexcept {
    try {
        if (!input || !output || input->version != ABI_VERSION || input->size != sizeof(Host) || !input->resolve)
            return invalid_value;
        if (host.resolve && host.resolve != input->resolve) return invalid_value;
        host = *input;
        check(initialize());
        *output = &api;
        return success;
    } catch (const CudaError& error) { return error.code; }
    catch (const std::bad_alloc&) { failed.store(true); return out_of_memory; }
    catch (...) { failed.store(true); return unknown; }
}
}
