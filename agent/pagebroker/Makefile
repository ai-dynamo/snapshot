PROTO := v1/pagebroker.proto
GTEST_FLAGS := $(shell pkg-config --cflags --libs gtest_main)
PROVIDER_DIR := ../criu-memory-provider
PROVIDER_LIB := $(PROVIDER_DIR)/build/libcriu_provider.a
BROKER_SOURCES := broker.cpp checkpoint_transaction_descriptor.cpp direct_restore_transaction_descriptor.cpp posix_copy_engine.cpp restore_transaction_descriptor.cpp transaction.cpp transfer_engine.cpp
DAEMON_SOURCES := $(BROKER_SOURCES) daemon.cpp main.cpp file_descriptor.cpp

.PHONY: daemon generate test

generate:
	protoc --proto_path=. --cpp_out=. $(PROTO)
	sed -i 's/[[:space:]]\+$$//' v1/pagebroker.pb.cc v1/pagebroker.pb.h

$(PROVIDER_LIB):
	$(MAKE) -C $(PROVIDER_DIR)

daemon: generate $(PROVIDER_LIB)
	c++ -I. -I$(PROVIDER_DIR)/include -std=c++20 -Wall -Werror $(DAEMON_SOURCES) v1/pagebroker.pb.cc $(PROVIDER_LIB) -lprotobuf -o pagebroker

test: generate $(PROVIDER_LIB) $(BROKER_SOURCES) daemon_test.cpp
	mkdir -p build
	c++ -I. -I$(PROVIDER_DIR)/include -I$(PROVIDER_DIR)/build -std=c++20 -Wall -Werror $(BROKER_SOURCES) daemon_test.cpp v1/pagebroker.pb.cc $(PROVIDER_LIB) -lprotobuf $(GTEST_FLAGS) -o build/pagebroker-test
	./build/pagebroker-test
