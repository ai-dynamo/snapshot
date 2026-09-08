// SPDX-License-Identifier: Apache-2.0

#include "internal.hpp"

#include <errno.h>
#include <sys/socket.h>
#include <unistd.h>

#include <array>
#include <cstring>
#include <string>

#include "extmem.pb.h"

namespace {
int Send(int socket, int status, int fd = -1)
{
	extmem_resp response;
	response.set_status(status);
	const std::string bytes = response.SerializeAsString();
	iovec iov{const_cast<char *>(bytes.data()), bytes.size()};
	std::array<char, CMSG_SPACE(sizeof(int))> control{};
	msghdr message{};
	message.msg_iov = &iov;
	message.msg_iovlen = 1;
	if (fd >= 0) {
		message.msg_control = control.data();
		message.msg_controllen = control.size();
		cmsghdr *header = CMSG_FIRSTHDR(&message);
		header->cmsg_level = SOL_SOCKET;
		header->cmsg_type = SCM_RIGHTS;
		header->cmsg_len = CMSG_LEN(sizeof(fd));
		std::memcpy(CMSG_DATA(header), &fd, sizeof(fd));
	}
	const ssize_t sent = sendmsg(socket, &message, MSG_NOSIGNAL);
	return sent == static_cast<ssize_t>(bytes.size()) ? 0 : sent < 0 ? -errno : -EIO;
}

void CloseOutputs(criu_provider_dump_session *session)
{
	for (auto &[_, output] : session->outputs) if (output.fd >= 0) close(output.fd);
	session->outputs.clear();
}

int Abort(criu_provider_dump_session *session)
{
	CloseOutputs(session);
	session->ops.abort(session->context);
	session->finished = true;
	return 0;
}

int Commit(criu_provider_dump_session *session)
{
	for (const auto &[_, output] : session->outputs)
		if (fsync(output.fd) < 0) return -errno;
	if (const int result = session->ops.commit(session->context, session->plan)) return result;
	CloseOutputs(session);
	session->finished = true;
	return 0;
}
}

int ServeDumpProtocol(criu_provider_dump_session *session, int socket)
{
	if (!session || session->finished) return -EINVAL;
	for (;;) {
		std::array<char, 8192> bytes;
		const ssize_t received = recv(socket, bytes.data(), bytes.size(), 0);
		if (received == 0) { Abort(session); return -ECONNRESET; }
		if (received < 0) { Abort(session); return -errno; }
		extmem_req request;
		if (!request.ParseFromArray(bytes.data(), received) || !request.IsInitialized()) {
			if (const int error = Send(socket, -EBADMSG)) return error;
			continue;
		}
		int result = 0;
		if (request.op() != EXTMEM_INIT && !session->active) {
			result = Send(socket, -EPROTO);
			if (result) return result;
			continue;
		}
		switch (request.op()) {
		case EXTMEM_INIT:
			if (session->active) { result = Send(socket, -EPROTO); break; }
			session->active = true;
			result = Send(socket, 0); break;
		case EXTMEM_OPEN_IMAGE: {
			if (!request.has_open_image()) { result = Send(socket, -EBADMSG); break; }
			criu_provider::v1::Image rule_image;
			const auto *image = FindImage(session->plan, request.open_image().name(), &rule_image);
			if (!image) { result = Send(socket, -EPROTO); break; }
			if (image->dump_mode() != criu_provider::v1::Image::DUMP_PROVIDER_OUTPUT) {
				result = Send(socket, -ENOTSUP); break;
			}
			const std::string &name = request.open_image().name();
			int fd = -1;
			const auto existing = session->outputs.find(name);
			if (existing != session->outputs.end()) fd = dup(existing->second.fd);
			else fd = session->ops.open_output_image(session->context, name.c_str(),
				request.open_image().flags());
			if (fd < 0) { result = Send(socket, fd); break; }
			if (existing == session->outputs.end()) session->outputs.emplace(name, ProviderFd{name, fd});
			result = Send(socket, 0, fd);
			if (existing != session->outputs.end()) close(fd);
			break;
		}
		case EXTMEM_COMMIT:
			result = Commit(session);
			if (result) Abort(session);
			if (const int error = Send(socket, result)) return error;
			return result;
		case EXTMEM_ABORT:
			Abort(session);
			if (const int error = Send(socket, 0)) return error;
			return -ECANCELED;
		default:
			result = Send(socket, -ENOTSUP); break;
		}
		if (result) { Abort(session); return result; }
	}
}

extern "C" int criu_provider_dump_session_serve(criu_provider_dump_session *session,
	int socket)
{
	return ServeDumpProtocol(session, socket);
}
