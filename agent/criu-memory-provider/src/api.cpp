// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#include "internal.hpp"

#include <errno.h>
#include <fcntl.h>
#include <unistd.h>

#include <filesystem>
#include <fstream>
#include <memory>
#include <cstring>

#include <google/protobuf/util/json_util.h>

namespace {
int EncodePlanJson(const criu_provider::v1::Plan &plan, std::string *json)
{
	google::protobuf::util::JsonPrintOptions options;
	options.add_whitespace = true;
	options.always_print_primitive_fields = true;
	options.preserve_proto_field_names = true;
	return google::protobuf::util::MessageToJsonString(plan, json, options).ok() ? 0 : -EINVAL;
}

int DecodePlan(const std::string &bytes, criu_provider::v1::Plan *plan)
{
	const auto first = bytes.find_first_not_of(" \t\r\n");
	if (first == std::string::npos) return -EINVAL;
	if (bytes[first] == '{') {
		google::protobuf::util::JsonParseOptions options;
		options.ignore_unknown_fields = false;
		return google::protobuf::util::JsonStringToMessage(bytes, plan, options).ok() ? 0 : -EINVAL;
	}
	return plan->ParseFromString(bytes) ? 0 : -EINVAL;
}
}

extern "C" {
int criu_provider_plan_from_checkpoint(const char *directory, criu_provider_plan **out)
{
	return CreatePlanFromCheckpoint(directory, out);
}

int criu_provider_plan_write(const criu_provider_plan *plan, const char *path)
{
	if (!plan || !path || ValidatePlan(plan->value))
		return -EINVAL;
	const std::filesystem::path target(path);
	const std::filesystem::path temporary = target.string() + ".tmp";
	std::string bytes;
	if (const int result = EncodePlanJson(plan->value, &bytes)) return result;
	const int fd = open(temporary.c_str(), O_WRONLY | O_CREAT | O_TRUNC | O_CLOEXEC, 0600);
	if (fd < 0)
		return -errno;
	size_t offset = 0;
	int saved = 0;
	while (offset < bytes.size()) {
		const ssize_t written = write(fd, bytes.data() + offset, bytes.size() - offset);
		if (written < 0) { if (errno == EINTR) continue; saved = -errno; break; }
		if (written == 0) { saved = -EIO; break; }
		offset += written;
	}
	if (!saved && fsync(fd) < 0) saved = -errno;
	close(fd);
	if (saved) { unlink(temporary.c_str()); return saved; }
	if (rename(temporary.c_str(), target.c_str()) < 0) { const int error = -errno; unlink(temporary.c_str()); return error; }
	const int directory = open(target.parent_path().c_str(), O_RDONLY | O_DIRECTORY | O_CLOEXEC);
	if (directory >= 0) { fsync(directory); close(directory); }
	return 0;
}

int criu_provider_plan_load(const char *path, criu_provider_plan **out)
{
	if (!path || !out)
		return -EINVAL;
	*out = nullptr;
	std::ifstream input(path, std::ios::binary);
	if (!input)
		return -errno;
	const std::string bytes((std::istreambuf_iterator<char>(input)),
		std::istreambuf_iterator<char>());
	auto plan = std::make_unique<criu_provider_plan>();
	if (DecodePlan(bytes, &plan->value) || ValidatePlan(plan->value))
		return -EINVAL;
	*out = plan.release();
	return 0;
}

void criu_provider_plan_destroy(criu_provider_plan *plan) { delete plan; }

int criu_provider_plan_requirements(const criu_provider_plan *plan,
		struct criu_provider_requirements *out)
{
	if (!plan || !out || ValidatePlan(plan->value)) return -EINVAL;
	const auto &in = plan->value.requirements();
	*out = {in.stored_bytes(), in.materialized_bytes(), in.logical_fd_bytes(),
		in.metadata_bytes(), in.workspace_bytes(), in.fd_count()};
	return 0;
}

int criu_provider_plan_enumerate_source_ranges(const criu_provider_plan *plan,
		criu_provider_source_range_callback callback, void *context)
{
	if (!plan || !callback || ValidatePlan(plan->value)) return -EINVAL;
	for (const auto &chunk : plan->value.chunks()) {
		const int result = callback(context, chunk.image().c_str(), chunk.source_offset(),
			chunk.stored_length());
		if (result) return result;
	}
	return 0;
}

int criu_provider_plan_enumerate_images(const criu_provider_plan *plan,
		criu_provider_image_callback callback, void *context)
{
	if (!plan || !callback || ValidatePlan(plan->value)) return -EINVAL;
	for (const auto &image : plan->value.images()) {
		const int result = callback(context, image.name().c_str(), image.size(),
			static_cast<enum criu_provider_image_role>(image.role()));
		if (result) return result;
	}
	return 0;
}

int criu_provider_dump_plan_create(uint64_t page_size, criu_provider_plan **out)
{
	if (!page_size || !out) return -EINVAL;
	*out = nullptr;
	auto plan = std::make_unique<criu_provider_plan>();
	plan->value.set_format_major(1);
	plan->value.set_page_size(page_size);
	AddDumpImageRules(plan->value);
	*out = plan.release();
	return 0;
}

int criu_provider_dump_plan_add_image(criu_provider_plan *plan, const char *name,
		enum criu_provider_restore_mode restore, enum criu_provider_dump_mode dump)
{
	if (!plan || !name || !*name || std::strchr(name, '/') || std::strchr(name, '\\'))
		return -EINVAL;
	for (const auto &image : plan->value.images()) if (image.name() == name) return -EEXIST;
	auto *image = plan->value.add_images();
	image->set_name(name);
	image->set_restore_mode(static_cast<criu_provider::v1::Image::RestoreMode>(restore));
	image->set_dump_mode(static_cast<criu_provider::v1::Image::DumpMode>(dump));
	return 0;
}

int criu_provider_session_create(const criu_provider_plan *plan,
		const struct criu_provider_source_ops *ops, void *context,
		criu_provider_session **out)
{
	if (!plan || !ops || !ops->read_range || !ops->open_ready_image || !out || ValidatePlan(plan->value))
		return -EINVAL;
	auto *session = new criu_provider_session();
	session->plan = plan;
	session->source_ops = *ops;
	session->source_context = context;
	*out = session;
	return 0;
}

void criu_provider_session_destroy(criu_provider_session *session)
{
	if (!session) return;
	ReleaseSessionFds(session);
	delete session;
}

int criu_provider_dump_session_create(const criu_provider_plan *plan,
		const criu_provider_dump_ops *ops, void *context, criu_provider_dump_session **out)
{
	if (!plan || !ops || !ops->open_output_image || !ops->commit || !ops->abort ||
		!out || ValidatePlan(plan->value)) return -EINVAL;
	auto *session = new criu_provider_dump_session();
	session->plan = plan;
	session->ops = *ops;
	session->context = context;
	*out = session;
	return 0;
}

void criu_provider_dump_session_destroy(criu_provider_dump_session *session)
{
	if (!session) return;
	if (!session->finished) session->ops.abort(session->context);
	for (auto &[_, output] : session->outputs) if (output.fd >= 0) close(output.fd);
	delete session;
}
}
