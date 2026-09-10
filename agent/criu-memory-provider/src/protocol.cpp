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

int ObjectFd(criu_provider_session *session, const std::string &key)
{
	const auto object = session->objects.find(key);
	if (object == session->objects.end()) return -1;
	return dup(object->second.fd);
}

}

int ServeProtocol(criu_provider_session *session, int socket)
{
	if (!session || !session->prepared) return -EINVAL;
	for (;;) {
		std::array<char, 1 << 20> bytes;
		const ssize_t received = recv(socket, bytes.data(), bytes.size(), 0);
		if (received == 0) return -ECONNRESET;
		if (received < 0) return -errno;
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
		case EXTMEM_WAIT_READY:
			result = Send(socket, 0); break;
		case EXTMEM_OPEN_IMAGE: {
			if (!request.has_open_image()) { result = Send(socket, -EBADMSG); break; }
			const std::string &name = request.open_image().name();
			criu_provider::v1::Image rule_image;
			const auto *image = FindImage(session->plan, name, &rule_image);
			if (!image) { result = Send(socket, -EPROTO); break; }
			if (image->restore_mode() == criu_provider::v1::Image::RESTORE_LOCAL ||
				image->restore_mode() == criu_provider::v1::Image::RESTORE_LOCAL_FALLBACK) {
				result = Send(socket, -ENOTSUP); break;
			}
			int fd = ObjectFd(session, name);
			if (fd < 0 && image->restore_mode() ==
				criu_provider::v1::Image::RESTORE_READY_LOCAL && session->restore_ops.open_ready_image)
				fd = session->restore_ops.open_ready_image(session->restore_context, name.c_str(),
					request.open_image().flags());
			result = fd < 0 ? Send(socket, -EPROTO) : Send(socket, 0, fd);
			if (fd >= 0) close(fd);
			break;
		}
		case EXTMEM_GET_VMA: {
			if (!request.has_get_vma()) { result = Send(socket, -EBADMSG); break; }
			const auto &vma = request.get_vma();
			int fd = -1;
			bool known = false;
			for (const auto &object : session->plan->value.objects())
				if (object.kind() == criu_provider::v1::Object::PRIVATE_VMA &&
					object.pid() == vma.pid() && object.vma_id() == vma.vma_id()) {
					known = true;
					if (object.start() == vma.vaddr() && object.length() == vma.length())
						fd = ObjectFd(session, object.key());
				}
			result = fd < 0 ? Send(socket, known ? -EPROTO : -ENOTSUP) : Send(socket, 0, fd);
			if (fd >= 0) close(fd);
			break;
		}
		case EXTMEM_GET_SHARED: {
			if (!request.has_get_shared()) { result = Send(socket, -EBADMSG); break; }
			const auto &shared = request.get_shared();
			int fd = -1;
			bool known = false;
			for (const auto &object : session->plan->value.objects())
				if (object.kind() == criu_provider::v1::Object::SHARED &&
					object.shmid() == shared.shmid()) {
					known = true;
					if (object.length() == shared.length())
						fd = ObjectFd(session, object.key());
				}
			result = fd < 0 ? Send(socket, known ? -EPROTO : -ENOTSUP) : Send(socket, 0, fd);
			if (fd >= 0) close(fd);
			break;
		}
		case EXTMEM_COMMIT:
			result = Send(socket, 0);
			if (!result) ReleaseSessionFds(session);
			return result;
		case EXTMEM_ABORT:
			result = Send(socket, 0);
			if (!result) ReleaseSessionFds(session);
			return result ? result : -ECANCELED;
		default:
			result = Send(socket, -ENOTSUP); break;
		}
		if (result) return result;
	}
}

extern "C" int criu_provider_session_serve(criu_provider_session *session, int socket)
{
	return ServeProtocol(session, socket);
}
