// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#include "state.hpp"
#include <algorithm>
#include <sys/mman.h>
#include <unistd.h>

namespace cuinterpose {
Context::Context(void* context, int32_t device) {
    check(driver::cuCtxGetCurrent(&previous_));
    if (!context) {
        check(driver::cuDevicePrimaryCtxRetain(&context, device));
        primary_ = device;
    }
    changed_ = context != previous_;
    if (changed_) {
        int result = driver::cuCtxSetCurrent(context);
        if (result) {
            if (primary_) driver::cuDevicePrimaryCtxRelease_v2(*primary_);
            throw CudaError{result};
        }
    }
}
int Context::leave() noexcept {
    int result = changed_ ? driver::cuCtxSetCurrent(previous_) : success;
    if (primary_) {
        int released = driver::cuDevicePrimaryCtxRelease_v2(*primary_);
        if (!result) result = released;
    }
    changed_ = false; primary_.reset();
    return result;
}
std::pair<std::unique_ptr<Arena>, uint32_t> Arena::save(const std::vector<Content>& allocations) {
    if (allocations.empty()) return {};
    auto arena = std::make_unique<Arena>();
    for (auto& allocation : allocations) {
        if (!allocation.driver || !allocation.size || allocation.size > SIZE_MAX - arena->size ||
            !arena->offsets.emplace(allocation.id, arena->size).second) throw CudaError{invalid_value};
        arena->size += allocation.size;
    }
    arena->context = allocations.front().context;
    arena->device = allocations.front().properties.location.id;
    arena->base = ::mmap(nullptr, arena->size, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    if (arena->base == MAP_FAILED) throw CudaError{out_of_memory};
    bool registered = false;
    try {
        Context::run(arena->context, arena->device, [&] {
            check(driver::cuMemHostRegister_v2(arena->base, arena->size, 1));
            registered = true;
        });
        uint32_t elapsed = arena->copy(allocations, false);
        return {std::move(arena), elapsed};
    } catch (...) {
        if (registered) arena->release();
        else ::munmap(arena->base, arena->size);
        throw;
    }
}
uint32_t Arena::load(std::vector<Content>& allocations) const {
    auto fresh = allocations;
    size_t total = 0;
    for (auto& allocation : fresh) {
        if (allocation.driver || offsets.at(allocation.id) != total || allocation.size > SIZE_MAX-total)
            throw CudaError{invalid_value};
        total += allocation.size;
    }
    if (total != size) throw CudaError{invalid_value};
    bool registered = false;
    try {
        Context::run(context, device, [&] {
            uint32_t flags = 0;
            if (driver::cuMemHostGetFlags(&flags, base)) {
                check(driver::cuMemHostRegister_v2(base, size, 1));
                registered = true;
            }
        });
        for (auto& allocation : fresh) {
            Context::run(allocation.context, allocation.properties.location.id, [&] {
                uint64_t handle = 0;
                check(driver::cuMemCreate(&handle, allocation.size, &allocation.properties, 0));
                allocation.driver = handle;
            });
        }
        uint32_t elapsed = copy(fresh, true);
        allocations = std::move(fresh);
        return elapsed;
    } catch (...) {
        for (auto& allocation : fresh) if (allocation.driver) {
            try { Context::run(allocation.context, allocation.properties.location.id,
                [&] { check(driver::cuMemRelease(*allocation.driver)); }); }
            catch (...) {}
        }
        if (registered) {
            try { Context::run(context, device, [&] { check(driver::cuMemHostUnregister(base)); }); }
            catch (...) {}
        }
        throw;
    }
}
uint32_t Arena::copy(const std::vector<Content>& allocations, bool load) const {
    std::map<std::pair<void*, int32_t>, std::vector<const Content*>> groups;
    for (auto& allocation : allocations) groups[{allocation.context, allocation.properties.location.id}].push_back(&allocation);
    std::chrono::microseconds elapsed{};
    for (auto& [key, group] : groups) {
        size_t total = 0;
        for (auto allocation : group) {
            if (allocation->size > SIZE_MAX-total) throw CudaError{out_of_memory};
            total += allocation->size;
        }
        std::vector<std::pair<uint64_t, size_t>> mapped;
        mapped.reserve(group.size());
        Context scope(key.first, key.second);
        std::optional<uint64_t> reservation;
        void* stream = nullptr;
        bool stream_created = false, synchronized = false;
        std::exception_ptr error;
        try {
            uint64_t address = 0;
            check(driver::cuMemAddressReserve(&address, total, 0, 0, 0));
            reservation = address;
            if (total > UINT64_MAX-address) throw CudaError{invalid_value};
            for (auto allocation : group) {
                if (!allocation->driver) throw CudaError{invalid_handle};
                check(driver::cuMemMap(address, allocation->size, 0, *allocation->driver, 0));
                mapped.emplace_back(address, allocation->size);
                ::Access access{allocation->properties.location, 3};
                check(driver::cuMemSetAccess(address, allocation->size, &access, 1));
                address += allocation->size;
            }
            check(driver::cuStreamCreate(&stream, 1));
            stream_created = true;
            auto started = std::chrono::steady_clock::now();
            try {
                for (size_t i = 0; i < group.size(); ++i) {
                    auto allocation = group[i];
                    auto host_address = static_cast<uint8_t*>(base) + offsets.at(allocation->id);
                    check(load ? driver::cuMemcpyHtoDAsync_v2(mapped[i].first, host_address, allocation->size, stream)
                        : driver::cuMemcpyDtoHAsync_v2(host_address, mapped[i].first, allocation->size, stream));
                }
                check(driver::cuStreamSynchronize(stream));
                synchronized = true;
            } catch (...) {
                elapsed += std::chrono::duration_cast<std::chrono::microseconds>(std::chrono::steady_clock::now()-started);
                throw;
            }
            elapsed += std::chrono::duration_cast<std::chrono::microseconds>(std::chrono::steady_clock::now()-started);
        } catch (...) { error = std::current_exception(); }
        // This cannot be an unconditional destructor: unknown DMA completion
        // forbids releasing any source, destination, or staging memory.
        auto cleanup = [&](int status) {
            if (status && !error) error = std::make_exception_ptr(CudaError{status});
        };
        if (stream_created) {
            if (!synchronized && driver::cuStreamSynchronize(stream)) {
                constexpr char message[] = "cuinterpose: CUDA copy completion unknown; terminating without cleanup\n";
                ::write(STDERR_FILENO, message, sizeof(message)-1);
                ::_exit(127);
            }
            cleanup(driver::cuStreamDestroy_v2(stream));
        }
        for (auto [address, length] : mapped) cleanup(driver::cuMemUnmap(address, length));
        if (reservation) cleanup(driver::cuMemAddressFree(*reservation, total));
        cleanup(scope.leave());
        if (error) std::rethrow_exception(error);
    }
    return static_cast<uint32_t>(std::min<int64_t>(elapsed.count(), UINT32_MAX));
}
int Arena::release() noexcept {
    try {
        Context scope(context, device);
        int result = driver::cuMemHostUnregister(base);
        int left = scope.leave();
        if (result) return result;
        int unmapped = ::munmap(base, size);
        return left ? left : unmapped ? unknown : success;
    } catch (const CudaError& error) { return error.code; }
    catch (...) { return unknown; }
}
}
