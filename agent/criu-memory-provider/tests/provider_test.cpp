// SPDX-License-Identifier: Apache-2.0

#include <fcntl.h>
#include <sys/socket.h>
#include <unistd.h>

#include <array>
#include <algorithm>
#include <cstring>
#include <filesystem>
#include <fstream>
#include <string>
#include <thread>

#include <gtest/gtest.h>

#include "criu_provider.h"
#include "internal.hpp"
#include "criu-images/mm.pb.h"
#include "criu-images/memfd.pb.h"
#include "criu-images/fdinfo.pb.h"
#include "criu-images/pagemap.pb.h"
#include "extmem.pb.h"

namespace {
int Read(void *, const char *image, uint64_t offset, void *buffer, size_t length)
{
	static constexpr char bytes[] = "abcdefgh";
	if (std::strcmp(image, "pages-1.img") || offset + length > sizeof(bytes) - 1) return -EINVAL;
	std::memcpy(buffer, bytes + offset, length);
	return 0;
}

int Open(void *, const char *image, int) { return !std::strcmp(image, "memfd.img") ? -ENOENT : -1; }

int OpenDump(void *, const char *, int) { return -1; }
int CommitDump(void *, const criu_provider_plan *) { return 0; }
void AbortDump(void *) {}

struct Range {
	std::string image;
	uint64_t offset = 0;
	uint64_t length = 0;
};

int SaveRange(void *context, const char *image, uint64_t offset, uint64_t length)
{
	auto *range = static_cast<Range *>(context);
	range->image = image;
	range->offset = offset;
	range->length = length;
	return 0;
}

int SendRequest(int socket, const extmem_req& request)
{
	const std::string bytes = request.SerializeAsString();
	return send(socket, bytes.data(), bytes.size(), 0) == static_cast<ssize_t>(bytes.size()) ? 0 : -errno;
}

int ReceiveResponse(int socket, extmem_resp *response)
{
	std::array<char, 128> bytes{};
	std::array<char, CMSG_SPACE(sizeof(int))> control{};
	iovec iov{bytes.data(), bytes.size()};
	msghdr message{};
	message.msg_iov = &iov;
	message.msg_iovlen = 1;
	message.msg_control = control.data();
	message.msg_controllen = control.size();
	const ssize_t received = recvmsg(socket, &message, 0);
	if (received < 0 || !response->ParseFromArray(bytes.data(), received)) return -1;
	const cmsghdr *header = CMSG_FIRSTHDR(&message);
	if (!header) return -1;
	int fd = -1;
	std::memcpy(&fd, CMSG_DATA(header), sizeof(fd));
	return fd;
}

criu_provider_plan Plan()
{
	criu_provider_plan plan;
	auto *value = &plan.value;
	value->set_format_major(1); value->set_page_size(4096);
	auto *image = value->add_images(); image->set_name("pages-1.img"); image->set_size(8);
	image->set_restore_mode(criu_provider::v1::Image::RESTORE_PROVIDER_FD);
	auto *missing = value->add_images(); missing->set_name("memfd.img");
	missing->set_role(criu_provider::v1::Image::METADATA);
	missing->set_restore_mode(criu_provider::v1::Image::RESTORE_LOCAL_FALLBACK);
	auto *object = value->add_objects(); object->set_key("vma:17:3"); object->set_length(8192);
	object->set_kind(criu_provider::v1::Object::PRIVATE_VMA); object->set_pid(17);
	object->set_vma_id(3); object->set_start(0x1000);
	auto *chunk = value->add_chunks(); chunk->set_image("pages-1.img"); chunk->set_stored_length(8);
	chunk->set_decoded_length(8); auto *place = chunk->add_placements();
	place->set_object_key("vma:17:3"); place->set_destination_offset(4096);
	return plan;
}

template <typename T>
void WriteRecord(std::ofstream &output, const T &message)
{
	const std::string bytes = message.SerializeAsString();
	const uint32_t size = bytes.size();
	output.write(reinterpret_cast<const char *>(&size), sizeof(size));
	output.write(bytes.data(), bytes.size());
}

void WriteImageHeader(std::ofstream &output, uint32_t magic)
{
	const uint32_t header[] = {0x54564319, magic};
	output.write(reinterpret_cast<const char *>(header), sizeof(header));
}

TEST(ImageIndex, CreatesPrivateAndResidualObjects)
{
	const auto directory = std::filesystem::temp_directory_path() / "criu-provider-index-test";
	std::filesystem::remove_all(directory); std::filesystem::create_directory(directory);
	mm_entry mm;
	mm.set_mm_start_code(0); mm.set_mm_end_code(0); mm.set_mm_start_data(0); mm.set_mm_end_data(0);
	mm.set_mm_start_stack(0); mm.set_mm_start_brk(0); mm.set_mm_brk(0); mm.set_mm_arg_start(0);
	mm.set_mm_arg_end(0); mm.set_mm_env_start(0); mm.set_mm_env_end(0); mm.set_exe_file_id(0);
	auto *private_vma = mm.add_vmas(); private_vma->set_start(0x1000); private_vma->set_end(0x2000);
	private_vma->set_pgoff(0); private_vma->set_shmid(0); private_vma->set_prot(0); private_vma->set_flags(0);
	private_vma->set_status((1U << 0) | (1U << 9)); private_vma->set_fd(-1);
	auto *excluded_vma = mm.add_vmas(); *excluded_vma = *private_vma; excluded_vma->set_start(0x2000); excluded_vma->set_end(0x3000); excluded_vma->set_status(1U << 1);
	auto *shared_vma = mm.add_vmas(); *shared_vma = *private_vma; shared_vma->set_start(0x3000); shared_vma->set_end(0x4000); shared_vma->set_status((1U << 8) | (1U << 14)); shared_vma->set_shmid(3);
	auto *memfd_vma = mm.add_vmas(); *memfd_vma = *private_vma; memfd_vma->set_start(0x4000); memfd_vma->set_end(0x5000); memfd_vma->set_status((1U << 8) | (1U << 14)); memfd_vma->set_shmid(4);
	std::ofstream mm_file(directory / "mm-7.img", std::ios::binary); WriteImageHeader(mm_file, 0x57492820); WriteRecord(mm_file, mm); mm_file.close();
	pagemap_head head; head.set_pages_id(1); pagemap_entry first; first.set_vaddr(0x1000); first.set_compat_nr_pages(2); first.set_nr_pages(2); first.set_flags(1U << 2);
	std::ofstream pmap_file(directory / "pagemap-7.img", std::ios::binary); WriteImageHeader(pmap_file, 0x56084025); WriteRecord(pmap_file, head); WriteRecord(pmap_file, first); pmap_file.close();
	std::ofstream pages(directory / "pages-1.img", std::ios::binary); pages << std::string(8192, 'x'); pages.close();
	pagemap_head shared_head; shared_head.set_pages_id(2); pagemap_entry shared; shared.set_vaddr(0); shared.set_compat_nr_pages(1); shared.set_nr_pages(1); shared.set_flags(1U << 2);
	std::ofstream shared_pmap(directory / "pagemap-shmem-42.img", std::ios::binary); WriteImageHeader(shared_pmap, 0x56084025); WriteRecord(shared_pmap, shared_head); WriteRecord(shared_pmap, shared); shared_pmap.close();
	std::ofstream shared_pages(directory / "pages-2.img", std::ios::binary); shared_pages << std::string(4096, 's'); shared_pages.close();
	memfd_inode_entry inode; inode.set_name("test"); inode.set_uid(0); inode.set_gid(0); inode.set_size(8192); inode.set_shmid(43); inode.set_seals(2); inode.set_inode_id(1);
	memfd_inode_entry anon_inode = inode; anon_inode.set_name("/dev/zero"); anon_inode.set_shmid(42); anon_inode.set_inode_id(2);
	std::ofstream memfd(directory / "memfd.img", std::ios::binary); WriteImageHeader(memfd, 0x48453499); WriteRecord(memfd, inode); WriteRecord(memfd, anon_inode); memfd.close();
	file_entry shared_file; shared_file.set_type(18); shared_file.set_id(3); shared_file.mutable_memfd()->set_id(3); shared_file.mutable_memfd()->set_flags(0); shared_file.mutable_memfd()->set_pos(0); shared_file.mutable_memfd()->mutable_fown()->set_pid(0); shared_file.mutable_memfd()->mutable_fown()->set_uid(0); shared_file.mutable_memfd()->mutable_fown()->set_euid(0); shared_file.mutable_memfd()->mutable_fown()->set_signum(0); shared_file.mutable_memfd()->mutable_fown()->set_pid_type(0); shared_file.mutable_memfd()->set_inode_id(2);
	file_entry memfd_file = shared_file; memfd_file.set_id(4); memfd_file.mutable_memfd()->set_id(4); memfd_file.mutable_memfd()->set_inode_id(1);
	std::ofstream files(directory / "files.img", std::ios::binary); WriteImageHeader(files, 0x56303138); WriteRecord(files, shared_file); WriteRecord(files, memfd_file); files.close();
	pagemap_head memfd_head; memfd_head.set_pages_id(3); pagemap_entry memfd_entry; memfd_entry.set_vaddr(4096); memfd_entry.set_compat_nr_pages(1); memfd_entry.set_nr_pages(1); memfd_entry.set_flags(1U << 2);
	std::ofstream memfd_pmap(directory / "pagemap-shmem-43.img", std::ios::binary); WriteImageHeader(memfd_pmap, 0x56084025); WriteRecord(memfd_pmap, memfd_head); WriteRecord(memfd_pmap, memfd_entry); memfd_pmap.close();
	std::ofstream memfd_pages(directory / "pages-3.img", std::ios::binary); memfd_pages << std::string(4096, 'm'); memfd_pages.close();
	criu_provider_plan *plan = nullptr; ASSERT_EQ(criu_provider_plan_from_checkpoint(directory.c_str(), &plan), 0);
	EXPECT_EQ(plan->value.objects_size(), 4); EXPECT_EQ(plan->value.chunks_size(), 4);
	criu_provider::v1::Image rule_image;
	const auto *fallback = FindImage(plan, "remap-fpath.img", &rule_image);
	ASSERT_NE(fallback, nullptr);
	EXPECT_EQ(fallback->restore_mode(), criu_provider::v1::Image::RESTORE_LOCAL_FALLBACK);
	EXPECT_EQ(FindImage(plan, "not-a-criu-image.img", &rule_image), nullptr);
	EXPECT_EQ(plan->value.chunks(2).stored_length(), 4096);
	EXPECT_EQ(plan->value.chunks(3).source_offset(), 4096);
	EXPECT_EQ(plan->value.chunks(3).placements(0).object_key(), "pages-1.img");
	const auto shared_object = std::find_if(plan->value.objects().begin(), plan->value.objects().end(),
		[](const auto& object) { return object.key() == "shared:42"; });
	ASSERT_NE(shared_object, plan->value.objects().end());
	const auto memfd_object = std::find_if(plan->value.objects().begin(), plan->value.objects().end(),
		[](const auto& object) { return object.key() == "shared:43"; });
	ASSERT_NE(memfd_object, plan->value.objects().end());
	EXPECT_EQ(memfd_object->saved_seals(), 2);
	criu_provider_plan_destroy(plan);
	plan = nullptr;
	std::filesystem::create_directory(directory / "parent-images");
	std::filesystem::create_directory_symlink("parent-images", directory / "parent");
	EXPECT_EQ(criu_provider_plan_from_checkpoint(directory.c_str(), &plan), -ENOTSUP);
	std::filesystem::remove(directory / "parent");
	first.mutable_blocks()->set_total_payload_size(8192);
	first.mutable_blocks()->set_pages_per_block(1);
	first.mutable_blocks()->add_block_sizes(4096);
	std::ofstream blocked_pmap(directory / "pagemap-7.img", std::ios::binary);
	WriteImageHeader(blocked_pmap, 0x56084025);
	WriteRecord(blocked_pmap, head);
	WriteRecord(blocked_pmap, first);
	blocked_pmap.close();
	EXPECT_EQ(criu_provider_plan_from_checkpoint(directory.c_str(), &plan), -ENOTSUP);
	std::fstream corrupt(directory / "mm-7.img", std::ios::in | std::ios::out | std::ios::binary);
	uint64_t invalid_header = 0;
	corrupt.write(reinterpret_cast<const char *>(&invalid_header), sizeof(invalid_header));
	corrupt.close();
	EXPECT_EQ(criu_provider_plan_from_checkpoint(directory.c_str(), &plan), -EINVAL);
	std::filesystem::remove_all(directory);
}

TEST(Materializer, PreservesSparseHole)
{
	auto plan = Plan();
	criu_provider_session *session = nullptr;
	const criu_provider_source_ops ops{Read, Open};
	ASSERT_EQ(criu_provider_session_create(&plan, &ops, nullptr, &session), 0);
	ASSERT_EQ(criu_provider_session_prepare(session), 0);
	const auto &object = session->objects.at("vma:17:3");
	std::array<char, 8> bytes{};
	ASSERT_EQ(pread(object.fd, bytes.data(), bytes.size(), 0), 8);
	EXPECT_EQ(std::string(bytes.data(), bytes.size()), std::string(8, '\0'));
	ASSERT_EQ(pread(object.fd, bytes.data(), bytes.size(), 4096), 8);
	EXPECT_EQ(std::string(bytes.data(), bytes.size()), "abcdefgh");
	criu_provider_session_destroy(session);
}

TEST(Protocol, ServesPreparedVmasAndRejectsUnknownImages)
{
	auto plan = Plan();
	criu_provider_session *session = nullptr;
	const criu_provider_source_ops ops{Read, Open};
	ASSERT_EQ(criu_provider_session_create(&plan, &ops, nullptr, &session), 0);
	ASSERT_EQ(criu_provider_session_prepare(session), 0);
	int sockets[2];
	if (socketpair(AF_UNIX, SOCK_SEQPACKET | SOCK_CLOEXEC, 0, sockets) < 0) {
		if (errno == EPERM) {
			criu_provider_session_destroy(session);
			GTEST_SKIP() << "Unix sockets are blocked by this sandbox";
		}
		criu_provider_session_destroy(session);
		FAIL() << "socketpair: " << std::strerror(errno);
	}
	std::thread server([&] { criu_provider_session_serve(session, sockets[1]); close(sockets[1]); });
	extmem_req before_init;
	before_init.set_op(EXTMEM_WAIT_READY);
	const int before_init_status = SendRequest(sockets[0], before_init);
	if (before_init_status == -EPERM) {
		close(sockets[0]);
		server.join();
		criu_provider_session_destroy(session);
		GTEST_SKIP() << "Unix socket sends are blocked by this sandbox";
	}
	ASSERT_EQ(before_init_status, 0);
	extmem_resp response;
	EXPECT_EQ(ReceiveResponse(sockets[0], &response), -1);
	EXPECT_EQ(response.status(), -EPROTO);
	extmem_req init;
	init.set_op(EXTMEM_INIT);
	ASSERT_EQ(SendRequest(sockets[0], init), 0);
	response.Clear();
	EXPECT_EQ(ReceiveResponse(sockets[0], &response), -1);
	EXPECT_EQ(response.status(), 0);
	extmem_req unknown;
	unknown.set_op(EXTMEM_OPEN_IMAGE);
	unknown.mutable_open_image()->set_name("unknown.img");
	unknown.mutable_open_image()->set_flags(O_RDONLY);
	const int unknown_status = SendRequest(sockets[0], unknown);
	if (unknown_status == -EPERM) {
		close(sockets[0]);
		server.join();
		criu_provider_session_destroy(session);
		GTEST_SKIP() << "Unix socket sends are blocked by this sandbox";
	}
	ASSERT_EQ(unknown_status, 0);
	response.Clear();
	EXPECT_EQ(ReceiveResponse(sockets[0], &response), -1);
	EXPECT_EQ(response.status(), -EPROTO);

	extmem_req missing;
	missing.set_op(EXTMEM_OPEN_IMAGE);
	missing.mutable_open_image()->set_name("memfd.img");
	missing.mutable_open_image()->set_flags(O_RDONLY);
	ASSERT_EQ(SendRequest(sockets[0], missing), 0);
	response.Clear();
	EXPECT_EQ(ReceiveResponse(sockets[0], &response), -1);
	EXPECT_EQ(response.status(), -ENOTSUP);

	extmem_req vma;
	vma.set_op(EXTMEM_GET_VMA);
	vma.mutable_get_vma()->set_pid(17);
	vma.mutable_get_vma()->set_vma_id(3);
	vma.mutable_get_vma()->set_vaddr(0x1000);
	vma.mutable_get_vma()->set_length(8192);
	ASSERT_EQ(SendRequest(sockets[0], vma), 0);
	response.Clear();
	const int fd = ReceiveResponse(sockets[0], &response);
	ASSERT_GE(fd, 0);
	EXPECT_EQ(response.status(), 0);
	std::array<char, 8> bytes{};
	EXPECT_EQ(pread(fd, bytes.data(), bytes.size(), 4096), 8);
	EXPECT_EQ(std::string(bytes.data(), bytes.size()), "abcdefgh");
	close(fd);
	vma.mutable_get_vma()->set_length(4096);
	ASSERT_EQ(SendRequest(sockets[0], vma), 0);
	response.Clear();
	EXPECT_EQ(ReceiveResponse(sockets[0], &response), -1);
	EXPECT_EQ(response.status(), -EPROTO);

	extmem_req commit;
	commit.set_op(EXTMEM_COMMIT);
	ASSERT_EQ(SendRequest(sockets[0], commit), 0);
	response.Clear();
	EXPECT_EQ(ReceiveResponse(sockets[0], &response), -1);
	EXPECT_EQ(response.status(), 0);
	close(sockets[0]);
	server.join();
	EXPECT_TRUE(session->objects.empty());
	criu_provider_session_destroy(session);

	ASSERT_EQ(criu_provider_session_create(&plan, &ops, nullptr, &session), 0);
	ASSERT_EQ(criu_provider_session_prepare(session), 0);
	ASSERT_EQ(socketpair(AF_UNIX, SOCK_SEQPACKET | SOCK_CLOEXEC, 0, sockets), 0);
	std::thread abort_server([&] { criu_provider_session_serve(session, sockets[1]); close(sockets[1]); });
	ASSERT_EQ(SendRequest(sockets[0], init), 0);
	response.Clear();
	EXPECT_EQ(ReceiveResponse(sockets[0], &response), -1);
	EXPECT_EQ(response.status(), 0);
	extmem_req abort;
	abort.set_op(EXTMEM_ABORT);
	ASSERT_EQ(SendRequest(sockets[0], abort), 0);
	response.Clear();
	EXPECT_EQ(ReceiveResponse(sockets[0], &response), -1);
	EXPECT_EQ(response.status(), 0);
	close(sockets[0]);
	abort_server.join();
	EXPECT_TRUE(session->objects.empty());
	criu_provider_session_destroy(session);
}

TEST(Plan, RejectsTraversalAndOutOfRangeChunk)
{
	auto plan = Plan();
	plan.value.mutable_images(0)->set_name("../pages");
	criu_provider_requirements requirements{};
	EXPECT_EQ(criu_provider_plan_requirements(&plan, &requirements), -EINVAL);
	plan = Plan(); plan.value.mutable_chunks(0)->set_source_offset(1);
	EXPECT_EQ(criu_provider_plan_requirements(&plan, &requirements), -EINVAL);
	plan = Plan();
	auto *duplicate = plan.value.add_objects(); *duplicate = plan.value.objects(0);
	duplicate->set_key("vma:17:other");
	EXPECT_EQ(criu_provider_plan_requirements(&plan, &requirements), -EINVAL);
}

TEST(Plan, WritesLoadsAndVisitsRanges)
{
	const auto path = std::filesystem::temp_directory_path() / "criu-provider-plan-test";
	std::filesystem::remove(path);
	auto plan = Plan();
	ASSERT_EQ(criu_provider_plan_write(&plan, path.c_str()), 0);
	std::ifstream input(path);
	const std::string json((std::istreambuf_iterator<char>(input)),
		std::istreambuf_iterator<char>());
	EXPECT_EQ(json.front(), '{');
	EXPECT_NE(json.find("\"source_offset\": \"0\""), std::string::npos);
	criu_provider_plan *loaded = nullptr;
	ASSERT_EQ(criu_provider_plan_load(path.c_str(), &loaded), 0);
	Range range;
	EXPECT_EQ(criu_provider_plan_enumerate_source_ranges(loaded, SaveRange, &range), 0);
	EXPECT_EQ(range.image, "pages-1.img");
	EXPECT_EQ(range.offset, 0);
	EXPECT_EQ(range.length, 8);
	criu_provider_plan_destroy(loaded);
	std::filesystem::remove(path);
}

TEST(DumpPlan, RecordsProviderAndFallbackImages)
{
	criu_provider_plan *plan = nullptr;
	ASSERT_EQ(criu_provider_dump_plan_create(4096, &plan), 0);
	criu_provider::v1::Image rule_image;
	const auto *rule = FindImage(plan, "pages-1.img", &rule_image);
	ASSERT_NE(rule, nullptr);
	EXPECT_EQ(rule->dump_mode(), criu_provider::v1::Image::DUMP_PROVIDER_OUTPUT);
	rule = FindImage(plan, "tmpfs-dev-203.tar.gz.img", &rule_image);
	ASSERT_NE(rule, nullptr);
	EXPECT_EQ(rule->restore_mode(),
		criu_provider::v1::Image::RESTORE_LOCAL_FALLBACK);
	ASSERT_EQ(criu_provider_dump_plan_add_image(plan, "pages-1.img",
		CRIU_PROVIDER_RESTORE_PROVIDER_FD, CRIU_PROVIDER_DUMP_PROVIDER_OUTPUT), 0);
	ASSERT_EQ(criu_provider_dump_plan_add_image(plan, "memfd.img",
		CRIU_PROVIDER_RESTORE_LOCAL_FALLBACK, CRIU_PROVIDER_DUMP_LOCAL_FALLBACK), 0);
	EXPECT_EQ(criu_provider_dump_plan_add_image(plan, "pages-1.img",
		CRIU_PROVIDER_RESTORE_PROVIDER_FD, CRIU_PROVIDER_DUMP_PROVIDER_OUTPUT), -EEXIST);
	const criu_provider_dump_ops ops{OpenDump, CommitDump, AbortDump};
	criu_provider_dump_session *session = nullptr;
	EXPECT_EQ(criu_provider_dump_session_create(plan, &ops, nullptr, &session), 0);
	criu_provider_dump_session_destroy(session);
	criu_provider_plan_destroy(plan);
}

TEST(DumpPlan, CoversCriuImageDescriptors)
{
	criu_provider_plan *plan = nullptr;
	ASSERT_EQ(criu_provider_dump_plan_create(4096, &plan), 0);
	const std::array names{
		"fdinfo-1.img", "pagemap-1.img", "pagemap-shmem-1.img",
		"reg-files.img", "ext-files.img", "ns-files.img", "eventfd.img",
		"eventpoll.img", "eventpoll-tfd.img", "signalfd.img", "inotify.img",
		"inotify-wd.img", "fanotify.img", "fanotify-mark.img", "core-1.img",
		"ids-1.img", "mm-1.img", "vmas-1.img", "pipes.img", "pipes-data.img",
		"fifo.img", "fifo-data.img", "pstree.img", "sigacts-1.img", "unixsk.img",
		"inetsk.img", "packetsk.img", "netlinksk.img", "sk-queues.img",
		"itimers-1.img", "posix-timers-1.img", "creds-1.img", "utsns-1.img",
		"ipcns-var-1.img", "ipcns-shm-1.img", "ipcns-msg-1.img", "ipcns-sem-1.img",
		"fs-1.img", "remap-fpath.img", "ghost-file-af.img", "memfd.img",
		"tcp-stream-af.img", "mountpoints-1.img", "netdev-1.img", "netns-1.img",
		"ifaddr-1.img", "route-1.img", "route6-1.img", "rule-1.img", "rule6-1.img",
		"iptables-1.img", "ip6tables-1.img", "nftables-1.img", "tmpfs-1.tar.gz.img",
		"tmpfs-dev-1.tar.gz.img", "autofs-1.img", "tty.img", "tty-info.img",
		"tty-data.img", "filelocks.img", "rlimit-1.img", "pages-1.img",
		"pages-shmem-1.img", "signal-s-1.img", "signal-p-1.img", "tunfile.img",
		"cgroup.img", "timerfd.img", "cpuinfo.img", "seccomp.img", "userns-1.img",
		"netns-ct-1.img", "netns-exp-1.img", "files.img", "timens-1.img",
		"pidns-1.img", "bpfmap-file.img", "bpfmap-data.img", "apparmor.img", "pidfd.img"};
	for (const char *name : names) {
		criu_provider::v1::Image rule_image;
		const auto *image = FindImage(plan, name, &rule_image);
		ASSERT_NE(image, nullptr) << name;
		EXPECT_EQ(image->restore_mode(), criu_provider::v1::Image::RESTORE_LOCAL_FALLBACK) << name;
		EXPECT_EQ(image->dump_mode(), criu_provider::v1::Image::DUMP_PROVIDER_OUTPUT) << name;
	}
	criu_provider::v1::Image rule_image;
	EXPECT_EQ(FindImage(plan, "stats-dump.img", &rule_image), nullptr);
	EXPECT_EQ(FindImage(plan, "irmap-cache.img", &rule_image), nullptr);
	criu_provider_plan_destroy(plan);
}

} // namespace
