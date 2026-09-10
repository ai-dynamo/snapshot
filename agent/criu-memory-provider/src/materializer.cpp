// SPDX-License-Identifier: Apache-2.0

#include "internal.hpp"

#include <errno.h>
#include <linux/memfd.h>
#include <sys/syscall.h>
#include <unistd.h>

#include <map>
#include <vector>

namespace {
int CreateFds(criu_provider_session *session, std::map<std::string, int> *fds)
{
	for (const auto &object : session->plan->value.objects()) {
		const int fd = syscall(SYS_memfd_create, "criu-provider",
			MFD_CLOEXEC | MFD_ALLOW_SEALING);
		if (fd < 0) return -errno;
		if (ftruncate(fd, static_cast<off_t>(object.length())) < 0) {
			const int error = errno;
			close(fd);
			return -error;
		}
		fds->emplace(object.key(), fd);
	}
	return 0;
}

void CloseFds(std::map<std::string, int> *fds)
{
	for (auto &[_, fd] : *fds) close(fd);
	fds->clear();
}

void KeepFds(criu_provider_session *session, std::map<std::string, int> *fds)
{
	for (auto &[key, fd] : *fds)
		session->objects.emplace(key, ProviderFd{key, fd});
	fds->clear();
	session->prepared = true;
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
	int result = CreateFds(session, &fds);
	if (result) {
		CloseFds(&fds);
		return result;
	}
	std::vector<criu_provider_write_range> ranges;
	std::map<std::string, enum criu_provider_image_role> roles;
	for (const auto &image : session->plan->value.images())
		roles.emplace(image.name(),
			static_cast<enum criu_provider_image_role>(image.role()));
	for (const auto &chunk : session->plan->value.chunks())
		for (const auto &placement : chunk.placements())
			ranges.push_back({chunk.image().c_str(), roles.at(chunk.image()),
				chunk.source_offset(),
				chunk.stored_length(), fds.at(placement.object_key()),
				placement.destination_offset()});
	result = session->restore_ops.write_ranges(session->restore_context,
		ranges.data(), ranges.size());
	if (result) {
		CloseFds(&fds);
		return result;
	}
	KeepFds(session, &fds);
	return 0;
}
