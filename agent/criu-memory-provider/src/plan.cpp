// SPDX-License-Identifier: Apache-2.0

#include "internal.hpp"

#include <errno.h>

#include <limits>
#include <set>
#include <tuple>
#include <cctype>

using criu_provider::v1::Plan;

namespace {
bool ValidName(const std::string &name)
{
	return !name.empty() && name.find('/') == std::string::npos &&
		name.find('\\') == std::string::npos && name != "." && name != "..";
}

bool Fits(uint64_t offset, uint64_t length, uint64_t size)
{
	return offset <= size && length <= size - offset;
}
}

const criu_provider::v1::Image *FindImage(const criu_provider_plan *plan,
	const std::string &name, criu_provider::v1::Image *rule_image)
{
	for (const auto &image : plan->value.images())
		if (image.name() == name) return &image;
	for (const auto &rule : plan->value.image_rules()) {
		if (name.size() < rule.prefix().size() + rule.suffix().size() ||
			name.compare(0, rule.prefix().size(), rule.prefix()) ||
			name.compare(name.size() - rule.suffix().size(), rule.suffix().size(), rule.suffix()))
			continue;
		const std::string selector = name.substr(rule.prefix().size(),
			name.size() - rule.prefix().size() - rule.suffix().size());
		if ((rule.selector() == criu_provider::v1::ImageRule::EXACT && !selector.empty()) ||
			(rule.selector() != criu_provider::v1::ImageRule::EXACT && selector.empty())) continue;
		const int base = rule.selector() == criu_provider::v1::ImageRule::HEX ? 16 : 10;
		bool valid = true;
		for (const char ch : selector)
			if (!(base == 16 ? std::isxdigit(static_cast<unsigned char>(ch)) :
				std::isdigit(static_cast<unsigned char>(ch)))) valid = false;
		if (!valid) continue;
		rule_image->Clear();
		rule_image->set_name(name);
		rule_image->set_restore_mode(rule.restore_mode());
		rule_image->set_dump_mode(rule.dump_mode());
		return rule_image;
	}
	return nullptr;
}

int ValidatePlan(const Plan &plan)
{
	if (plan.format_major() != 1 || plan.format_minor() != 0 || plan.page_size() == 0)
		return -EPROTONOSUPPORT;
	std::map<std::string, uint64_t> images;
	for (const auto &image : plan.images()) {
		if (!ValidName(image.name()) || !images.emplace(image.name(), image.size()).second)
			return -EINVAL;
	}
	std::set<std::tuple<std::string, std::string, int>> rules;
	for (const auto &rule : plan.image_rules()) {
		if (!ValidName(rule.prefix()) ||
			(!rule.suffix().empty() && !ValidName("x" + rule.suffix())) ||
			rule.selector() < criu_provider::v1::ImageRule::EXACT ||
			rule.selector() > criu_provider::v1::ImageRule::HEX ||
			!rules.emplace(rule.prefix(), rule.suffix(), rule.selector()).second)
			return -EINVAL;
	}
	std::map<std::string, uint64_t> objects;
	std::set<std::tuple<uint32_t, uint32_t, uint64_t, uint64_t>> vmas;
	std::set<std::tuple<uint64_t, uint64_t>> shared;
	for (const auto &object : plan.objects()) {
		if (!ValidName(object.key()) || object.length() == 0 ||
			!objects.emplace(object.key(), object.length()).second)
			return -EINVAL;
		if (object.kind() == criu_provider::v1::Object::PRIVATE_VMA) {
			if (object.pid() == 0 || object.start() == 0 ||
				!vmas.emplace(object.pid(), object.vma_id(), object.start(), object.length()).second)
				return -EINVAL;
		}
		if (object.kind() == criu_provider::v1::Object::SHARED) {
			if (object.shmid() == 0 || !shared.emplace(object.shmid(), object.length()).second)
				return -EINVAL;
		}
		if (object.kind() == criu_provider::v1::Object::RESIDUAL &&
			(!ValidName(object.image()) || object.image() != object.key()))
			return -EINVAL;
	}
	for (const auto &chunk : plan.chunks()) {
		const auto image = images.find(chunk.image());
		if (image == images.end() || chunk.encoding() != criu_provider::v1::Chunk::RAW ||
			chunk.stored_length() == 0 || chunk.stored_length() != chunk.decoded_length() ||
			!Fits(chunk.source_offset(), chunk.stored_length(), image->second))
			return -EINVAL;
		for (const auto &placement : chunk.placements()) {
			const auto object = objects.find(placement.object_key());
			if (object == objects.end() || !Fits(placement.destination_offset(),
				chunk.decoded_length(), object->second))
				return -EINVAL;
		}
	}
	return 0;
}
