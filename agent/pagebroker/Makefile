PROTO := v1/pagebroker.proto
GTEST_FLAGS = $(shell pkg-config --cflags --libs gtest_main)
MODEL_STREAMER_LIB_DIR ?= /usr/local/lib
MODEL_STREAMER_FLAGS := -L$(MODEL_STREAMER_LIB_DIR) -lstreamer -Wl,-rpath,$(MODEL_STREAMER_LIB_DIR)
BROKER_SOURCES := broker.cpp checkpoint_transaction_descriptor.cpp file_descriptor.cpp restore_transaction_descriptor.cpp transaction.cpp transfer/model_streamer_restore.cpp transfer/model_streamer_transfer_engine.cpp transfer/posix_copy_engine.cpp transfer/transfer_engine.cpp utils/event_loop.cpp
DAEMON_SOURCES := $(BROKER_SOURCES) daemon.cpp main.cpp
TEST_SOURCES := daemon_test.cpp utils/event_loop_test.cpp
MODEL_STREAMER_RECOVERY_TEST_SOURCES := file_descriptor.cpp transfer/model_streamer_restore.cpp transfer/model_streamer_transfer_engine.cpp transfer/model_streamer_transfer_engine_test.cpp transfer/posix_copy_engine.cpp transfer/transfer_engine.cpp utils/event_loop.cpp

.PHONY: daemon generate test

generate:
	protoc --proto_path=. --cpp_out=. $(PROTO)

daemon: generate
	c++ -I. -std=c++20 -Wall -Werror $(DAEMON_SOURCES) v1/pagebroker.pb.cc -lprotobuf $(MODEL_STREAMER_FLAGS) -o pagebroker

test: generate $(BROKER_SOURCES) $(TEST_SOURCES) $(MODEL_STREAMER_RECOVERY_TEST_SOURCES)
	mkdir -p build
	c++ -I. -std=c++20 -Wall -Werror $(BROKER_SOURCES) $(TEST_SOURCES) v1/pagebroker.pb.cc -lprotobuf $(GTEST_FLAGS) $(MODEL_STREAMER_FLAGS) -o build/pagebroker-test
	LD_LIBRARY_PATH="$(MODEL_STREAMER_LIB_DIR):$$LD_LIBRARY_PATH" ./build/pagebroker-test
	c++ -I. -std=c++20 -Wall -Werror $(MODEL_STREAMER_RECOVERY_TEST_SOURCES) v1/pagebroker.pb.cc -lprotobuf $(GTEST_FLAGS) -o build/model-streamer-recovery-test
	./build/model-streamer-recovery-test
