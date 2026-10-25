# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Include alongside agent/pagebroker/Makefile, with that directory as cwd.
INTEGRATION_DIR := ../../hack/pagebroker-integration
INTEGRATION_VENV := build/integration-venv

.PHONY: integration-build integration-test integration-fixtures integration-lifetime
integration-build: generate | build
	$(CXX) $(CPPFLAGS) -std=c++20 -Wall -Werror $(ENGINE_SOURCES) $(INTEGRATION_DIR)/native/model_streamer_s3_test.cpp $(INTEGRATION_DIR)/native/s3_round_trip_test.cpp v1/pagebroker.pb.cc -Wl,-Bstatic -lprotobuf -Wl,-Bdynamic $(GTEST_FLAGS) $(MODEL_STREAMER_FLAGS) $(CRYPTO_FLAGS) -o build/model-streamer-s3-test
	$(CXX) $(CPPFLAGS) $(CXXFLAGS) $(PAGEBROKER_SOURCES) daemon.cpp $(INTEGRATION_DIR)/native/daemon_transport_test.cpp v1/pagebroker.pb.cc -Wl,--wrap=_ZN8snapshot10pagebroker18filesystem_storage12StageRestoreERKNS0_19CheckpointInventoryERKNSt10filesystem7__cxx114pathENS0_12CancellationE -lprotobuf $(GTEST_FLAGS) $(NIXL_FLAGS) $(TRANSFER_FLAGS) -ldl -o build/daemon-transport-test

$(INTEGRATION_VENV)/.installed: $(INTEGRATION_DIR)/requirements.txt | build
	python3 -m venv $(INTEGRATION_VENV)
	$(INTEGRATION_VENV)/bin/python3 -m pip install --disable-pip-version-check -r $<
	touch $@

integration-fixtures: $(INTEGRATION_VENV)/.installed
	$(INTEGRATION_VENV)/bin/python3 -m unittest discover -s $(INTEGRATION_DIR) -p 'test_*.py' -v

integration-lifetime:
	LD_LIBRARY_PATH="$(MODEL_STREAMER_LIB_DIR):$$LD_LIBRARY_PATH" python3 $(INTEGRATION_DIR)/model_streamer_submission_lifetime.py

integration-test: integration-build integration-fixtures integration-lifetime
	./build/daemon-transport-test
	LD_LIBRARY_PATH="$(MODEL_STREAMER_LIB_DIR):$$LD_LIBRARY_PATH" $(INTEGRATION_VENV)/bin/python3 $(INTEGRATION_DIR)/s3_integration.py --binary ./build/model-streamer-s3-test --resources
	LD_LIBRARY_PATH="$(MODEL_STREAMER_LIB_DIR):$$LD_LIBRARY_PATH" $(INTEGRATION_VENV)/bin/python3 $(INTEGRATION_DIR)/s3_integration.py --binary ./build/model-streamer-s3-test --tls
