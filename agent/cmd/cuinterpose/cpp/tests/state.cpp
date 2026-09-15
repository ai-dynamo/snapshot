// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#include "../state.hpp"
#include <cassert>
#include <cstring>
#include <iostream>
#include <sys/mman.h>
#include <unistd.h>

extern "C" int cuinterpose_core_init(const Host*, const Core**) noexcept;
using namespace cuinterpose;

namespace {
int switched{}, released{}, registered{}, registrations{};
int get_context(void** out) { *out = reinterpret_cast<void*>(1); return 0; }
int set_context(void*) { ++switched; return 711; }
int retain(void** out, int) { *out = reinterpret_cast<void*>(2); return 0; }
int release(int) { ++released; return 0; }
int register_host(void*, size_t, uint32_t) { ++registered; ++registrations; return 0; }
int unregister_host(void*) { --registered; return 0; }
int create(uint64_t*, size_t, const AllocationProp*, uint64_t) { return 713; }
void* resolve(const char* name) {
#define RESOLVE(symbol, function) if (std::strcmp(name, symbol) == 0) return reinterpret_cast<void*>(function)
    RESOLVE("cuCtxGetCurrent", get_context);
    RESOLVE("cuCtxSetCurrent", set_context);
    RESOLVE("cuDevicePrimaryCtxRetain", retain);
    RESOLVE("cuDevicePrimaryCtxRelease_v2", release);
    RESOLVE("cuMemHostRegister_v2", register_host);
    RESOLVE("cuMemHostUnregister", unregister_host);
    RESOLVE("cuMemCreate", create);
#undef RESOLVE
    return nullptr;
}
}

int main() {
    auto page = static_cast<size_t>(::sysconf(_SC_PAGESIZE));
    auto memory = static_cast<uint8_t*>(::mmap(nullptr, page * 2, PROT_READ | PROT_WRITE,
        MAP_PRIVATE | MAP_ANONYMOUS, -1, 0));
    assert(memory != MAP_FAILED && ::mprotect(memory + page, page, PROT_NONE) == 0);
    auto prefix = reinterpret_cast<uint32_t*>(memory + page - 8);
    for (auto [version, size] : {std::pair{999U, uint32_t(sizeof(Host))}, std::pair{uint32_t(ABI_VERSION), 8U}}) {
        prefix[0] = version; prefix[1] = size;
        const Core* result = nullptr;
        assert(cuinterpose_core_init(reinterpret_cast<Host*>(prefix), &result) == invalid_value);
        assert(!result && !host.resolve);
    }
    assert(::munmap(memory, page * 2) == 0);
    State state;
    struct Transition { Operation operation; Phase before, after; };
    const Transition transitions[] = {
        {Operation::PrepareMulticast, Phase::Active, Phase::MulticastPrepared},
        {Operation::SaveAllocations, Phase::MulticastPrepared, Phase::AllocationsSaved},
        {Operation::PrepareUnicast, Phase::AllocationsSaved, Phase::UnicastPrepared},
        {Operation::LoadAllocations, Phase::UnicastPrepared, Phase::AllocationsLoaded},
        {Operation::RestoreUnicast, Phase::AllocationsLoaded, Phase::UnicastRestored},
        {Operation::RestoreMulticastCreators, Phase::UnicastRestored, Phase::MulticastCreatorsRestored},
        {Operation::RestoreMulticastImporters, Phase::MulticastCreatorsRestored, Phase::MulticastImportersRestored},
        {Operation::RestoreMulticastDevices, Phase::MulticastImportersRestored, Phase::MulticastDevicesRestored},
        {Operation::RestoreMulticastBindings, Phase::MulticastDevicesRestored, Phase::Active},
    };
    for (const auto& transition : transitions) {
        for (auto phase : {Phase::Active, Phase::MulticastPrepared, Phase::AllocationsSaved,
                          Phase::UnicastPrepared, Phase::AllocationsLoaded, Phase::UnicastRestored,
                          Phase::MulticastCreatorsRestored, Phase::MulticastImportersRestored,
                          Phase::MulticastDevicesRestored, Phase::ReconstructingMulticast}) {
            state.phase = phase;
            try {
                auto next = state.next_phase(transition.operation);
                assert(phase == transition.before && next == transition.after);
            } catch (const CudaError& error) {
                assert(phase != transition.before && error.code == not_ready);
            }
        }
    }
    state.phase = Phase::Active;
    for (size_t i = 0; i <= max_records; ++i) state.mappings.emplace(i * 4096, Mapping{});
    try { state.inspect(); assert(false); }
    catch (const CudaError& error) { assert(error.code == not_supported); }

    host = {ABI_VERSION, sizeof(Host), resolve, ::getpid()};
    Context same(reinterpret_cast<void*>(1), 0);
    assert(same.leave() == 0 && switched == 0);
    try { Context primary(nullptr, 0); assert(false); }
    catch (const CudaError& error) { assert(error.code == 711 && released == 1); }
    Arena arena;
    Id id = Id::random();
    arena.base = reinterpret_cast<void*>(0x1000); arena.size = 4096;
    arena.context = reinterpret_cast<void*>(1); arena.offsets.emplace(id, 0);
    AllocationProp properties{};
    properties.kind = properties.handle_types = properties.location.kind = 1;
    std::vector<Content> contents{{id, {}, 4096, properties, arena.context}};
    try { arena.load(contents); assert(false); }
    catch (const CudaError& error) { assert(error.code == 713); }
    assert(registrations == 1 && registered == 0 && !contents.front().driver);

    // Missing entry points must not publish the zero-initialized temporary
    // handle. Exercise the real exported callback table, not a backend helper.
    char directory[] = "/tmp/cuinterpose-state-test-XXXXXX";
    assert(::mkdtemp(directory));
    assert(::setenv("SNAPSHOT_CONTROL_DIR", directory, 1) == 0);
    host = {};
    Host unavailable{ABI_VERSION, sizeof(Host), [](const char*) -> void* { return nullptr; }, ::getpid()};
    const Core* api = nullptr;
    assert(cuinterpose_core_init(&unavailable, &api) == success && api);
    uint64_t unicast = 0xAAAA, multicast = 0xBBBB;
    MulticastProp group{2, 4096, 1, 0};
    assert(api->cuMemCreate(&unicast, 4096, &properties, 0) == not_initialized);
    assert(api->cuMulticastCreate(&multicast, &group) == not_initialized);
    assert(unicast == 0xAAAA && multicast == 0xBBBB);
    assert(api->ensure_ready() == success && !failed.load());
    auto endpoint = std::string(directory) + "/cuinterpose-" + std::to_string(::getpid()) + ".sock";
    assert(::unlink(endpoint.c_str()) == 0 && ::rmdir(directory) == 0);
    std::cout << "PASS C ABI prefix guards, typed phases, inspection limits, carrier ownership, and missing entry points\n";
}
