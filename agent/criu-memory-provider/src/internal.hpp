// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <map>
#include <string>
#include <vector>

#include "criu_provider.h"
#include "criu_provider_plan.pb.h"

struct criu_provider_plan { criu_provider::v1::Plan value; };

struct ProviderFd {
	std::string key;
	int fd = -1;
};

struct criu_provider_session {
	const criu_provider_plan *plan = nullptr;
	criu_provider_source_ops source_ops{};
	void *source_context = nullptr;
	std::map<std::string, ProviderFd> objects;
	bool prepared = false;
	bool active = false;
};

struct criu_provider_dump_session {
	const criu_provider_plan *plan = nullptr;
	criu_provider_dump_ops ops{};
	void *context = nullptr;
	std::map<std::string, ProviderFd> outputs;
	bool active = false;
	bool finished = false;
};

int ValidatePlan(const criu_provider::v1::Plan &plan);
const criu_provider::v1::Image *FindImage(const criu_provider_plan *plan,
	const std::string &name, criu_provider::v1::Image *rule_image);
int CreatePlanFromCheckpoint(const char *directory, criu_provider_plan **out);
void AddDumpImageRules(criu_provider::v1::Plan &plan);
int ServeProtocol(criu_provider_session *session, int fd);
void ReleaseSessionFds(criu_provider_session *session);
int ServeDumpProtocol(criu_provider_dump_session *session, int fd);
