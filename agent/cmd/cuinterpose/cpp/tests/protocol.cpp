// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#include "../state.hpp"
#include <cassert>
#include <fcntl.h>
#include <future>
#include <iostream>
#include <sys/socket.h>
#include <unistd.h>

using namespace cuinterpose;
using namespace std::chrono_literals;

template<class F> void rejects(F&& function) {
    bool rejected = false;
    try { function(); } catch (...) { rejected = true; }
    assert(rejected);
}

int main() {
    Id id = Id::parse("0123456789abcdef0123456789abcdef");
    assert(decode(encode(id)).get<Id>() == id);
    for (auto invalid : {"", "A123456789abcdef0123456789abcdef0",
                         "0123456789ABCDEF0123456789abcdef", "0123456789abcdef"})
        rejects([&] { Id::parse(invalid); });
    auto old = Json::to_msgpack(Json{{"version", 3}, {"body", Request{Handshake{}}}});
    rejects([&] { decode(old); });
    auto trailing = encode(Request{Handshake{}});
    trailing.push_back(0);
    rejects([&] { decode(trailing); });
    Record mapping = MappingRecord{id, true, UINT64_MAX, UINT64_MAX, UINT64_MAX,
        std::vector<AccessRecord>(max_access, {INT32_MAX, INT32_MAX, UINT64_MAX})};
    Inspection maximum{std::vector<Record>(max_records, mapping), 0, 0};
    auto maximum_bytes = encode(Response{id, Reply{maximum}});
    assert(maximum_bytes.size() < max_bytes);
    assert(std::get<Inspection>(std::get<Reply>(decode(maximum_bytes).get<Response>().result)).records.size() == max_records);
    maximum.records.push_back(mapping);
    rejects([&] { decode(encode(Response{id, Reply{maximum}})); });
    std::get<MappingRecord>(mapping).access.push_back({});
    rejects([&] { decode(encode(mapping)).get<Record>(); });
    Json invalid_scalar = AllocationRecord{id, true, false, 4096, 1, 1, 1, 0, 1};
    invalid_scalar["handle_types"] = uint64_t{1} << 32;
    rejects([&] { decode(encode(Json{{"allocation", invalid_scalar}})).get<Record>(); });
    invalid_scalar["handle_types"] = 1;
    invalid_scalar["size"] = -1;
    rejects([&] { decode(encode(Json{{"allocation", invalid_scalar}})).get<Record>(); });
    // A declared huge container must be refused without allocating its elements.
    rejects([&] { decode(std::array<uint8_t, 5>{0xdd, 0xff, 0xff, 0xff, 0xff}); });

    int pair[2];
    assert(::socketpair(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0, pair) == 0);
    Fd sender(pair[0]), receiver(pair[1]), zero(::open("/dev/zero", O_RDONLY | O_CLOEXEC));
    send(sender.get(), Request{Handshake{}}, zero.get());
    send(sender.get(), Request{Handshake{}});
    auto first = receive(receiver.get());
    assert(std::holds_alternative<Handshake>(first.body.get<Request>()));
    assert(::fcntl(first.descriptor.get(), F_GETFD) & FD_CLOEXEC);
    char byte = 1;
    assert(::read(first.descriptor.get(), &byte, 1) == 1 && byte == 0);
    assert(receive(receiver.get()).descriptor.get() < 0);
    auto bytes = encode(Request{Handshake{}});
    auto fragmented = std::async(std::launch::async, [&] {
        uint32_t size = bytes.size();
        for (unsigned shift = 0; shift < 32; shift += 8) {
            uint8_t value = size >> shift;
            write_all(sender.get(), std::span(&value, 1));
        }
        for (auto value : bytes) write_all(sender.get(), std::span(&value, 1));
    });
    assert(std::holds_alternative<Handshake>(receive(receiver.get()).body.get<Request>()));
    fragmented.get();
    std::array<uint8_t, 4> oversized{1, 0, 0, 2};
    write_all(sender.get(), oversized);
    rejects([&] { receive(receiver.get()); });

    Ticket ticket{id, Id::random(), "/tmp/cuinterpose-test.sock", std::monostate{}};
    auto ticket_fd = export_ticket(ticket);
    assert(Json(*read_ticket(ticket_fd.get())) == Json(ticket));
    assert(!read_ticket(zero.get()));
    for (size_t count : {1, 2, 4}) {
        int probe[2];
        assert(::socketpair(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0, probe) == 0);
        Fd reader(probe[0]), writer(probe[1]);
        timeval timeout{1, 0};
        assert(::setsockopt(reader.get(), SOL_SOCKET, SO_RCVTIMEO, &timeout, sizeof(timeout)) == 0);
        std::array<uint8_t, 5> invalid{1, 0, 0, 0, 0xc1};
        iovec data{invalid.data(), invalid.size()};
        alignas(cmsghdr) std::array<char, CMSG_SPACE(sizeof(int) * 4)> ancillary{};
        msghdr message{};
        message.msg_iov = &data; message.msg_iovlen = 1;
        message.msg_control = ancillary.data(); message.msg_controllen = CMSG_SPACE(sizeof(int) * count);
        auto header = CMSG_FIRSTHDR(&message);
        header->cmsg_level = SOL_SOCKET; header->cmsg_type = SCM_RIGHTS;
        header->cmsg_len = CMSG_LEN(sizeof(int) * count);
        std::fill_n(reinterpret_cast<int*>(CMSG_DATA(header)), count, writer.get());
        assert(::sendmsg(sender.get(), &message, MSG_NOSIGNAL) == 5);
        writer = Fd{};
        rejects([&] { receive(receiver.get()); });
        assert(::read(reader.get(), &byte, 1) == 0);
    }

    ExportCache cache;
    ExportCache::Key key{ResourceKind::Unicast, id}, other{ResourceKind::Multicast, id};
    cache.replace(key, zero.duplicate());
    rejects([&] { cache.acquire(other); });
    auto lease = cache.acquire(key);
    auto replacing = std::async(std::launch::async, [&] { cache.replace(key, zero.duplicate()); });
    assert(replacing.wait_for(20ms) == std::future_status::timeout);
    lease.reset(); replacing.get();
    lease = cache.acquire(key);
    cache.replace(other, zero.duplicate());
    { auto unrelated = cache.acquire(other); }
    auto clearing = std::async(std::launch::async, [&] { cache.clear(); });
    assert(clearing.wait_for(20ms) == std::future_status::timeout);
    rejects([&] { cache.acquire(key); });
    lease.reset(); clearing.get();
    rejects([&] { cache.acquire(key); });
    std::cout << "PASS typed protocol limits, binary identities, tickets, frames, descriptor ownership, and export leases\n";
}
