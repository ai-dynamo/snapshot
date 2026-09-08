BUILD := build
PROTO := proto/extmem.proto proto/criu_provider_plan.proto $(wildcard proto/criu-images/*.proto)
PROTO_CC := $(patsubst proto/%.proto,$(BUILD)/%.pb.cc,$(PROTO))
PROTO_H := $(PROTO_CC:.cc=.h)
OBJECTS := $(BUILD)/api.o $(BUILD)/plan.o $(BUILD)/image_index.o $(BUILD)/materializer.o $(BUILD)/protocol.o $(BUILD)/dump.o \
	$(PROTO_CC:.cc=.o)
LIB := $(BUILD)/libcriu_provider.a
CXXFLAGS ?= -std=c++20 -Wall -Wextra -Werror -Iinclude -Isrc -I$(BUILD)
GTEST_FLAGS := $(shell pkg-config --cflags --libs gtest_main)

.PHONY: all clean test
all: $(LIB)
$(BUILD):
	mkdir -p $@
$(PROTO_CC) $(PROTO_H): $(PROTO) | $(BUILD)
	protoc --proto_path=proto --cpp_out=$(BUILD) $(PROTO)
$(BUILD)/%.o: src/%.cpp $(PROTO_H) | $(BUILD)
	c++ $(CXXFLAGS) -c $< -o $@
$(BUILD)/%.pb.o: $(BUILD)/%.pb.cc $(PROTO_H)
	c++ $(CXXFLAGS) -c $< -o $@
$(LIB): $(OBJECTS)
	ar rcs $@ $^
$(BUILD)/provider-test: tests/provider_test.cpp $(LIB)
	c++ $(CXXFLAGS) $< $(LIB) -lprotobuf -pthread $(GTEST_FLAGS) -o $@
test: $(BUILD)/provider-test
	$(BUILD)/provider-test
clean:
	rm -rf $(BUILD)
