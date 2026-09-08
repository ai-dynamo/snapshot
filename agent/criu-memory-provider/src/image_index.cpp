// SPDX-License-Identifier: Apache-2.0

#include "internal.hpp"

#include <errno.h>
#include <unistd.h>

#include <filesystem>
#include <fstream>
#include <algorithm>

#include "criu-images/mm.pb.h"
#include "criu-images/memfd.pb.h"
#include "criu-images/fdinfo.pb.h"
#include "criu-images/pagemap.pb.h"

namespace {
constexpr uint32_t kCommonMagic = 0x54564319;
constexpr uint32_t kMmMagic = 0x57492820;
constexpr uint32_t kPagemapMagic = 0x56084025;
constexpr uint32_t kMemfdMagic = 0x48453499;
constexpr uint32_t kFilesMagic = 0x56303138;
constexpr uint32_t kMemfdFileType = 18;
constexpr unsigned kPresent = 1U << 2;
constexpr unsigned kRegular = 1U << 0;
constexpr unsigned kAnonPrivate = 1U << 9;
constexpr unsigned kExcluded = (1U << 1) | (1U << 2) | (1U << 3) | (1U << 12);
constexpr unsigned kAnonShared = 1U << 8;
constexpr unsigned kMemfd = 1U << 14;

template <typename T> int Read(std::ifstream &in, T *message, bool *eof)
{
	uint32_t size = 0; in.read(reinterpret_cast<char *>(&size), sizeof(size));
	if (in.eof()) { *eof = true; return 0; }
	if (!in || size > (16U << 20)) return -EINVAL;
	std::string bytes(size, '\0'); in.read(bytes.data(), size);
	if (!in || !message->ParseFromString(bytes) || !message->IsInitialized()) return -EINVAL;
	*eof = false; return 0;
}

int OpenImage(std::ifstream *input, const std::filesystem::path& path, uint32_t magic)
{
	input->open(path, std::ios::binary);
	uint32_t header[2]{};
	input->read(reinterpret_cast<char *>(header), sizeof(header));
	return *input && header[0] == kCommonMagic && header[1] == magic ? 0 : -EINVAL;
}

bool Eligible(const vma_entry &vma)
{
	return (vma.status() & (kRegular | kAnonPrivate)) == (kRegular | kAnonPrivate) &&
		!(vma.status() & kExcluded) && !(vma.flags() & 0x0100) &&
		!(vma.has_madv() && (vma.madv() & (1ULL << 18)));
}

int AddImage(criu_provider::v1::Plan& plan, const std::filesystem::path& path,
	criu_provider::v1::Image::Role role)
{
	for (const auto& image : plan.images()) if (image.name() == path.filename()) return 0;
	std::error_code error;
	const uint64_t size = std::filesystem::file_size(path, error);
	if (error) return -EIO;
	auto* image = plan.add_images(); image->set_name(path.filename()); image->set_size(size); image->set_role(role);
	if (image->name() == "inventory.img") {
		image->set_role(criu_provider::v1::Image::BOOTSTRAP_LOCAL);
		image->set_restore_mode(criu_provider::v1::Image::RESTORE_LOCAL);
		image->set_dump_mode(criu_provider::v1::Image::DUMP_LOCAL);
	} else if (role == criu_provider::v1::Image::PAGES) {
		image->set_restore_mode(criu_provider::v1::Image::RESTORE_PROVIDER_FD);
		image->set_dump_mode(criu_provider::v1::Image::DUMP_PROVIDER_OUTPUT);
	} else {
		image->set_restore_mode(criu_provider::v1::Image::RESTORE_READY_LOCAL);
		image->set_dump_mode(criu_provider::v1::Image::DUMP_PROVIDER_OUTPUT);
	}
	return 0;
}

void AddRule(criu_provider::v1::Plan& plan, const char* prefix, const char* suffix,
	criu_provider::v1::ImageRule::Selector selector,
	criu_provider::v1::Image::RestoreMode restore,
	criu_provider::v1::Image::DumpMode dump)
{
	auto* rule = plan.add_image_rules();
	rule->set_prefix(prefix);
	rule->set_suffix(suffix);
	rule->set_selector(selector);
	rule->set_restore_mode(restore);
	rule->set_dump_mode(dump);
}

void AddCriuRules(criu_provider::v1::Plan& plan,
	criu_provider::v1::Image::DumpMode dump)
{
	// This is our small, data-only compatibility table for the CRIU selector
	// revision. It is derived from image-desc.c; it is not CRIU implementation.
	using Rule = criu_provider::v1::ImageRule;
	for (const auto& name : {"reg-files.img", "ext-files.img", "ns-files.img",
		"eventfd.img", "eventpoll.img", "eventpoll-tfd.img", "signalfd.img",
		"inotify.img", "inotify-wd.img", "fanotify.img", "fanotify-mark.img",
		"pipes.img", "pipes-data.img", "fifo.img", "fifo-data.img", "pstree.img",
		"unixsk.img", "inetsk.img", "packetsk.img", "netlinksk.img", "sk-queues.img",
		"remap-fpath.img", "memfd.img", "tty.img", "tty-info.img", "tty-data.img",
		"filelocks.img", "tunfile.img", "cgroup.img", "timerfd.img", "cpuinfo.img",
		"seccomp.img", "bpfmap-file.img", "bpfmap-data.img", "apparmor.img", "pidfd.img",
		"files.img"})
		AddRule(plan, name, "", Rule::EXACT,
			criu_provider::v1::Image::RESTORE_LOCAL_FALLBACK, dump);
	for (const auto& name : {"fdinfo-", "core-", "ids-", "mm-", "vmas-", "sigacts-",
		"itimers-", "posix-timers-", "creds-", "utsns-", "ipcns-var-", "ipcns-shm-",
		"ipcns-msg-", "ipcns-sem-", "fs-", "mountpoints-", "netdev-", "netns-",
		"ifaddr-", "route-", "route6-", "rule-", "rule6-", "iptables-", "ip6tables-",
		"nftables-", "autofs-", "rlimit-", "pagemap-", "pagemap-shmem-", "pages-", "pages-shmem-", "signal-s-",
		"signal-p-", "userns-", "netns-ct-", "netns-exp-", "timens-", "pidns-"})
		AddRule(plan, name, ".img", Rule::DECIMAL,
			criu_provider::v1::Image::RESTORE_LOCAL_FALLBACK, dump);
	for (const auto& name : {"ghost-file-", "tcp-stream-"})
		AddRule(plan, name, ".img", Rule::HEX,
			criu_provider::v1::Image::RESTORE_LOCAL_FALLBACK, dump);
	for (const auto& name : {"tmpfs-", "tmpfs-dev-"})
		AddRule(plan, name, ".tar.gz.img", Rule::DECIMAL,
			criu_provider::v1::Image::RESTORE_LOCAL_FALLBACK, dump);
}

int IndexShared(const std::filesystem::path& root, uint64_t shmid, uint64_t known_size, uint32_t seals,
	criu_provider::v1::Plan& plan)
{
	const std::string key = "shared:" + std::to_string(shmid);
	for (const auto& object : plan.objects()) if (object.key() == key) return 0;
	const auto pmap = root / ("pagemap-shmem-" + std::to_string(shmid) + ".img");
	std::ifstream input;
	if (OpenImage(&input, pmap, kPagemapMagic)) return -EINVAL;
	pagemap_head head; bool eof;
	if (Read(input, &head, &eof) || eof) return -EINVAL;
	const auto pages = root / ("pages-" + std::to_string(head.pages_id()) + ".img");
	std::error_code error;
	if (!std::filesystem::is_regular_file(pages, error)) return -EINVAL;
	if (const int status = AddImage(plan, pmap, criu_provider::v1::Image::METADATA)) return status;
	if (const int status = AddImage(plan, pages, criu_provider::v1::Image::PAGES)) return status;
	auto* object = plan.add_objects(); object->set_key(key); object->set_kind(criu_provider::v1::Object::SHARED); object->set_shmid(shmid);
	uint64_t source_offset = 0;
	for (;;) {
		pagemap_entry entry; if (Read(input, &entry, &eof)) return -EINVAL; if (eof) break;
		if (entry.in_parent() || entry.has_blocks()) return -ENOTSUP;
		const uint64_t count = entry.has_nr_pages() ? entry.nr_pages() : entry.compat_nr_pages();
		if (!count || count > UINT64_MAX / plan.page_size()) return -EINVAL;
		const uint64_t length = count * plan.page_size();
		if (entry.vaddr() > UINT64_MAX - length) return -EINVAL;
	object->set_length(std::max(object->length(), entry.vaddr() + length));
		if (!entry.has_flags() || !(entry.flags() & kPresent)) continue;
		auto* chunk = plan.add_chunks(); chunk->set_image(pages.filename()); chunk->set_source_offset(source_offset);
		chunk->set_stored_length(length); chunk->set_decoded_length(length);
		auto* placement = chunk->add_placements(); placement->set_object_key(key); placement->set_destination_offset(entry.vaddr());
		if (source_offset > UINT64_MAX - length) return -EINVAL;
		source_offset += length;
	}
	if (known_size) {
		if (object->length() > known_size) return -EINVAL;
		object->set_length(known_size);
	}
	if (source_offset != std::filesystem::file_size(pages, error) || error) return -EINVAL;
	object->set_saved_seals(seals);
	return object->length() ? 0 : -EINVAL;
}

int LoadMemfdFiles(const std::filesystem::path& root, std::map<uint32_t, uint32_t> *inodes)
{
	std::ifstream input;
	if (OpenImage(&input, root / "files.img", kFilesMagic)) return -EINVAL;
	for (;;) {
		file_entry entry; bool eof;
		if (Read(input, &entry, &eof)) return -EINVAL;
		if (eof) return 0;
		if (entry.type() != kMemfdFileType) continue;
		if (!entry.has_memfd() || !inodes->emplace(entry.id(), entry.memfd().inode_id()).second)
			return -EINVAL;
	}
}

int IndexMemfd(const std::filesystem::path& root, uint32_t file_id,
	const std::map<uint32_t, uint32_t>& files, criu_provider::v1::Plan& plan)
{
	const auto file = files.find(file_id);
	if (file == files.end()) return -ENOTSUP;
	const auto image = root / "memfd.img";
	std::ifstream input;
	if (OpenImage(&input, image, kMemfdMagic)) return -ENOTSUP;
	if (const int status = AddImage(plan, image, criu_provider::v1::Image::METADATA)) return status;
	for (;;) {
		memfd_inode_entry entry; bool eof;
		if (Read(input, &entry, &eof)) return -EINVAL;
		if (eof) return -ENOTSUP;
		if (entry.inode_id() != file->second) continue;
		if (entry.has_hugetlb_flag()) return -ENOTSUP;
		return IndexShared(root, entry.shmid(), entry.size(), entry.seals(), plan);
	}
}
}

