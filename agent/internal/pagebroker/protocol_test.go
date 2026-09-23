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
	if len(fixtures.Cases) != 21 {
		t.Fatalf("missing wire fixtures: %d", len(fixtures.Cases))
	}
	for _, fixture := range fixtures.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			var expected, decoded proto.Message
			switch fixture.Type {
			case "Request":
				expected, decoded = new(Request), new(Request)
			case "Response":
				expected, decoded = new(Response), new(Response)
			default:
				t.Fatalf("unknown fixture type %q", fixture.Type)
			}
			if err := prototext.Unmarshal([]byte(fixture.Text), expected); err != nil {
				t.Fatal(err)
			}
			wire, err := hex.DecodeString(fixture.WireHex)
			if err != nil {
				t.Fatal(err)
			}
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
			if hex.EncodeToString(encoded) != fixture.WireHex {
				t.Fatal("wire contract changed")
			}
		})
	}
}
