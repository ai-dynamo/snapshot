// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#include "state.hpp"
#include <algorithm>

namespace cuinterpose {
namespace {
class Flight {
    State& state_;
    std::optional<Id> object_, member_;
    std::optional<uint64_t> driver_;
public:
    Flight(State& state, std::optional<Id> object = {}, std::optional<Id> member = {})
        : state_(state), object_(object), member_(member) {
        if (object_) {
            auto& value = state_.multicasts.at(*object_);
            if (!value.driver) throw CudaError{invalid_handle};
            driver_ = value.driver;
            ++value.inflight;
        }
        if (member_) ++state_.allocations.at(*member_).pins;
        ++state_.inflight;
    }
    void finish(std::unique_lock<std::mutex>& lock) {
        lock.lock();
        --state_.inflight;
        if (member_) --state_.allocations.at(*member_).pins;
        if (object_) {
            auto& value = state_.multicasts.at(*object_);
            --value.inflight;
            if (value.driver != driver_) {
                failed.store(true);
                throw CudaError{not_ready};
            }
        }
        if (state_.phase != Phase::Active) {
            failed.store(true);
            throw CudaError{not_ready};
        }
    }
};
int apply(const BindingRecord& binding, uint64_t group, uint64_t member) {
    if (auto memory = std::get_if<MemberRange>(&binding.source)) {
        if (binding.version == BindingVersion::V1)
            return driver::cuMulticastBindMem(group, binding.offset, member, memory->offset, binding.size, binding.flags);
        return driver::cuMulticastBindMem_v2(group, binding.device, binding.offset, member, memory->offset, binding.size, binding.flags);
    }
    auto address = std::get<AddressBinding>(binding.source).address;
    if (binding.version == BindingVersion::V1)
        return driver::cuMulticastBindAddr(group, binding.offset, address, binding.size, binding.flags);
    return driver::cuMulticastBindAddr_v2(group, binding.device, binding.offset, address, binding.size, binding.flags);
}
int bind(uint64_t handle, int32_t device, uint64_t offset, size_t size, uint64_t flags,
         BindingVersion version, bool memory, uint64_t source, uint64_t member_offset) {
    auto lock = lock_state();
    auto& state = generation.load()->state;
    std::optional<Id> target, member;
    if (state.handles.contains(handle)) target = state.handles.at(handle);
    else if ((handle & handle_mask) == handle_tag) return invalid_handle;
    BindingSource binding_source;
    uint64_t member_driver = 0;
    if (memory) {
        auto found = state.handles.find(source);
        if (found == state.handles.end() || !state.allocations.contains(found->second)) {
            if ((source & handle_mask) == handle_tag) return invalid_handle;
            if (target) return not_supported;
            lock.unlock();
            return version == BindingVersion::V1
                ? driver::cuMulticastBindMem(handle, offset, source, member_offset, size, flags)
                : driver::cuMulticastBindMem_v2(handle, device, offset, source, member_offset, size, flags);
        }
        member = found->second;
        auto& allocation = state.allocations.at(*member);
        if (size > UINT64_MAX-member_offset ||
            (allocation.size && member_offset + size > allocation.size)) return invalid_value;
        if (!allocation.driver) return invalid_handle;
        member_driver = *allocation.driver;
        if (version == BindingVersion::V1) device = allocation.properties.location.id;
        binding_source = MemberRange{*member, member_offset};
    } else {
        if (size > UINT64_MAX-source) return invalid_value;
        auto end = source + size;
        for (auto [base, length] : state.pending_maps)
            if (target && base < end && source < base + length) return not_ready;
        std::optional<MemberRange> range;
        for (const auto& [address, mapping] : state.mappings) {
            if (address >= end || source >= address + mapping.size) continue;
            if (source < address || end > address + mapping.size || !state.allocations.contains(mapping.id))
                return invalid_value;
            member = mapping.id;
            range = MemberRange{*member, mapping.offset + source - address};
            if (version == BindingVersion::V1) device = state.allocations.at(*member).properties.location.id;
        }
        if (!range && version == BindingVersion::V1) check(driver::cuCtxGetDevice(&device));
        binding_source = AddressBinding{source, range};
    }
    BindingRecord binding{target.value_or(Id{}), binding_source, size, offset, flags, version, device};
    if (!target) {
        lock.unlock();
        return apply(binding, handle, member_driver);
    }
    if (!size || size > UINT64_MAX-offset) return invalid_value;
    auto found = state.multicasts.find(*target);
    if (found == state.multicasts.end() || !found->second.driver) return invalid_handle;
    auto& object = found->second;
    object.bindings.reserve(object.bindings.size() + object.inflight + 1);
    auto driver_handle = *object.driver;
    Flight flight(state, target, member);
    lock.unlock();
    int result = apply(binding, driver_handle, member_driver);
    try { flight.finish(lock); }
    catch (...) {
        if (!result) check(driver::cuMulticastUnbind(driver_handle, binding.device, binding.offset, binding.size));
        throw;
    }
    check(result);
    if (member) state.allocations.at(*member).shared = true;
    object.extent = std::max(object.extent, static_cast<size_t>(offset + size));
    object.bindings.push_back(std::move(binding));
    if (!object.context) object.context = current_context();
    return success;
}
}
namespace backend {
int cuMulticastCreate(uint64_t* out, const MulticastProp* prop) {
    if (!out || !prop) return invalid_value;
    auto create = reinterpret_cast<decltype(Core::cuMulticastCreate)>(host.resolve("cuMulticastCreate"));
    if (!create) return not_initialized;
    auto lock = lock_state();
    auto& state = generation.load()->state;
    auto properties = *prop;
    Id id = Id::random();
    Flight flight(state);
    lock.unlock();
    uint64_t driver_handle = 0;
    int result = create(&driver_handle, &properties);
    flight.finish(lock);
    if (result) {
        *out = driver_handle;
        return result;
    }
    if ((driver_handle & handle_mask) == handle_tag) {
        driver::cuMemRelease(driver_handle);
        return invalid_handle;
    }
    if (properties.handle_types != 1) {
        state.unsupported += properties.handle_types != 0;
        *out = driver_handle;
        return success;
    }
    try {
        Multicast object;
        object.id = id;
        object.ticket = {state.identity, id, state.endpoint,
            MulticastResource{properties.devices, properties.size, properties.handle_types, properties.flags}};
        object.properties = properties;
        object.driver = driver_handle;
        object.context = current_context();
        object.creator = true;
        object.extent = properties.size;
        state.multicasts.emplace(id, std::move(object));
        *out = state.mint(id);
    } catch (...) {
        state.multicasts.erase(id);
        driver::cuMemRelease(driver_handle);
        throw;
    }
    return success;
}
int cuMulticastAddDevice(uint64_t handle, int32_t device) {
    auto lock = lock_state();
    auto& state = generation.load()->state;
    auto found = state.handles.find(handle);
    if (found == state.handles.end()) {
        if ((handle & handle_mask) == handle_tag) return invalid_handle;
        lock.unlock();
        return driver::cuMulticastAddDevice(handle, device);
    }
    auto object = state.multicasts.find(found->second);
    if (object == state.multicasts.end() || !object->second.driver) return invalid_handle;
    auto backing = *object->second.driver;
    // Allocate the device node before a driver call without a CUDA inverse.
    std::set<int32_t> pending{device};
    auto node = pending.extract(device);
    Flight flight(state, found->second);
    lock.unlock();
    int result = driver::cuMulticastAddDevice(backing, device);
    flight.finish(lock);
    check(result);
    object->second.devices.insert(std::move(node));
    if (!object->second.context) object->second.context = current_context();
    return success;
}
int cuMulticastBindMem(uint64_t handle, size_t offset, uint64_t member, size_t member_offset, size_t size, uint64_t flags) {
    return bind(handle, -1, offset, size, flags, BindingVersion::V1, true, member, member_offset);
}
int cuMulticastBindMem_v2(uint64_t handle, int32_t device, size_t offset, uint64_t member, size_t member_offset, size_t size, uint64_t flags) {
    return bind(handle, device, offset, size, flags, BindingVersion::V2, true, member, member_offset);
}
int cuMulticastBindAddr(uint64_t handle, size_t offset, uint64_t address, size_t size, uint64_t flags) {
    return bind(handle, -1, offset, size, flags, BindingVersion::V1, false, address, 0);
}
int cuMulticastBindAddr_v2(uint64_t handle, int32_t device, size_t offset, uint64_t address, size_t size, uint64_t flags) {
    return bind(handle, device, offset, size, flags, BindingVersion::V2, false, address, 0);
}
int cuMulticastGetGranularity(size_t* out, const MulticastProp* prop, uint32_t flags) {
    return driver::cuMulticastGetGranularity(out, prop, flags);
}
int cuMulticastUnbind(uint64_t handle, int32_t device, size_t offset, size_t size) {
    auto lock = lock_state();
    auto& state = generation.load()->state;
    auto found = state.handles.find(handle);
    if (found == state.handles.end()) {
        if ((handle & handle_mask) == handle_tag) return invalid_handle;
        return driver::cuMulticastUnbind(handle, device, offset, size);
    }
    auto object = state.multicasts.find(found->second);
    if (object == state.multicasts.end() || !object->second.driver) return invalid_handle;
    if (object->second.inflight) return not_ready;
    if (size > SIZE_MAX-offset) return invalid_value;
    for (auto& binding : object->second.bindings)
        if (binding.device == device && binding.offset < offset + size && offset < binding.offset + binding.size &&
            (binding.offset < offset || binding.offset + binding.size > offset + size)) return invalid_value;
    check(driver::cuMulticastUnbind(*object->second.driver, device, offset, size));
    std::erase_if(object->second.bindings, [&](auto& binding) {
        return binding.device == device && binding.offset >= offset && binding.offset + binding.size <= offset + size;
    });
    state.settle(found->second);
    return success;
}
}
int map_multicast(Id id, uint64_t address, size_t size, size_t offset, uint64_t flags, std::unique_lock<std::mutex>& lock) {
    auto& state = generation.load()->state;
    if (!size || size > UINT64_MAX-offset || !state.covered(address, size).empty()) return invalid_value;
    auto& object = state.multicasts.at(id);
    if (!object.driver) return invalid_handle;
    auto backing = *object.driver;
    std::map<uint64_t, Mapping> pending;
    pending.emplace(address, Mapping{id, address, size, offset, {}, false, false, flags});
    auto node = pending.extract(address);
    state.pending_maps.emplace_back(address, size);
    Flight flight(state, id);
    lock.unlock();
    int result = driver::cuMemMap(address, size, offset, backing, flags);
    flight.finish(lock);
    std::erase(state.pending_maps, std::pair{address, size});
    check(result);
    state.mappings.insert(std::move(node));
    object.extent = std::max(object.extent, offset + size);
    if (!object.context) object.context = current_context();
    return success;
}
int import_multicast(uint64_t* out, Ticket ticket, std::unique_lock<std::mutex>& lock) {
    auto& state = generation.load()->state;
    auto id = ticket.allocation;
    if (state.allocations.contains(id)) return invalid_handle;
    if (auto found = state.multicasts.find(id); found != state.multicasts.end()) {
        if (found->second.ticket != ticket) return invalid_value;
        found->second.shared = true;
        *out = state.mint(id);
        return success;
    }
    Flight flight(state);
    lock.unlock();
    std::optional<uint64_t> backing;
    std::exception_ptr failure;
    try {
        auto fd = request_export(ticket);
        backing = driver::import_posix(fd.get());
    } catch (...) {
        failure = std::current_exception();
    }
    flight.finish(lock);
    if (failure) std::rethrow_exception(failure);
    if (!backing || (*backing & handle_mask) == handle_tag) {
        if (backing) driver::cuMemRelease(*backing);
        return invalid_handle;
    }
    if (state.multicasts.contains(id)) {
        int result = driver::cuMemRelease(*backing);
        if (result) {
            state.unreleased_handles.push_back(*backing);
            failed.store(true);
            return result;
        }
        if (state.multicasts.at(id).ticket != ticket) return invalid_value;
        state.multicasts.at(id).shared = true;
        *out = state.mint(id);
        return success;
    }
    try {
        auto properties = std::get<MulticastResource>(ticket.resource);
        Multicast object;
        object.id = id;
        object.ticket = std::move(ticket);
        object.driver = backing;
        object.properties = {properties.devices, static_cast<size_t>(properties.size), properties.handle_types, properties.flags};
        object.extent = properties.size;
        object.context = current_context();
        object.shared = true;
        state.multicasts.emplace(id, std::move(object));
        *out = state.mint(id);
    } catch (...) {
        state.multicasts.erase(id);
        driver::cuMemRelease(*backing);
        throw;
    }
    return success;
}
void prepare_multicast(State& state) {
    for (auto& [id, object] : state.multicasts) {
        if (!object.driver) throw CudaError{invalid_handle};
        generation.load()->cache.replace({ResourceKind::Multicast, id});
        int device = object.devices.empty() ? object.bindings.empty() ? 0 : object.bindings.front().device : *object.devices.begin();
        Context::run(object.context, device, [&] {
            for (auto& [address, mapping] : state.mappings) if (mapping.id == id) {
                check(driver::cuMemUnmap(address, mapping.size));
                mapping.checkpointed = true;
            }
            for (auto& binding : object.bindings)
                check(driver::cuMulticastUnbind(*object.driver, binding.device, binding.offset, binding.size));
            check(driver::cuMemRelease(*object.driver));
            object.driver.reset();
            object.checkpointed = true;
        });
    }
}
void restore_multicast(Operation operation, std::unique_lock<std::mutex>& lock) {
    auto& state = generation.load()->state;
    auto next = state.next_phase(operation);
    auto objects = state.multicasts;
    auto mappings = state.mappings;
    auto allocations = state.allocations;
    state.phase = Phase::ReconstructingMulticast;
    lock.unlock();
    std::exception_ptr failure;
    try {
        for (auto& [id, object] : objects) {
            if (!object.checkpointed ||
                (operation == Operation::RestoreMulticastCreators && !object.creator) ||
                (operation == Operation::RestoreMulticastImporters && object.creator)) continue;
            int device = object.devices.empty() ? object.bindings.empty() ? 0 : object.bindings.front().device : *object.devices.begin();
            Context::run(object.context, device, [&] {
                switch (operation) {
                case Operation::RestoreMulticastCreators: {
                    uint64_t backing = 0;
                    check(driver::cuMulticastCreate(&backing, &object.properties));
                    object.driver = backing;
                    if (object.shared) generation.load()->cache.replace({ResourceKind::Multicast, id}, driver::export_posix(backing));
                    break;
                }
                case Operation::RestoreMulticastImporters: {
                    auto fd = request_export(object.ticket);
                    object.driver = driver::import_posix(fd.get());
                    break;
                }
                case Operation::RestoreMulticastDevices:
                    if (!object.driver) throw CudaError{invalid_handle};
                    for (auto device_id : object.devices) check(driver::cuMulticastAddDevice(*object.driver, device_id));
                    break;
                case Operation::RestoreMulticastBindings:
                    if (!object.driver) throw CudaError{invalid_handle};
                    for (auto& binding : object.bindings) {
                        uint64_t member = 0;
                        bool temporary = false;
                        if (auto memory = std::get_if<MemberRange>(&binding.source)) {
                            auto& allocation = allocations.at(memory->allocation);
                            if (allocation.driver) member = *allocation.driver;
                            else {
                                auto mapping = std::ranges::find_if(mappings, [&](auto& item) {
                                    return item.second.id == memory->allocation && !item.second.checkpointed;
                                });
                                if (mapping == mappings.end()) throw CudaError{invalid_handle};
                                check(driver::cuMemRetainAllocationHandle(&member, reinterpret_cast<void*>(mapping->first)));
                                temporary = true;
                            }
                        }
                        int result = apply(binding, *object.driver, member);
                        int released = temporary ? driver::cuMemRelease(member) : success;
                        check(result);
                        check(released);
                    }
                    for (auto& [address, mapping] : mappings) if (mapping.id == id && mapping.checkpointed) {
                        check(driver::cuMemMap(address, mapping.size, mapping.offset, *object.driver, mapping.flags));
                        if (!mapping.access.empty()) check(driver::cuMemSetAccess(address, mapping.size, mapping.access.data(), mapping.access.size()));
                        mapping.checkpointed = false;
                    }
                    object.checkpointed = false;
                    break;
                default: throw CudaError{invalid_value};
                }
            });
        }
    } catch (...) {
        failure = std::current_exception();
    }
    lock.lock();
    state.multicasts = std::move(objects);
    state.mappings = std::move(mappings);
    if (failure) std::rethrow_exception(failure);
    state.phase = next;
}
}
