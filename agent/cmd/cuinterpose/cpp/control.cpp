// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#include "state.hpp"
#include <cstring>
#include <poll.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/un.h>
#include <unistd.h>

namespace cuinterpose {
Fd request_export(const Ticket& ticket) try {
    ticket.validate();
    auto kind = std::holds_alternative<std::monostate>(ticket.resource) ? ResourceKind::Unicast : ResourceKind::Multicast;
    // Outgoing peer sockets belong to the same atfork registry as listeners
    // and queued connections, including while CUDA collective calls are unlocked.
    auto socket = open_socket([&] { return connect(ticket.endpoint, timeout()); });
    send(socket->get(), Request{Export{ticket.creator, kind, ticket.allocation}});
    auto received = receive(socket->get());
    auto response = received.body.get<Response>();
    if (std::holds_alternative<Failure>(response.result)) throw CudaError{invalid_handle};
    auto exported = std::get_if<Exported>(&std::get<Reply>(response.result));
    if (response.participant != ticket.creator || !exported || exported->resource != kind ||
        exported->allocation != ticket.allocation || received.descriptor.get() < 0)
        throw CudaError{invalid_handle};
    return std::move(received.descriptor);
} catch (const Json::exception&) {
    throw CudaError{invalid_handle};
} catch (const std::invalid_argument&) {
    throw CudaError{invalid_handle};
} catch (const std::runtime_error&) {
    // Refusal, a missing creator, and malformed/closed peer connections are
    // ordinary import failures. Allocation failures and unexpected exceptions
    // still reach the ABI failure boundary. During restore, the lifecycle
    // handler makes even this recoverable CUDA error fatal to reconstruction.
    throw CudaError{invalid_handle};
}
namespace {
struct Work { std::unique_ptr<Socket> socket; Request request; };
struct Queue {
    std::mutex mutex;
    std::condition_variable ready;
    std::deque<Work> work;
    bool stopped{};
};
void refuse(int socket, Id id, const std::string& message) {
    send(socket, Response{id, Failure{message}});
}
void serve(Work work, Generation& current) {
    std::variant<Reply, Failure> response;
    bool loaded = false;
    try {
        auto lock = lock_state(false);
        auto& state = current.state;
        if (std::holds_alternative<Handshake>(work.request)) response = Reply{Handshake{}};
        else if (std::holds_alternative<Inspect>(work.request)) {
            auto stats = state.stats();
            try {
                response = Reply{Inspection{state.inspect(), stats.live_raw_imports, stats.unsupported_exportable_creations}};
            } catch (const CudaError&) {
                response = Failure{"cannot inspect current CUDA state"};
            }
        } else {
            auto operation = std::get<Execute>(work.request).operation;
            state.validate_lifecycle(operation);
            try {
                Transfer transfer;
                if (operation >= Operation::RestoreMulticastCreators) restore_multicast(operation, lock);
                else transfer = state.lifecycle(operation);
                response = Reply{Completed{operation, transfer.bytes, transfer.copy_us}};
                loaded = operation == Operation::LoadAllocations;
            } catch (...) {
                failed.store(true);
                throw;
            }
        }
    } catch (const CudaError& error) { response = Failure{"CUDA lifecycle operation refused or failed: " + std::to_string(error.code)}; }
    catch (const std::exception& error) { failed.store(true); response = Failure{error.what()}; }
    try {
        send(work.socket->get(), Response{current.state.identity, std::move(response)});
    } catch (const std::system_error&) {
        // Losing a read-only client cannot invalidate CUDA state. A lifecycle
        // reply failure still reaches the worker's fail-closed boundary.
        if (std::holds_alternative<Execute>(work.request)) throw;
        return;
    }
    if (loaded) {
        auto lock = lock_state(false);
        auto arena = std::move(current.state.arena);
        if (arena && arena->release()) failed.store(true);
    }
}
}
void start_control(Generation& current) {
    auto listener = open_socket([] {
        int descriptor = ::socket(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0);
        if (descriptor < 0) throw std::system_error(errno, std::generic_category(), "socket");
        return Fd(descriptor);
    });
    sockaddr_un address{};
    address.sun_family = AF_UNIX;
    std::memcpy(address.sun_path, current.state.endpoint.c_str(), current.state.endpoint.size()+1);
    if (::bind(listener->get(), reinterpret_cast<sockaddr*>(&address), sizeof(address)))
        throw std::system_error(errno, std::generic_category(), "bind control socket");
    auto queue = std::make_shared<Queue>();
    try {
        if (::chmod(current.state.endpoint.c_str(), 0600) || ::listen(listener->get(), 128))
            throw std::system_error(errno, std::generic_category(), "listen");
        std::thread worker([queue, &current] {
            try {
                for (;;) {
                    Work work;
                    {
                        std::unique_lock lock(queue->mutex);
                        queue->ready.wait(lock, [&] { return queue->stopped || !queue->work.empty(); });
                        if (queue->work.empty()) return;
                        work = std::move(queue->work.front());
                        queue->work.pop_front();
                    }
                    try { serve(std::move(work), current); }
                    catch (...) { failed.store(true); }
                }
            } catch (...) { failed.store(true); }
        });
        worker.detach();
        try {
            std::thread peer([queue, listener = std::move(listener), &current] {
                try {
                    for (;;) {
                        pollfd event{listener->get(), POLLIN, 0};
                        int result;
                        do { result = ::poll(&event, 1, -1); } while (result < 0 && errno == EINTR);
                        if (result != 1 || event.revents != POLLIN) { failed.store(true); return; }
                        auto socket = open_socket([&] {
                            int descriptor = ::accept4(listener->get(), nullptr, nullptr, SOCK_CLOEXEC);
                            if (descriptor < 0) throw std::system_error(errno, std::generic_category(), "accept");
                            return Fd(descriptor);
                        });
                        try {
                            timeval value{timeout().count(), 0};
                            if (::setsockopt(socket->get(), SOL_SOCKET, SO_RCVTIMEO, &value, sizeof(value)) ||
                                ::setsockopt(socket->get(), SOL_SOCKET, SO_SNDTIMEO, &value, sizeof(value)))
                                throw std::system_error(errno, std::generic_category(), "timeout");
                            auto received = receive(socket->get());
                            auto request = received.body.get<Request>();
                            bool identified = std::visit([&](const auto& item) {
                                if constexpr (requires { item.participant; }) return item.participant == current.state.identity;
                                else return true;
                            }, request);
                            if (received.descriptor.get() >= 0 || !identified) {
                                refuse(socket->get(), current.state.identity, "invalid cuinterpose control request");
                                continue;
                            }
                            if (auto export_request = std::get_if<Export>(&request)) {
                                if (failed.load()) { refuse(socket->get(), current.state.identity, "cuinterpose state failed"); continue; }
                                try {
                                    auto lease = current.cache.acquire({export_request->resource, export_request->allocation});
                                    send(socket->get(), Response{current.state.identity, Reply{Exported{export_request->resource, export_request->allocation}}},
                                         lease->descriptor.get());
                                } catch (...) { refuse(socket->get(), current.state.identity, "creator resource is unavailable"); }
                            } else {
                                std::lock_guard lock(queue->mutex);
                                if (queue->work.size() == 8) refuse(socket->get(), current.state.identity, "control queue full; refused without mutation");
                                else { queue->work.push_back({std::move(socket), std::move(request)}); queue->ready.notify_one(); }
                            }
                        } catch (...) { /* Malformed/closed connections do not poison CUDA state. */ }
                    }
                } catch (...) { failed.store(true); }
            });
            peer.detach();
        } catch (...) {
            std::lock_guard lock(queue->mutex);
            queue->stopped = true; queue->ready.notify_one();
            // Never join the worker while a constructor may hold loader locks.
            throw;
        }
    } catch (...) { ::unlink(current.state.endpoint.c_str()); throw; }
}
}
