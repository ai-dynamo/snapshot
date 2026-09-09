PROTO := v1/pagebroker.proto
GTEST_FLAGS := $(shell pkg-config --cflags --libs gtest_main)
BROKER_SOURCES := broker.cpp checkpoint_transaction_descriptor.cpp restore_transaction_descriptor.cpp transaction.cpp transfer/posix_copy_engine.cpp transfer/transfer_engine.cpp utils/event_loop.cpp
DAEMON_SOURCES := $(BROKER_SOURCES) daemon.cpp main.cpp file_descriptor.cpp
TEST_SOURCES := daemon_test.cpp utils/event_loop_test.cpp

.PHONY: daemon generate test

generate:
	protoc --proto_path=. --cpp_out=. $(PROTO)

daemon: generate
	c++ -I. -std=c++20 -Wall -Werror $(DAEMON_SOURCES) v1/pagebroker.pb.cc -lprotobuf -o pagebroker

test: generate $(BROKER_SOURCES) $(TEST_SOURCES)
	mkdir -p build
	c++ -I. -std=c++20 -Wall -Werror $(BROKER_SOURCES) $(TEST_SOURCES) v1/pagebroker.pb.cc -lprotobuf $(GTEST_FLAGS) -o build/pagebroker-test
	./build/pagebroker-test
