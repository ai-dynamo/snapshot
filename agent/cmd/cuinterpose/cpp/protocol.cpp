// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES.
// SPDX-License-Identifier: Apache-2.0

#include "protocol.hpp"
#include <algorithm>
#include <charconv>
#include <cstring>
#include <filesystem>
#include <fcntl.h>
#include <sys/mman.h>
#include <sys/random.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/un.h>
#include <unistd.h>
#include <utility>

namespace cuinterpose {
namespace {
// Library integer conversions allow narrowing. Comparing typed round-trip
// scalar values preserves strict v4 ranges, including nested access records.
void check_integer_ranges(const Json& input, const Json& typed) {
    if (typed.is_number_integer()) {
        if (!input.is_number_integer() || input.dump() != typed.dump())
            throw std::invalid_argument("integer outside declared range");
    } else if (typed.is_object()) {
        for (auto field = typed.begin(); field != typed.end(); ++field)
            check_integer_ranges(input.at(field.key()), field.value());
    } else if (typed.is_array()) {
        for (size_t i = 0; i < typed.size(); ++i) check_integer_ranges(input.at(i), typed[i]);
    }
}
template<class T> T checked_record(const Json& input) {
    T value = input.get<T>();
    check_integer_ranges(input, Json(value));
    return value;
}
void system_error(const char* action) {
    throw std::system_error(errno, std::generic_category(), action);
}
template<class E, size_t N>
void enum_encode(Json& out, E value, const std::array<std::pair<E, const char*>, N>& names) {
    for (auto [item, name] : names) if (item == value) { out = name; return; }
    throw std::invalid_argument("invalid enum value");
}
template<class E, size_t N>
void enum_decode(const Json& in, E& value, const std::array<std::pair<E, const char*>, N>& names) {
    for (auto [item, name] : names) if (in == name) { value = item; return; }
    throw std::invalid_argument("unknown enum name");
}
constexpr std::array operation_names{
    std::pair{Operation::PrepareMulticast, "prepare_multicast"},
    std::pair{Operation::SaveAllocations, "save_allocations"},
    std::pair{Operation::PrepareUnicast, "prepare_unicast"},
    std::pair{Operation::LoadAllocations, "load_allocations"},
    std::pair{Operation::RestoreUnicast, "restore_unicast"},
    std::pair{Operation::RestoreMulticastCreators, "restore_multicast_creators"},
    std::pair{Operation::RestoreMulticastImporters, "restore_multicast_importers"},
    std::pair{Operation::RestoreMulticastDevices, "restore_multicast_devices"},
    std::pair{Operation::RestoreMulticastBindings, "restore_multicast_bindings"}};
constexpr std::array resource_names{
    std::pair{ResourceKind::Unicast, "unicast"}, std::pair{ResourceKind::Multicast, "multicast"}};
constexpr std::array binding_names{
    std::pair{BindingVersion::V1, "v1"}, std::pair{BindingVersion::V2, "v2"}};

// Use the library's SAX parser so container sizes/depth are rejected before
// constructing a DOM. A bounded input alone does not bound declared lengths.
class BoundedDocument final : public nlohmann::json_sax<Json> {
    struct Frame { Json* value; std::string key; };
    std::vector<Frame> stack_;
    Json* append(Json value) {
        if (stack_.empty()) { document = std::move(value); return &document; }
        auto& frame = stack_.back();
        if (frame.value->is_array()) {
            if (frame.value->size() >= max_records) throw std::invalid_argument("too many entries");
            frame.value->push_back(std::move(value));
            return &frame.value->back();
        }
        if (frame.value->contains(frame.key)) throw std::invalid_argument("duplicate field");
        auto& target = (*frame.value)[frame.key];
        target = std::move(value);
        return &target;
    }
    bool start(bool object, size_t size) {
        if (size > max_records || stack_.size() >= 32) return false;
        stack_.push_back({append(object ? Json::object() : Json::array()), {}});
        return true;
    }
public:
    Json document;
    bool null() override { append(nullptr); return true; }
    bool boolean(bool value) override { append(value); return true; }
    bool number_integer(number_integer_t value) override { append(value); return true; }
    bool number_unsigned(number_unsigned_t value) override { append(value); return true; }
    bool number_float(number_float_t, const string_t&) override { return false; }
    bool string(string_t& value) override { append(std::move(value)); return true; }
    bool binary(binary_t& value) override { append(Json::binary(std::move(value))); return true; }
    bool key(string_t& value) override { stack_.back().key = std::move(value); return true; }
    bool start_object(size_t size) override { return start(true, size); }
    bool start_array(size_t size) override { return start(false, size); }
    bool end_object() override { stack_.pop_back(); return true; }
    bool end_array() override { stack_.pop_back(); return true; }
    bool parse_error(size_t, const std::string&, const nlohmann::detail::exception&) override { return false; }
};
void read_exact(int fd, std::span<uint8_t> bytes) {
    while (!bytes.empty()) {
        auto count = ::read(fd, bytes.data(), bytes.size());
        if (count < 0 && errno == EINTR) continue;
        if (count < 0) system_error("read");
        if (!count) throw std::runtime_error("unexpected end of stream");
        bytes = bytes.subspan(static_cast<size_t>(count));
    }
}
}

Id Id::random() {
    Id id;
    size_t offset = 0;
    while (offset < id.bytes.size()) {
        auto count = ::getrandom(id.bytes.data() + offset, id.bytes.size() - offset, 0);
        if (count < 0 && errno == EINTR) continue;
        if (count <= 0) system_error("getrandom");
        offset += static_cast<size_t>(count);
    }
    return id;
}
Id Id::parse(const std::string& text) {
    if (text.size() != 32 || !std::ranges::all_of(text, [](char c) {
            return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f');
        }))
        throw std::invalid_argument("participant ID must have 32 lowercase hex digits");
    Id id;
    for (size_t i = 0; i < id.bytes.size(); ++i) {
        unsigned value;
        auto [end, error] = std::from_chars(text.data()+2*i, text.data()+2*i+2, value, 16);
        if (error != std::errc{} || end != text.data()+2*i+2) throw std::invalid_argument("invalid participant ID");
        id.bytes[i] = static_cast<uint8_t>(value);
    }
    return id;
}
void to_json(Json& out, const Id& id) { out = Json::binary({id.bytes.begin(), id.bytes.end()}); }
void from_json(const Json& in, Id& id) {
    if (!in.is_binary() || in.get_binary().size() != id.bytes.size()) throw std::invalid_argument("invalid ID");
    std::copy(in.get_binary().begin(), in.get_binary().end(), id.bytes.begin());
}
void to_json(Json& out, Operation value) { enum_encode(out, value, operation_names); }
void from_json(const Json& in, Operation& value) { enum_decode(in, value, operation_names); }
void to_json(Json& out, ResourceKind value) { enum_encode(out, value, resource_names); }
void from_json(const Json& in, ResourceKind& value) { enum_decode(in, value, resource_names); }
void to_json(Json& out, BindingVersion value) { enum_encode(out, value, binding_names); }
void from_json(const Json& in, BindingVersion& value) { enum_decode(in, value, binding_names); }

void to_json(Json& out, const BindingSource& value) {
    if (auto memory = std::get_if<MemberRange>(&value)) out = {{"memory", *memory}};
    else {
        auto& address = std::get<AddressBinding>(value);
        out = {{"address", {{"address", address.address},
            {"tracked_member", address.tracked_member ? Json(*address.tracked_member) : Json(nullptr)}}}};
    }
}
void from_json(const Json& in, BindingSource& value) {
    if (in.size() != 1) throw std::invalid_argument("invalid binding source");
    if (in.contains("memory")) value = checked_record<MemberRange>(in.at("memory"));
    else {
        const auto& address = in.at("address");
        AddressBinding result{address.at("address").get<uint64_t>(), {}};
        if (!address.at("tracked_member").is_null()) result.tracked_member = checked_record<MemberRange>(address.at("tracked_member"));
        value = result;
    }
}
void to_json(Json& out, const Record& value) {
    static constexpr std::array names{"allocation", "mapping", "multicast",
        "multicast_device", "multicast_binding", "multicast_mapping"};
    std::visit([&](const auto& record) { out = {{names[value.index()], record}}; }, value);
}
void from_json(const Json& in, Record& value) {
    if (in.size() != 1) throw std::invalid_argument("invalid record tag");
    const auto& item = in.begin().value();
    const auto& tag = in.begin().key();
    if (tag == "allocation") value = checked_record<AllocationRecord>(item);
    else if (tag == "mapping") value = checked_record<MappingRecord>(item);
    else if (tag == "multicast") value = checked_record<MulticastRecord>(item);
    else if (tag == "multicast_device") value = checked_record<DeviceRecord>(item);
    else if (tag == "multicast_binding") value = checked_record<BindingRecord>(item);
    else if (tag == "multicast_mapping") value = checked_record<MulticastMappingRecord>(item);
    else throw std::invalid_argument("unknown record kind");
    std::visit([](const auto& record) {
        if constexpr (requires { record.access; })
            if (record.access.size() > max_access) throw std::invalid_argument("too many access grants");
    }, value);
}
void to_json(Json& out, const Resource& value) {
    if (auto multicast = std::get_if<MulticastResource>(&value))
        out = {{"kind", "multicast"}, {"devices", multicast->devices}, {"size", multicast->size},
            {"handle_types", multicast->handle_types}, {"flags", multicast->flags}};
    else out = {{"kind", "unicast"}};
}
void from_json(const Json& in, Resource& value) {
    auto kind = in.at("kind").get<ResourceKind>();
    if (kind == ResourceKind::Unicast) value = std::monostate{};
    else value = MulticastResource{in.at("devices"), in.at("size"), in.at("handle_types"), in.at("flags")};
}
void to_json(Json& out, const Request& value) {
    std::visit([&](const auto& request) {
        using T = std::decay_t<decltype(request)>;
        if constexpr (std::is_same_v<T, Handshake>) out = {{"kind", "handshake"}};
        else {
            out = {{"participant", request.participant}};
            if constexpr (std::is_same_v<T, Inspect>) out["kind"] = "inspect";
            else if constexpr (std::is_same_v<T, Execute>) {
                out["kind"] = "execute"; out["operation"] = request.operation;
            } else {
                out["kind"] = "export"; out["resource"] = request.resource; out["allocation"] = request.allocation;
            }
        }
    }, value);
}
void from_json(const Json& in, Request& value) {
    auto kind = in.at("kind").get<std::string>();
    if (kind == "handshake") value = Handshake{};
    else {
        auto id = in.at("participant").get<Id>();
        if (kind == "inspect") value = Inspect{id};
        else if (kind == "execute") value = Execute{id, in.at("operation").get<Operation>()};
        else if (kind == "export") value = Export{id, in.at("resource").get<ResourceKind>(), in.at("allocation").get<Id>()};
        else throw std::invalid_argument("unknown request");
    }
}
void to_json(Json& out, const Response& value) {
    out = {{"participant", value.participant}};
    if (auto failure = std::get_if<Failure>(&value.result)) out["result"] = {{"Err", failure->message}};
    else {
        const auto& reply = std::get<Reply>(value.result);
        std::visit([&](const auto& item) {
            using T = std::decay_t<decltype(item)>;
            Json body;
            if constexpr (std::is_same_v<T, Handshake>) body = "handshake";
            else if constexpr (std::is_same_v<T, Inspection>) body = {{"inspection", item}};
            else if constexpr (std::is_same_v<T, Completed>) body = {{"completed", item}};
            else body = {{"export", item}};
            out["result"] = {{"Ok", std::move(body)}};
        }, reply);
    }
}
void from_json(const Json& in, Response& value) {
    value.participant = in.at("participant").get<Id>();
    const auto& result = in.at("result");
    if (result.size() != 1) throw std::invalid_argument("invalid result");
    if (result.contains("Err")) value.result = Failure{result.at("Err").get<std::string>()};
    else {
        const auto& body = result.at("Ok");
        if (body == "handshake") value.result = Reply{Handshake{}};
        else if (body.size() != 1) throw std::invalid_argument("invalid reply");
        else if (body.contains("inspection")) value.result = Reply{checked_record<Inspection>(body.at("inspection"))};
        else if (body.contains("completed")) value.result = Reply{checked_record<Completed>(body.at("completed"))};
        else value.result = Reply{body.at("export").get<Exported>()};
    }
}
std::vector<uint8_t> encode(const Json& value) {
    auto bytes = Json::to_msgpack(Json{{"version", protocol_version}, {"body", value}});
    if (bytes.size() > max_bytes) throw std::invalid_argument("message exceeds limit");
    return bytes;
}
Json decode(std::span<const uint8_t> bytes) {
    if (bytes.size() > max_bytes) throw std::invalid_argument("message exceeds limit");
    BoundedDocument document;
    if (!Json::sax_parse(bytes.begin(), bytes.end(), &document, Json::input_format_t::msgpack, true))
        throw std::invalid_argument("invalid or oversized MessagePack");
    if (document.document.at("version") != protocol_version) throw std::invalid_argument("unsupported protocol version");
    return std::move(document.document.at("body"));
}
std::chrono::seconds timeout(std::optional<Operation> operation) {
    bool content = operation == Operation::SaveAllocations || operation == Operation::LoadAllocations;
    unsigned seconds = content ? 3600 : 10;
    if (auto text = std::getenv(content ? "SNAPSHOT_CARRIER_TIMEOUT_SECONDS" : "SNAPSHOT_CONTROL_TIMEOUT_SECONDS")) {
        unsigned value;
        auto [end, error] = std::from_chars(text, text + std::strlen(text), value);
        if (error == std::errc{} && !*end && value && value <= INT32_MAX) seconds = value;
    }
    return std::chrono::seconds(seconds);
}
Fd::~Fd() { if (value_ >= 0) ::close(value_); }
Fd::Fd(Fd&& other) noexcept : value_(other.release()) {}
Fd& Fd::operator=(Fd&& other) noexcept {
    if (this != &other) { Fd old(value_); value_ = other.release(); }
    return *this;
}
int Fd::release() noexcept { return std::exchange(value_, -1); }
Fd Fd::duplicate() const {
    int fd = ::fcntl(value_, F_DUPFD_CLOEXEC, 0);
    if (fd < 0) system_error("duplicate fd");
    return Fd(fd);
}
void write_all(int fd, std::span<const uint8_t> bytes) {
    while (!bytes.empty()) {
        auto count = ::write(fd, bytes.data(), bytes.size());
        if (count < 0 && errno == EINTR) continue;
        if (count <= 0) system_error("write");
        bytes = bytes.subspan(static_cast<size_t>(count));
    }
}
void send(int socket, const Json& message, int descriptor) {
    auto body = encode(message);
    std::array<uint8_t, 4> prefix;
    for (size_t i = 0; i < prefix.size(); ++i) prefix[i] = static_cast<uint8_t>(body.size() >> (i*8));
    iovec buffers[]{{prefix.data(), prefix.size()}, {body.data(), body.size()}};
    alignas(cmsghdr) char ancillary[CMSG_SPACE(sizeof(int))]{};
    msghdr header{};
    header.msg_iov = buffers; header.msg_iovlen = 2;
    if (descriptor >= 0) {
        header.msg_control = ancillary; header.msg_controllen = sizeof(ancillary);
        auto control = CMSG_FIRSTHDR(&header);
        control->cmsg_level = SOL_SOCKET; control->cmsg_type = SCM_RIGHTS; control->cmsg_len = CMSG_LEN(sizeof(int));
        std::memcpy(CMSG_DATA(control), &descriptor, sizeof(descriptor));
    }
    ssize_t count;
    do { count = ::sendmsg(socket, &header, MSG_NOSIGNAL); } while (count < 0 && errno == EINTR);
    if (count <= 0) system_error("sendmsg");
    size_t sent = static_cast<size_t>(count);
    // Descriptor rights belong only to the first sendmsg, never to a retry.
    auto remainder = [&](std::span<const uint8_t> bytes) {
        while (!bytes.empty()) {
            auto n = ::send(socket, bytes.data(), bytes.size(), MSG_NOSIGNAL);
            if (n < 0 && errno == EINTR) continue;
            if (n <= 0) system_error("send");
            bytes = bytes.subspan(static_cast<size_t>(n));
        }
    };
    if (sent < prefix.size()) { remainder(std::span(prefix).subspan(sent)); sent = prefix.size(); }
    remainder(std::span(body).subspan(sent - prefix.size()));
}
Received receive(int socket) {
    std::array<uint8_t, 4> prefix{};
    alignas(cmsghdr) char ancillary[CMSG_SPACE(sizeof(int)*2)]{};
    iovec buffer{prefix.data(), prefix.size()};
    msghdr header{};
    header.msg_iov = &buffer; header.msg_iovlen = 1;
    header.msg_control = ancillary; header.msg_controllen = sizeof(ancillary);
    ssize_t count;
    do { count = ::recvmsg(socket, &header, MSG_CMSG_CLOEXEC); } while (count < 0 && errno == EINTR);
    if (count <= 0) throw std::runtime_error("control socket closed");
    std::vector<Fd> descriptors;
    for (auto control = CMSG_FIRSTHDR(&header); control; control = CMSG_NXTHDR(&header, control))
        if (control->cmsg_level == SOL_SOCKET && control->cmsg_type == SCM_RIGHTS) {
            auto length = (control->cmsg_len - CMSG_LEN(0)) / sizeof(int);
            for (size_t i = 0; i < length; ++i) {
                int fd; std::memcpy(&fd, CMSG_DATA(control) + i*sizeof(int), sizeof(fd));
                descriptors.emplace_back(fd);
            }
        }
    if ((header.msg_flags & (MSG_CTRUNC | MSG_TRUNC)) || descriptors.size() > 1)
        throw std::runtime_error("invalid ancillary descriptors");
    read_exact(socket, std::span(prefix).subspan(static_cast<size_t>(count)));
    uint32_t size = 0;
    for (size_t i = 0; i < prefix.size(); ++i) size |= uint32_t(prefix[i]) << (i*8);
    if (size > max_bytes) throw std::invalid_argument("frame exceeds limit");
    std::vector<uint8_t> bytes(size);
    read_exact(socket, bytes);
    return {decode(bytes), descriptors.empty() ? Fd{} : std::move(descriptors.front())};
}
void validate_endpoint(const std::string& path) {
    if (path.empty() || path[0] != '/' || path.size() >= sizeof(sockaddr_un::sun_path) || path.find('\0') != std::string::npos)
        throw std::invalid_argument("invalid control socket path");
}
Fd connect(const std::string& path, std::chrono::seconds duration) {
    validate_endpoint(path);
    Fd socket(::socket(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0));
    if (socket.get() < 0) system_error("socket");
    timeval value{duration.count(), 0};
    if (::setsockopt(socket.get(), SOL_SOCKET, SO_RCVTIMEO, &value, sizeof(value)) ||
        ::setsockopt(socket.get(), SOL_SOCKET, SO_SNDTIMEO, &value, sizeof(value))) system_error("socket timeout");
    sockaddr_un address{};
    address.sun_family = AF_UNIX;
    std::memcpy(address.sun_path, path.c_str(), path.size()+1);
    if (::connect(socket.get(), reinterpret_cast<sockaddr*>(&address), sizeof(address))) system_error("connect");
    return socket;
}
std::vector<uint8_t> read_file(const std::string& path) {
    Fd file(::open(path.c_str(), O_RDONLY | O_CLOEXEC));
    if (file.get() < 0) system_error("open state");
    struct stat info{};
    if (::fstat(file.get(), &info)) system_error("stat state");
    if (info.st_size < 0 || uint64_t(info.st_size) > max_bytes) throw std::invalid_argument("state exceeds limit");
    std::vector<uint8_t> bytes(static_cast<size_t>(info.st_size));
    read_exact(file.get(), bytes);
    return bytes;
}
void write_atomic(const std::string& path, std::span<const uint8_t> bytes) {
    auto directory = std::filesystem::path(path).parent_path();
    auto temporary = (directory / ".cuinterpose-state-XXXXXX").string();
    Fd file(::mkstemp(temporary.data()));
    if (file.get() < 0) system_error("create state temporary");
    try {
        write_all(file.get(), bytes);
        if (::fsync(file.get()) || ::rename(temporary.c_str(), path.c_str())) system_error("publish state");
        Fd parent(::open(directory.c_str(), O_RDONLY | O_DIRECTORY | O_CLOEXEC));
        if (parent.get() < 0 || ::fsync(parent.get())) system_error("sync state directory");
    } catch (...) { ::unlink(temporary.c_str()); throw; }
}
void Ticket::validate() const {
    validate_endpoint(endpoint);
    if (allocation == Id{}) throw std::invalid_argument("empty allocation ID");
    if (auto group = std::get_if<MulticastResource>(&resource))
        if (!group->devices || !group->size || group->handle_types != 1) throw std::invalid_argument("invalid multicast ticket");
}
Fd export_ticket(const Ticket& ticket) {
    ticket.validate();
    auto bytes = encode(ticket);
    if (bytes.size() + 4 > max_ticket_bytes) throw std::invalid_argument("ticket exceeds limit");
    Fd fd(::memfd_create("cuinterpose-ticket", MFD_CLOEXEC | MFD_ALLOW_SEALING));
    if (fd.get() < 0) system_error("memfd_create");
    const std::array<uint8_t, 4> magic{'C', 'M', 'V', 'D'};
    write_all(fd.get(), magic); write_all(fd.get(), bytes);
    if (::fcntl(fd.get(), F_ADD_SEALS, F_SEAL_SEAL | F_SEAL_WRITE | F_SEAL_GROW | F_SEAL_SHRINK)) system_error("seal ticket");
    return fd;
}
std::optional<Ticket> read_ticket(int descriptor) {
    if (descriptor < 0) throw std::invalid_argument("negative import descriptor");
    std::array<char, 4> magic;
    if (::pread(descriptor, magic.data(), magic.size(), 0) != 4 || std::memcmp(magic.data(), "CMVD", 4)) return {};
    struct stat info{};
    if (::fstat(descriptor, &info)) system_error("stat ticket");
    constexpr int seals = F_SEAL_SEAL | F_SEAL_WRITE | F_SEAL_GROW | F_SEAL_SHRINK;
    int actual_seals = ::fcntl(descriptor, F_GET_SEALS);
    if (info.st_size < 4 || info.st_size > static_cast<off_t>(max_ticket_bytes) ||
        actual_seals < 0 || (actual_seals & seals) != seals) throw std::invalid_argument("invalid ticket");
    std::vector<uint8_t> bytes(static_cast<size_t>(info.st_size)-4);
    if (::pread(descriptor, bytes.data(), bytes.size(), 4) != static_cast<ssize_t>(bytes.size())) system_error("read ticket");
    auto ticket = checked_record<Ticket>(decode(bytes));
    ticket.validate();
    return ticket;
}
}
