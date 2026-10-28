// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pagebroker

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
)

// Both languages read the same fixed wire vectors. Changing a tag or dropping a
// field must fail here even when a same-version round trip would still succeed.
// The fixture bytes are in field-number order, which is what the C++ encoder
// emits and checks byte for byte. Go emits oneof members after the plain
// fields, so it checks its own encoding by length and by decoding it back.
func TestSharedProtocolFixtures(t *testing.T) {
	data, err := os.ReadFile("../../pagebroker/testdata/storage-contract/wire.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures struct {
		Cases []struct {
			Name, Type, Text string
			WireHex          string `json:"wire_hex"`
		}
	}
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	if len(fixtures.Cases) != 24 {
		t.Fatalf("missing wire fixtures: %d", len(fixtures.Cases))
	}
	for _, fixture := range fixtures.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			newMessage := func() proto.Message {
				switch fixture.Type {
				case "Request":
					return new(Request)
				case "Response":
					return new(Response)
				}
				t.Fatalf("unknown fixture type %q", fixture.Type)
				return nil
			}
			expected := newMessage()
			if err := prototext.Unmarshal([]byte(fixture.Text), expected); err != nil {
				t.Fatal(err)
			}
			wire, err := hex.DecodeString(fixture.WireHex)
			if err != nil {
				t.Fatal(err)
			}
			decoded := newMessage()
			if err := proto.Unmarshal(wire, decoded); err != nil {
				t.Fatal(err)
			}
			if !proto.Equal(expected, decoded) {
				t.Fatalf("decoded fields differ: %v", decoded)
			}
			encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(expected)
			if err != nil {
				t.Fatal(err)
			}
			if len(encoded) != len(wire) {
				t.Fatalf("wire contract changed: encoded %d bytes, fixture %d", len(encoded), len(wire))
			}
			reencoded := newMessage()
			if err := proto.Unmarshal(encoded, reencoded); err != nil {
				t.Fatal(err)
			}
			if !proto.Equal(expected, reencoded) {
				t.Fatalf("re-encoded fields differ: %v", reencoded)
			}
		})
	}
}