int CreatePlanFromCheckpoint(const char *directory, criu_provider_plan **out)
{
	if (!directory || !out) return -EINVAL;
	*out = nullptr; std::filesystem::path root(directory); std::error_code error;
	if (!std::filesystem::is_directory(root, error)) return -ENOENT;
	if (std::filesystem::exists(root / "parent", error)) return -ENOTSUP;
	std::map<uint32_t, uint32_t> memfd_files;
	if (std::filesystem::exists(root / "files.img", error)) {
		if (const int status = LoadMemfdFiles(root, &memfd_files)) return status;
	}
	auto plan = std::make_unique<criu_provider_plan>(); auto &value = plan->value;
	value.set_format_major(1); value.set_page_size(getpagesize());
	for (const auto &file : std::filesystem::directory_iterator(root)) {
		const std::string name = file.path().filename();
		if (name.rfind("mm-", 0) || file.path().extension() != ".img") continue;
		const std::string pid_text = name.substr(3, name.size() - 7); char *end = nullptr;
		unsigned long pid = strtoul(pid_text.c_str(), &end, 10);
		if (!pid || !end || *end || pid > UINT32_MAX) return -EINVAL;
		std::ifstream mm_file; mm_entry mm; bool eof;
		if (OpenImage(&mm_file, file.path(), kMmMagic)) return -EINVAL;
		if (Read(mm_file, &mm, &eof) || eof) return -EINVAL;
		for (const auto& vma : mm.vmas()) {
			if ((vma.status() & kMemfd) &&
				IndexMemfd(root, vma.shmid(), memfd_files, value)) return -ENOTSUP;
			if ((vma.status() & kAnonShared) && !(vma.status() & kMemfd) &&
				IndexShared(root, vma.shmid(), 0, 0, value)) return -ENOTSUP;
		}
		std::filesystem::path pmap = root / ("pagemap-" + std::to_string(pid) + ".img");
		std::ifstream pmap_file; pagemap_head head;
		if (OpenImage(&pmap_file, pmap, kPagemapMagic)) return -EINVAL;
		if (Read(pmap_file, &head, &eof) || eof) return -EINVAL;
		std::filesystem::path pages = root / ("pages-" + std::to_string(head.pages_id()) + ".img");
		if (!std::filesystem::is_regular_file(pages, error)) return -EINVAL;
		for (const auto &[path, role] : {std::pair{file.path(), criu_provider::v1::Image::METADATA},
				std::pair{pmap, criu_provider::v1::Image::METADATA},
				std::pair{pages, criu_provider::v1::Image::PAGES}}) {
			if (const int status = AddImage(value, path, role)) return status;
		}
		bool residual = false;
		for (const auto &object : value.objects()) if (object.key() == pages.filename()) residual = true;
		if (!residual) {
			auto *object = value.add_objects(); object->set_key(pages.filename());
			object->set_kind(criu_provider::v1::Object::RESIDUAL);
			object->set_image(pages.filename()); object->set_length(std::filesystem::file_size(pages, error));
		}
		for (int i = 0; i < mm.vmas_size(); ++i) if (Eligible(mm.vmas(i))) {
			if (mm.vmas(i).end() <= mm.vmas(i).start()) return -EINVAL;
			auto *object = value.add_objects(); object->set_key("vma:" + std::to_string(pid) + ":" + std::to_string(i));
			object->set_kind(criu_provider::v1::Object::PRIVATE_VMA); object->set_pid(pid); object->set_vma_id(i);
			object->set_start(mm.vmas(i).start()); object->set_length(mm.vmas(i).end() - mm.vmas(i).start());
		}
		uint64_t offset = 0;
		for (;;) { pagemap_entry entry; if (Read(pmap_file, &entry, &eof)) return -EINVAL; if (eof) break;
			if (entry.in_parent() || entry.has_blocks()) return -ENOTSUP;
			uint64_t pages_count = entry.has_nr_pages() ? entry.nr_pages() : entry.compat_nr_pages();
			if (!entry.has_flags() || !(entry.flags() & kPresent)) continue;
			if (!pages_count || pages_count > UINT64_MAX / value.page_size()) return -EINVAL;
			uint64_t length = pages_count * value.page_size(); int index = -1;
			if (entry.vaddr() > UINT64_MAX - length || offset > UINT64_MAX - length) return -EINVAL;
			uint64_t address = entry.vaddr();
			uint64_t source_offset = offset;
			uint64_t remaining = length;
			while (remaining) {
				for (int i = 0; i < mm.vmas_size(); ++i)
					if (address >= mm.vmas(i).start() && address < mm.vmas(i).end()) { index = i; break; }
				if (index < 0) return -EINVAL;
				const uint64_t part = std::min(remaining, mm.vmas(index).end() - address);
				auto *chunk=value.add_chunks(); chunk->set_image(pages.filename());
				chunk->set_source_offset(source_offset); chunk->set_stored_length(part);
				chunk->set_decoded_length(part);
				auto *place=chunk->add_placements();
				if (Eligible(mm.vmas(index))) {
					place->set_object_key("vma:"+std::to_string(pid)+":"+std::to_string(index));
					place->set_destination_offset(address-mm.vmas(index).start());
				} else { place->set_object_key(pages.filename()); place->set_destination_offset(source_offset); }
				address += part; source_offset += part; remaining -= part; index = -1;
			}
			offset += length;
		}
		if (offset != std::filesystem::file_size(pages, error) || error) return -EINVAL;
	}
	for (const auto& entry : std::filesystem::directory_iterator(root)) {
		if (!entry.is_regular_file() || entry.is_symlink()) return -EINVAL;
		const std::string name = entry.path().filename();
		if (name.rfind("pages-", 0) == 0) continue;
		if (const int status = AddImage(value, entry.path(), criu_provider::v1::Image::METADATA)) return status;
	}
	AddCriuRules(value, criu_provider::v1::Image::DUMP_LOCAL_FALLBACK);
	if (!value.objects_size()) return -ENOTSUP;
	auto *need = value.mutable_requirements();
	for (const auto& image : value.images())
		if (image.role() != criu_provider::v1::Image::PAGES)
			need->set_metadata_bytes(need->metadata_bytes() + image.size());
	for (const auto& chunk : value.chunks()) {
		need->set_stored_bytes(need->stored_bytes() + chunk.stored_length());
		need->set_materialized_bytes(need->materialized_bytes() + chunk.decoded_length() * chunk.placements_size());
	}
	for (const auto& object : value.objects()) need->set_logical_fd_bytes(need->logical_fd_bytes() + object.length());
	need->set_fd_count(value.objects_size()); need->set_workspace_bytes(1 << 20);
	if (int error_code = ValidatePlan(value)) return error_code;
	*out = plan.release();
	return 0;
}

void AddDumpImageRules(criu_provider::v1::Plan& plan)
{
	AddCriuRules(plan, criu_provider::v1::Image::DUMP_PROVIDER_OUTPUT);
}
