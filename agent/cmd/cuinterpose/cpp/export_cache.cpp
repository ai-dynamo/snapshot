// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#include "state.hpp"
namespace cuinterpose {
ExportCache::Lease::Lease(ExportCache* owner, Key identity, Fd fd)
    : cache(owner), key(identity), descriptor(std::move(fd)) {}
ExportCache::Lease::~Lease() {
    descriptor = Fd{};
    std::lock_guard lock(cache->mutex);
    --cache->transfers;
    --cache->entries.at(key).transfers;
    cache->drained.notify_all();
}
std::unique_ptr<ExportCache::Lease> ExportCache::acquire(Key key) {
    std::lock_guard lock(mutex);
    auto entry = entries.find(key);
    if (draining || entry == entries.end() || entry->second.retiring) throw CudaError{invalid_handle};
    auto lease = std::make_unique<Lease>(this, key, entry->second.descriptor.duplicate());
    ++transfers; ++entry->second.transfers;
    return lease;
}
void ExportCache::replace(Key key, Fd fd) {
    std::lock_guard mutation(mutations_);
    std::unique_lock lock(mutex);
    if (auto entry = entries.find(key); entry != entries.end()) {
        entry->second.retiring = true;
        drained.wait(lock, [&] { return entry->second.transfers == 0; });
        entries.erase(entry);
    }
    if (fd.get() >= 0) entries.emplace(key, Entry{std::move(fd), 0, false});
}
void ExportCache::clear() {
    std::lock_guard mutation(mutations_);
    std::unique_lock lock(mutex);
    draining = true;
    drained.wait(lock, [&] { return transfers == 0; });
    entries.clear();
    draining = false;
}
}
