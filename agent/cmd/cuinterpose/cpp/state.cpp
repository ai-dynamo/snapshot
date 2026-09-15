// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#include "state.hpp"
#include <algorithm>
#include <limits>

namespace cuinterpose {
namespace driver {
Fd export_posix(uint64_t handle) {
    int fd = -1;
    check(cuMemExportToShareableHandle(&fd, handle, 1, 0));
    if (fd < 0) throw CudaError{invalid_handle};
    return Fd(fd);
}
uint64_t import_posix(int fd) {
    uint64_t handle = 0;
    check(cuMemImportFromShareableHandle(&handle, reinterpret_cast<void*>(intptr_t(fd)), 1));
    return handle;
}
}
void* current_context() {
    void* context = nullptr;
    driver::cuCtxGetCurrent(&context);
    return context;
}
std::unique_lock<std::mutex> lock_state(bool active) {
    auto* current = generation.load(std::memory_order_acquire);
    if (!current || failed.load()) throw CudaError{not_initialized};
    std::unique_lock lock(current->mutex);
    if (failed.load()) throw CudaError{unknown};
    if (active && current->state.phase != Phase::Active) throw CudaError{not_ready};
    return lock;
}
uint64_t State::mint(Id id) {
    if (next & handle_mask) throw CudaError{out_of_memory};
    uint64_t handle = handle_tag | next++;
    handles.emplace(handle, id);
    return handle;
}
void State::settle(Id id) {
    bool has_handles = std::ranges::any_of(handles, [&](auto& item) { return item.second == id; });
    bool has_mappings = std::ranges::any_of(mappings, [&](auto& item) { return item.second.id == id; });
    auto& cache = generation.load()->cache;
    if (auto found = multicasts.find(id); found != multicasts.end()) {
        if (!has_handles && !has_mappings) {
            if (found->second.inflight || found->second.checkpointed) throw CudaError{not_ready};
            cache.replace({ResourceKind::Multicast, id});
            if (found->second.driver) check(driver::cuMemRelease(*found->second.driver));
            multicasts.erase(found);
        }
        return;
    }
    auto found = allocations.find(id);
    if (found == allocations.end()) throw CudaError{invalid_handle};
    auto& allocation = found->second;
    if (!has_handles && allocation.driver) {
        check(driver::cuMemRelease(*allocation.driver));
        allocation.driver.reset();
    }
    if (!has_handles && !has_mappings) {
        cache.replace({ResourceKind::Unicast, id});
        allocations.erase(found);
    }
}
std::vector<uint64_t> State::covered(uint64_t address, size_t size) const {
    if (size > UINT64_MAX - address) throw CudaError{invalid_value};
    auto end = address + size;
    for (auto [base, length] : pending_maps)
        if (address < base + length && base < end) throw CudaError{not_ready};
    std::vector<uint64_t> result;
    for (const auto& [base, mapping] : mappings)
        if (address < base + mapping.size && base < end) {
            if (base < address || base + mapping.size > end) throw CudaError{invalid_value};
            result.push_back(base);
        }
    return result;
}
DebugStats State::stats() const {
    DebugStats result{};
    result.allocations = allocations.size();
    result.multicasts = multicasts.size();
    for (auto& [handle, id] : handles) { (void)handle; result.handles += allocations.contains(id); }
    for (auto& [address, mapping] : mappings) { (void)address; result.mappings += allocations.contains(mapping.id); }
    {
        auto& cache = generation.load()->cache;
        std::lock_guard lock(cache.mutex);
        result.cached_exports = cache.entries.size();
    }
    for (auto [handle, count] : raw) { (void)handle; result.live_raw_imports += count; }
    result.unsupported_exportable_creations = unsupported;
    result.phase = failed.load() ? 5 : phase == Phase::Active ? 1 :
        (phase == Phase::MulticastPrepared || phase == Phase::AllocationsSaved) ? 2 :
        phase == Phase::UnicastPrepared ? 3 : 4;
    return result;
}
std::vector<Record> State::inspect() const {
    if (phase != Phase::Active || inflight) throw CudaError{not_ready};
    size_t count = allocations.size() + mappings.size();
    for (auto& [id, object] : multicasts) { (void)id; count += 1 + object.devices.size() + object.bindings.size(); }
    if (count > max_records) throw CudaError{not_supported};
    std::vector<Record> records;
    records.reserve(count);
    for (auto& [id, allocation] : allocations) {
        auto handles_count = std::ranges::count_if(handles, [&](auto& item) { return item.second == id; });
        const auto& prop = allocation.properties;
        records.emplace_back(AllocationRecord{id, allocation.creator,
            allocation.creator && allocation.shared && prop.handle_types && prop.kind == 1 && prop.location.kind == 1,
            allocation.size, prop.kind, prop.handle_types, prop.location.kind, prop.location.id,
            static_cast<uint32_t>(handles_count)});
    }
    for (auto& [id, object] : multicasts) {
        auto handles_count = std::ranges::count_if(handles, [&](auto& item) { return item.second == id; });
        records.emplace_back(MulticastRecord{id, object.ticket.creator, object.creator,
            object.extent, static_cast<uint32_t>(handles_count), object.properties.handle_types,
            object.properties.flags, object.properties.devices});
        for (auto device : object.devices) records.emplace_back(DeviceRecord{id, device});
        for (auto binding : object.bindings) records.emplace_back(std::move(binding));
    }
    for (auto& [address, mapping] : mappings) {
        if (mapping.unknown) throw CudaError{not_supported};
        std::vector<AccessRecord> access;
        for (auto descriptor : mapping.access)
            access.push_back({descriptor.location.kind, descriptor.location.id, descriptor.flags});
        std::ranges::sort(access);
        if (multicasts.contains(mapping.id))
            records.emplace_back(MulticastMappingRecord{mapping.id, address, mapping.size, mapping.offset, mapping.flags, std::move(access)});
        else records.emplace_back(MappingRecord{mapping.id, allocations.at(mapping.id).creator,
            address, mapping.size, mapping.offset, std::move(access)});
    }
    return records;
}
Phase State::next_phase(Operation operation) const {
    struct Transition { Operation operation; Phase before, after; };
    static constexpr Transition transitions[] = {
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
    for (const auto& transition : transitions)
        if (operation == transition.operation && phase == transition.before) return transition.after;
    throw CudaError{not_ready};
}
void State::validate_lifecycle(Operation operation) const {
    next_phase(operation);
    if (inflight) throw CudaError{not_ready};
    if (unsupported || !raw.empty()) throw CudaError{not_supported};
    if (operation == Operation::PrepareMulticast) inspect();
}
Transfer State::lifecycle(Operation operation) {
    auto next_phase_value = next_phase(operation);
    auto selected = [](const Allocation& a) {
        return a.creator && a.shared && a.properties.handle_types &&
            a.properties.kind == 1 && a.properties.location.kind == 1;
    };
    Transfer transfer;
    auto contents = [&](bool save) {
        std::vector<Content> result;
        for (auto& [id, allocation] : allocations) {
            if (!(save ? selected(allocation) : allocation.host_checkpointed)) continue;
            if (allocation.size > UINT64_MAX - transfer.bytes) throw CudaError{out_of_memory};
            transfer.bytes += allocation.size;
            result.push_back({id, allocation.driver, allocation.size, allocation.properties, allocation.context});
        }
        return result;
    };
    switch (operation) {
    case Operation::PrepareMulticast: prepare_multicast(*this); break;
    case Operation::SaveAllocations: {
        std::vector<Id> recovered;
        recovered.reserve(allocations.size());
        try {
            for (auto& [id, allocation] : allocations) {
                if (!selected(allocation) || allocation.driver) continue;
                auto mapping = std::ranges::find_if(mappings, [&](auto& item) { return item.second.id == id; });
                if (mapping == mappings.end()) throw CudaError{invalid_handle};
                Context::run(allocation.context, allocation.properties.location.id, [&] {
                    uint64_t handle = 0;
                    check(driver::cuMemRetainAllocationHandle(&handle, reinterpret_cast<void*>(mapping->first)));
                    allocation.driver = handle;
                    recovered.push_back(id);
                });
            }
            auto saved = Arena::save(contents(true));
            arena = std::move(saved.first);
            transfer.copy_us = saved.second;
        } catch (...) {
            for (auto id : recovered) {
                auto& allocation = allocations.at(id);
                try {
                    Context::run(allocation.context, allocation.properties.location.id, [&] {
                        check(driver::cuMemRelease(*allocation.driver));
                        allocation.driver.reset();
                    });
                } catch (...) { /* Retain failed cleanup ownership for fail-stop. */ }
            }
            throw;
        }
        for (auto& [id, allocation] : allocations) {
            (void)id;
            if (selected(allocation)) allocation.host_checkpointed = true;
        }
        break;
    }
    case Operation::PrepareUnicast:
        generation.load()->cache.clear();
        for (auto& [id, allocation] : allocations) {
            if (!allocation.shared) continue;
            Context::run(allocation.context, allocation.properties.location.id, [&] {
                for (auto& [address, mapping] : mappings) if (mapping.id == id) {
                    check(driver::cuMemUnmap(address, mapping.size));
                    mapping.checkpointed = true;
                }
                if (allocation.driver) check(driver::cuMemRelease(*allocation.driver));
                allocation.driver.reset();
                allocation.checkpointed = true;
            });
        }
        break;
    case Operation::LoadAllocations: {
        auto allocations_content = contents(false);
        if (arena) transfer.copy_us = arena->load(allocations_content);
        else if (!allocations_content.empty()) throw CudaError{invalid_value};
        for (auto& allocation : allocations_content) allocations.at(allocation.id).driver = allocation.driver;
        remap(true);
        break;
    }
    case Operation::RestoreUnicast:
        for (auto& [id, allocation] : allocations) {
            (void)id;
            if (allocation.creator || !allocation.checkpointed) continue;
            auto descriptor = request_export(allocation.ticket);
            Context::run(allocation.context, allocation.properties.location.id, [&] {
                allocation.driver = driver::import_posix(descriptor.get());
            });
        }
        remap(false);
        break;
    default: throw CudaError{not_supported};
    }
    phase = next_phase_value;
    return transfer;
}
void State::remap(bool creator) {
    for (auto& [id, allocation] : allocations) {
        if (!allocation.checkpointed || allocation.creator != creator) continue;
        Context::run(allocation.context, allocation.properties.location.id, [&] {
            if (!allocation.driver) throw CudaError{invalid_handle};
            for (auto& [address, mapping] : mappings) if (mapping.id == id && mapping.checkpointed) {
                check(driver::cuMemMap(address, mapping.size, mapping.offset, *allocation.driver, 0));
                if (!mapping.access.empty())
                    check(driver::cuMemSetAccess(address, mapping.size, mapping.access.data(), mapping.access.size()));
                mapping.checkpointed = false;
            }
            if (creator) generation.load()->cache.replace({ResourceKind::Unicast, id}, driver::export_posix(*allocation.driver));
            if (!std::ranges::any_of(handles, [&](auto& item) { return item.second == id; })) {
                check(driver::cuMemRelease(*allocation.driver));
                allocation.driver.reset();
            }
            allocation.checkpointed = allocation.host_checkpointed = false;
        });
    }
}
namespace backend {
int cuMemCreate(uint64_t* out, size_t size, const AllocationProp* prop, uint64_t flags) {
    if (!out || !prop) return invalid_value;
    auto create = reinterpret_cast<decltype(Core::cuMemCreate)>(host.resolve("cuMemCreate"));
    if (!create) return not_initialized;
    auto lock = lock_state(false);
    auto& state = generation.load()->state;
    if (prop->handle_types == 1 && state.phase != Phase::Active) return not_ready;
    Id id = Id::random();
    uint64_t handle = 0;
    int result = create(&handle, size, prop, flags);
    if (result) {
        *out = handle;
        return result;
    }
    if ((handle & handle_mask) == handle_tag) {
        driver::cuMemRelease(handle);
        return invalid_handle;
    }
    if (prop->handle_types != 1) {
        state.unsupported += prop->handle_types != 0;
        *out = handle;
        return success;
    }
    try {
        Allocation allocation;
        allocation.id = id;
        allocation.ticket = {state.identity, id, state.endpoint, std::monostate{}};
        allocation.driver = handle;
        allocation.size = size;
        allocation.properties = *prop;
        allocation.creator = true;
        allocation.context = current_context();
        state.allocations.emplace(id, std::move(allocation));
        *out = state.mint(id);
    } catch (...) {
        state.allocations.erase(id);
        driver::cuMemRelease(handle);
        throw;
    }
    return success;
}
int cuMemRelease(uint64_t handle) {
    auto lock = lock_state(false);
    auto& state = generation.load()->state;
    if (auto found = state.handles.find(handle); found != state.handles.end()) {
        auto id = found->second;
        if (state.phase != Phase::Active ||
            (state.multicasts.contains(id) && state.multicasts.at(id).inflight) ||
            (state.allocations.contains(id) && state.allocations.at(id).pins)) return not_ready;
        auto node = state.handles.extract(found);
        try { state.settle(id); }
        catch (...) {
            state.handles.insert(std::move(node));
            throw;
        }
    } else {
        if ((handle & handle_mask) == handle_tag) return invalid_handle;
        check(driver::cuMemRelease(handle));
        if (state.raw.contains(handle) && --state.raw[handle] == 0) state.raw.erase(handle);
    }
    return success;
}
int cuMemRetainAllocationHandle(uint64_t* out, void* pointer) {
    if (!out) return invalid_value;
    auto lock = lock_state(false);
    auto& state = generation.load()->state;
    auto address = reinterpret_cast<uint64_t>(pointer);
    for (auto [base, size] : state.pending_maps) if (address >= base && address - base < size) return not_ready;
    auto mapping = std::ranges::find_if(state.mappings, [&](auto& item) {
        return address >= item.first && address - item.first < item.second.size;
    });
    if (mapping != state.mappings.end() && state.phase != Phase::Active) return not_ready;
    if (mapping != state.mappings.end() && state.multicasts.contains(mapping->second.id)) {
        if (!state.multicasts.at(mapping->second.id).driver) return invalid_handle;
        *out = state.mint(mapping->second.id);
        return success;
    }
    uint64_t driver_handle = 0;
    check(driver::cuMemRetainAllocationHandle(&driver_handle, pointer));
    if (mapping == state.mappings.end()) {
        if ((driver_handle & handle_mask) == handle_tag) {
            driver::cuMemRelease(driver_handle);
            return invalid_handle;
        }
        *out = driver_handle;
    } else {
        auto& allocation = state.allocations.at(mapping->second.id);
        if (allocation.driver) {
            int result = driver::cuMemRelease(driver_handle);
            if (result) {
                state.unreleased_handles.push_back(driver_handle);
                failed.store(true);
                return result;
            }
        } else allocation.driver = driver_handle;
        *out = state.mint(allocation.id);
    }
    return success;
}
int cuMemMap(uint64_t address, size_t size, size_t offset, uint64_t handle, uint64_t flags) {
    auto lock = lock_state(false);
    auto& state = generation.load()->state;
    auto found = state.handles.find(handle);
    if (found == state.handles.end()) {
        if ((handle & handle_mask) == handle_tag) return invalid_handle;
        if (!state.covered(address, size).empty()) return invalid_value;
        return driver::cuMemMap(address, size, offset, handle, flags);
    }
    if (state.phase != Phase::Active) return not_ready;
    auto id = found->second;
    if (state.multicasts.contains(id)) return map_multicast(id, address, size, offset, flags, lock);
    if (!size || !state.covered(address, size).empty()) return invalid_value;
    auto& allocation = state.allocations.at(id);
    if (!allocation.driver) return invalid_handle;
    Mapping mapping{id, address, size, offset, {}, false, false, flags};
    auto [entry, inserted] = state.mappings.emplace(address, std::move(mapping));
    if (!inserted) return invalid_value;
    int result = driver::cuMemMap(address, size, offset, *allocation.driver, flags);
    if (result) state.mappings.erase(entry);
    else if (!allocation.context) allocation.context = current_context();
    return result;
}
int cuMemUnmap(uint64_t address, size_t size) {
    auto lock = lock_state(false);
    auto& state = generation.load()->state;
    auto mappings = state.covered(address, size);
    if (!mappings.empty() && state.phase != Phase::Active) return not_ready;
    for (auto base : mappings) {
        auto id = state.mappings.at(base).id;
        if ((state.multicasts.contains(id) && state.multicasts.at(id).inflight) ||
            (state.allocations.contains(id) && state.allocations.at(id).pins)) return not_ready;
    }
    check(driver::cuMemUnmap(address, size));
    for (auto base : mappings) {
        auto id = state.mappings.at(base).id;
        state.mappings.erase(base);
        state.settle(id);
    }
    return success;
}
int cuMemSetAccess(uint64_t address, size_t size, const ::Access* access, size_t count) {
    auto lock = lock_state(false);
    auto& state = generation.load()->state;
    auto mappings = state.covered(address, size);
    if (!mappings.empty() && state.phase != Phase::Active) return not_ready;
    if (mappings.empty() || !access) return driver::cuMemSetAccess(address, size, access, count);
    if (count > SIZE_MAX / sizeof(::Access)) return invalid_value;
    std::vector<std::vector<::Access>> merged;
    for (auto base : mappings) {
        auto entries = state.mappings.at(base).access;
        for (size_t i = 0; i < count; ++i) {
            auto descriptor = access[i];
            std::erase_if(entries, [&](auto entry) {
                return entry.location.kind == descriptor.location.kind && entry.location.id == descriptor.location.id;
            });
            if (descriptor.flags) entries.push_back(descriptor);
            if (entries.size() > max_access) return not_supported;
        }
        merged.push_back(std::move(entries));
    }
    int result = driver::cuMemSetAccess(address, size, access, count);
    for (size_t i = 0; i < mappings.size(); ++i) {
        auto& mapping = state.mappings.at(mappings[i]);
        if (result) mapping.unknown = true;
        else mapping.access = std::move(merged[i]);
    }
    return result;
}
int cuMemExportToShareableHandle(void* out, uint64_t handle, uint32_t kind, uint64_t flags) {
    auto lock = lock_state(false);
    auto& state = generation.load()->state;
    auto found = state.handles.find(handle);
    if (found == state.handles.end()) {
        if ((handle & handle_mask) == handle_tag) return invalid_handle;
        return driver::cuMemExportToShareableHandle(out, handle, kind, flags);
    }
    if (state.phase != Phase::Active) return not_ready;
    if (!out || kind != 1 || flags) return invalid_value;
    auto id = found->second;
    auto publish = [&](auto& allocation, ResourceKind resource) {
        auto& cache = generation.load()->cache;
        bool cached;
        { std::lock_guard guard(cache.mutex); cached = cache.entries.contains({resource, id}); }
        if (allocation.creator && !cached) {
            if (!allocation.driver) throw CudaError{invalid_handle};
            cache.replace({resource, id}, driver::export_posix(*allocation.driver));
        }
        Fd ticket;
        try { ticket = export_ticket(allocation.ticket); }
        catch (const std::system_error&) { throw CudaError{out_of_memory}; }
        allocation.shared = true;
        if (!allocation.context) allocation.context = current_context();
        *static_cast<int*>(out) = ticket.release();
    };
    if (state.multicasts.contains(id)) publish(state.multicasts.at(id), ResourceKind::Multicast);
    else publish(state.allocations.at(id), ResourceKind::Unicast);
    return success;
}
int cuMemImportFromShareableHandle(uint64_t* out, void* fd, uint32_t kind) {
    if (!out) return invalid_value;
    std::optional<Ticket> ticket;
    try { if (kind == 1) ticket = read_ticket(static_cast<int>(reinterpret_cast<intptr_t>(fd))); }
    catch (...) { return invalid_handle; }
    auto lock = lock_state(false);
    auto& state = generation.load()->state;
    if (!ticket) {
        uint64_t handle = 0;
        check(driver::cuMemImportFromShareableHandle(&handle, fd, kind));
        if ((handle & handle_mask) == handle_tag) {
            driver::cuMemRelease(handle);
            return invalid_handle;
        }
        ++state.raw[handle];
        *out = handle;
        return success;
    }
    if (state.phase != Phase::Active) return not_ready;
    if (std::holds_alternative<MulticastResource>(ticket->resource)) return import_multicast(out, *ticket, lock);
    Id id = ticket->allocation;
    if (state.multicasts.contains(id)) return invalid_handle;
    if (state.allocations.contains(id)) {
        auto& allocation = state.allocations.at(id);
        if (allocation.ticket != *ticket) return invalid_value;
        if (!allocation.driver) {
            auto raw = request_export(*ticket);
            allocation.driver = driver::import_posix(raw.get());
        }
        allocation.shared = true;
        *out = state.mint(id);
        return success;
    }
    auto descriptor = request_export(*ticket);
    auto handle = driver::import_posix(descriptor.get());
    try {
        if ((handle & handle_mask) == handle_tag) throw CudaError{invalid_handle};
        Allocation allocation;
        allocation.id = id;
        allocation.ticket = *ticket;
        allocation.driver = handle;
        allocation.shared = true;
        allocation.context = current_context();
        check(driver::cuMemGetAllocationPropertiesFromHandle(&allocation.properties, handle));
        state.allocations.emplace(id, std::move(allocation));
        *out = state.mint(id);
    } catch (...) {
        state.allocations.erase(id);
        driver::cuMemRelease(handle);
        throw;
    }
    return success;
}
int cuMemGetAllocationPropertiesFromHandle(AllocationProp* out, uint64_t handle) {
    auto lock = lock_state(false);
    auto& state = generation.load()->state;
    if (state.handles.contains(handle)) {
        if (state.phase != Phase::Active) return not_ready;
        auto id = state.handles.at(handle);
        auto backing = state.multicasts.contains(id) ? state.multicasts.at(id).driver : state.allocations.at(id).driver;
        if (!backing) return invalid_handle;
        handle = *backing;
    } else if ((handle & handle_mask) == handle_tag) return invalid_handle;
    return driver::cuMemGetAllocationPropertiesFromHandle(out, handle);
}
}
}
