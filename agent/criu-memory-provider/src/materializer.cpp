// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "internal.hpp"

#include <errno.h>
#include <linux/memfd.h>
#include <sys/syscall.h>
#include <unistd.h>

#include <array>
#include <map>

namespace {
constexpr size_t kCopySize = 1 << 20;

int WriteAll(int fd, const void *data, size_t size, uint64_t offset)
{
	const ssize_t written = pwrite(fd, data, size, static_cast<off_t>(offset));
	return written == static_cast<ssize_t>(size) ? 0 : written < 0 ? -errno : -EIO;
}

int Fill(int fd, const criu_provider_session *session, const char *image,
	uint64_t source_offset, uint64_t length, uint64_t destination_offset)
{
	std::array<char, kCopySize> buffer;
	for (uint64_t copied = 0; copied < length;) {
		const size_t size = std::min<uint64_t>(buffer.size(), length - copied);
		int result = session->source_ops.read_range(session->source_context,
			image, source_offset + copied, buffer.data(), size);
		if (result) return result;
		result = WriteAll(fd, buffer.data(), size, destination_offset + copied);
		if (result) return result;
		copied += size;
	}
	return 0;
}
}

void ReleaseSessionFds(criu_provider_session *session)
{
	for (auto &[_, object] : session->objects)
		if (object.fd >= 0) close(object.fd);
	session->objects.clear();
}

extern "C" int criu_provider_session_prepare(criu_provider_session *session)
{
	if (!session || session->prepared) return -EINVAL;
	std::map<std::string, int> fds;
	for (const auto &object : session->plan->value.objects()) {
		const int fd = syscall(SYS_memfd_create, "criu-provider", MFD_CLOEXEC | MFD_ALLOW_SEALING);
		if (fd < 0) goto fail;
		if (ftruncate(fd, static_cast<off_t>(object.length())) < 0) { close(fd); goto fail; }
		fds.emplace(object.key(), fd);
	}
	for (const auto &image : session->plan->value.images()) {
		if (image.role() != criu_provider::v1::Image::METADATA) continue;
		const int fd = syscall(SYS_memfd_create, "criu-provider-image",
			MFD_CLOEXEC | MFD_ALLOW_SEALING);
		if (fd < 0) goto fail;
		if (ftruncate(fd, static_cast<off_t>(image.size())) < 0) {
			close(fd);
			goto fail;
		}
		fds.emplace(image.name(), fd);
		const int result = Fill(fd, session, image.name().c_str(), 0,
			image.size(), 0);
		if (result) { errno = -result; goto fail; }
	}
	for (const auto &chunk : session->plan->value.chunks()) {
		std::array<char, kCopySize> buffer;
		for (uint64_t copied = 0; copied < chunk.stored_length();) {
			const size_t size = std::min<uint64_t>(buffer.size(), chunk.stored_length() - copied);
			int result = session->source_ops.read_range(session->source_context,
				chunk.image().c_str(), chunk.source_offset() + copied, buffer.data(), size);
			if (result) { errno = -result; goto fail; }
			for (const auto &placement : chunk.placements()) {
				result = WriteAll(fds.at(placement.object_key()), buffer.data(), size,
					placement.destination_offset() + copied);
				if (result) { errno = -result; goto fail; }
			}
			copied += size;
		}
	}
	for (auto &[key, fd] : fds) session->objects.emplace(key, ProviderFd{key, fd});
	session->prepared = true;
	return 0;
fail:
	{ const int error = errno; for (auto &[_, fd] : fds) close(fd); return -error; }
}
